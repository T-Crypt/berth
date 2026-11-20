// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runs"
)

// #300: a refused re-attach was only on the wrapper's stderr, so the daemon's
// log showed an engine that never came back and no reason why.
func TestReattachRefusalIsLogged(t *testing.T) {
	b, _, _ := setup(t, procs.Fake{Starts: map[int]uint64{100: 7}})
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := b.Reattach(Request{Engine: "qwen-3.8-27b"}, 100, 8); err == nil {
		t.Fatal("a re-attach with the wrong start time was granted")
	}
	got := buf.String()
	for _, want := range []string{`msg="re-attach refused"`, "engine=qwen-3.8-27b", "pid=100", "is not the process that started"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log = %q, want it to contain %q", got, want)
		}
	}
}

// lostRun puts an unleased qwen-3.8-27b on the card: pid 101 under a berth
// run (pid 100) that lost its daemon and did not re-attach in the window.
func lostRun(t *testing.T) (*Broker, *card) {
	t.Helper()
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	b, c, _ := setup(t, tree)
	b.cfg.RunsDir = t.TempDir()
	b.cfg.ReattachWindow = time.Nanosecond
	if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
		Engine: "qwen-3.8-27b", PID: 100, Start: 1, Started: time.Now(), Reason: runs.ReasonLost,
	}); err != nil {
		t.Fatal(err)
	}
	c.set(gpu.Proc{PID: 101, Name: "llama-server", Used: 14 * GiB})
	b.Tick(context.Background())
	return b, c
}

// refusal is the reason a sized waiter is refused with.
func refusal(t *testing.T, b *Broker, r Request) string {
	t.Helper()
	r.Wait = 200 * time.Millisecond
	_, err := b.Acquire(context.Background(), r, nil)
	if err == nil {
		t.Fatal("granted onto a card its own unleased run fills")
	}
	return err.Error()
}

// #300: a waiter for the engine whose own orphan fills the card waits on
// itself. The refusal says so, so whoever retries it can stop the orphan
// instead of asking again forever.
func TestRefusalNamesTheWaitersOwnUnleasedRun(t *testing.T) {
	const want = "14336 MiB of that is an unleased qwen-3.8-27b (pid 101)"
	t.Run("no holders", func(t *testing.T) {
		b, _ := lostRun(t)
		if got := refusal(t, b, Request{Engine: "qwen-3.8-27b", Needs: 14 * GiB}); !strings.Contains(got, want) {
			t.Fatalf("refusal = %q, want it to contain %q", got, want)
		}
	})
	t.Run("a holder that cannot drain", func(t *testing.T) {
		b, _ := lostRun(t)
		must(t, acquire(b, Request{Engine: "game", Priority: Hold, Needs: 2 * GiB}))
		if got := refusal(t, b, Request{Engine: "qwen-3.8-27b", Needs: 14 * GiB}); !strings.Contains(got, want) {
			t.Fatalf("refusal = %q, want it to contain %q", got, want)
		}
	})
	t.Run("another engine's waiter", func(t *testing.T) {
		b, _ := lostRun(t)
		if got := refusal(t, b, Request{Engine: "gemma", Needs: 14 * GiB}); strings.Contains(got, "unleased") {
			t.Fatalf("refusal = %q names a run that is not the waiter's", got)
		}
	})
}

