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
	"slices"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

// DefaultIdleAfter is how long the card must be free before a background
// engine starts, when its entry names no idle_after.
const DefaultIdleAfter = 60 * time.Second

// cardFree is whether nothing holds, uses or waits for the card and the
// card serves this machine. A parked lease holds it: a batch job must not
// evict an engine someone parked in RAM to come back to. A stowaway or a
// tracked app (Blender) holds memory no lease accounts for, and a start
// beside it is the collision Berth exists to prevent.
func cardFree(st *proto.Status) bool {
	if st == nil || st.Passthrough != nil || st.ProbeErr != "" ||
		len(st.Leases) > 0 || len(st.Waiting) > 0 || len(st.Squatters) > 0 || cordoned(st) {
		return false
	}
	// Status lists every claim rule; only one matching right now is in use.
	return !slices.ContainsFunc(st.Claims, func(c proto.Claim) bool { return c.Active })
}

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
		if q := g.queueingComfy(); q != nil && cardFree(node.View(st, q.GPU)) && g.submitting.CompareAndSwap(false, true) {
			go func() {
				defer g.submitting.Store(false)
				g.submitDeferred(ctx)
			}()
		}
		now := time.Now()
		for _, e := range bg {
			if !cardFree(node.View(st, e.GPU)) {
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
