// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Round-robin turns. Everything here is off until the [turns] table sets it,
// and with it off the broker decides exactly as before.
//
// A turn is the time one lease holds the card. Three rules shape it, none of
// which ever cuts a request off (a drain still starts at the slot or job
// boundary):
//
//   - A maximum turn. A holder past it stops being protected by its priority:
//     a waiter of any lower priority may drain it, so a busy high-priority
//     engine cannot keep the card from a waiting one indefinitely. A pinned
//     lease and a hold are never drained.
//   - Rotation. Waiters of one priority are served in the order of a
//     configured cycle, starting with the engine after the last one granted,
//     instead of strictly by arrival.
//   - Preemption. A waiter of a strictly higher priority may drain a holder
//     inside its minimum turn, at most once per preemption gap, since each
//     such drain costs the engine it empties a cold reload.

import (
	"fmt"
	"slices"
	"time"
)

// DefaultPreemptGap is the gap between two preemptions when Preempt is on
// and no gap is set.
const DefaultPreemptGap = 45 * time.Second

// Turns is the round-robin policy. The zero value is off.
type Turns struct {
	// MaxTurn is the longest a holder keeps the card against a waiter of
	// lower priority; 0 = no limit. MaxTurnBy overrides it per engine key
	// (a lease's group, else its engine name).
	MaxTurn   time.Duration
	MaxTurnBy map[string]time.Duration
	// Cycle orders the waiters of one priority by engine key: the one after
	// the last granted goes first. Keys not listed go after the listed ones,
	// in arrival order. Empty = arrival order.
	Cycle []string
	// Preempt lets a waiter of a higher priority drain a holder inside its
	// minimum turn, once per PreemptGap (0 = DefaultPreemptGap).
	Preempt    bool
	PreemptGap time.Duration
}

func (t Turns) maxTurn(key string) time.Duration {
	if d, ok := t.MaxTurnBy[key]; ok {
		return d
	}
	return t.MaxTurn
}

func (t Turns) preemptGap() time.Duration {
	if t.PreemptGap > 0 {
		return t.PreemptGap
	}
	return DefaultPreemptGap
}

// turnExpired is whether h has held the card past its maximum turn. A hold
// has no maximum turn.
func (b *Broker) turnExpired(h *lease, now time.Time) bool {
	if h.prio == Hold || h.pinned || h.granted.IsZero() {
		return false
	}
	max := b.cfg.Turns.maxTurn(h.key())
	return max > 0 && now.Sub(h.granted) >= max
}

// preemptWait is how long until a preemption is allowed again; 0 = now.
func (b *Broker) preemptWait(now time.Time) time.Duration {
	if b.preemptedAt.IsZero() {
		return 0
	}
	return max(0, b.cfg.Turns.preemptGap()-now.Sub(b.preemptedAt))
}

// preempts says whether waiter w may drain h inside h's minimum turn, and if
// not, why (empty when preemption does not apply to this pair at all).
func (b *Broker) preempts(h, w *lease, now time.Time) (bool, string) {
	if !b.cfg.Turns.Preempt || w.prio <= h.prio {
		return false, ""
	}
	if left := b.preemptWait(now); left > 0 {
		return false, fmt.Sprintf("a turn was preempted %s ago; the next preemption is allowed in %s",
			now.Sub(b.preemptedAt).Round(time.Second), left.Round(time.Second))
	}
	return true, ""
}

// notePreemption records a drain that cut a minimum turn short.
func (b *Broker) notePreemption(now time.Time) {
	b.preemptedAt = now
}

// cycleRank is where an engine key stands in the rotation: 0 is next after
// the last key granted. Keys outside the cycle rank after every listed one.
func (b *Broker) cycleRank(key string) int {
	c := b.cfg.Turns.Cycle
	i := slices.Index(c, key)
	if i < 0 {
		return len(c)
	}
	last := slices.Index(c, b.lastKey)
	if last < 0 {
		return i
	}
	return (i - last - 1 + len(c)) % len(c)
}
