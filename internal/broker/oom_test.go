// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
)

// The tests' process trees name made-up PIDs, so no test may write a real
// /proc/PID/oom_score_adj.
func TestMain(m *testing.M) {
	setOOMScoreAdj = func(int, int) (int, error) { return 0, errors.ErrUnsupported }
	os.Exit(m.Run())
}

// oomTable is the process table of a run: wrapper 100, engine 101.
func oomTable(b *Broker) {
	b.cfg.ProcTable = func() []procs.Proc {
		return []procs.Proc{{PID: 100, Parent: 1, Start: 1}, {PID: 101, Parent: 100, Start: 1}}
	}
}

// adjs records each process's oom_score_adj as the broker sets it.
func adjs(b *Broker, start map[int]int) map[int]int {
	b.cfg.OOMScoreAdj = func(pid, adj int) (int, error) {
		old, ok := start[pid]
		if !ok {
			return 0, os.ErrNotExist
		}
		start[pid] = adj
		return old, nil
	}
	return start
}

// A parked engine is the first thing the kernel or earlyoom kills when RAM
// runs out, ahead of the render that pushed RAM there; a resume gives each
// process its own score back.
func TestParkedEngineIsFirstForTheOOMKillerUntilItResumes(t *testing.T) {
	b, c, _ := parkSetup(t, parkTree(100, 101), 32*GiB, 0)
	oomTable(b)
	adj := adjs(b, map[int]int{100: 0, 101: 200})
	warmLease(t, b, c, "ninfer", 8*GiB, 100, 101)
	if adj[100] != 1000 || adj[101] != 1000 {
		t.Fatalf("parked: oom_score_adj = %v, want both 1000", adj)
	}

	if err := b.Resume("ninfer", false); err != nil {
		t.Fatal(err)
	}
	if adj[100] != 0 || adj[101] != 200 {
		t.Fatalf("resumed: oom_score_adj = %v, want 100:0 101:200", adj)
	}
}

// An engine a client woke behind Berth's back is a running engine again,
// with its own score.
func TestAParkedEngineLoadedBackGetsItsScoreBack(t *testing.T) {
	b, c, _ := parkSetup(t, parkTree(100, 101), 32*GiB, 0)
	oomTable(b)
	adj := adjs(b, map[int]int{100: 0, 101: 200})
	warmLease(t, b, c, "ninfer", 8*GiB, 100, 101)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-engine", Used: 8 * GiB})
	b.Tick(context.Background())
	if adj[100] != 0 || adj[101] != 200 {
		t.Fatalf("loaded back: oom_score_adj = %v, want 100:0 101:200", adj)
	}
}

// An evicted engine is stopping: it stays first.
func TestAnEvictedEngineStaysFirst(t *testing.T) {
	b, c, _ := parkSetup(t, parkTree(100, 101), 32*GiB, 0)
	oomTable(b)
	adj := adjs(b, map[int]int{100: 0, 101: 200})
	warmLease(t, b, c, "ninfer", 8*GiB, 100, 101)
	b.Unpark("ninfer")
	if adj[100] != 1000 || adj[101] != 1000 {
		t.Fatalf("evicted: oom_score_adj = %v, want both 1000", adj)
	}
}
