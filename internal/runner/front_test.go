// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package runner

// The checkpoint park end to end: a real daemon, a real run with a front,
// an engine that serves HTTP on the port the run picks, and a stand-in
// cuda-checkpoint on PATH that records its calls.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/proto"
)

// TestMain doubles as the engine a test run starts: with
// BERTH_RUNNER_ENGINE set, the binary serves an OpenAI-shaped engine on the
// port its arguments name (--port N) and exits after a minute.
func TestMain(m *testing.M) {
	if os.Getenv("BERTH_RUNNER_ENGINE") != "" && len(os.Args) == 3 && os.Args[1] == "--port" {
		serveEngine(os.Args[2])
		return
	}
	os.Exit(m.Run())
}

func serveEngine(port string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"ninfer"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "engine reply")
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { time.Sleep(time.Minute); _ = srv.Close() }()
	_ = srv.ListenAndServe()
}

func freeFront(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// A checkpointed engine never sees a request while it sleeps: the drain
// checkpoints it behind its front, a request that arrives then waits, and
// it is answered once Berth gives the card back and the restore runs.
func TestCheckpointParkHoldsRequestsUntilTheRestore(t *testing.T) {
	tools := t.TempDir()
	calls := filepath.Join(tools, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(tools, "cuda-checkpoint"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BERTH_RUNNER_ENGINE", "1")

	// llama-swap as the run sees it: an unload URL whose server has no
	// /slots. A park must not need either.
	var unloads atomic.Int32
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/unload/ninfer" {
			unloads.Add(1)
			return
		}
		http.NotFound(w, r)
	}))
	defer swap.Close()

	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 32<<30)
	addr := freeFront(t)
	log := &syncBuf{}
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "ninfer", Priority: "interactive", Needs: []uint64{8 << 30},
			DrainPost: swap.URL + "/api/models/unload/ninfer",
			Front:     addr, Checkpoint: true, ParkProbe: "/v1/models", Grace: 2 * time.Second,
			Argv: []string{os.Args[0], "--port", PortPlaceholder}, Stderr: log})
	}()
	pid := attachAndMeasure(t, b, card)
	stopAtEnd(t, pid, exited)
	front := "http://" + addr
	waitFor(t, "the engine through its front", func() bool {
		code, body := post(front + "/v1/chat/completions")
		return code == 200 && body == "engine reply"
	})

	// A foreground waiter drains the run: the front closes, the engine is
	// checkpointed, and its memory leaves the card.
	holder := acquireIn(b, broker.Request{Engine: "comfyui", Priority: broker.Foreground})
	waitFor(t, "the checkpoint", func() bool { return strings.Contains(readFile(calls), "--action checkpoint") })
	card.set()
	waitFor(t, "parked", func() bool { return b.Status().Leases[0].State == "parked" })
	hold := <-holder
	if hold == nil {
		t.Fatalf("the waiter was never granted; log:\n%s", log.String())
	}

	// Berth's own engine reader polls the model's /metrics and /slots
	// through its proxy address, the front: a read must not wake it.
	if resp, err := http.Get(front + "/metrics"); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /metrics on the sleeping engine = %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	time.Sleep(200 * time.Millisecond)
	if st := b.Status().Leases; st[0].Engine != "ninfer" || st[0].State != "parked" {
		t.Fatalf("a read woke the engine: leases = %+v", st)
	}

	// A request now waits at the front instead of reaching the engine.
	answer := make(chan string, 1)
	go func() { _, body := post(front + "/v1/chat/completions"); answer <- body }()
	select {
	case body := <-answer:
		t.Fatalf("a request reached the checkpointed engine: %q", body)
	case <-time.After(300 * time.Millisecond):
	}

	// The holder leaves; the front's wake gets the card, the restore runs,
	// and the held request is answered.
	hold.Release()
	select {
	case body := <-answer:
		if body != "engine reply" {
			t.Fatalf("held request got %q; log:\n%s", body, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the held request was never answered; log:\n%s", log.String())
	}
	want := fmt.Sprintf("--action lock --pid %d --timeout 2000\n--action checkpoint --pid %d\n--action restore --pid %d\n--action unlock --pid %d\n", pid, pid, pid, pid)
	if got := readFile(calls); got != want {
		t.Fatalf("cuda-checkpoint calls:\n%s\nwant:\n%s", got, want)
	}
	if st := b.Status().Leases; len(st) != 1 || st[0].State != "active" {
		t.Fatalf("leases after the wake = %+v", st)
	}
	if n := unloads.Load(); n != 0 {
		t.Fatalf("the drain asked llama-swap to unload %d time(s); a park keeps the engine", n)
	}
}

func post(url string) (int, string) {
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"model":"ninfer"}`))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// With no cuda-checkpoint anywhere, a drain stops the engine as it always
// has, and berthd says why on the event stream.
func TestCheckpointWithoutTheToolDrainsAndSaysWhy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BERTH_RUNNER_ENGINE", "1")
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 32<<30)
	exited := make(chan int, 1)
	log := &syncBuf{}
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "ninfer", Priority: "interactive", Needs: []uint64{8 << 30},
			Front: freeFront(t), Checkpoint: true, ParkProbe: "/v1/models", Grace: time.Second,
			Argv: []string{os.Args[0], "--port", PortPlaceholder}, Stderr: log})
	}()
	attachAndMeasure(t, b, card)
	holder := acquireIn(b, broker.Request{Engine: "comfyui", Priority: broker.Foreground})
	select {
	case code := <-exited:
		if code != 143 {
			t.Fatalf("exit %d, want 143; log:\n%s", code, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the engine was never stopped; log:\n%s", log.String())
	}
	card.set()
	if hold := <-holder; hold == nil {
		t.Fatal("the waiter was never granted")
	} else {
		hold.Release()
	}
	evs, _, _ := b.Events(0, false)
	var refused string
	for _, e := range evs {
		if e.Kind == proto.EvParkRefused {
			refused = e.Detail
		}
	}
	want := "park failed: cuda-checkpoint is not on PATH or beside the berth binary (github.com/NVIDIA/cuda-checkpoint, driver 550 or later); stopping it instead"
	if refused != want {
		t.Fatalf("park_refused = %q, want %q", refused, want)
	}
}

// A front whose port is still held by the run before it (a router that

// The engine behind a front binds its port only once the lease is granted, so
// the port must be one the kernel never hands to another socket in the
// meantime: below the ephemeral range, and not one something listens on.
func TestFreePortSkipsATakenPortAndStaysBelowTheEphemeralRange(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	busy := taken.Addr().(*net.TCPAddr).Port
	prev := pickPort
	t.Cleanup(func() { pickPort = prev })
	tries := 0
	pickPort = func() int {
		tries++
		if tries == 1 {
			return busy
		}
		return prev()
	}

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if port == busy || port < 20000 || port >= 30000 {
		t.Fatalf("freePort = %d, want a free port in 20000-29999 after skipping %d", port, busy)
	}
	if tries < 2 {
		t.Fatalf("tries = %d, want the taken port skipped", tries)
	}
}

// restarts a model reuses the port) waits for it instead of failing.
func TestListenSoonWaitsForAPortToFree(t *testing.T) {
	old, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := old.Addr().String()
	go func() { time.Sleep(300 * time.Millisecond); old.Close() }()
	ln, err := listenSoon(addr, 5*time.Second)
	if err != nil {
		t.Fatalf("listenSoon = %v", err)
	}
	ln.Close()
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if _, err := listenSoon(busy.Addr().String(), 200*time.Millisecond); err == nil {
		t.Fatal("listenSoon on a port that stays taken = nil")
	}
}

// A drain that arrives while another is parking the engine is kept, not
// dropped: berth stop during a slow checkpoint kills the engine once the
// park ends, instead of leaving it parked against the person's word.
func TestStopDuringAParkStopsTheEngine(t *testing.T) {
	tools := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *checkpoint*) sleep 1 ;; esac\n"
	if err := os.WriteFile(filepath.Join(tools, "cuda-checkpoint"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BERTH_RUNNER_ENGINE", "1")
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 32<<30)
	log := &syncBuf{}
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "ninfer", Priority: "interactive", Needs: []uint64{8 << 30},
			Front: freeFront(t), Checkpoint: true, ParkProbe: "/v1/models", Grace: 2 * time.Second,
			Argv: []string{os.Args[0], "--port", PortPlaceholder}, Stderr: log})
	}()
	attachAndMeasure(t, b, card)
	holder := acquireIn(b, broker.Request{Engine: "comfyui", Priority: broker.Foreground})
	waitFor(t, "the park under way", func() bool { return b.Status().Leases[0].State == "parking" })
	if _, err := b.Stop("ninfer"); err != nil {
		t.Fatalf("berth stop: %v", err)
	}
	card.set()
	select {
	case code := <-exited:
		if code != 137 {
			t.Fatalf("exit %d, want 137; log:\n%s", code, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the engine was left running after berth stop; log:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "killing checkpointed ninfer: it cannot shut down while its CUDA calls are locked") {
		t.Fatalf("no stop line; log:\n%s", log.String())
	}
	if hold := <-holder; hold != nil {
		hold.Release()
	}
}
