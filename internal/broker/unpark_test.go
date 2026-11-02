// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// berth unpark and berth park off, through the lease table: unpark evicts
// only parked leases (by name or all), a running engine is never touched,
// park off refuses new parks and evicts what is parked, park on restores.

import (
	"context"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/proto"
)

func drainReason(t *testing.T, l *Lease) string {
	t.Helper()
	select {
	case why := <-l.Drains():
		return why
	default:
		t.Fatal("no drain asked of the parked lease")
	}
	return ""
}

func TestUnparkEvictsTheNamedParkedEngine(t *testing.T) {
	tree := parkTree(100, 101)
	b, c, _ := parkSetup(t, tree, 32*GiB, 0)
	l := warmLease(t, b, c, "ninfer", 8*GiB, 100, 101)
	if got := b.Unpark("someone-else"); len(got) != 0 {
		t.Fatalf("unpark of another engine = %v, want nothing", got)
	}
	got := b.Unpark("ninfer")
	if len(got) != 1 || got[0] != l.ID() {
		t.Fatalf("unpark = %v, want [%s]", got, l.ID())
	}
	if why := drainReason(t, l); !strings.Contains(why, "unparked by berth unpark") {
		t.Fatalf("drain reason = %q", why)
	}
	if st := b.Status(); st.Leases[0].State != "draining" {
		t.Fatalf("state = %s, want draining", st.Leases[0].State)
	}
	evs, _, _ := b.Events(0, false)
	if evs[len(evs)-1].Kind != proto.EvParkEvicted {
		t.Fatalf("last event = %+v, want park_evicted", evs[len(evs)-1])
	}
}

func TestUnparkLeavesARunningEngineAlone(t *testing.T) {
	tree := parkTree(100, 101)
	b, c, _ := parkSetup(t, tree, 32*GiB, 0)
	l := must(t, acquire(b, Request{Engine: "comfyui", Needs: 8 * GiB}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "comfyui-engine", Used: 8 * GiB})
	b.Tick(context.Background())
	if got := b.Unpark(""); len(got) != 0 {
		t.Fatalf("unpark --all = %v, want nothing (the engine is active, not parked)", got)
	}
	select {
	case why := <-l.Drains():
		t.Fatalf("active lease was asked to drain: %q", why)
	default:
	}
}

func TestParkOffRefusesAndEvictsAndOnRestores(t *testing.T) {
	tree := parkTree(100, 101)
	b, c, _ := parkSetup(t, tree, 32*GiB, 0)
	l := warmLease(t, b, c, "ninfer", 8*GiB, 100, 101)
	got := b.SetParkOff(true)
	if len(got) != 1 || got[0] != l.ID() || !b.ParkOff() || !b.Status().ParkOff {
		t.Fatalf("park off = %v (off %v)", got, b.ParkOff())
	}
	if why := drainReason(t, l); !strings.Contains(why, "parking switched off") {
		t.Fatalf("drain reason = %q", why)
	}

	// A new drain while parking is off is refused a park.
	tree.Parents[102], tree.Parents[103] = 103, 1
	tree.Starts[102], tree.Starts[103] = 1, 1
	m := must(t, acquire(b, Request{Engine: "sglang", Needs: 4 * GiB}))
	m.Attach(103)
	c.set(gpu.Proc{PID: 102, Name: "sglang-engine", Used: 4 * GiB})
	b.Tick(context.Background())
	w := acquire(b, Request{Engine: "next2", Priority: Foreground})
	pending(t, w)
	drained(t, m)
	if ok, why := m.Park(); ok || !strings.Contains(why, "parking is switched off") {
		t.Fatalf("park = %v %q, want refused while off", ok, why)
	}

	if got := b.SetParkOff(false); len(got) != 0 || b.ParkOff() {
		t.Fatalf("park on = %v (off %v)", got, b.ParkOff())
	}
}
