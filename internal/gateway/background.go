// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

// Background engines. An engine with background = true runs whenever the
// card is free: a tagger, an indexer, a transcription queue. The gateway
// starts it at batch priority once the card has been free for the engine's
// idle_after; any other request drains it as it drains any batch lease, and
// it stays out until the card is free that long again, so a game or a hold
// keeps it out for as long as it lasts.

import (
	"context"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/fit"
	"github.com/T-Crypt/berth/internal/node"
)

// DefaultIdleAfter is how long the card must be free before a background
// engine starts, when its entry names no idle_after.
const DefaultIdleAfter = 60 * time.Second

// Background starts the background engines while ctx lives, checking the
// card every `every` (0 = 5 s): queued ComfyUI jobs as soon as the card is
// free, then one background engine at a time, the first in berth.toml whose
// card has been free for its idle_after.
func (g *Gateway) Background(ctx context.Context, every time.Duration) {
	var bg []*config.Engine
	for i := range g.Engines {
		if g.Engines[i].Background {
			bg = append(bg, &g.Engines[i])
		}
	}
	if len(bg) == 0 && g.queueingComfy() == nil {
		return
	}
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	// freeSince is when each engine's cards (its gpu key) were last seen to
	// turn free; a card in use drops its key.
	freeSince := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st := g.nodeStatus()
		// Work someone asked for while the card was out of reach goes
		// first, as soon as the card is free, before any background engine.
		if q := g.queueingComfy(); q != nil && fit.CardFree(node.View(st, q.GPU)) && g.submitting.CompareAndSwap(false, true) {
			go func() {
				defer g.submitting.Store(false)
				g.submitDeferred(ctx)
			}()
		}
		now := time.Now()
		for _, e := range bg {
			// Outside its window the engine stays down: started, it would
			// only sit in the queue all day for a lease berthd cannot grant
			// yet. Its idle clock keeps running (freeSince is the card's,
			// shared by every engine on it), so it starts on the tick its
			// window opens with.
			if !e.Window.Contains(now) {
				continue
			}
			if !fit.CardFree(node.View(st, e.GPU)) {
				delete(freeSince, e.GPU)
				continue
			}
			since, ok := freeSince[e.GPU]
			if !ok {
				since = now
				freeSince[e.GPU] = now
			}
			idle := e.IdleAfter.Duration
			if idle <= 0 {
				idle = DefaultIdleAfter
			}
			if now.Sub(since) < idle {
				continue
			}
			g.log().Info("the card is free; starting a background engine", "engine", e.Name, "gpu", e.GPU, "free_for", now.Sub(since).Round(time.Second))
			delete(freeSince, e.GPU)
			go func() {
				sctx, cancel := context.WithTimeout(ctx, g.ready())
				defer cancel()
				if err := g.ensure(sctx, e, false); err != nil {
					g.log().Warn("background engine did not start", "engine", e.Name, "err", err)
				}
			}()
			break
		}
	}
}
