// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// night is the window the tests use: cheap hours, crossing midnight.
var night = proto.Window{Start: 23 * 60, End: 7 * 60}

// The clock instants the tests pin, in the node's own zone: a window reads
// the wall clock where the node is, and the test clock a broker reads is
// local, as any real one is.
var (
	// outside, 3h12m before the window opens again.
	at1948 = time.Date(2026, 10, 20, 19, 48, 0, 0, time.Local)
	// inside, half an hour after it opened.
	at2330 = time.Date(2026, 10, 20, 23, 30, 0, 0, time.Local)
	// outside, half an hour after it closed.
	at0730 = time.Date(2026, 10, 21, 7, 30, 0, 0, time.Local)
)

// A waiter outside its window waits for its hours: it
// drains nothing, not even a holder of its own priority, and it reads when
// the window opens. Once now is inside, that same waiter is granted.
func TestWindowWaiterWaitsForItsHours(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Batch, Needs: 8 * GiB}))
	clk.set(at1948)

	w := acquireW(b, Request{Engine: "job", Priority: Batch, Needs: 8 * GiB, Window: night})
	pending(t, w.ch)
	select {
	case why := <-holderDrains(b, "comfyui"):
		t.Fatalf("a waiter outside its window drained a holder of its own priority: %s", why)
	default:
	}
	if got, want := firstReason(t, w), "outside its window 23:00-07:00; opens in 3h12m"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}

	holder.Release()
	clk.set(at2330)
	b.Tick(context.Background())
	granted(t, b, "job", w.ch)
}

// A waiter outside its window waits on its hours, not on the card, so it
// holds up nobody: a later batch job with no window takes the card once it
// frees, and the windowed waiter reads its own reason, with no place in line.
func TestWindowWaiterHoldsUpNobody(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	clk.set(at1948)
	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, Needs: 20 * GiB}))

	nightly := acquireW(b, Request{Engine: "nightly", Priority: Batch, Needs: 20 * GiB, Window: night})
	pending(t, nightly.ch)
	if got, want := firstReason(t, nightly), "outside its window 23:00-07:00; opens in 3h12m"; got != want {
		t.Errorf("nightly reason = %q, want %q", got, want)
	}
	embed := acquireW(b, Request{Engine: "embed", Priority: Batch, Needs: 20 * GiB})
	pending(t, embed.ch)

	holder.Release()
	b.Tick(context.Background())
	granted(t, b, "embed", embed.ch)
	pending(t, nightly.ch)
}

// A window close drains its holder at the boundary, as any drain does, and
// asks once. A lease with no window is untouched at the same instant.
func TestWindowCloseDrainsItsHolder(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	closed := must(t, acquire(b, Request{Engine: "trainer", Priority: Batch, Needs: 8 * GiB, Window: night}))
	open := must(t, acquire(b, Request{Engine: "watcher", Priority: Batch, Needs: 8 * GiB}))
	clk.set(at0730)

	b.Tick(context.Background())
	if got, want := drained(t, closed), "its window 23:00-07:00 closed"; got != want {
		t.Errorf("drain reason = %q, want %q", got, want)
	}
	b.Tick(context.Background())
	select {
	case why := <-closed.Drains():
		t.Fatalf("the closed window asked a second time: %s", why)
	default:
	}
	select {
	case why := <-open.Drains():
		t.Fatalf("a lease with no window was drained: %s", why)
	default:
	}
	if st := b.Status(); len(st.Leases) != 2 {
		t.Errorf("leases = %+v, want both still known while the closed one drains", st.Leases)
	}
}

// A parked engine has nothing running to drain, so a window close evicts it
// the way its TTL expiry evicts it: stopped, not resumed.
func TestWindowCloseEvictsAParkedEngine(t *testing.T) {
	tree := parkTree(100, 101)
	b, c, _ := parkSetup(t, tree, 16*GiB, 0)
	// A lease carries its hours from the request that asked for the card, so
	// the clock sits inside them while it warms up and parks.
	b.nowFn = func() time.Time { return at2330 }
	l := warmLeaseAs(t, b, c, Request{Engine: "vllm", Needs: 8 * GiB, Window: night}, 100, 101)
	b.nowFn = func() time.Time { return at0730 }

	b.Tick(context.Background())
	got, want := drained(t, l), "evicted: its window 23:00-07:00 closed"
	if got != want {
		t.Fatalf("eviction reason = %q, want %q", got, want)
	}
	b.Tick(context.Background())
	select {
	case why := <-l.Drains():
		t.Fatalf("the closed window evicted twice: %s", why)
	default:
	}
}

// The reason a waiter reads is the wait to the minute: hours and minutes
// when there are both, one unit when there is only one.
func TestOpensIn(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{20 * time.Second, "0m"}, {30 * time.Second, "1m"}, {45 * time.Minute, "45m"}, {3*time.Hour + 12*time.Minute, "3h12m"},
		{2 * time.Hour, "2h"}, {time.Hour + time.Minute, "1h1m"}, {59 * time.Second, "1m"}} {
		if got := opensIn(tc.d); got != tc.want {
			t.Errorf("opensIn(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// A window is a wait the operator chose: a run started at noon for a night
// window waits for the night, past berthd's default wait, unless the request
// names a deadline of its own.
func TestWindowWaiterOutlastsTheDefaultWait(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	clk.set(at1948)
	b.cfg.DefaultWait = 50 * time.Millisecond

	nightly := acquireW(b, Request{Engine: "nightly", Priority: Batch, Needs: 8 * GiB, Window: night})
	time.Sleep(200 * time.Millisecond)
	pending(t, nightly.ch)

	bounded := acquireW(b, Request{Engine: "bounded", Priority: Batch, Needs: 8 * GiB, Window: night, Wait: 50 * time.Millisecond})
	select {
	case r := <-bounded.ch:
		if r.err == nil {
			t.Fatal("a windowed request with its own deadline was granted outside its window")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a windowed request with its own deadline waited past it")
	}
}
