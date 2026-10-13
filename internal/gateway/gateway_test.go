package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
)

// TestMain doubles as a fake OpenAI-compatible engine: with
// BERTH_GW_ENGINE=addr,model the test binary serves that model on addr and
// streams a three-chunk reply, then exits after a few seconds.
func TestMain(m *testing.M) {
	if spec := os.Getenv("BERTH_GW_ENGINE"); spec != "" {
		addr, model, _ := strings.Cut(spec, ",")
		fakeEngine(addr, model)
		return
	}
	os.Exit(m.Run())
}

func fakeEngine(addr, model string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, model)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range []string{"lease", "hand off", "never collide"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	time.Sleep(300 * time.Millisecond) // a real engine takes a moment to load
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { time.Sleep(8 * time.Second); _ = srv.Close() }()
	_ = srv.ListenAndServe()
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

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func daemon(t *testing.T) (string, *broker.Broker) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 10 * time.Second, Log: log}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	go func() { _ = server.Serve(ctx, ln, b, server.Extras{}, log) }()
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
	go fakeEngine(addr, "llama3")
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
