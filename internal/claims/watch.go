// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package claims

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/proto"
)

// HoldWait is how long a claim's hold waits for the card. A game is already
// running, so the hold keeps asking for as long as the game does; the linger
// ends it when the game is gone.
const HoldWait = 24 * time.Hour

// Broker is the part of the broker the watcher drives.
type Broker interface {
	Acquire(ctx context.Context, r broker.Request, onWait func(string)) (*broker.Lease, error)
	SetClaims(tracked map[int]string, states []proto.Claim)
	Note(e proto.Event)
}

// Watcher scans the process table and keeps the claims of every card on the
// node current. A hold holds the cards its rule names, or every card: the
// process table cannot say which card a game will draw on. Cards[0] notes
// the claim events, since the node's cards share one event log.
type Watcher struct {
	Rules []Rule // compiled
	Every time.Duration
	Scan  func() ([]Proc, error) // nil = Scan
	Cards []Broker
	Log   *slog.Logger

	mu    sync.Mutex
	holds map[string]*hold
}

type hold struct {
	cancel   context.CancelFunc
	lastSeen time.Time
	leases   []string // granted lease id per card, "" while that card waits
	waiting  string   // the last wait reason any card gave
}

// Run scans until ctx ends, then lets every hold go.
func (w *Watcher) Run(ctx context.Context) {
	if w.Every <= 0 {
		w.Every = 2 * time.Second
	}
	if w.Scan == nil {
		w.Scan = Scan
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	w.holds = map[string]*hold{}
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		w.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			w.mu.Lock()
			for _, h := range w.holds {
				h.cancel()
			}
			w.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// Tick runs one scan: holds start for new matches and end past their linger,
// track matches go to the broker.
func (w *Watcher) Tick(ctx context.Context, now time.Time) {
	ps, err := w.Scan()
	if err != nil {
		w.Log.Debug("claims: process scan failed", "err", err)
		return
	}
	matches := Match(w.Rules, ps)
	tracked := map[int]string{}
	var states []proto.Claim

	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.Rules {
		r := &w.Rules[i]
		m := matches[r.Name]
		st := proto.Claim{Name: r.Name, Action: r.Action, Builtin: r.Builtin, Active: len(m) > 0,
			PIDs: pids(m), Procs: names(m)}
		switch r.Action {
		case ActionTrack:
			for _, p := range m {
				tracked[p.PID] = r.Name
			}
		case ActionHold:
			h := w.holds[r.Name]
			switch {
			case len(m) > 0 && h == nil:
				w.start(ctx, r, m, now)
				h = w.holds[r.Name]
			case len(m) > 0:
				h.lastSeen = now
			case h != nil && now.Sub(h.lastSeen) >= r.Linger:
				h.cancel()
				delete(w.holds, r.Name)
				w.Log.Info("claim ended", "claim", r.Name)
				w.Cards[0].Note(proto.Event{Kind: proto.EvClaimEnded, Engine: "claim:" + r.Name,
					Detail: fmt.Sprintf("gone for %s", r.Linger)})
				h = nil
			}
			if h != nil {
				st.Active, st.Lease = true, h.granted()
				if st.Lease == "" && h.waiting != "" {
					st.Procs = strings.TrimSpace(st.Procs + " (waiting: " + h.waiting + ")")
				}
			}
		}
		states = append(states, st)
	}
	for i, b := range w.Cards {
		b.SetClaims(tracked, w.onCard(states, i))
	}
}

// onCard is the claim states as card i reports them: a hold whose rule
// names other cards is not active here. states holds one entry per rule,
// in rule order.
func (w *Watcher) onCard(states []proto.Claim, i int) []proto.Claim {
	out := slices.Clone(states)
	for j, r := range w.Rules {
		if r.Cards != nil && !slices.Contains(r.Cards, i) {
			out[j].Active, out[j].Lease = false, ""
		}
	}
	return out
}

// granted is the lease IDs the hold has been granted so far, comma
// separated. Called with w.mu held.
func (h *hold) granted() string {
	return strings.Join(slices.DeleteFunc(slices.Clone(h.leases), func(id string) bool { return id == "" }), ",")
}

// start asks for the card on the rule's behalf. Called with w.mu held.
func (w *Watcher) start(ctx context.Context, r *Rule, m []Proc, now time.Time) {
	hctx, cancel := context.WithCancel(ctx)
	h := &hold{cancel: cancel, lastSeen: now, leases: make([]string, len(w.Cards))}
	w.holds[r.Name] = h
	who := names(m)
	cards := r.Cards
	if cards == nil {
		for i := range w.Cards {
			cards = append(cards, i)
		}
	}
	w.Log.Info("claim started: holding the card", "claim", r.Name, "procs", who, "cards", len(cards))
	w.Cards[0].Note(proto.Event{Kind: proto.EvClaimStarted, Engine: "claim:" + r.Name, Detail: who})
	req := broker.Request{Engine: "claim:" + r.Name, Group: "claim", Priority: broker.Hold,
		Needs: r.NeedsMiB << 20, Wait: HoldWait, Pinned: true}
	for _, i := range cards {
		b := w.Cards[i]
		go func() {
			l, err := b.Acquire(hctx, req, func(why string) {
				w.mu.Lock()
				h.waiting = why
				w.mu.Unlock()
			})
			if err != nil {
				if hctx.Err() == nil {
					w.Log.Warn("claim hold refused", "claim", r.Name, "err", err)
				}
				return
			}
			w.mu.Lock()
			h.leases[i] = l.ID()
			w.mu.Unlock()
			<-hctx.Done()
			l.Release()
		}()
	}
}

func pids(ps []Proc) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.PID
	}
	sort.Ints(out)
	return out
}

func names(ps []Proc) string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		n := p.Argv0
		if n == "" {
			n = p.Name
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) > 4 {
		out = append(out[:4], fmt.Sprintf("+%d more", len(out)-4))
	}
	return strings.Join(out, ", ")
}
