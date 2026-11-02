// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package engines

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

func TestParseProm(t *testing.T) {
	v := ParseProm(`# HELP llamacpp:tokens_predicted_total x
# TYPE llamacpp:tokens_predicted_total counter
llamacpp:tokens_predicted_total 120
llamacpp:requests_processing{slot="0"} 1
llamacpp:requests_processing{slot="1",note="a}b"} 2
bad line here
ninfer:draft_accepted_tokens_total 7e1
`)
	if v["llamacpp:tokens_predicted_total"] != 120 || v["llamacpp:requests_processing"] != 3 || v["ninfer:draft_accepted_tokens_total"] != 70 {
		t.Fatalf("parsed %v", v)
	}
}

// llama-swap lists a loaded NInfer and a loading model; only the ready one's
// upstream metrics are summed, and decode tok/s comes from counter deltas.
func TestLlamaSwapFollowsUpstreamsAndComputesRates(t *testing.T) {
	var tokens atomic.Int64
	tokens.Store(1000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := tokens.Load()
		fmt.Fprintf(w, "llamacpp:tokens_predicted_total %d\nllamacpp:tokens_predicted_seconds_total %g\nllamacpp:requests_processing 1\nninfer:draft_tokens_total 30\n", n, float64(n)/100)
	}))
	defer upstream.Close()
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"running":[{"model":"NInfer-Qwen3.8-27B","state":"ready","proxy":%q},{"model":"Next","state":"starting","proxy":"http://127.0.0.1:1"}]}`, upstream.URL)
	}))
	defer swap.Close()

	w := &Watcher{Engines: []config.Engine{{Name: "llama-swap", Type: "llama-swap", URL: swap.URL}}}
	w.Poll(context.Background())
	tokens.Store(1500) // 500 tokens in 5 s of decode = 100 t/s
	w.Poll(context.Background())

	got := w.Snapshot()
	if len(got) != 1 || !got[0].Up || len(got[0].Models) != 2 {
		t.Fatalf("snapshot = %+v", got)
	}
	e := got[0]
	if e.Metrics.OutputTokens != 1500 || e.Metrics.Processing != 1 || e.Metrics.DraftTokens != 30 {
		t.Fatalf("metrics = %+v", e.Metrics)
	}
	if e.DecodeTPS < 99.9 || e.DecodeTPS > 100.1 {
		t.Fatalf("decode = %v t/s, want 100", e.DecodeTPS)
	}
	if !e.MetricsReported {
		t.Fatal("metrics_reported = false with a serving upstream")
	}
}

// llama-swap starts llama-server without --metrics, so the proxy answers 501:
// the engine stays up with its model listed and the counters read unreported,
// never a measured zero.
func TestLlamaSwapWithoutMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprint(w, "This server does not support metrics endpoint. Start it with --metrics")
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"running":[{"model":"Qwen3-8B","state":"ready","proxy":%q}]}`, upstream.URL)
	}))
	defer swap.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "llama-swap", Type: "llama-swap", URL: swap.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || len(e.Models) != 1 || e.Models[0].Name != "Qwen3-8B" {
		t.Fatalf("engine = %+v", e)
	}
	if e.Metrics.OutputTokens != 0 || e.MetricsReported {
		t.Fatalf("metrics = %+v reported = %v, want zero counters unreported", e.Metrics, e.MetricsReported)
	}
}

// A llama-swap id is whatever the operator named the model, so the file its
// cmd loads is what ties it to the library (#197).
func TestLlamaSwapReportsTheModelFile(t *testing.T) {
	const cmd = `llama-server --port 5800 --model "F:\AI\.lmstudio\models\unsloth\Qwen3.8-27B-GGUF\Qwen3.8-27B-UD-IQ4_XS.gguf" -c 32768`
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"running":[{"model":"qwen-3.8-27b","state":"ready","cmd":%q},{"model":"no-cmd","state":"ready"}]}`, cmd)
	}))
	defer swap.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "llama-swap", Type: "llama-swap", URL: swap.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if len(e.Models) != 2 {
		t.Fatalf("engine = %+v", e)
	}
	if got, want := e.Models[0].File, `F:\AI\.lmstudio\models\unsloth\Qwen3.8-27B-GGUF\Qwen3.8-27B-UD-IQ4_XS.gguf`; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	if e.Models[1].File != "" {
		t.Errorf("a model with no cmd names no file, got %q", e.Models[1].File)
	}
}

func TestOllamaReportsModelsAsEngineClaims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"qwen3:8b","size_vram":6000000000}]}`)
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "ollama", Type: "ollama", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || len(e.Models) != 1 || e.Models[0].ReportedVRAM != 6000000000 {
		t.Fatalf("engine = %+v", e)
	}
}

