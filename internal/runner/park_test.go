// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

// Park driven through a real daemon and a real Run: admission and the
// adapter on a drain, the wake on a grant, the eviction of a parked
// lease, and the fallbacks a refused or failed park takes.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
)

// parkCard fakes the card the broker reads.
type parkCard struct {
	mu    sync.Mutex
	total uint64
	procs []gpu.Proc
}

func (c *parkCard) set(ps ...gpu.Proc) {
	c.mu.Lock()
	c.procs = ps
	c.mu.Unlock()
}

func (c *parkCard) Read(context.Context) (gpu.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var used uint64
	for _, p := range c.procs {
		used += p.Used
	}
	return gpu.Snapshot{Total: c.total, Used: used, Free: c.total - used,
		Procs: append([]gpu.Proc(nil), c.procs...), At: time.Now()}, nil
}

// parkDaemon is a berthd over the fake card, with RAM readings the test
// holds. BERTH_PARK_DEBUG=1 prints both sides' logs.
func parkDaemon(t *testing.T, card gpu.Probe, avail uint64) (string, *broker.Broker) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	b, _ := parkDaemonAt(t, sock, card, avail)
	return sock, b
}

// parkDaemonAt is parkDaemon on a given socket, with a stop that returns
// once the daemon has let go of it, so a test can restart berthd there.
func parkDaemonAt(t *testing.T, sock string, card gpu.Probe, avail uint64) (*broker.Broker, func()) {
	t.Helper()
	h := &slog.HandlerOptions{}
	if os.Getenv("BERTH_PARK_DEBUG") != "" {
		h.Level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, h))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second,
		JuggleWindow: -1, ParkFloor: 1 << 30, ParkResidualMax: 2 << 30,
		MemAvailable: func() uint64 { return avail }, Log: log}, card, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go b.Run(ctx)
	go func() { _ = server.Serve(ctx, ln, node.One(b), server.Extras{}, log); close(done) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return b, stop
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// acquireIn takes a lease on its own goroutine: the grant lands only
// when the card is free, and the test drives the freeing itself.
func acquireIn(b *broker.Broker, r broker.Request) chan *broker.Lease {
	out := make(chan *broker.Lease, 1)
	go func() {
		l, err := b.Acquire(context.Background(), r, nil)
		if err != nil {
			out <- nil
			return
		}
		out <- l
	}()
	return out
}

// parkStub is an engine stand-in with the sleep-mode endpoints the
// built-in adapter calls, the model list the probe reads, Berth's park
// protocol, and a sleep endpoint that can fail.
type parkStub struct {
	mu        sync.Mutex
	sleeps    int
	wakes     int
	parkPosts int
	resumes   int
	wakeDelay time.Duration
	failSleep bool
}

func (s *parkStub) count(what string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch what {
	case "sleep":
		return s.sleeps
	case "wake":
		return s.wakes
	case "park":
		return s.parkPosts
	case "resume":
		return s.resumes
	}
	return 0
}

func (s *parkStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sleep", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.sleeps++
		fail := s.failSleep
		s.mu.Unlock()
		if fail {
			http.Error(w, "sleep mode is off", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/wake_up", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delay := s.wakeDelay
		s.wakes++
		s.mu.Unlock()
		time.Sleep(delay)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/berth/park", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.parkPosts++
		s.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/berth/resume", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.resumes++
		s.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/berth/state", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"state":"hot"}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"vllm-model"}]}`)
	})
	return mux
}

// runPark is one Run of a parked-capable engine on its own goroutine.
func runPark(t *testing.T, sock, engine, stubURL string, hook bool) (chan int, *syncBuf) {
	t.Helper()
	adapter, ok := park.For("vllm", "", "")
	if !ok {
		t.Fatal("the vllm adapter is missing")
	}
	log := &syncBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.String())
		}
	})
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: engine, Priority: "interactive", Needs: []uint64{8 << 30},
			Park: &adapter, ParkBase: stubURL, ParkProbe: "/v1/models", Hook: hook,
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: log})
	}()
	return exited, log
}

// stopAtEnd kills an engine the test leaves running and waits for its
// wrapper to exit before the temp dirs go. Run would otherwise see the
// daemon stop at cleanup and write its unprotected-run record into a
// directory being removed.
func stopAtEnd(t *testing.T, pid int, exited <-chan int) {
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	})
}

// attachAndMeasure waits for the run's lease to carry a PID and gives the
// card its footprint, so a drain can be admitted.
func attachAndMeasure(t *testing.T, b *broker.Broker, card *parkCard) int {
	t.Helper()
	var pid int
	waitFor(t, "the engine attached", func() bool {
		st := b.Status()
		if len(st.Leases) == 0 || st.Leases[0].PID == 0 {
			return false
		}
		pid = st.Leases[0].PID
		return true
	})
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 8 << 30})
	waitFor(t, "the run measured", func() bool {
		return b.Status().Leases[0].Used == 8<<30
	})
	return pid
}

