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
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "llama-server", Type: "llamacpp", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Serves: []string{"qwen3-8b"}, Priority: "interactive"}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"qwen3-8b"`) {
		t.Fatalf("models = %s", body)
	}

	resp, err = http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"qwen3-8b","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
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
	g := &Gateway{Socket: sock}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `no engine on this node serves \"nope\"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := len(b.Status().Leases) + len(b.Status().Waiting); n != 0 {
		t.Fatalf("%d leases after an unknown model", n)
	}
}

// A model an engine already serves goes straight to it: no lease, no start.
func TestLiveEngineIsProxiedWithoutStarting(t *testing.T) {
	addr := freeAddr(t)
	go fakeEngine(addr, "llama3")
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Ready: 5 * time.Second,
		Engines: []config.Engine{{Name: "ollama", Type: "ollama", URL: "http://" + addr, Serves: []string{"llama3"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !g.answers(context.Background(), "http://"+addr) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"llama3"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "never collide") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := len(b.Status().Leases); n != 0 {
		t.Fatalf("the gateway took %d lease(s) for an engine that was already up", n)
	}
}
