// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
)

// minClock is a pinned clock the test moves. The broker reads it on every
// now(); the test is the only writer and moves it through the atomic, so a
// broker goroutine and the test never race on the time.
type minClock struct{ at atomic.Int64 }

func (c *minClock) set(t time.Time) { c.at.Store(t.UnixNano()) }

func (c *minClock) now() time.Time { return time.Unix(0, c.at.Load()) }

func newMinClock(base time.Time) *minClock {
	c := &minClock{}
	c.set(base)
	return c
}

// turnBase is the pinned grant instant in the tests.
var turnBase = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// waitAcquire is acquire with a catcher for the wait reasons.
type waitAcquire struct {
	ch      chan result
	reasons chan string
}

func acquireW(b *Broker, r Request) *waitAcquire {
	w := &waitAcquire{ch: make(chan result, 1), reasons: make(chan string, 16)}
	go func() {
		l, err := b.Acquire(context.Background(), r, func(reason string) {
			select {
			case w.reasons <- reason:
			default:
			}
		})
		w.ch <- result{l, err}
	}()
	return w
}

func firstReason(t *testing.T, w *waitAcquire) string {
	t.Helper()
	select {
	case r := <-w.reasons:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no wait reason arrived")
	}
	return ""
}

// startTurnBroker pins the broker to a clock at the base instant, before
// any goroutine reads it, and the caller moves the clock afterwards.
func startTurnBroker(t *testing.T, tree procs.Tree) (*Broker, *minClock) {
	t.Helper()
	b, _, _ := setup(t, tree)
	clk := newMinClock(turnBase)
	b.nowFn = clk.now
	return b, clk
}

// A minimum turn keeps the card against a waiter of the same priority: no
// drain goes out, and the waiter is told how much of the turn is left.
func TestMinTurnKeepsTheCardForAnEqualPriorityWaiter(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(30 * time.Second))

	w := acquireW(b, Request{Engine: "strata", Priority: Interactive})
	pending(t, w.ch)
	select {
	case why := <-holderDrains(b, "comfyui"):
		t.Fatalf("the holder was drained inside its minimum turn: %s", why)
	default:
	}
	if got, want := firstReason(t, w), "next in line: comfyui has its minimum turn, 4m30s left"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// A higher-priority request waits out the turn too; only a hold takes the
// card out before the turn ends.
func TestMinTurnKeepsTheCardForAHigherPriorityWaiter(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(30 * time.Second))

	w := acquireW(b, Request{Engine: "strata", Priority: Foreground})
	pending(t, w.ch)
	if got, want := firstReason(t, w), "next in line: comfyui has its minimum turn, 4m30s left"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// A lower-priority request waits out the turn as well, as it waits out any
// higher-priority holder: it is blocked, never a drain victim.
func TestMinTurnKeepsTheCardForALowerPriorityWaiter(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(30 * time.Second))

	w := acquireW(b, Request{Engine: "batchjob", Priority: Batch})
	pending(t, w.ch)
	select {
	case why := <-holderDrains(b, "comfyui"):
		t.Fatalf("the holder was drained inside its minimum turn: %s", why)
	default:
	}
	reason := firstReason(t, w)
	if !strings.Contains(reason, "held by comfyui") {
		t.Errorf("reason = %q, want the holder named", reason)
	}
}

// A hold takes the card out of a minimum turn at once: the holder is
// drained mid-turn, and the hold is granted when the card frees.
func TestHoldTakesTheCardOutOfAMinimumTurn(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(30 * time.Second))

	ch := acquire(b, Request{Engine: "hold", Priority: Hold, Pinned: true})
	pending(t, ch)
	if why := drained(t, holder); !strings.Contains(why, "hold") {
		t.Fatalf("drain reason %q does not name the hold", why)
	}
	holder.Release()
	must(t, ch)
}

// When the turn ends the normal rules apply: the drain starts, and the
// waiter is granted only once the card shows the holder's memory gone.
func TestMinTurnEndsAndTheWaiterIsGrantedOnTheNormalDrain(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100})
	clk := newMinClock(turnBase)
	b.nowFn = clk.now

	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute}))
	holder.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "comfyui", Used: 20 << 30})
	b.Tick(context.Background())
	clk.set(turnBase.Add(30 * time.Second))

	w := acquireW(b, Request{Engine: "strata", Priority: Interactive})
	pending(t, w.ch)
	if got, want := firstReason(t, w), "next in line: comfyui has its minimum turn, 4m30s left"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}

	clk.set(turnBase.Add(5*time.Minute + 10*time.Second))
	b.Tick(context.Background())
	if why := drained(t, holder); !strings.Contains(why, "strata") {
		t.Fatalf("after the turn ends the holder is not drained for the waiter: %q", why)
	}
	holder.Release()
	pending(t, w.ch) // wrapper gone, memory not yet
	c.set()
	b.Tick(context.Background())
	must(t, w.ch)
}

// A lease with no minimum turn is drained at once, as before the feature.
func TestNoMinimumTurnDrainsAtOnce(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive}))

	w := acquireW(b, Request{Engine: "strata", Priority: Interactive})
	pending(t, w.ch)
	if why := drained(t, holder); !strings.Contains(why, "strata") {
		t.Fatalf("drain reason = %q", why)
	}
}

// The status carries the seconds of the turn a lease has left, and nothing
// for a lease with no turn.
func TestStatusCarriesTheRemainingTurn(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute}))
	clk.set(turnBase.Add(30 * time.Second))
	b.Tick(context.Background())

	st := b.Status()
	if len(st.Leases) != 1 || st.Leases[0].Engine != "comfyui" {
		t.Fatalf("leases = %+v", st.Leases)
	}
	if got := st.Leases[0].TurnS; got != 270 {
		t.Errorf("turn_s = %.1f, want 270 (4m30s left of the 5m turn)", got)
	}

	holder.Release()
	must(t, acquire(b, Request{Engine: "llama", Priority: Interactive}))
	st = b.Status()
	if st.Leases[0].TurnS != 0 {
		t.Errorf("turn_s = %.1f, want 0 for a lease with no turn", st.Leases[0].TurnS)
	}
}

// holderDrains exposes a holder's drain channel by engine, for the tests
// that hold on to nothing but the name.
func holderDrains(b *Broker, engine string) <-chan string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.leases {
		if l.engine == engine {
			return l.drainCh
		}
	}
	return nil
}

// A re-attach after a daemon restart keeps the turn the engine was in: a
// deploy that restarts berthd must not hand every holder a fresh minimum turn.
func TestReattachKeepsTheTurnItWasIn(t *testing.T) {
	b, _, _ := setup(t, procs.Fake{Starts: map[int]uint64{100: 7}})
	clk := newMinClock(turnBase)
	b.nowFn = clk.now
	if _, err := b.Reattach(Request{Engine: "comfyui", Priority: Interactive, MinTurn: 5 * time.Minute, Held: 4 * time.Minute}, 100, 7); err != nil {
		t.Fatal(err)
	}
	clk.set(turnBase.Add(30 * time.Second))

	w := acquireW(b, Request{Engine: "strata", Priority: Interactive})
	pending(t, w.ch)
	if got, want := firstReason(t, w), "next in line: comfyui has its minimum turn, 30s left"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}