// A vLLM-shaped engine parks on a drain and wakes on a resume, and one
// that a client loads back onto the card while parked parks again on the
// next drain instead of being stopped.
func TestRunParksAndResumes(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	stub := &parkStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	exited, log := runPark(t, sock, "vllm", srv.URL, false)
	pid := attachAndMeasure(t, b, card)
	stopAtEnd(t, pid, exited)

	// A whole-card foreground waiter cannot fit: the lease drains, and
	// the park moves the engine's weights to RAM instead of stopping it.
	holder := acquireIn(b, broker.Request{Engine: "game", Priority: broker.Foreground})
	waitFor(t, "the park call", func() bool { return stub.count("sleep") == 1 })
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	waitFor(t, "parked", func() bool { return b.Status().Leases[0].State == "parked" })
	if out := log.String(); !strings.Contains(out, "parked vllm in RAM") {
		t.Fatalf("no parked line; log:\n%s", out)
	}
	select {
	case code := <-exited:
		t.Fatalf("the parked runner exited with %d", code)
	default:
	}

	// The wake runs on the grant, and the engine answers again.
	if err := b.Resume("vllm", false); err != nil {
		t.Fatal(err)
	}
	hold := <-holder
	if hold == nil {
		t.Fatal("the waiter was never granted")
	}
	hold.Release()
	waitFor(t, "the wake call", func() bool { return stub.count("wake") == 1 })
	waitFor(t, "active again", func() bool { return b.Status().Leases[0].State == "active" })
	if out := log.String(); !strings.Contains(out, "resumed vllm in") {
		t.Fatalf("no resumed line; log:\n%s", out)
	}
	evs, _, _ := b.Events(0, false)
	found := false
	for _, e := range evs {
		if e.Kind == proto.EvResumed && e.Engine == "vllm" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no lease.resumed event among %d events", len(evs))
	}

	// A second cycle: parked again. The holder leaves, and a client then
	// sends the parked engine work directly: it loads back onto the card
	// past the residual max with no resume. The broker takes it as awake,
	// the runner hears the wake, and the next waiter's drain parks it
	// again instead of stopping it.
	holder2 := acquireIn(b, broker.Request{Engine: "game2", Priority: broker.Foreground})
	waitFor(t, "the second park call", func() bool { return stub.count("sleep") == 2 })
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	waitFor(t, "parked again", func() bool { return b.Status().Leases[0].State == "parked" })
	hold2 := <-holder2
	if hold2 == nil {
		t.Fatal("the second waiter was never granted")
	}
	hold2.Release()
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 3 << 30})
	waitFor(t, "the wake after a direct load", func() bool { return stub.count("wake") == 2 })
	waitFor(t, "active after a direct load", func() bool { return b.Status().Leases[0].State == "active" })
	evs, _, _ = b.Events(0, false)
	var unparked *proto.Event
	for _, e := range evs {
		if e.Kind == proto.EvUnparked {
			unparked = &e
		}
	}
	if unparked == nil || unparked.Detail != "vllm loaded 3.0 GiB back onto the card while parked, without a resume (a client used it directly)" {
		t.Fatalf("unparked event = %+v", unparked)
	}

	holder3 := acquireIn(b, broker.Request{Engine: "game3", Priority: broker.Foreground})
	waitFor(t, "the third park call", func() bool { return stub.count("sleep") == 3 })
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	hold3 := <-holder3
	if hold3 == nil {
		t.Fatalf("the third waiter was never granted; log:\n%s", log.String())
	}
	if st := b.Status().Leases; st[0].Engine != "vllm" || st[0].State != "parked" {
		t.Fatalf("leases = %+v, want vllm parked beside game3", st)
	}
	select {
	case code := <-exited:
		t.Fatalf("the re-parked runner exited with %d; log:\n%s", code, log.String())
	default:
	}
	hold3.Release()
}

