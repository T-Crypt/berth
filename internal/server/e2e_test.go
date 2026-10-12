package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
)

type emptyCard struct{}

func (emptyCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func start(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1, Log: log}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	// The test daemon runs the alert evaluator too: nobody else ticks it,
	// so the initial snapshot is the whole feed. The built-in danger keys
	// make the table non-empty without any operator config.
	ev := alerts.New(alerts.FromConfig(config.AlertsConfig{}), 3, nil)
	lastAlerts := &atomic.Pointer[proto.Alerts]{}
	lastAlerts.Store(ev.Snapshot())
	go server.Serve(ctx, ln, b, server.Extras{Alerts: lastAlerts.Load}, log)
	return sock
}

func launch(o runner.Options) (chan int, *syncBuf) {
	out := &syncBuf{}
	o.Stderr = out
	ch := make(chan int, 1)
	go func() { ch <- runner.Run(o) }()
	return ch, out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A second engine drains the first through the real socket and wrapper:
// the drain URL is tried, then the process group gets SIGTERM.
func TestDrainEndToEnd(t *testing.T) {
	sock := start(t)
	var posts atomic.Int32
	unload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
	}))
	defer unload.Close()

	first, out1 := launch(runner.Options{Socket: sock, Engine: "ninfer", Grace: 200 * time.Millisecond,
		DrainPost: unload.URL, Argv: []string{"sleep", "30"}})
	waitFor(t, "first grant", func() bool { return strings.Contains(out1.String(), "granted") })

	second, out2 := launch(runner.Options{Socket: sock, Engine: "strata", Argv: []string{"true"}})

	select {
	case code := <-first:
		if code != 128+15 {
			t.Fatalf("first exit = %d, want 143 (SIGTERM); stderr:\n%s", code, out1.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("first engine was not drained; stderr:\n%s", out1.String())
	}
	if posts.Load() != 1 {
		t.Fatalf("drain URL posted %d times, want 1", posts.Load())
	}
	if code := <-second; code != 0 {
		t.Fatalf("second exit = %d; stderr:\n%s", code, out2.String())
	}
	for _, want := range []string{"waiting: draining ninfer", "granted strata"} {
		if !strings.Contains(out2.String(), want) {
			t.Fatalf("second stderr lacks %q:\n%s", want, out2.String())
		}
	}
}

func TestFailOpenAndFailClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nobody.sock")
	code, out := launch(runner.Options{Socket: missing, Engine: "x", Argv: []string{"sleep", "0.5"}})
	// While it runs, the unprotected run is on record for berthd to list.
	waitFor(t, "unprotected record", func() bool {
		m, _ := filepath.Glob(filepath.Join(runs.Dir(missing), "*.json"))
		return len(m) == 1
	})
	if c := <-code; c != 0 || !strings.Contains(out.String(), "UNPROTECTED") {
		t.Fatalf("fail-open: code %d, stderr %q", c, out.String())
	}
	if m, _ := filepath.Glob(filepath.Join(runs.Dir(missing), "*.json")); len(m) != 0 {
		t.Fatalf("record not removed after exit: %v", m)
	}
	code, _ = launch(runner.Options{Socket: missing, Engine: "x", FailClosed: true, Argv: []string{"true"}})
	if c := <-code; c != runner.ExitRefused {
		t.Fatalf("fail-closed: code %d", c)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	sock := start(t)
	if _, err := server.Listen(sock); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second Listen err = %v", err)
	}
}

// GET /v1/status's body, served over the socket today: versioned, with the
// device and the unprotected-run list present even when empty.
func TestSocketStatusIsVersioned(t *testing.T) {
	sock := start(t)
	time.Sleep(50 * time.Millisecond)
	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		t.Fatal(err)
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		t.Fatalf("status: %v %+v", err, m)
	}
	st := m.Status
	if st.API != proto.APIVersion || len(st.GPUs) != 1 || st.GPUs[0].Total != 24<<30 || st.Unprotected == nil {
		t.Fatalf("status = %+v", st)
	}
	// The alert state travels over the socket: the table is always present
	// and carries the built-in danger keys even with no operator config.
	if st.Alerts == nil || len(st.Alerts.Table) == 0 {
		t.Fatalf("status has no alerts: %+v", st.Alerts)
	}
	for _, k := range []string{"probe", "stuck"} {
		if _, ok := st.Alerts.Table[k]; !ok {
			t.Errorf("alert table lacks %q: %+v", k, st.Alerts.Table)
		}
	}
}

