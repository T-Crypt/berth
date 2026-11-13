// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
)

// TestMain doubles as a fake OpenAI-compatible engine: with
// BERTH_GW_ENGINE=addr,model[,load] the test binary serves that model on
// addr and streams a three-chunk reply, answering only after load (a
// duration, 300ms by default), then exits after a few seconds. With
// BERTH_GW_LOG_EVERY set the engine prints a line to its stderr at that
// cadence, the way a busy engine does. With BERTH_GW_COMFY=addr it
// serves a ComfyUI stand-in instead. With "run" as its first argument it
// is a `berth run` stand-in for the gateway's spawned engine processes.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		testRun(os.Args[2:])
		return
	}
	if spec := os.Getenv("BERTH_GW_ENGINE"); spec != "" {
		addr, rest, _ := strings.Cut(spec, ",")
		model, load, _ := strings.Cut(rest, ",")
		d := 300 * time.Millisecond
		if load != "" {
			var err error
			if d, err = time.ParseDuration(load); err != nil {
				fmt.Fprintf(os.Stderr, "bad load %q: %v\n", load, err)
				os.Exit(2)
			}
		}
		fakeEngine(addr, model, d)
		return
	}
	if addr := os.Getenv("BERTH_GW_COMFY"); addr != "" {
		fakeComfy(addr)
		return
	}
	os.Exit(m.Run())
}

// slotsSpec parses the env a fake engine reads for its slot surface:
// BERTH_GW_SLOTS is the idle slot count to list (unset: no slot endpoints
// at all), and BERTH_GW_KVLOG the file its slot calls and chat requests
// are appended to, one line each in arrival order.
func slotsSpec() (n int, kvlog string) {
	if spec := os.Getenv("BERTH_GW_SLOTS"); spec != "" {
		if _, err := fmt.Sscanf(spec, "%d", &n); err != nil {
			fmt.Fprintf(os.Stderr, "bad BERTH_GW_SLOTS %q: %v\n", spec, err)
			os.Exit(2)
		}
	}
	return n, os.Getenv("BERTH_GW_KVLOG")
}

// testRun is the `berth run` a spawned gateway engine process stands in
// for: the same flags the gateway passes, then the engine's command after
// "--". It takes a real lease from the test daemon.
func testRun(argv []string) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	socket := fs.String("socket", "", "")
	as := fs.String("as", "", "")
	group := fs.String("group", "", "")
	prio := fs.String("priority", "", "")
	var needs config.MiBs
	fs.Var(&needs, "needs-mib", "")
	wait := fs.Duration("wait", 0, "")
	grace := fs.Duration("grace", 0, "")
	card := fs.String("gpu", "", "")
	fs.String("drain-post", "", "")
	fs.String("drain-slots", "", "")
	fs.String("drain-comfyui", "", "")
	fs.Duration("drain-max", 0, "")
	fs.Bool("keep-comfyui-up", false, "")
	fs.String("config", "", "")
	fs.String("api-key-env", "", "")
	fs.String("api-key-file", "", "")
	fs.Bool("pinned", false, "")
	switched := fs.Bool("switched", false, "")
	fs.Bool("fail-closed", false, "")
	fs.Bool("exit-comfyui-idle", false, "")
	fs.Duration("exit-comfyui-idle-after", 0, "")
	if err := fs.Parse(argv); err != nil {
		os.Exit(2)
	}
	os.Exit(runner.Run(runner.Options{
		Socket: *socket, Engine: *as, Group: *group, Priority: *prio, Needs: needs.Bytes(), Card: *card,
		Switched: *switched, Wait: *wait, Grace: *grace, Argv: fs.Args(), Stderr: os.Stderr,
	}))
}