// A park the broker refuses (no RAM above the floor) or whose adapter
// call fails falls back to the stop ladder, with the why on the log.
func TestRunFallsBackWhenParkRefusedOrFails(t *testing.T) {
	tests := []struct {
		name      string
		avail     uint64
		failSleep bool
		says      string
		wantCalls int // adapter park calls before the fallback
	}{
		{"refused: no RAM", 1 << 30, false, "park refused: needs", 0},
		{"failed: the adapter said no", 16 << 30, true, "park call failed", 1},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			card := &parkCard{total: 24 << 30}
			sock, b := parkDaemon(t, card, c.avail)
			stub := &parkStub{failSleep: c.failSleep}
			srv := httptest.NewServer(stub.handler())
			defer srv.Close()
			exited, log := runPark(t, sock, "vllm", srv.URL, false)
			attachAndMeasure(t, b, card)

			holder := acquireIn(b, broker.Request{Engine: "game", Priority: broker.Foreground})
			select {
			case code := <-exited:
				if code != 143 {
					t.Fatalf("the fallback runner exited with %d, want 143", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the fallback never stopped the engine; log:\n%s", log.String())
			}
			if n := stub.count("sleep"); n != c.wantCalls {
				t.Fatalf("park call hits = %d, want %d", n, c.wantCalls)
			}
			card.set()
			if l := <-holder; l == nil {
				t.Fatal("the waiter was never granted")
			}
			if out := log.String(); !strings.Contains(out, c.says) {
				t.Fatalf("log lacks %q:\n%s", c.says, out)
			}
		})
	}
}

// A park hook that names no URLs probes Berth's park protocol once; an
// engine that answers it parks through /berth/park and wakes through
// /berth/resume.
func TestRunParksThroughBerthProtocol(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	stub := &parkStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	log := &syncBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.String())
		}
	})
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "custom", Priority: "interactive", Needs: []uint64{8 << 30},
			Hook: true, ParkBase: srv.URL, ParkProbe: "/v1/models",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: log})
	}()
	stopAtEnd(t, attachAndMeasure(t, b, card), exited)

	holder := acquireIn(b, broker.Request{Engine: "game", Priority: broker.Foreground})
	waitFor(t, "the protocol park", func() bool { return stub.count("park") == 1 })
	card.set(gpu.Proc{PID: -1, Name: "sleep", Used: 512 << 20}) // the pid is unknown here; an unowned residual
	waitFor(t, "parked", func() bool { return b.Status().Leases[0].State == "parked" })
	if out := log.String(); !strings.Contains(out, "parked custom in RAM") {
		t.Fatalf("no parked line; log:\n%s", out)
	}
	if err := b.Resume("custom", false); err != nil {
		t.Fatal(err)
	}
	hold := <-holder
	if hold == nil {
		t.Fatal("the waiter was never granted")
	}
	hold.Release()
	waitFor(t, "the protocol wake", func() bool { return stub.count("resume") == 1 })
	waitFor(t, "active again", func() bool { return b.Status().Leases[0].State == "active" })
}

// keep_comfyui_up is the park alias: the unload stays the park (with the
// RAM copy kept), the engine stays on its port, and the lease is parked
// instead of released; the wake is the engine's own next-prompt load.
func TestRunParksComfyUIKeepUp(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	stub := &comfyStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	adapter, ok := park.For("comfyui", "", "")
	if !ok {
		t.Fatal("the comfyui adapter is missing")
	}
	log := &syncBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.String())
		}
	})
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "comfyui", Priority: "interactive", Needs: []uint64{8 << 30},
			DrainComfyUI: srv.URL, Park: &adapter, ParkBase: srv.URL, ParkProbe: "/system_stats",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: log})
	}()
	pid := attachAndMeasure(t, b, card)
	stopAtEnd(t, pid, exited)

	holder := acquireIn(b, broker.Request{Engine: "game", Priority: broker.Foreground})
	waitFor(t, "the park unload", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.frees >= 1
	})
	stub.mu.Lock()
	body := ""
	if len(stub.bodies) > 0 {
		body = stub.bodies[len(stub.bodies)-1] // the park adapter's unload is the last ask
	}
	stub.mu.Unlock()
	if !strings.Contains(body, `"unload_models":true`) || strings.Contains(body, "free_memory") {
		t.Fatalf("the park unload = %s, want unload_models without free_memory (the RAM copy stays)", body)
	}
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	waitFor(t, "parked", func() bool { return b.Status().Leases[0].State == "parked" })
	if out := log.String(); !strings.Contains(out, "parked comfyui in RAM") {
		t.Fatalf("no parked line; log:\n%s", out)
	}
	select {
	case code := <-exited:
		t.Fatalf("the parked runner exited with %d", code)
	default:
	}

	// The wake is nothing to call; the readiness probe answers, the lease
	// is active, and the engine is still on its port.
	if err := b.Resume("comfyui", false); err != nil {
		t.Fatal(err)
	}
	hold := <-holder
	if hold == nil {
		t.Fatal("the waiter was never granted")
	}
	hold.Release()
	wake := time.Now().Add(5 * time.Second)
	for {
		if out := log.String(); strings.Contains(out, "resumed comfyui in") {
			break
		}
		if time.Now().After(wake) {
			t.Fatalf("no resumed line; log:\n%s", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case code := <-exited:
		t.Fatalf("the wake killed the server (%d)", code)
	default:
	}
}

// A hook whose engine answers nothing: the probe is negative once, the
// drain stops the engine as today, and no probe repeats.
func TestRunHookProbeNegative(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()

	log := &syncBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.String())
		}
	})
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "custom", Priority: "interactive", Needs: []uint64{8 << 30},
			Hook: true, ParkBase: srv.URL, ParkProbe: "/v1/models",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: log})
	}()
	attachAndMeasure(t, b, card)

	holder := acquireIn(b, broker.Request{Engine: "game", Priority: broker.Foreground})
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("the engine never stopped; log:\n%s", log.String())
	}
	card.set()
	if l := <-holder; l == nil {
		t.Fatal("the waiter was never granted")
	}
	if out := log.String(); strings.Count(out, "does not speak Berth's park protocol") != 1 {
		t.Fatalf("the negative probe did not say once:\n%s", out)
	}
}

