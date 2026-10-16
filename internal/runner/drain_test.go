package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runs"
)

// comfyStub is a ComfyUI stand-in: an empty or busy queue, a /free that
// records what it was asked, and stats that free the device unless hold is
// set (an unload that did not take, for the timeout fallback).
type comfyStub struct {
	busy   atomic.Bool
	hold   atomic.Bool
	freed  atomic.Bool
	mu     sync.Mutex
	frees  int
	bodies []string
}

func (c *comfyStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		if c.busy.Load() {
			fmt.Fprint(w, `{"queue_running":[[1,"job"]],"queue_pending":[]}`)
			return
		}
		fmt.Fprint(w, `{"queue_running":[],"queue_pending":[]}`)
	})
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.frees++
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		if !c.hold.Load() {
			c.freed.Store(true)
		}
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		used := uint64(17) << 30
		if c.freed.Load() {
			used = 512 << 20
		}
		total := uint64(24) << 30
		fmt.Fprintf(w, `{"devices":[{"vram_free":%d,"vram_total":%d}]}`, total-used, total)
	})
	return mux
}

// startSleep starts a stand-in engine and reaps it; waited fires with its
// exit when (and only when) the process actually dies.
func startSleep(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	isolate(cmd) // as command() does: the drain signals the process group
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	// Cleanup only kills: the reaper above always collects the exit into
	// the buffered channel, so nothing here may receive from waited again.
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, waited
}

func sayTo(logs *strings.Builder) func(string, ...any) {
	return func(f string, a ...any) { fmt.Fprintf(logs, f+"\n", a...) }
}

// A drain with an empty queue unloads via POST /free with ComfyUI's own
// shape, then keeps the server up when asked: the lease is released, the
// run is recorded, and the process never exits.
func TestDrainUnloadsThenKeepsUp(t *testing.T) {
	stub := &comfyStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder
	sock := filepath.Join(t.TempDir(), "berthd.sock")

	drain(Options{Engine: "comfyui", DrainComfyUI: srv.URL, KeepComfyUI: true, Socket: sock,
		Grace: 50 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))

	select {
	case <-released:
	default:
		t.Fatalf("lease was not released; log:\n%s", &logs)
	}
	stub.mu.Lock()
	frees, body := stub.frees, ""
	if len(stub.bodies) > 0 {
		body = stub.bodies[0]
	}
	stub.mu.Unlock()
	if frees != 1 {
		t.Fatalf("/free hits = %d, want 1; log:\n%s", frees, &logs)
	}
	if !strings.Contains(body, `"unload_models":true`) || !strings.Contains(body, `"free_memory":true`) {
		t.Fatalf("/free body = %s, want unload_models and free_memory", body)
	}
	select {
	case err := <-waited:
		t.Fatalf("keep-up killed the server (%v); log:\n%s", err, &logs)
	case <-time.After(200 * time.Millisecond):
	}
	if out := logs.String(); !strings.Contains(out, "leaving the server up") {
		t.Fatalf("no keep-up line; log:\n%s", out)
	}
	records := runs.List(runs.Dir(sock), procs.System{}.StartTime)
	if len(records) != 1 || records[0].Reason != runs.ReasonKeepUp || records[0].Engine != "comfyui" {
		t.Fatalf("keep-up records = %+v, want one comfyui run with reason %q", records, runs.ReasonKeepUp)
	}
}

