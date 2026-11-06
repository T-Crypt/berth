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