// A drain that reaches an engine already parked stops it, and the runner
// says why before the stop ladder runs: an exit with no line reads as a
// crash.
func TestDrainOfAParkedEngineSaysWhy(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	line := newLine(nil)
	line.parked = true
	var log syncBuf
	say := func(format string, a ...any) { fmt.Fprintf(&log, format+"\n", a...) }

	drain(Options{Engine: "comfyui", Hook: true, Grace: 5 * time.Second}, cmd, line, done,
		"evicted: parked 12h0m1s, past the ttl", say)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked engine was not stopped")
	}
	want := "stopping parked comfyui instead of parking it again: evicted: parked 12h0m1s, past the ttl\n"
	if !strings.Contains(log.String(), want) {
		t.Fatalf("log = %q, want the line %q", log.String(), want)
	}
}

// A parked engine whose daemon restarts comes back parked: the new daemon
// grants a whole-card waiter beside its residual, and the engine is never
// drained, where an active lease would be drained and stopped.
// An engine that exits while its daemon is gone leaves no unprotected-run
// record behind once Run returns.
func TestRunReturnsWithItsLostRecordGone(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock := filepath.Join(t.TempDir(), "berth.sock")
	b, stop := parkDaemonAt(t, sock, card, 16<<30)
	exited, _ := runPark(t, sock, "vllm", "http://127.0.0.1:1", false)
	pid := attachAndMeasure(t, b, card)
	stopAtEnd(t, pid, exited)

	stop()
	dir := runs.Dir(sock)
	waitFor(t, "the lost record", func() bool {
		left, _ := os.ReadDir(dir)
		return len(left) > 0
	})
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	<-exited
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("records left after Run returned: %v", left)
	}
}

func TestParkedEngineReattachesParked(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock := filepath.Join(t.TempDir(), "berth.sock")
	b, stop := parkDaemonAt(t, sock, card, 16<<30)
	stub := &parkStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	exited, log := runPark(t, sock, "vllm", srv.URL, false)
	pid := attachAndMeasure(t, b, card)
	stopAtEnd(t, pid, exited)

	holder := acquireIn(b, broker.Request{Engine: "game", Priority: broker.Foreground})
	waitFor(t, "the park call", func() bool { return stub.count("sleep") == 1 })
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	waitFor(t, "parked", func() bool { return b.Status().Leases[0].State == "parked" })
	(<-holder).Release()

	stop()
	before := len(log.String())
	b2, _ := parkDaemonAt(t, sock, card, 16<<30)
	waitFor(t, "the re-attach", func() bool { return strings.Contains(log.String(), "re-attached vllm") })
	if st := b2.Status().Leases; len(st) != 1 || st[0].Engine != "vllm" || st[0].State != "parked" {
		t.Fatalf("leases after the restart = %+v", st)
	}
	waitFor(t, "the residual owned again", func() bool {
		st := b2.Status()
		return st.Leases[0].Used == 512<<20 && len(st.Squatters) == 0
	})
	hold := <-acquireIn(b2, broker.Request{Engine: "game2", Priority: broker.Foreground})
	if hold == nil {
		t.Fatalf("the waiter was never granted beside the parked engine; log:\n%s", log.String())
	}
	hold.Release()
	select {
	case code := <-exited:
		t.Fatalf("the parked engine exited with %d after the restart; log:\n%s", code, log.String())
	default:
	}
	if strings.Contains(log.String()[before:], "draining vllm") {
		t.Fatalf("the parked engine was drained after the restart; log:\n%s", log.String())
	}
}