// OpRequests is the harbor's feed: the node's request log after since,
// oldest first, over the socket.
func TestRequestsSinceOverSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1, Log: log}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	reqs := []proto.Request{
		{ID: "a", Engine: "e", T: time.Now().Add(-2 * time.Hour), Status: 200},
		{ID: "b", Engine: "e", T: time.Now().Add(-time.Hour), Status: 200},
	}
	x := server.Extras{Requests: func(since time.Time) []proto.Request {
		var out []proto.Request
		for _, q := range reqs {
			if q.T.After(since) {
				out = append(out, q)
			}
		}
		return out
	}}
	go server.Serve(ctx, ln, b, x, log)

	ask := func(since time.Time) proto.Msg {
		conn, raw, err := server.Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		if err := conn.Send(proto.Msg{Op: proto.OpRequests, SinceT: since}); err != nil {
			t.Fatal(err)
		}
		m, err := conn.Recv()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	m := ask(time.Time{})
	if m.Op != proto.OpRequests || len(m.Requests) != 2 || m.Requests[0].ID != "a" {
		t.Fatalf("all: %+v", m)
	}
	m = ask(reqs[0].T)
	if len(m.Requests) != 1 || m.Requests[0].ID != "b" {
		t.Fatalf("after since: %+v", m)
	}
}

