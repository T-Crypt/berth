// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
)

// holderTree is comfyui (100 -> 101) and ninfer (102 -> 103).
func holderTree() procs.Fake {
	return procs.Fake{
		Parents: map[int]int{101: 100, 100: 1, 103: 102, 102: 1},
		Starts:  map[int]uint64{100: 1, 101: 1, 102: 1, 103: 1},
	}
}

// comfyRAM puts comfyui's run (root 100) in comfyScope and has the scope
// read *mem at each tick, so a test moves the reading between ticks.
func comfyRAM(b *Broker, mem *procs.Mem) {
	b.cfg.Scopes = func(pid int) (string, bool) { return comfyScope, pid == 100 }
	b.cfg.ScopeMem = func(s string) (procs.Mem, bool) { return *mem, s == comfyScope }
}

// A render that peaked at 40 GiB and dropped back to 4 GiB between stages
// will climb again: a park admitted on the 42 GiB that looks free now is
// the one the kernel kills a minute later. The holder's peak stays booked.
func TestParkBooksTheHoldersPeakRAMThisRun(t *testing.T) {
	b, c, _ := parkSetup(t, holderTree(), 46*GiB, 0)
	mem := procs.Mem{Anon: 40 * GiB}
	comfyRAM(b, &mem)

	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground, Needs: 4 * GiB}))
	comfy.Attach(100)
	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Needs: 18 * GiB}))
	ninfer.Attach(102)
	c.set(gpu.Proc{PID: 101, Name: "comfyui", Used: 4 * GiB}, gpu.Proc{PID: 103, Name: "ninfer", Used: 18 * GiB})
	b.Tick(context.Background())
	mem = procs.Mem{Anon: 4 * GiB}
	b.Tick(context.Background())

	w := acquire(b, Request{Engine: "next", Priority: Foreground, Needs: 18 * GiB})
	pending(t, w)
	drained(t, ninfer)
	ok, why := ninfer.Park()
	want := "park refused: needs 18.0 GiB, 9.0 GiB available above the floor; 36.0 GiB more is kept for comfyui, which has held 40.0 GiB"
	if ok || why != want {
		t.Fatalf("park = %v %q, want refused: %q", ok, why, want)
	}
}

// The next run of a RAM-heavy engine is booked at its last runs' peak
// while it waits: the park that makes room on the card for a video render
// must not take the RAM the render is about to use.
func TestParkBooksAWaitersLearnedPeakRAM(t *testing.T) {
	b, c, _ := parkSetup(t, holderTree(), 46*GiB, 0)
	mem := procs.Mem{Anon: 40 * GiB}
	comfyRAM(b, &mem)

	first := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground, Needs: 20 * GiB}))
	first.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "comfyui", Used: 20 * GiB})
	b.Tick(context.Background())
	first.Release()
	c.set()
	b.Tick(context.Background())
	mem = procs.Mem{Anon: 1 * GiB}

	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Needs: 18 * GiB}))
	ninfer.Attach(102)
	c.set(gpu.Proc{PID: 103, Name: "ninfer", Used: 18 * GiB})
	b.Tick(context.Background())

	w := acquire(b, Request{Engine: "comfyui", Priority: Foreground, Needs: 20 * GiB})
	pending(t, w)
	drained(t, ninfer)
	if ok, why := ninfer.Park(); ok || !strings.Contains(why, "comfyui") {
		t.Fatalf("park = %v %q, want refused for the 40 GiB comfyui's runs reach", ok, why)
	}

	b.cfg.MemAvailable = func() uint64 { return 80 * GiB }
	if ok, why := ninfer.Park(); !ok {
		t.Fatalf("park with room for both refused: %s", why)
	}
}