func TestDownEngineIsReportedNotHidden(t *testing.T) {
	w := &Watcher{Engines: []config.Engine{{Name: "gone", Type: "llamacpp", URL: "http://127.0.0.1:1"}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if e.Up || e.Error == "" {
		t.Fatalf("down engine = %+v", e)
	}
	if e.State != "down" {
		t.Errorf("state = %q, want down (never confirmed up)", e.State)
	}
}

// TestEngineStateLatches: an engine that answers is up; one that was up
// and stops answering is latched - distinct from down, which was never
// confirmed up; answering again clears the latch, and a later failure
// latches it once more.
func TestEngineStateLatches(t *testing.T) {
	var up atomic.Bool
	up.Store(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "llamacpp:tokens_predicted_total 10\n")
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{
		{Name: "flip", Type: "llamacpp", URL: srv.URL},
		{Name: "never", Type: "llamacpp", URL: "http://127.0.0.1:1"},
	}}
	poll := func() (string, string, bool) {
		w.Poll(context.Background())
		s := w.Snapshot()
		return s[0].State, s[1].State, s[0].Up
	}
	cases := []struct {
		name   string
		up     bool
		flip   string // flip's state after the poll
		never  string
		flipUp bool
	}{
		{"fresh failure is down; never-up stays down", false, "down", "down", false},
		{"recovery is up", true, "up", "down", true},
		{"failure after up latches", false, "latched", "down", false},
		{"failure keeps it latched", false, "latched", "down", false},
		{"up again clears the latch", true, "up", "down", true},
		{"failure latches again", false, "latched", "down", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up.Store(c.up)
			gotFlip, gotNever, gotUp := poll()
			if gotFlip != c.flip {
				t.Errorf("flip state = %q, want %q", gotFlip, c.flip)
			}
			if gotNever != c.never {
				t.Errorf("never state = %q, want %q", gotNever, c.never)
			}
			if gotUp != c.flipUp {
				t.Errorf("flip up = %v, want %v (Up stays the boolean form)", gotUp, c.flipUp)
			}
		})
	}
}

// TestEngineStoppedNote: Stopped records the story a stop carried; a
// reading while the engine is down carries it on its card, the first
// story stays, and an up or latched reading clears it.
func TestEngineStoppedNote(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "llamacpp:tokens_predicted_total 10\n")
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "flip", Type: "llamacpp", URL: srv.URL}}}
	snap := func() (state, note string) {
		w.Poll(context.Background())
		s := w.Snapshot()[0]
		return s.State, s.StoppedNote
	}
	// A note that arrives while the engine is answering shows nothing
	// until it is down.
	w.Stopped("flip", "Turned down for ollama")
	if state, note := snap(); state != "up" || note != "" {
		t.Fatalf("answering engine shows note %q in state %q, want none", note, state)
	}
	// Berth takes the card: the silence reads down, carrying the story.
	up.Store(false)
	w.Finished("flip")
	if state, note := snap(); state != "down" || note != "Turned down for ollama" {
		t.Fatalf("ordered stop = %q with note %q, want down carrying the story", state, note)
	}
	// The first story stays: a later stop cannot overwrite it.
	w.Stopped("flip", "Stopped on command")
	if state, note := snap(); state != "down" || note != "Turned down for ollama" {
		t.Fatalf("second story replaced the first: %q in state %q", note, state)
	}
	// Answering again clears the note. Wait past the clock tick so the
	// recovery reading's stamp is strictly after the Finished mark, as in
	// operation.
	time.Sleep(20 * time.Millisecond)
	up.Store(true)
	if state, note := snap(); state != "up" || note != "" {
		t.Fatalf("recovery = %q with note %q, want up with none", state, note)
	}
	// A failure no one ordered latches: the note clears with the latch,
	// and the raw error is what the latched card carries.
	w.Stopped("flip", "Stopped for games")
	if state, note := snap(); state != "up" || note != "" {
		t.Fatalf("answering engine shows note %q in state %q, want none", note, state)
	}
	up.Store(false)
	if state, note := snap(); state != "latched" || note != "" {
		t.Fatalf("latched read = %q with note %q, want none", state, note)
	}
}

func TestCustomMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "my_out_total 42\nmy_queue 3\n")
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "mine", Type: "custom", URL: srv.URL, Metrics: "/stats",
		Map: map[string]string{"output_tokens": "my_out_total", "waiting": "my_queue"}}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if e.Metrics.OutputTokens != 42 || e.Metrics.Waiting != 3 {
		t.Fatalf("metrics = %+v", e.Metrics)
	}
	if !e.MetricsReported {
		t.Fatal("metrics_reported = false after a mapped metrics read")
	}
}

// llamacppSlotsFake serves the llama.cpp family's three endpoints: the
// required /metrics, and /props + /slots with one busy and one idle slot.
func llamacppSlotsFake() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, "llamacpp:prompt_tokens_total 100\nllamacpp:tokens_predicted_total 50\nllamacpp:requests_processing 1\n")
		case "/props":
			fmt.Fprint(w, `{"n_ctx":8192,"default_generation_settings":{}}`)
		case "/slots":
			fmt.Fprint(w, `[
				{"id":0,"state":"processing","n_ctx":8192,"n_prompt_tokens":4000,"n_prompt_tokens_cache":3500},
				{"id":1,"state":"idle","n_ctx":8192,"n_prompt_tokens":0,"n_prompt_tokens_cache":0}]`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// The llama.cpp family reads its context window from /props and its live KV
// pool from /slots, and both sit beside the metrics that decide up/down.
func TestLlamaCppReadsPropsAndSlots(t *testing.T) {
	srv := llamacppSlotsFake()
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "ninfer", Type: "llamacpp", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || e.Error != "" {
		t.Fatalf("engine = %+v", e)
	}
	if !e.MetricsReported {
		t.Fatal("metrics_reported = false after /metrics answered")
	}
	if e.Window != 8192 {
		t.Fatalf("window = %d, want 8192", e.Window)
	}
	if len(e.Slots) != 2 {
		t.Fatalf("slots = %+v, want 2", e.Slots)
	}
	s0, s1 := e.Slots[0], e.Slots[1]
	if s0.ID != "s0" || s0.Window != 8192 || s0.Used != 4000 || s0.Cached != 3500 || !s0.Processing || s0.Model != "" {
		t.Fatalf("slot 0 = %+v", s0)
	}
	if s1.ID != "s1" || s1.Used != 0 || s1.Cached != 0 || s1.Processing || s1.Model != "" {
		t.Fatalf("slot 1 = %+v", s1)
	}
}

// A build without /props and /slots (404 on both) stays up: zero window, no
// slots, no error - the pool panel simply has nothing for this engine.
func TestLlamaCppDegradesWithoutPropsAndSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			fmt.Fprint(w, "llamacpp:tokens_predicted_total 10\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "kobold", Type: "llamacpp", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || e.Error != "" {
		t.Fatalf("engine = %+v", e)
	}
	if e.Window != 0 || len(e.Slots) != 0 {
		t.Fatalf("window = %d slots = %+v, want 0 and none", e.Window, e.Slots)
	}
}

// NInfer shares the llama.cpp reader: the same /props and /slots fetches.
func TestNInferReadsSlots(t *testing.T) {
	srv := llamacppSlotsFake()
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "ninfer", Type: "ninfer", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || e.Window != 8192 || len(e.Slots) != 2 {
		t.Fatalf("engine = %+v", e)
	}
	if e.Slots[0].ID != "s0" || e.Slots[0].Used != 4000 || !e.Slots[0].Processing {
		t.Fatalf("slot 0 = %+v", e.Slots[0])
	}
}

