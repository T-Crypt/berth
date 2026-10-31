// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
)

// startTurnsBroker is startTurnBroker with a round-robin policy.
func startTurnsBroker(t *testing.T, p Turns) (*Broker, *minClock) {
	t.Helper()
	b, clk := startTurnBroker(t, procs.Map{})
	b.cfg.Turns = p
	return b, clk
}

// With no [turns] policy a higher-priority holder keeps the card against a
// lower-priority waiter however long it holds it: the behaviour before turns.
func TestNoPolicyKeepsAHigherPriorityHolderForever(t *testing.T) {
	b, clk := startTurnsBroker(t, Turns{})
	must(t, acquire(b, Request{Engine: "llm", Priority: Foreground}))
	clk.set(turnBase.Add(24 * time.Hour))

	w := acquireW(b, Request{Engine: "image", Priority: Batch})
	pending(t, w.ch)
	select {
	case why := <-holderDrains(b, "llm"):
		t.Fatalf("drained with no policy: %s", why)
	default:
	}
	if r := firstReason(t, w); !strings.Contains(r, "held by") {
		t.Errorf("reason = %q, want a holder named", r)
	}
}

// Past its maximum turn a holder no longer outranks a waiting lower priority.
func TestMaxTurnLetsALowerPriorityWaiterDrainTheHolder(t *testing.T) {
	b, clk := startTurnsBroker(t, Turns{MaxTurn: 10 * time.Minute})
	holder := must(t, acquire(b, Request{Engine: "llm", Priority: Foreground}))

	w := acquireW(b, Request{Engine: "image", Priority: Batch})
	pending(t, w.ch)
	clk.set(turnBase.Add(9 * time.Minute))
	b.Tick(t.Context())
	select {
	case why := <-holderDrains(b, "llm"):
		t.Fatalf("drained before the maximum turn: %s", why)
	default:
	}

	clk.set(turnBase.Add(10 * time.Minute))
	b.Tick(t.Context())
	if why := drained(t, holder); !strings.Contains(why, "image") {
		t.Fatalf("drain reason = %q, want the waiter named", why)
	}
}

// The per-engine maximum turn overrides the default, and a hold has none.
func TestMaxTurnByEngineAndHoldsAreExempt(t *testing.T) {
	b, clk := startTurnsBroker(t, Turns{MaxTurn: time.Hour, MaxTurnBy: map[string]time.Duration{"llm": time.Minute}})
	holder := must(t, acquire(b, Request{Engine: "llm", Priority: Foreground}))
	clk.set(turnBase.Add(2 * time.Minute))
	w := acquireW(b, Request{Engine: "image", Priority: Batch})
	pending(t, w.ch)
	if why := drained(t, holder); !strings.Contains(why, "image") {
		t.Fatalf("drain reason = %q", why)
	}

	b2, clk2 := startTurnsBroker(t, Turns{MaxTurn: time.Minute})
	must(t, acquire(b2, Request{Engine: "game", Priority: Hold, Pinned: true}))
	clk2.set(turnBase.Add(time.Hour))
	w2 := acquireW(b2, Request{Engine: "image", Priority: Batch})
	pending(t, w2.ch)
	select {
	case why := <-holderDrains(b2, "game"):
		t.Fatalf("a hold was drained: %s", why)
	default:
	}
}

// Of two waiters at one priority, the cycle serves the engine after the last
// one granted first, not the earlier arrival.
func TestCycleServesTheNextEngineFirst(t *testing.T) {
	b, _ := startTurnsBroker(t, Turns{Cycle: []string{"a", "b", "c"}})
	first := must(t, acquire(b, Request{Engine: "a", Priority: Interactive}))

	wc := acquireW(b, Request{Engine: "c", Priority: Interactive})
	pending(t, wc.ch)
	// b arrives after c, but is next in the cycle after a.
	wb := acquireW(b, Request{Engine: "b", Priority: Interactive})
	pending(t, wb.ch)
	first.Release()
	b.Tick(t.Context())
	must(t, wb.ch)
	pending(t, wc.ch)
}

// Without a cycle the same waiters are served by arrival.
func TestNoCycleServesByArrival(t *testing.T) {
	b, _ := startTurnsBroker(t, Turns{})
	first := must(t, acquire(b, Request{Engine: "a", Priority: Interactive}))
	wc := acquireW(b, Request{Engine: "c", Priority: Interactive})
	pending(t, wc.ch)
	wb := acquireW(b, Request{Engine: "b", Priority: Interactive})
	pending(t, wb.ch)
	first.Release()
	b.Tick(t.Context())
	must(t, wc.ch)
	pending(t, wb.ch)
}

// With preemption off, a higher priority waits out a minimum turn.
func TestPreemptionOffWaitsOutTheTurn(t *testing.T) {
	b, clk := startTurnsBroker(t, Turns{})
	must(t, acquire(b, Request{Engine: "image", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(10 * time.Second))
	w := acquireW(b, Request{Engine: "llm", Priority: Foreground})
	pending(t, w.ch)
	select {
	case why := <-holderDrains(b, "image"):
		t.Fatalf("drained inside the minimum turn: %s", why)
	default:
	}
}

// With preemption on, a higher priority cuts a minimum turn; an equal one
// still waits it out.
func TestPreemptionCutsAMinimumTurnForAHigherPriorityOnly(t *testing.T) {
	b, clk := startTurnsBroker(t, Turns{Preempt: true})
	holder := must(t, acquire(b, Request{Engine: "image", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(10 * time.Second))

	eq := acquireW(b, Request{Engine: "other", Priority: Interactive})
	pending(t, eq.ch)
	select {
	case why := <-holderDrains(b, "image"):
		t.Fatalf("an equal priority preempted: %s", why)
	default:
	}
	if r := firstReason(t, eq); !strings.Contains(r, "minimum turn") {
		t.Errorf("reason = %q", r)
	}

	hi := acquireW(b, Request{Engine: "llm", Priority: Foreground})
	pending(t, hi.ch)
	if why := drained(t, holder); !strings.Contains(why, "llm") {
		t.Fatalf("drain reason = %q, want the preempting waiter named", why)
	}
}

// A second preemption inside the gap is refused with the time left, and is
// allowed again once the gap has passed.
func TestPreemptionIsLimitedToOncePerGap(t *testing.T) {
	b, clk := startTurnsBroker(t, Turns{Preempt: true, PreemptGap: time.Minute})
	h1 := must(t, acquire(b, Request{Engine: "image", Priority: Batch, MinTurn: 10 * time.Minute}))
	clk.set(turnBase.Add(5 * time.Second))
	w1 := acquireW(b, Request{Engine: "llm", Priority: Interactive, MinTurn: 10 * time.Minute})
	pending(t, w1.ch)
	drained(t, h1) // preemption number one
	h1.Release()
	b.Tick(t.Context())
	must(t, w1.ch)

	clk.set(turnBase.Add(15 * time.Second))
	w2 := acquireW(b, Request{Engine: "urgent", Priority: Foreground})
	pending(t, w2.ch)
	select {
	case why := <-holderDrains(b, "llm"):
		t.Fatalf("a second preemption inside the gap: %s", why)
	default:
	}
	if r := firstReason(t, w2); !strings.Contains(r, "next preemption is allowed in 50s") {
		t.Errorf("reason = %q, want the time left", r)
	}

	clk.set(turnBase.Add(66 * time.Second))
	b.Tick(t.Context())
	select {
	case <-holderDrains(b, "llm"):
	case <-time.After(2 * time.Second):
		t.Fatal("no preemption after the gap")
	}
}
