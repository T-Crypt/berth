package broker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runs"
)

// comfyCard builds a broker whose runs dir already holds records, as a fresh
// daemon does after a restart: New reads them before the first tick.
func comfyCard(t *testing.T, dir string, comfy []ComfyEngine, tree procs.Tree) (*Broker, *card) {
	t.Helper()
	c := &card{total: 24 * GiB}
	b := New(Config{
		RunsDir:      dir,
		Comfy:        comfy,
		ReleaseGrace: time.Hour,
		DefaultWait:  2 * time.Second,
		JuggleWindow: -1, // these tests are about the comfy path; juggles have their own
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, c, tree)
	b.Tick(context.Background())
	return b, c
}

// keepUpRun writes the record a keep-up drain leaves behind: the ComfyUI
// server stays up unloaded, unprotected.
func keepUpRun(t *testing.T, dir string) {
	t.Helper()
	if _, err := runs.Write(dir, runs.Record{Engine: "comfyui", PID: 100, Start: 1,
		Started: time.Now(), Reason: runs.ReasonKeepUp}); err != nil {
		t.Fatal(err)
	}
}

func waitFrees(t *testing.T, frees *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for frees.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := frees.Load(); got < want {
		t.Fatalf("/free asks = %d, want %d", got, want)
	}
}

// A keep-up ComfyUI is an unprotected run, not a lease. A daemon that starts
// after the server was left behind (a restart) reads the record at New and must
// still poke its /free when a whole-card waiter needs the card it squats.
func TestComfyKeepUpSquatPokedAfterRestart(t *testing.T) {
	srv, frees := comfyStub(t)
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	dir := t.TempDir()
	keepUpRun(t, dir)
	b, c := comfyCard(t, dir, []ComfyEngine{{Name: "comfyui", URL: srv.URL}}, tree)
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 17 * GiB})
	b.Tick(context.Background())

	ch := acquire(b, Request{Engine: "ninfer"})
	pending(t, ch)
	waitFrees(t, frees, 1)
	if got := frees.Load(); got != 1 {
		t.Fatalf("/free asks = %d, want exactly 1 (rate limited)", got)
	}

	// The unload lands: the server stays alive, well under the threshold.
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 512 << 20})
	b.Tick(context.Background())
	must(t, ch)
}

// Under comfySquatMin an idle server never blocks and is never poked: a
// whole-card waiter is granted with the small footprint still on the card.
func TestComfySquatBelowThresholdGrants(t *testing.T) {
	srv, frees := comfyStub(t)
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	dir := t.TempDir()
	keepUpRun(t, dir)
	b, c := comfyCard(t, dir, []ComfyEngine{{Name: "comfyui", URL: srv.URL}}, tree)
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 512 << 20}) // < comfySquatMin
	b.Tick(context.Background())

	must(t, acquire(b, Request{Engine: "ninfer"}))
	if got := frees.Load(); got != 0 {
		t.Fatalf("/free asks = %d, want none under the squat threshold", got)
	}
}

// An armed engine with no URL cannot be poked. The waiter is still held (Berth
// has no proof the memory will free) but no request is ever sent.
func TestComfyPokeSkipsEmptyURL(t *testing.T) {
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	dir := t.TempDir()
	keepUpRun(t, dir)
	b, c := comfyCard(t, dir, []ComfyEngine{{Name: "comfyui", URL: ""}}, tree)
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 17 * GiB})
	b.Tick(context.Background())

	ch := acquire(b, Request{Engine: "ninfer"})
	pending(t, ch)

	// The server unloads on its own; the grant then proceeds, with no poke.
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 512 << 20})
	b.Tick(context.Background())
	must(t, ch)
}

// syncBuffer is a writer safe to read while slog writes from a goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A failed /free must be visible: the wait reason already claims the ask went
// out, so a non-2xx is logged as a warning instead of swallowed.
func TestComfyPokeLogsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wedged", http.StatusInternalServerError)
	}))
	defer srv.Close()

	logs := &syncBuffer{}
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	dir := t.TempDir()
	keepUpRun(t, dir)
	c := &card{total: 24 * GiB}
	b := New(Config{RunsDir: dir, Comfy: []ComfyEngine{{Name: "comfyui", URL: srv.URL}},
		ReleaseGrace: time.Hour, DefaultWait: time.Second, JuggleWindow: -1,
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}, c, tree)
	b.Tick(context.Background())
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 17 * GiB})
	b.Tick(context.Background())

	ch := acquire(b, Request{Engine: "ninfer"})
	pending(t, ch)

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "free ask failed") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "free ask failed") {
		t.Fatalf("no warning for a failed /free; log:\n%s", logs.String())
	}
}