func fakeEngine(addr, model string, load time.Duration) {
	nSlots, kvlog := slotsSpec()
	record := func(line string) {
		if kvlog == "" {
			return
		}
		// One small Fprintln behind O_APPEND: each line lands whole, so
		// order between handlers is arrival order.
		f, err := os.OpenFile(kvlog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintln(f, line)
	}
	mux := http.NewServeMux()
	// The port is bound at once and answers 503 until load passes, as a real
	// engine does while it loads: binding late would leave a window in which
	// another process takes the port and this engine never answers.
	loaded := time.Now().Add(load)
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if time.Now().Before(loaded) {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, model)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		record("chat " + model)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range []string{"lease", "hand off", "never collide"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	if nSlots > 0 || kvlog != "" {
		// The slot surface, off unless the test asks: an idle slot list,
		// and save/restore that answer the engine's own numbers and log
		// the call in arrival order.
		mux.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
			ss := []string{}
			for i := 0; i < nSlots; i++ {
				ss = append(ss, fmt.Sprintf(`{"id":%d,"is_processing":false}`, i))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(ss, ","))
		})
		mux.HandleFunc("/slots/", func(w http.ResponseWriter, r *http.Request) {
			slot, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/slots/"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			action := r.URL.Query().Get("action")
			var b struct {
				Filename string `json:"filename"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			switch action {
			case "restore":
				record(fmt.Sprintf("restore %d %s", slot, b.Filename))
				fmt.Fprintf(w, `{"id_slot":%d,"filename":%q,"n_restored":12,"n_read":4096,"timings":{"restore_ms":1.0}}`, slot, b.Filename)
			case "save":
				record(fmt.Sprintf("save %d %s", slot, b.Filename))
				fmt.Fprintf(w, `{"id_slot":%d,"filename":%q,"n_saved":1000,"n_written":8192,"timings":{"save_ms":1.0}}`, slot, b.Filename)
			default:
				http.NotFound(w, r)
			}
		})
	}
	if every := os.Getenv("BERTH_GW_LOG_EVERY"); every != "" {
		d, err := time.ParseDuration(every)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad BERTH_GW_LOG_EVERY %q: %v\n", every, err)
			os.Exit(2)
		}
		go func() {
			for i := 0; ; i++ {
				fmt.Fprintf(os.Stderr, "engine line %d\n", i)
				time.Sleep(d)
			}
		}()
	}
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { time.Sleep(load + 8*time.Second); _ = srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "fake engine on %s: %v\n", addr, err)
	}
}

type emptyCard struct{}

func (emptyCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const testKey = "berth-test-key-0123456789"

// call sends an authorised request the way an OpenAI client does.
func call(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// pickedPorts are the ports freeAddr handed out in this test binary.
var pickedPorts sync.Map

// freeAddr is a loopback address for a fake engine to bind later. A port the
// kernel picked (127.0.0.1:0) goes back to the ephemeral pool once closed,
// and any socket opened before the engine binds can take it: an httptest
// server, or a client's local port. So the port comes from below every OS's
// ephemeral range (Linux starts at 32768, Windows and macOS at 49152), which
// the kernel never hands out on its own, and never twice in one binary.
func freeAddr(t *testing.T) string {
	t.Helper()
	for range 200 {
		port := 20000 + rand.IntN(12000)
		if _, taken := pickedPorts.LoadOrStore(port, true); taken {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // something on this machine already listens there
		}
		ln.Close()
		return ln.Addr().String()
	}
	t.Fatal("no free loopback port below the ephemeral range")
	return ""
}

func daemon(t *testing.T) (string, *broker.Broker) {
	t.Helper()
	return daemonWith(t, broker.Config{})
}

// daemonWith is daemon with cfg's fields on top of the test defaults.
func daemonWith(t *testing.T, cfg broker.Config) (string, *broker.Broker) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Poll, cfg.DefaultWait, cfg.Log = 20*time.Millisecond, 10*time.Second, log
	b := broker.New(cfg, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	go func() { _ = server.Serve(ctx, ln, node.One(b), server.Extras{}, log) }()
	// Cleanups run last first, so this runs before the cancel above. An
	// engine a test's gateway started may still be up; stopping the daemon
	// under it makes its berth run write an unprotected-run record beside
	// the socket while the temp dir is being removed. A hold drains every
	// engine first, through the normal ladder, and is let go at once.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if l, err := b.Acquire(ctx, broker.Request{Engine: "test-cleanup", Priority: broker.Hold, Pinned: true, Wait: 10 * time.Second}, nil); err == nil {
			l.Release()
		}
	})
	return sock, b
}

// A request for a model nobody is running: the gateway takes the lease for
// its engine, which drains the engine holding the card, starts it once the
// card is free, and streams its reply. The broker counts the avoided
// collision, as for any lease that waited.
func TestColdStartDrainsHolderAndStreams(t *testing.T) {
	sock, b := daemon(t)

	holderOut := &syncBuf{}
	holder := make(chan int, 1)
	go func() {
		holder <- runner.Run(runner.Options{Socket: sock, Engine: "comfyui", Priority: "batch",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: holderOut})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(holderOut.String(), "granted") {
		if time.Now().After(deadline) {
			t.Fatalf("holder never granted: %s", holderOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",qwen3-8b")
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "llama-server", Type: "llamacpp", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Serves: []string{"qwen3-8b"}, Priority: "interactive"}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "GET", gw.URL+"/v1/models", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"qwen3-8b"`) {
		t.Fatalf("models = %s", body)
	}

	resp = call(t, "POST", gw.URL+"/v1/chat/completions",
		`{"model":"qwen3-8b","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var chunks []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			chunks = append(chunks, line)
		}
	}
	if len(chunks) != 4 || chunks[3] != "data: [DONE]" || !strings.Contains(chunks[2], "never collide") {
		t.Fatalf("stream = %q", chunks)
	}

	select {
	case code := <-holder:
		if code != 128+15 {
			t.Fatalf("holder exit %d, want 143 (drained)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder was not drained")
	}
	st := b.Status()
	if st.Counters.CollisionsAvoided != 1 || st.Counters.Drains != 1 {
		t.Fatalf("counters = %+v", st.Counters)
	}
	if len(st.Leases) != 1 || st.Leases[0].Engine != "llama-server" {
		t.Fatalf("leases = %+v", st.Leases)
	}
}

// An unknown model is a 404 naming the catalog, and nothing is started.
func TestUnknownModel(t *testing.T) {
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Key: testKey}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"nope"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `no engine on this node serves \"nope\"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := len(b.Status().Leases) + len(b.Status().Waiting); n != 0 {
		t.Fatalf("%d leases after an unknown model", n)
	}
}

// A configured engine that is already up (started by hand or by its own
// manager) goes straight to it: no lease, no start.
func TestRunningEngineIsProxiedWithoutStarting(t *testing.T) {
	addr := freeAddr(t)
	go fakeEngine(addr, "llama3", 300*time.Millisecond)
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey,
		Engines: []config.Engine{{Name: "ollama", Type: "ollama", URL: "http://" + addr, Serves: []string{"llama3"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !g.answers(context.Background(), "http://"+addr) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"llama3"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "never collide") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := len(b.Status().Leases); n != 0 {
		t.Fatalf("the gateway took %d lease(s) for an engine that was already up", n)
	}
}

// Without the key, from a browser page, or under a rebinding Host name, a
// request is refused before anything is resolved or started.
func TestGuard(t *testing.T) {
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Key: testKey,
		Engines: []config.Engine{{Name: "e", URL: "http://127.0.0.1:1", Start: []string{"false"}, Serves: []string{"m"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	cases := []struct {
		name   string
		header map[string]string
		host   string
		want   int
	}{
		{"no key", nil, "", http.StatusUnauthorized},
		{"wrong key", map[string]string{"Authorization": "Bearer nope"}, "", http.StatusUnauthorized},
		{"key as basic", map[string]string{"Authorization": "Basic " + testKey}, "", http.StatusUnauthorized},
		{"browser page", map[string]string{"Authorization": "Bearer " + testKey, "Origin": "https://evil.example"}, "", http.StatusForbidden},
		{"rebinding host", map[string]string{"Authorization": "Bearer " + testKey}, "evil.example:9799", http.StatusForbidden},
	}
	for _, c := range cases {
		for _, ep := range []struct{ method, path, body string }{
			{"GET", "/v1/models", ""},
			{"POST", "/v1/chat/completions", `{"model":"m"}`},
		} {
			req, _ := http.NewRequest(ep.method, gw.URL+ep.path, strings.NewReader(ep.body))
			for k, v := range c.header {
				req.Header.Set(k, v)
			}
			if c.host != "" {
				req.Host = c.host
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("%s %s %s: status %d, want %d", c.name, ep.method, ep.path, resp.StatusCode, c.want)
			}
		}
	}
	if n := len(b.Status().Leases) + len(b.Status().Waiting); n != 0 {
		t.Fatalf("%d leases after refused requests", n)
	}
	resp := call(t, "GET", gw.URL+"/v1/models", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with the key: status %d", resp.StatusCode)
	}
}

// TestAllowHosts proves a loopback gateway behind a same-machine proxy (tailscale
// serve forwards the tailnet name as Host): the named host is admitted with the
// key, the key and Origin checks still apply to it, and any other name stays refused.
func TestAllowHosts(t *testing.T) {
	sock, _ := daemon(t)
	g := &Gateway{Socket: sock, Key: testKey, AllowHosts: []string{"box.tail1234.ts.net"}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	cases := []struct {
		name   string
		host   string
		header map[string]string
		want   int
	}{
		{"allowed name, key", "box.tail1234.ts.net:9799", map[string]string{"Authorization": "Bearer " + testKey}, http.StatusOK},
		{"allowed name, any case", "BOX.tail1234.ts.net", map[string]string{"Authorization": "Bearer " + testKey}, http.StatusOK},
		{"allowed name, no key", "box.tail1234.ts.net", nil, http.StatusUnauthorized},
		{"allowed name, browser page", "box.tail1234.ts.net", map[string]string{"Authorization": "Bearer " + testKey, "Origin": "https://evil.example"}, http.StatusForbidden},
		{"other name", "evil.example", map[string]string{"Authorization": "Bearer " + testKey}, http.StatusForbidden},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", gw.URL+"/v1/models", nil)
		req.Host = c.host
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestDecideGuard(t *testing.T) {
	const GiB = uint64(1 << 30)
	cases := []struct {
		name      string
		avail     uint64
		floor     uint64
		estimate  uint64
		wantAllow bool
		wantCode  string
	}{
		{"plenty allows", 32 * GiB, 2 * GiB, 8 * GiB, true, ""},
		{"exact need allows", 10 * GiB, 2 * GiB, 8 * GiB, true, ""},
		{"one byte short denies", 10*GiB - 1, 2 * GiB, 8 * GiB, false, "no_headroom"},
		{"spike footprint denies", 20 * GiB, 2 * GiB, 30 * GiB, false, "no_headroom"},
		{"unknown estimate allows even when tight", GiB / 2, 2 * GiB, 0, true, ""},
		{"nothing free denies a real estimate", 0, 2 * GiB, 8 * GiB, false, "no_headroom"},
	}
	for _, c := range cases {
		d := decideGuard(c.avail, c.floor, c.estimate)
		if d.allow != c.wantAllow || d.code != c.wantCode {
			t.Errorf("%s: allow=%v code=%q, want %v %q", c.name, d.allow, d.code, c.wantAllow, c.wantCode)
		}
		if !c.wantAllow && d.msg == "" {
			t.Errorf("%s: deny carries no message", c.name)
		}
		if d.avail != c.avail {
			t.Errorf("%s: avail=%d, want %d", c.name, d.avail, c.avail)
		}
	}
	// A deny names the numbers so the operator sees what to tune.
	d := decideGuard(5*GiB, 2*GiB, 8*GiB)
	if !strings.Contains(d.msg, "MiB") {
		t.Errorf("deny message names no sizes: %q", d.msg)
	}
}

func postModel(t *testing.T, g *Gateway, model string) (int, string) {
	t.Helper()
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":`+strconv.Quote(model)+`}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func guardGateway(t *testing.T, avail uint64, ok bool, needsRAMMiB uint64, marker string) *Gateway {
	t.Helper()
	return &Gateway{
		Socket: filepath.Join(t.TempDir(), "no-daemon.sock"),
		Ready:  10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MemAvailable: func() (uint64, bool) { return avail, ok },
		Engines: []config.Engine{{Name: "strata", Type: "strata",
			URL:   "http://127.0.0.1:1", // nothing answers here
			Start: []string{"touch", marker}, Serves: []string{"big-model"},
			NeedsRAMMiB: needsRAMMiB}},
	}
}

// A cold start that would leave the machine without its RAM floor is refused
// before anything spawns: a 503 naming no_headroom, and the start command
// never runs.
func TestColdStartRefusedWithoutHeadroom(t *testing.T) {
	const GiB = uint64(1 << 30)
	marker := filepath.Join(t.TempDir(), "started")
	g := guardGateway(t, 4*GiB, true, 8<<10, marker)
	status, body := postModel(t, g, "big-model")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", status, body)
	}
	if !strings.Contains(body, "no_headroom") {
		t.Fatalf("body names no code: %s", body)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the refused start ran anyway")
	}
}

// An engine with no configured footprint, or a box that does not report
// memory, fails open toward starting: the guard never strands a request on
// unknown data. Here the start runs (fail-open `berth run` touches the
// marker) and the failure, if any, names something other than no_headroom.
func TestColdStartFailsOpenOnUnknownData(t *testing.T) {
	const GiB = uint64(1 << 30)
	for _, c := range []struct {
		name        string
		avail       uint64
		ok          bool
		needsRAMMiB uint64
	}{
		{"no estimate", GiB / 2, true, 0},
		{"no memory readings", 0, false, 8 << 10},
	} {
		marker := filepath.Join(t.TempDir(), "started")
		g := guardGateway(t, c.avail, c.ok, c.needsRAMMiB, marker)
		status, body := postModel(t, g, "big-model")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want 503: %s", c.name, status, body)
		}
		if strings.Contains(body, "no_headroom") {
			t.Fatalf("%s: refused on unknown data: %s", c.name, body)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("%s: the allowed start never ran: %v", c.name, err)
		}
	}
}

// A start that cannot get the card within the request's window gives up with
// the request: when the holder later leaves, the engine is not started.
func TestQueuedStartDoesNotOutliveTheRequest(t *testing.T) {
	sock, b := daemon(t)
	holderOut := &syncBuf{}
	holder := make(chan int, 1)
	go func() { // foreground beats the gateway's interactive start: no drain
		holder <- runner.Run(runner.Options{Socket: sock, Engine: "ninfer", Priority: "foreground",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "3"}, Stderr: holderOut})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(holderOut.String(), "granted") {
		if time.Now().After(deadline) {
			t.Fatalf("holder never granted: %s", holderOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",late")
	g := &Gateway{Socket: sock, Ready: 500 * time.Millisecond, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "late-engine", URL: "http://" + addr, Start: []string{os.Args[0]}, Serves: []string{"late"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"late"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 while the card is held", resp.StatusCode)
	}
	<-holder
	time.Sleep(time.Second) // the card is free now; a leftover waiter would be granted here
	for _, l := range b.Status().Leases {
		if l.Engine == "late-engine" {
			t.Fatalf("the abandoned start was granted after the request gave up: %+v", l)
		}
	}
	if g.answers(context.Background(), "http://"+addr) {
		t.Fatal("the abandoned start ran the engine")
	}
}

// A start that ended before its engine answered says why, as a code, and a
// crash quotes the engine's own cause line, not berth run's last line.
func TestClassify(t *testing.T) {
	for _, c := range []struct {
		exit   int
		stderr string
		output string
		code   string
		says   string
	}{
		{75, "berth: waiting: juggle: llama-swap keeps the card over strata (order: llama-swap > strata)\nberth: refused: gave up", "", "outranked", "juggle"},
		{143, "berth: granted strata-1\nberth: draining strata: NInfer (foreground) needs the card", "", "drained_while_starting", "NInfer"},
		{runner.ExitRefused, "berth: refused: gave up after 10m0s", "", "no_berth", "gave up"},
		{1, "berth: granted strata-1", "loading\nTraceback (most recent call last):\n  boom\nValueError: bad pack\n", "start_failed", "ValueError: bad pack"},
		{1, "berth: granted strata-1", "", "start_failed", "granted strata-1"},
		// 2026-10-19: Strata's run config lost its 700 MiB VRAM reserve; the
		// expert cache took the card and cuBLAS could not make its handle.
		{1, "berth: granted strata-17", "EngineDied: the engine exited before it was ready\n  strata serve: prefill gemm: cublasCreate: cuBLAS status 1\n", "engine_oom", "cuBLAS status 1"},
		{1, "berth: granted llamacpp-2", "ggml_backend_cuda_buffer_type_alloc_buffer: allocating 9000 MiB on device 0: cudaMalloc failed: out of memory\nmain: exiting due to model loading error\n", "engine_oom", "model loading error"},
		{1, "berth: granted comfyui-3", "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB\n", "engine_oom", "OutOfMemoryError"},
	} {
		got := classify("strata", c.exit, c.stderr, c.output)
		if got.code != c.code || !strings.Contains(got.msg, c.says) {
			t.Errorf("classify(%d, %q, %q) = %s %q, want %s naming %q", c.exit, c.stderr, c.output, got.code, got.msg, c.code, c.says)
		}
	}
}

// An engine that dies before it answers: the 503 names the engine's own
// cause, and the retries a client fires straight after it do not cold-start
// the same crash again; they get the cause at once, with a Retry-After.
func TestCrashedStartSaysWhyAndCoolsDown(t *testing.T) {
	sock, _ := daemon(t)
	marker := filepath.Join(t.TempDir(), "starts")
	crash := `echo start >> "$1"; echo "loading the model"; echo "prefill gemm: cublasCreate: cuBLAS status 1" >&2; exit 1`
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "crashy", URL: "http://" + freeAddr(t), Start: []string{"sh", "-c", crash, "sh", marker}, Serves: []string{"m"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	for i, wantRetry := range []string{"30", "15", "15"} {
		resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"m"}`)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"engine_oom"`) ||
			!strings.Contains(string(body), "cuBLAS status 1") || strings.Contains(string(body), "granted") {
			t.Fatalf("try %d: %d %s, want a 503 engine_oom quoting the engine", i, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Retry-After"); got != wantRetry {
			t.Fatalf("try %d: Retry-After %q, want %q (the cooldown left): %s", i, got, wantRetry, body)
		}
	}
	starts, _ := os.ReadFile(marker)
	if n := strings.Count(string(starts), "start"); n != 1 {
		t.Fatalf("the engine started %d times, want 1: the retries must not re-run the crash", n)
	}
}

func TestCooldownDoublesToItsCap(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 15 * time.Second, 2: 30 * time.Second, 3: time.Minute, 6: 5 * time.Minute, 40: 5 * time.Minute} {
		if got := cooldown(n); got != want {
			t.Errorf("cooldown(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestTailBufKeepsTheEnd(t *testing.T) {
	var b tailBuf
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "token batch %d\n", i)
	}
	fmt.Fprint(&b, "the real cause\n")
	if s := b.String(); len(s) > tailMax || !strings.HasSuffix(s, "the real cause\n") {
		t.Fatalf("tail is %d bytes ending %q", len(s), s[max(0, len(s)-40):])
	}
}

// comfyMux is the ComfyUI stand-in both the fake child and the tests serve:
// the queue and prompt endpoints plus the read-only surface, each answering
// a marker the tests assert was proxied unchanged. Like ComfyUI, it refuses
// a request whose Origin names another host than its Host.
func comfyMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"queue_running":[],"queue_pending":[]}`)
	})
	prompt := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"class_type"`) {
			http.Error(w, "not a workflow", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"prompt_id":"gw-test-1","number":1,"node_errors":{}}`)
	}
	mux.HandleFunc("/prompt", prompt)
	mux.HandleFunc("/api/prompt", prompt)
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"system":{"os":"stats-marker"}}`)
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"gw-test-1":{"marker":"history-marker"}}`)
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		fmt.Fprint(w, "view-marker-bytes")
	})
	mux.HandleFunc("/upload/image", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"up.png","subfolder":"","type":"input"}`)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		_, _ = io.Copy(rw, rw) // echo raw frames back
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://") != r.Host {
			http.Error(w, "origin does not match host", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func fakeComfy(addr string) {
	time.Sleep(300 * time.Millisecond) // a real engine takes a moment to load
	srv := &http.Server{Addr: addr, Handler: comfyMux()}
	go func() { time.Sleep(8 * time.Second); _ = srv.Close() }()
	_ = srv.ListenAndServe()
}

// A /prompt with ComfyUI down takes its lease like any other request: the
// holder is drained, the engine starts, and the queued prompt_id comes back.
func TestComfyPromptColdStartsAndProxies(t *testing.T) {
	sock, b := daemon(t)

	holderOut := &syncBuf{}
	holder := make(chan int, 1)
	go func() {
		holder <- runner.Run(runner.Options{Socket: sock, Engine: "llama-swap", Priority: "batch",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: holderOut})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(holderOut.String(), "granted") {
		if time.Now().After(deadline) {
			t.Fatalf("holder never granted: %s", holderOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	addr := freeAddr(t)
	t.Setenv("BERTH_GW_COMFY", addr)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Priority: "interactive"}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "POST", gw.URL+"/prompt", `{"prompt":{"3":{"class_type":"KSampler"}},"client_id":"gw-test"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"prompt_id":"gw-test-1"`) {
		t.Fatalf("prompt %d: %s", resp.StatusCode, body)
	}

	select {
	case code := <-holder:
		if code != 128+15 {
			t.Fatalf("holder exit %d, want 143 (drained)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder was not drained")
	}
	st := b.Status()
	if st.Counters.CollisionsAvoided != 1 || st.Counters.Drains != 1 {
		t.Fatalf("counters = %+v", st.Counters)
	}
	if len(st.Leases) != 1 || st.Leases[0].Engine != "comfyui" {
		t.Fatalf("leases = %+v", st.Leases)
	}
}

// ComfyUI's own port behind Berth answers a browser with no key, the way
// ComfyUI's port did: the web UI's /api/prompt starts ComfyUI under a
// lease, draining the holder, and the rest of the surface passes through.
func TestComfyUIPortStartsOnPromptAndPassesTheRest(t *testing.T) {
	sock, b := daemon(t)
	holderOut := &syncBuf{}
	holder := make(chan int, 1)
	go func() {
		holder <- runner.Run(runner.Options{Socket: sock, Engine: "llama-swap", Priority: "batch",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: holderOut})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(holderOut.String(), "granted") {
		if time.Now().After(deadline) {
			t.Fatalf("holder never granted: %s", holderOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	addr := freeAddr(t)
	t.Setenv("BERTH_GW_COMFY", addr)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Priority: "interactive"}}}
	front := httptest.NewServer(g.ComfyUIHandler())
	defer front.Close()
	browser := func(method, path, body string) (int, string) {
		req, err := http.NewRequest(method, front.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "10.0.0.200:8188"
		req.Header.Set("Origin", "http://10.0.0.200:8188")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := browser("POST", "/api/prompt", `{"prompt":{"3":{"class_type":"KSampler"}}}`); code != http.StatusOK ||
		body != `{"prompt_id":"gw-test-1","number":1,"node_errors":{}}` {
		t.Fatalf("/api/prompt %d: %s", code, body)
	}
	select {
	case code := <-holder:
		if code != 128+15 {
			t.Fatalf("holder exit %d, want 143 (drained)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder was not drained")
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "comfyui" {
		t.Fatalf("leases = %+v", st.Leases)
	}
	if code, body := browser("GET", "/system_stats", ""); code != http.StatusOK || body != `{"system":{"os":"stats-marker"}}` {
		t.Fatalf("/system_stats %d: %s", code, body)
	}
}

// With ComfyUI already up the whole surface proxies unchanged, and the
// gateway takes no lease for it.
func TestComfyPassthroughWhenUp(t *testing.T) {
	srv := httptest.NewServer(comfyMux())
	defer srv.Close()
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey,
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: srv.URL}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	cases := []struct{ method, path, body, want string }{
		{"POST", "/prompt", `{"prompt":{"3":{"class_type":"KSampler"}}}`, `"prompt_id":"gw-test-1"`},
		{"GET", "/queue", "", `"queue_pending":[]`},
		{"GET", "/history/gw-test-1", "", "history-marker"},
		{"GET", "/view?filename=out.png", "", "view-marker-bytes"},
		{"POST", "/upload/image", "bytes", `"name":"up.png"`},
	}
	for _, c := range cases {
		resp := call(t, c.method, gw.URL+c.path, c.body)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), c.want) {
			t.Errorf("%s %s: %d %s, want %s", c.method, c.path, resp.StatusCode, body, c.want)
		}
	}
	if n := len(b.Status().Leases); n != 0 {
		t.Fatalf("the gateway took %d lease(s) for a ComfyUI that was already up", n)
	}
}

// The websocket upgrade splices through to ComfyUI: 101 plus an echo.
func TestComfyWebSocketProxies(t *testing.T) {
	srv := httptest.NewServer(comfyMux())
	defer srv.Close()
	sock, _ := daemon(t)
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey,
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: srv.URL}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	host := strings.TrimPrefix(gw.URL, "http://")
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /ws?clientId=gw-test HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		host, testKey)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade status = %q, err %v", status, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("ping-bytes")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len("ping-bytes"))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(echo) != "ping-bytes" {
		t.Fatalf("echo = %q", echo)
	}
}

// With no ComfyUI anywhere a /prompt is a 404, and the side routes are a
// 503 naming /prompt when one is configured but down.
func TestComfyPromptResolves(t *testing.T) {
	sock, _ := daemon(t)
	g := &Gateway{Socket: sock, Key: testKey}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	resp := call(t, "POST", gw.URL+"/prompt", `{"prompt":{}}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("prompt with no comfyui = %d, want 404", resp.StatusCode)
	}

	g2 := &Gateway{Socket: sock, Key: testKey,
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://" + freeAddr(t)}}}
	gw2 := httptest.NewServer(g2.Handler())
	defer gw2.Close()
	resp = call(t, "GET", gw2.URL+"/queue", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "POST /prompt first") {
		t.Fatalf("queue with comfyui down = %d %s, want the 503", resp.StatusCode, body)
	}
}

// A start that times out while its lease still waits for the card tells the
// client where it stands: its place in line, and a Retry-After of at least
// 30 s. Here a pinned hold keeps the card, so the gateway's run waits.
func TestNotReadyCarriesTheQueue(t *testing.T) {
	sock, b := daemon(t)
	hold, err := b.Acquire(context.Background(), broker.Request{Engine: "game", Priority: broker.Hold, Pinned: true, Wait: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	g := &Gateway{Socket: sock, Key: testKey, Ready: 500 * time.Millisecond, Log: slogDiscard(),
		Engines: []config.Engine{{Name: "e", URL: "http://" + freeAddr(t), Start: []string{os.Args[0]}, Serves: []string{"m"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"m"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Berth-Queue-Position") != "1" ||
		resp.Header.Get("Retry-After") != "30" || !strings.Contains(string(body), "number 1 in line") {
		t.Fatalf("status %d, queue %q, Retry-After %q: %s", resp.StatusCode,
			resp.Header.Get("X-Berth-Queue-Position"), resp.Header.Get("Retry-After"), body)
	}
}

// The broker's start estimate for the engine's lease reaches the client
// rounded up and marked as a lower bound, and Retry-After waits for the
// larger of it and 30 s. Another engine's place in line is not this one's.
func TestQueuedStartHeaders(t *testing.T) {
	for _, tc := range []struct {
		estimate            float64
		retry, queue, start string
	}{
		{44.2, "45", "2", "45; lower-bound"},
		{5, "30", "2", "5; lower-bound"},
		{0, "30", "2", ""},
	} {
		sock := statusDaemon(t, proto.Status{
			GPUs: []proto.GPU{{Card: "gpu0", Scheduled: true}},
			Waiting: []proto.Lease{
				{Card: "gpu0", Engine: "other", Position: 1, EstimateS: 900},
				{Card: "gpu0", Engine: "e", Position: 2, EstimateS: tc.estimate},
			},
		})
		g := &Gateway{Socket: sock, Log: slogDiscard()}
		w := httptest.NewRecorder()
		g.startFailed(w, "e", "m", g.queued(&config.Engine{Name: "e"}, &startError{code: "not_ready", msg: "e did not answer"}))
		h := w.Result().Header
		got := [...]string{h.Get("Retry-After"), h.Get("X-Berth-Queue-Position"), h.Get("X-Berth-Start-Estimate")}
		if got != [...]string{tc.retry, tc.queue, tc.start} {
			t.Errorf("estimate %v s: Retry-After, queue, start = %q", tc.estimate, got)
		}
	}
}