// A keep-up release waits for the driver to show the models gone; the
// lease is not released while the card still holds them.
func TestDrainKeepUpWaitsForFreedVRAM(t *testing.T) {
	stub := &comfyStub{}
	stub.hold.Store(true) // /free succeeds but the models stay for now
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder
	sock := filepath.Join(t.TempDir(), "berthd.sock")

	go drain(Options{Engine: "comfyui", DrainComfyUI: srv.URL, KeepComfyUI: true, Socket: sock,
		Grace: 200 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))

	select {
	case <-released:
		t.Fatal("released while the models were still on the card")
	case <-time.After(120 * time.Millisecond):
	}
	stub.freed.Store(true)
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatalf("no release after the device freed; log:\n%s", &logs)
	}
	select {
	case err := <-waited:
		t.Fatalf("keep-up killed the server (%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// An unload that never frees the card falls back to stopping the server:
// no keep-up record, the process dies, and the lease is not released early.
func TestDrainKeepUpTimeoutStops(t *testing.T) {
	stub := &comfyStub{}
	stub.hold.Store(true)
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder
	sock := filepath.Join(t.TempDir(), "berthd.sock")

	drain(Options{Engine: "comfyui", DrainComfyUI: srv.URL, KeepComfyUI: true, Socket: sock,
		Grace: 50 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))

	select {
	case <-released:
		t.Fatalf("timeout released the lease; log:\n%s", &logs)
	default:
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatalf("server still alive after the timeout; log:\n%s", &logs)
	}
	if out := logs.String(); !strings.Contains(out, "did not free within") {
		t.Fatalf("no timeout line; log:\n%s", out)
	}
	if records := runs.List(runs.Dir(sock), procs.System{}.StartTime); len(records) != 0 {
		t.Fatalf("timeout wrote keep-up records: %+v", records)
	}
}

// Without keep-up the server is stopped after the unload: the process dies.
func TestDrainUnloadsThenStops(t *testing.T) {
	stub := &comfyStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder

	drain(Options{Engine: "comfyui", DrainComfyUI: srv.URL,
		Grace: 50 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))

	select {
	case <-released:
		t.Fatalf("stop path released the lease; log:\n%s", &logs)
	default:
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatalf("server still alive after the drain; log:\n%s", &logs)
	}
	stub.mu.Lock()
	frees := stub.frees
	stub.mu.Unlock()
	if frees != 1 {
		t.Fatalf("/free hits = %d, want 1 (unload-first); log:\n%s", frees, &logs)
	}
}

// A failed unload falls back to stopping the server: the process dies and
// the lease is not released early.
func TestDrainUnloadFailureStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/queue":
			fmt.Fprint(w, `{"queue_running":[],"queue_pending":[]}`)
		default:
			http.Error(w, "wedged", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder

	drain(Options{Engine: "comfyui", DrainComfyUI: srv.URL, KeepComfyUI: true,
		Grace: 50 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))

	select {
	case <-released:
		t.Fatalf("failed unload released the lease; log:\n%s", &logs)
	default:
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatalf("server still alive after failed unload; log:\n%s", &logs)
	}
	if out := logs.String(); !strings.Contains(out, "stopping the server instead") {
		t.Fatalf("no fallback line; log:\n%s", out)
	}
}

// slotStub is a drain-post target with a llama.cpp-style /slots and an
// unload URL that records its hits.
type slotStub struct {
	busy     atomic.Bool
	mu       sync.Mutex
	unloads  int
	busySeen int
}

func (s *slotStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
		if s.busy.Load() {
			s.mu.Lock()
			s.busySeen++
			s.mu.Unlock()
			fmt.Fprint(w, `[{"id":0,"is_processing":true}]`)
			return
		}
		fmt.Fprint(w, `[{"id":0,"is_processing":false}]`)
	})
	mux.HandleFunc("/unload", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.unloads++
		s.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	return mux
}

func (s *slotStub) counts() (unloads, busySeen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unloads, s.busySeen
}

// The unload waits past a busy boundary: nothing is posted while slots
// process, and it fires once they idle.
func TestDrainSlotBoundaryWaits(t *testing.T) {
	stub := &slotStub{}
	stub.busy.Store(true)
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		drain(Options{Engine: "llama-swap", DrainPost: srv.URL + "/unload",
			Grace: 50 * time.Millisecond, Retry: 20 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))
	}()

	time.Sleep(200 * time.Millisecond)
	if unloads, seen := stub.counts(); unloads != 0 {
		t.Fatalf("unloaded while slots were busy")
	} else if seen == 0 {
		t.Fatal("drain never read the busy boundary")
	}

	stub.busy.Store(false)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after slots idled")
	}
	if unloads, _ := stub.counts(); unloads != 1 {
		t.Fatalf("unloads = %d, want 1", unloads)
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("engine still alive after the drain")
	}
	if out := logs.String(); !strings.Contains(out, "engine idle; unloading") {
		t.Fatalf("no boundary line; log:\n%s", out)
	}
}