// NInfer's live build reports is_processing, not llama.cpp's state, and 404s
// on /props: a generating slot must still read as processing, and the zero
// window must not read as an error.
func TestNInferSlotsUseIsProcessing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, "llamacpp:prompt_tokens_total 100\nllamacpp:tokens_predicted_total 50\nllamacpp:requests_processing 1\n")
		case "/slots":
			fmt.Fprint(w, `[
				{"id":0,"is_processing":true,"n_ctx":262144,"n_prompt_tokens":111166,"n_prompt_tokens_cache":111166},
				{"id":1,"is_processing":false,"n_ctx":262144,"n_prompt_tokens":0,"n_prompt_tokens_cache":0}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "ninfer", Type: "ninfer", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || e.Error != "" {
		t.Fatalf("engine = %+v", e)
	}
	if e.Window != 0 {
		t.Fatalf("window = %d, want 0: this build 404s on /props", e.Window)
	}
	if len(e.Slots) != 2 {
		t.Fatalf("slots = %+v, want 2", e.Slots)
	}
	s0, s1 := e.Slots[0], e.Slots[1]
	if s0.ID != "s0" || s0.Window != 262144 || s0.Used != 111166 || s0.Cached != 111166 || !s0.Processing {
		t.Fatalf("slot 0 = %+v, want the generating slot processing", s0)
	}
	if s1.ID != "s1" || s1.Processing {
		t.Fatalf("slot 1 = %+v, want idle", s1)
	}
}

// A down engine's best-effort fetches are skipped: a server that serves
// /slots and /props but not the /metrics that decides up/down stays down
// and carries no window and no slots.
func TestDownEngineFetchesNoSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			fmt.Fprint(w, `{"n_ctx":8192}`)
		case "/slots":
			fmt.Fprint(w, `[{"id":0,"state":"processing","n_ctx":8192,"n_prompt_tokens":4000,"n_prompt_tokens_cache":0}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "ninfer", Type: "ninfer", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if e.Up || e.Error == "" {
		t.Fatalf("engine = %+v, want down: /metrics 404 decides up/down", e)
	}
	if e.Window != 0 || len(e.Slots) != 0 {
		t.Fatalf("down engine = window %d slots %+v, want 0 and none", e.Window, e.Slots)
	}
}

// llama-swap takes each ready model's window from its proxy's /props and the
// model's slots from the proxy's /slots, tagged with the model's name; a
// starting model contributes neither. The engine's window is the max.
func TestLlamaSwapModelWindowAndSlots(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, "llamacpp:tokens_predicted_total 100\nllamacpp:tokens_predicted_seconds_total 1\n")
		case "/props":
			fmt.Fprint(w, `{"n_ctx":4096}`)
		case "/slots":
			fmt.Fprint(w, `[{"id":0,"state":"processing","n_ctx":4096,"n_prompt_tokens":1000,"n_prompt_tokens_cache":500}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/running" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"running":[{"model":"NInfer-Qwen3.8-27B","state":"ready","proxy":%q},{"model":"Next","state":"starting","proxy":"http://127.0.0.1:1"}]}`, upstream.URL)
	}))
	defer swap.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "llama-swap", Type: "llama-swap", URL: swap.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || len(e.Models) != 2 {
		t.Fatalf("engine = %+v", e)
	}
	if e.Models[0].Name != "NInfer-Qwen3.8-27B" || e.Models[0].Window != 4096 {
		t.Fatalf("ready model = %+v, want window 4096", e.Models[0])
	}
	if e.Models[1].Window != 0 {
		t.Fatalf("starting model = %+v, must carry no window", e.Models[1])
	}
	if e.Window != 4096 {
		t.Fatalf("engine window = %d, want the max ready model's 4096", e.Window)
	}
	if len(e.Slots) != 1 {
		t.Fatalf("slots = %+v, want the one slot of the ready model", e.Slots)
	}
	s := e.Slots[0]
	if s.ID != "s0" || s.Model != "NInfer-Qwen3.8-27B" || s.Window != 4096 || s.Used != 1000 || s.Cached != 500 || !s.Processing {
		t.Fatalf("slot = %+v", s)
	}
}