// #354: a restarted daemon granted a second whole-card lease while the
// first run's engine was still alive. Its berth run had not yet noticed the
// lost socket, so no unprotected record existed. Every run now records its
// engine from the moment the child starts, so a fresh daemon holds the card
// until the run re-attaches or the window closes, whatever its berth run is
// doing.
func TestLiveRunHoldsTheCardUntilItReattaches(t *testing.T) {
	b, _, _ := setup(t, procs.Fake{Starts: map[int]uint64{100: 1, 99: 5}})
	b.cfg.RunsDir = t.TempDir()
	b.cfg.ReattachWindow = time.Hour
	// No reason: its berth run still believes the lease is live.
	if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
		Engine: "qualification-active", PID: 100, Start: 1,
		Wrapper: 99, WrapperStart: 5, Started: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	b.Tick(context.Background())

	_, err := b.Acquire(context.Background(),
		Request{Engine: "qualification-next", Pinned: true, Wait: 100 * time.Millisecond}, nil)
	if err == nil {
		t.Fatal("granted a second whole-card lease while the first run's engine is alive")
	}
	if !strings.Contains(err.Error(), "berthd restarted") || !strings.Contains(err.Error(), "1 running engine") {
		t.Fatalf("refusal = %q, want the restart window named", err)
	}

	// Once it re-attaches, its own lease is what keeps the contender out.
	if _, err := b.Reattach(Request{Engine: "qualification-active", Pinned: true}, 100, 1); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	_, err = b.Acquire(context.Background(),
		Request{Engine: "qualification-next", Pinned: true, Wait: 100 * time.Millisecond}, nil)
	if err == nil {
		t.Fatal("granted beside a live pinned lease")
	}
	if strings.Contains(err.Error(), "berthd restarted") {
		t.Fatalf("refusal = %q; the re-attach window should not be the reason any more", err)
	}
}

// A record for a run whose lease is live holds nothing: the engine is
// accounted for by the lease, and the daemon grants as usual.
func TestRecordForALeasedRunHoldsNothing(t *testing.T) {
	b, _, _ := setup(t, procs.Fake{Starts: map[int]uint64{100: 1}})
	b.cfg.RunsDir = t.TempDir()
	b.cfg.ReattachWindow = time.Hour

	l := must(t, acquire(b, Request{Engine: "a", Priority: Batch, Needs: GiB}))
	l.Attach(100)
	if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
		Engine: "a", PID: 100, Start: 1, Started: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	b.Tick(context.Background())
	if n := b.reattaching(b.now()); n != 0 {
		t.Fatalf("a leased run counts as due back %d time(s)", n)
	}

	must(t, acquire(b, Request{Engine: "b", Priority: Batch, Needs: GiB}))
}

// #300: an engine whose berth run has exited has nobody left to re-attach
// it, and one whose berth run is alive should have. The daemon could not
// tell the two apart, so the report could not say which one happened.
func TestOrphanSaysWhetherItsBerthRunIsAlive(t *testing.T) {
	orphan := func(t *testing.T, starts map[int]uint64, wrapper int) (*Broker, *bytes.Buffer) {
		t.Helper()
		starts[100] = 1
		b, c, _ := setup(t, procs.Fake{Starts: starts})
		var buf bytes.Buffer
		b.log = slog.New(slog.NewTextHandler(&buf, nil))
		b.cfg.RunsDir = t.TempDir()
		b.cfg.ReattachWindow = time.Nanosecond
		if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
			Engine: "qwen-3.8-27b", PID: 100, Start: 1, Wrapper: wrapper, WrapperStart: 5, Started: time.Now(), Reason: runs.ReasonLost,
		}); err != nil {
			t.Fatal(err)
		}
		c.set(gpu.Proc{PID: 100, Name: "llama-server", Used: 14 * GiB})
		b.Tick(context.Background())
		return b, &buf
	}
	for _, tc := range []struct {
		name    string
		starts  map[int]uint64
		wrapper int
		reason  string
		log     string
	}{
		{"exited", map[int]uint64{}, 99, "its berth run (pid 99) has exited, so nothing will re-attach it", "berth_run=exited"},
		{"pid reused", map[int]uint64{99: 6}, 99, "its berth run (pid 99) has exited, so nothing will re-attach it", "berth_run=exited"},
		{"alive", map[int]uint64{99: 5}, 99, "its berth run (pid 99) is alive and has not re-attached", "berth_run=alive"},
		{"older record", map[int]uint64{}, 0, "(pid 100)", "berth_run=\"\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, log := orphan(t, tc.starts, tc.wrapper)
			got := refusal(t, b, Request{Engine: "qwen-3.8-27b", Needs: 14 * GiB})
			if !strings.Contains(got, tc.reason) {
				t.Fatalf("refusal = %q, want it to contain %q", got, tc.reason)
			}
			if tc.wrapper == 0 && strings.Contains(got, "berth run") {
				t.Fatalf("refusal = %q guesses about a berth run the record does not name", got)
			}
			if !strings.Contains(log.String(), tc.log) {
				t.Fatalf("log = %q, want it to contain %q", log.String(), tc.log)
			}
		})
	}
}