// A drain-post host with no /slots unloads at once instead of hanging.
func TestDrainNoSlotsProceeds(t *testing.T) {
	var mu sync.Mutex
	unloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unload" {
			mu.Lock()
			unloads++
			mu.Unlock()
			fmt.Fprint(w, `{}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cmd, _ := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder

	start := time.Now()
	drain(Options{Engine: "llama-swap", DrainPost: srv.URL + "/unload",
		Grace: 50 * time.Millisecond, Retry: 20 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("drain took %s without a slot boundary", el)
	}
	mu.Lock()
	defer mu.Unlock()
	if unloads != 1 {
		t.Fatalf("unloads = %d, want 1; log:\n%s", unloads, &logs)
	}
	if out := logs.String(); !strings.Contains(out, "no slot boundary") {
		t.Fatalf("no fail-open line; log:\n%s", out)
	}
}

// A sick boundary waits instead of cutting work off: 500s from /slots
// hold the unload back, and it fires once the slots read idle.
func TestDrainSlotSickWaits(t *testing.T) {
	var mu sync.Mutex
	sick := true
	unloads, polls500 := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/unload":
			unloads++
			fmt.Fprint(w, `{}`)
		case "/slots":
			if sick {
				polls500++
				http.Error(w, "wedged", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `[{"id":0,"is_processing":false}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cmd, waited := startSleep(t)
	done := make(chan struct{})
	released := make(chan struct{})
	var logs strings.Builder

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		drain(Options{Engine: "llama-swap", DrainPost: srv.URL + "/unload",
			Grace: 50 * time.Millisecond, Retry: 20 * time.Millisecond}, cmd, done, released, "test", sayTo(&logs))
	}()

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	u, p := unloads, polls500
	mu.Unlock()
	if u != 0 {
		t.Fatal("unloaded while the slot boundary was sick")
	}
	if p == 0 {
		t.Fatal("drain never polled the sick boundary")
	}

	mu.Lock()
	sick = false
	mu.Unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after slots read idle")
	}
	mu.Lock()
	u = unloads
	mu.Unlock()
	if u != 1 {
		t.Fatalf("unloads = %d, want 1; log:\n%s", u, &logs)
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("engine still alive after the drain")
	}
	if out := logs.String(); !strings.Contains(out, "engine idle; unloading") {
		t.Fatalf("no boundary line; log:\n%s", out)
	}
}

// failOpen tells "no boundary here" from "boundary sick": 404, garbage,
// and an unreachable host fail open; 500 and timeouts wait.
func TestSlotsFailOpenKinds(t *testing.T) {
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wedged", http.StatusInternalServerError)
	}))
	defer sick.Close()
	if _, err := slotsBusy(sick.URL); err == nil || failOpen(err) {
		t.Fatalf("500 should wait, got %v", err)
	}

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer missing.Close()
	if _, err := slotsBusy(missing.URL); err == nil || !failOpen(err) {
		t.Fatalf("404 should fail open, got %v", err)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer garbage.Close()
	if _, err := slotsBusy(garbage.URL); err == nil || !failOpen(err) {
		t.Fatalf("bad JSON should fail open, got %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if _, err := slotsBusy(deadURL); err == nil || !failOpen(err) {
		t.Fatalf("unreachable should fail open, got %v", err)
	}

	if !slotsTimeout(context.DeadlineExceeded) {
		t.Fatal("deadline exceeded is a timeout")
	}
	if slotsTimeout(errors.New("connection refused")) {
		t.Fatal("refused is not a timeout")
	}
	if failOpen(&slotsErr{timeout: true, err: context.DeadlineExceeded}) {
		t.Fatal("a timeout should wait, not fail open")
	}
}