// strata's /health is open on every install; /metrics totals counters since
// the server started, and the rates come from the deltas between polls.
func TestStrataReadsHealthAndMetrics(t *testing.T) {
	var prompt, output, promptMS, decodeMS atomic.Int64
	prompt.Store(1000)
	output.Store(1000)
	promptMS.Store(2000)
	decodeMS.Store(50000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok","max_context":32768,"model":"strata-iq2_xs","images":true,"api_key":false,"loaded":true,"service":"strata"}`)
		case "/metrics":
			fmt.Fprintf(w, `{"engine":{"model":"strata-iq2_xs"},"live":{"state":"generating","queued":2},"totals":{"since":1,"requests":9,"prompt_tokens":%d,"reused":1,"output_tokens":%d,"prompt_ms":%d,"decode_ms":%d}}`, prompt.Load(), output.Load(), promptMS.Load(), decodeMS.Load())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "strata", Type: "strata", URL: srv.URL}}}
	w.Poll(context.Background())
	prompt.Add(1000) // 1000 prompt tokens in 2 s = 500 t/s
	output.Add(500)  // 500 tokens in 25 s of decode = 20 t/s
	promptMS.Add(2000)
	decodeMS.Add(25000)
	w.Poll(context.Background())

	got := w.Snapshot()
	if len(got) != 1 || !got[0].Up {
		t.Fatalf("snapshot = %+v", got)
	}
	e := got[0]
	if len(e.Models) != 1 || e.Models[0].Name != "strata-iq2_xs" || e.Models[0].State != "loaded" {
		t.Fatalf("models = %+v", e.Models)
	}
	if e.Metrics.OutputTokens != 1500 || e.Metrics.Processing != 1 || e.Metrics.Waiting != 2 {
		t.Fatalf("metrics = %+v", e.Metrics)
	}
	if !e.MetricsReported {
		t.Fatal("metrics_reported = false with serving /metrics")
	}
	if e.DecodeTPS < 19.9 || e.DecodeTPS > 20.1 {
		t.Fatalf("decode = %v t/s, want 20", e.DecodeTPS)
	}
	if e.PrefillTPS < 499.9 || e.PrefillTPS > 500.1 {
		t.Fatalf("prefill = %v t/s, want 500", e.PrefillTPS)
	}
	// the window comes from /health's max_context, on the model and on the engine
	if e.Window != 32768 || e.Models[0].Window != 32768 {
		t.Fatalf("windows: engine %d model %d, want 32768", e.Window, e.Models[0].Window)
	}
}

// a keyed strata install refuses /metrics; /health still reports the model
// and its window, so the engine stays up with zero counters.
func TestStrataKeyedInstallStaysUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{"status":"ok","max_context":16384,"model":"m1","loaded":true,"service":"strata"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	w := &Watcher{Engines: []config.Engine{{Name: "strata", Type: "strata", URL: srv.URL}}}
	w.Poll(context.Background())
	e := w.Snapshot()[0]
	if !e.Up || e.Error != "" {
		t.Fatalf("engine = %+v", e)
	}
	if len(e.Models) != 1 || e.Models[0].Name != "m1" || e.Models[0].State != "loaded" || e.Metrics.OutputTokens != 0 {
		t.Fatalf("engine = %+v", e)
	}
	// the window still parses from /health alone while /metrics is a 401
	if e.Window != 16384 || e.Models[0].Window != 16384 {
		t.Fatalf("windows: engine %d model %d, want 16384", e.Window, e.Models[0].Window)
	}
	if e.MetricsReported {
		t.Fatal("metrics_reported = true while /metrics is refused")
	}
}

// TestHeldRateDuringGeneration replays issue 147: llama.cpp adds a
// request's tokens to its counters only when the request ends, so reads
// inside a generation see frozen counters while requests_processing is 1.
// The engine shows the rate the counters last measured while it works, 0
// while idle, and forgets the rate when the counters go back (a restart).
func TestHeldRateDuringGeneration(t *testing.T) {
	var mu sync.Mutex
	tokens, seconds, processing := 1000.0, 100.0, 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "llamacpp:tokens_predicted_total %g\nllamacpp:tokens_predicted_seconds_total %g\nllamacpp:requests_processing %d\n", tokens, seconds, processing)
	}))
	defer srv.Close()
	set := func(tok, sec float64, proc int) {
		mu.Lock()
		tokens, seconds, processing = tok, sec, proc
		mu.Unlock()
	}
	w := &Watcher{Engines: []config.Engine{{Name: "llama", Type: "llamacpp", URL: srv.URL}}}
	decode := func() float64 {
		w.Poll(context.Background())
		return w.Snapshot()[0].DecodeTPS
	}
	for i, step := range []struct {
		tok, sec float64
		proc     int
		want     float64
	}{
		{1000, 100, 1, 0},    // first read: nothing measured yet
		{1000, 100, 1, 0},    // generating, no rate measured before
		{1525, 150, 1, 10.5}, // the request ended: 525 tokens in 50 s
		{1525, 150, 1, 10.5}, // the next request, counters frozen
		{1525, 150, 1, 10.5}, // still frozen, still generating
		{1525, 150, 0, 0},    // idle
		{1525, 150, 1, 10.5}, // a new request after idle shows the last rate
		{10, 1, 1, 0},        // a restart: the counters went back
		{10, 1, 1, 0},        // and the old rate is gone
	} {
		set(step.tok, step.sec, step.proc)
		if got := decode(); got != step.want {
			t.Fatalf("step %d: decode = %v t/s, want %v", i, got, step.want)
		}
	}
}