// TestStatusCarriesWindowsAndSlots: the socket status carries each engine's
// context window and live KV slots, and the JSON the harbor relays verbatim
// round-trips them.
func TestStatusCarriesWindowsAndSlots(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, "llamacpp:tokens_predicted_total 50\nllamacpp:requests_processing 1\n")
		case "/props":
			fmt.Fprint(w, `{"n_ctx":8192}`)
		case "/slots":
			fmt.Fprint(w, `[
				{"id":0,"state":"processing","n_ctx":8192,"n_prompt_tokens":4000,"n_prompt_tokens_cache":3500},
				{"id":1,"state":"idle","n_ctx":8192,"n_prompt_tokens":0,"n_prompt_tokens_cache":0}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	w := &engines.Watcher{Engines: []config.Engine{{Name: "ninfer", Type: "llamacpp", URL: upstream.URL}}}
	w.Poll(context.Background())

	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1, Log: log}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	go server.Serve(ctx, ln, b, server.Extras{Engines: w.Snapshot}, log)

	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		t.Fatal(err)
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		t.Fatalf("status: %v %+v", err, m)
	}
	st := m.Status
	if len(st.Engines) != 1 {
		t.Fatalf("engines = %+v", st.Engines)
	}
	e := st.Engines[0]
	if e.Name != "ninfer" || !e.Up || e.Window != 8192 || len(e.Slots) != 2 {
		t.Fatalf("engine = %+v", e)
	}
	if e.Slots[0].ID != "s0" || e.Slots[0].Used != 4000 || e.Slots[0].Cached != 3500 || !e.Slots[0].Processing || e.Slots[1].Processing {
		t.Fatalf("slots = %+v", e.Slots)
	}
	// The harbor relays the status as JSON verbatim: the wire shape must
	// carry the new fields back unchanged.
	wire, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	back := proto.Status{}
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Engines) != 1 || back.Engines[0].Window != 8192 || len(back.Engines[0].Slots) != 2 ||
		back.Engines[0].Slots[0].Used != 4000 || back.Engines[0].Slots[0].Cached != 3500 || !back.Engines[0].Slots[0].Processing {
		t.Fatalf("round trip = %+v", back.Engines)
	}
}

func TestEventsWatchOverSocket(t *testing.T) {
	sock := start(t)
	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpEvents, Watch: true}); err != nil {
		t.Fatal(err)
	}
	code, _ := launch(runner.Options{Socket: sock, Engine: "probe", Argv: []string{"true"}})
	if c := <-code; c != 0 {
		t.Fatalf("run exit %d", c)
	}
	var kinds []string
	for len(kinds) < 3 {
		m, err := conn.Recv()
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, m.Event.Kind)
	}
	if strings.Join(kinds, ",") != "lease.requested,lease.granted,lease.released" {
		t.Fatalf("watched %v", kinds)
	}
}

// daemon starts a berthd on sock that stops when the returned func is called.
func daemon(t *testing.T, sock string, window time.Duration) (*broker.Broker, func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second,
		RunsDir: runs.Dir(sock), ReattachWindow: window, Log: log}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go b.Run(ctx)
	go func() { _ = server.Serve(ctx, ln, b, server.Extras{}, log); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return b, stop
}

// P4: a daemon restart under a running engine. The engine keeps running,
// is on record as unprotected while no daemon answers, and is leased the
// card again by the next daemon; a newcomer waits for that re-attach
// instead of being granted a card the engine already holds.
func TestReattachAfterDaemonRestart(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "berth.sock")
	_, stop := daemon(t, sock, time.Hour)
	eng, out := launch(runner.Options{Socket: sock, Engine: "llamacpp", Priority: "foreground",
		Retry: 50 * time.Millisecond, Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}})
	waitFor(t, "grant", func() bool { return strings.Contains(out.String(), "granted") })

	stop()
	waitFor(t, "unprotected record", func() bool {
		return len(runs.List(runs.Dir(sock), procs.System{}.StartTime)) == 1
	})

	b2, _ := daemon(t, sock, time.Hour)
	waitFor(t, "re-attach", func() bool { return strings.Contains(out.String(), "re-attached llamacpp") })
	st := b2.Status()
	if len(st.Leases) != 1 || st.Leases[0].Engine != "llamacpp" || st.Leases[0].State != "active" || st.Leases[0].PID == 0 {
		t.Fatalf("leases after re-attach = %+v", st.Leases)
	}
	waitFor(t, "record removed", func() bool { return len(runs.List(runs.Dir(sock), procs.System{}.StartTime)) == 0 })

	// The re-attached lease still drains like any other.
	second, out2 := launch(runner.Options{Socket: sock, Engine: "comfyui", Priority: "foreground", Argv: []string{"true"}})
	if code := <-eng; code != 128+15 {
		t.Fatalf("engine exit = %d, want 143 after drain; stderr:\n%s", code, out.String())
	}
	if code := <-second; code != 0 {
		t.Fatalf("second exit = %d; stderr:\n%s", code, out2.String())
	}
}

// While a run that lost the last daemon is alive and has not re-attached,
// a new daemon grants nothing; once the window passes it schedules as usual.
func TestRestartWindowHoldsNewGrants(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "berth.sock")
	orphan := exec.Command("sleep", "30")
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orphan.Process.Kill(); _ = orphan.Wait() })
	pid := orphan.Process.Pid
	if _, err := runs.Write(runs.Dir(sock), runs.Record{Engine: "vllm", PID: pid, Start: procs.System{}.StartTime(pid),
		Started: time.Now(), Reason: runs.ReasonLost}); err != nil {
		t.Fatal(err)
	}
	daemon(t, sock, 600*time.Millisecond)
	t0 := time.Now()
	code, out := launch(runner.Options{Socket: sock, Engine: "ollama", Argv: []string{"true"}})
	if c := <-code; c != 0 {
		t.Fatalf("exit = %d; stderr:\n%s", c, out.String())
	}
	if !strings.Contains(out.String(), "waiting: berthd restarted") {
		t.Fatalf("no restart wait in stderr:\n%s", out.String())
	}
	if el := time.Since(t0); el < 400*time.Millisecond {
		t.Fatalf("granted after %s, inside the re-attach window", el)
	}
}

// P6a: an LLM asks for the card while ComfyUI is mid-job. The drain waits
// for ComfyUI's queue to empty, never cutting the job off, then stops it;
// only then is the LLM granted.
func TestComfyUIDrainWaitsForJobBoundary(t *testing.T) {
	sock := start(t)
	var busy atomic.Bool
	busy.Store(true)
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if busy.Load() {
			fmt.Fprint(w, `{"queue_running":[[1,"job"]],"queue_pending":[]}`)
			return
		}
		fmt.Fprint(w, `{"queue_running":[],"queue_pending":[]}`)
	}))
	defer comfy.Close()

	img, out1 := launch(runner.Options{Socket: sock, Engine: "comfyui", Priority: "batch", Retry: 20 * time.Millisecond,
		Grace: 200 * time.Millisecond, DrainComfyUI: comfy.URL, Argv: []string{"sleep", "30"}})
	waitFor(t, "comfyui grant", func() bool { return strings.Contains(out1.String(), "granted") })

	llm, out2 := launch(runner.Options{Socket: sock, Engine: "llama.cpp", Priority: "interactive", Argv: []string{"true"}})
	waitFor(t, "job-boundary wait", func() bool { return strings.Contains(out1.String(), "waiting for ComfyUI to finish: 1 running") })
	select {
	case c := <-img:
		t.Fatalf("ComfyUI stopped mid-job (exit %d); stderr:\n%s", c, out1.String())
	case <-time.After(300 * time.Millisecond):
	}
	if strings.Contains(out2.String(), "granted") {
		t.Fatalf("LLM granted while ComfyUI was mid-job:\n%s", out2.String())
	}

	busy.Store(false)
	if c := <-img; c != 128+15 {
		t.Fatalf("ComfyUI exit = %d, want 143; stderr:\n%s", c, out1.String())
	}
	if c := <-llm; c != 0 {
		t.Fatalf("LLM exit = %d; stderr:\n%s", c, out2.String())
	}
	if !strings.Contains(out1.String(), "ComfyUI queue empty; handing off") {
		t.Fatalf("no handoff line:\n%s", out1.String())
	}
}
