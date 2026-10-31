// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package engines reads what the inference engines on this node report about
// themselves: loaded models, token counters, queue depth. It is read-only:
// Berth never sends an engine a prompt. These figures are the engine's own
// claims, shown beside the driver-measured memory, never mixed with it.
package engines

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/models"
	"github.com/T-Crypt/berth/internal/proto"
)

// llama.cpp's metric names; NInfer serves the same ones.
var llamacpp = map[string]string{
	"prompt_tokens":  "llamacpp:prompt_tokens_total",
	"output_tokens":  "llamacpp:tokens_predicted_total",
	"prompt_seconds": "llamacpp:prompt_seconds_total",
	"output_seconds": "llamacpp:tokens_predicted_seconds_total",
	"processing":     "llamacpp:requests_processing",
	"waiting":        "llamacpp:requests_deferred",
}

// Watcher reads every configured engine on a timer.
type Watcher struct {
	Engines []config.Engine
	Every   time.Duration
	Client  *http.Client
	// Discover turns on discovery (discover.go) over this machine, Hosts
	// and the extra Ports.
	Discover bool
	Hosts    []string
	Ports    []int

	mu       sync.Mutex
	state    map[string]proto.Engine
	latched  map[string]bool      // engine -> was up on a previous read and is failing again (State "latched")
	exited   map[string]time.Time // engine -> when its run last exited 0
	reqs     []proto.Request      // the request log, oldest first
	gen      []genSample          // counter deltas while exactly one model was loaded
	seen     map[string]bool
	since    time.Time
	ins      *proto.Insights // the last Insights result, computed at insAt
	insAt    time.Time
	insDirty bool                 // a request or generation sample arrived since insAt
	found    []config.Engine      // engines discovery added, after the configured ones
	lastUp   map[string]time.Time // engine -> last read that answered
	held     map[string]heldRates // engine -> the rates its counters last measured
}

// all is the configured engines then the discovered ones. Called with w.mu held.
func (w *Watcher) all() []config.Engine {
	out := make([]config.Engine, 0, len(w.Engines)+len(w.found))
	return append(append(out, w.Engines...), w.found...)
}

func (w *Watcher) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: 3 * time.Second}
}

func (w *Watcher) Run(ctx context.Context) {
	every := w.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var nextDiscovery time.Time
	for {
		if w.Discover && !time.Now().Before(nextDiscovery) {
			w.discover(ctx)
			nextDiscovery = time.Now().Add(DiscoveryEvery)
		}
		w.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollOnce reads every engine once, discovering first when discovery is on.
// It is the one-shot form of Run: no timer, no history and no rates, since a
// rate needs two readings.
func (w *Watcher) PollOnce(ctx context.Context) {
	if w.Discover {
		w.discover(ctx)
	}
	w.Poll(ctx)
}

// Poll reads every engine once.
func (w *Watcher) Poll(ctx context.Context) {
	w.mu.Lock()
	engines := w.all()
	w.mu.Unlock()
	for _, e := range engines {
		cur := w.read(ctx, e)
		w.mu.Lock()
		if w.lastUp == nil {
			w.lastUp = map[string]time.Time{}
		}
		if cur.Up {
			w.lastUp[e.Name] = cur.At
		}
		if w.state == nil {
			w.state = map[string]proto.Engine{}
		}
		if w.latched == nil {
			w.latched = map[string]bool{}
		}
		prev := w.state[e.Name]
		switch {
		case cur.Up:
			cur.State = "up"
			delete(w.latched, e.Name)
		case w.latched[e.Name]:
			cur.State = "latched" // was up; still not answering
		case prev.Up && prev.At.After(w.exited[e.Name]):
			cur.State = "latched"
			w.latched[e.Name] = true
		default:
			cur.State = "down" // never confirmed up
		}
		rates(&cur, prev)
		w.hold(&cur, prev)
		w.attribute(cur, prev)
		w.state[e.Name] = cur
		w.mu.Unlock()
	}
	w.mu.Lock()
	w.prune(time.Now())
	w.mu.Unlock()
}

// Finished records that the engine named name ended on purpose (exit 0,
// or a stop its run carried out), so it stopped answering on purpose: its
// latch clears, and a reading taken before the exit cannot latch it again.
func (w *Watcher) Finished(name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.all() {
		if !strings.EqualFold(e.Name, name) {
			continue
		}
		if w.exited == nil {
			w.exited = map[string]time.Time{}
		}
		w.exited[e.Name] = time.Now()
		delete(w.latched, e.Name)
	}
}

// Snapshot is the last reading of every engine, in config order.
func (w *Watcher) Snapshot() []proto.Engine {
	if w == nil {
		return []proto.Engine{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	engines := w.all()
	out := make([]proto.Engine, 0, len(engines))
	for _, e := range engines {
		if s, ok := w.state[e.Name]; ok {
			s.Age = time.Since(s.At).Seconds()
			out = append(out, s)
		}
	}
	return out
}

// genSample is what an engine's own counters say it generated between two
// reads, credited to the one model it had loaded. It measures decode speed
// and draft acceptance for engines whose request log does not report them.
type genSample struct {
	t                    time.Time
	engine, model        string
	out, outSec          float64
	draft, draftAccepted float64
}

// attribute records a counter delta when exactly one model was ready in both
// reads, so it can only belong to that model. Called with w.mu held.
func (w *Watcher) attribute(cur, prev proto.Engine) {
	if !cur.Up || !prev.Up {
		return
	}
	ready := func(e proto.Engine) string {
		var name string
		for _, m := range e.Models {
			if m.State == "ready" || m.State == "loaded" {
				if name != "" {
					return ""
				}
				name = m.Name
			}
		}
		return name
	}
	m := ready(cur)
	if m == "" || m != ready(prev) {
		return
	}
	g := genSample{t: cur.At, engine: cur.Name, model: m,
		out: cur.Metrics.OutputTokens - prev.Metrics.OutputTokens, outSec: cur.Metrics.OutputSeconds - prev.Metrics.OutputSeconds,
		draft: cur.Metrics.DraftTokens - prev.Metrics.DraftTokens, draftAccepted: cur.Metrics.DraftAccepted - prev.Metrics.DraftAccepted}
	if g.out <= 0 || g.outSec <= 0 || g.draft < 0 || g.draftAccepted < 0 {
		return // idle, or the engine restarted and its counters went back to 0
	}
	w.gen = append(w.gen, g)
	w.insDirty = true
	if w.since.IsZero() {
		w.since = time.Now()
	}
	cut := time.Now().Add(-keepRequests)
	i := 0
	for i < len(w.gen) && w.gen[i].t.Before(cut) {
		i++
	}
	w.gen = w.gen[i:]
}

// rates turns counter deltas into decode and prefill tok/s. Only a counter
// that moved forward yields a rate; a restart (counter went down) yields none.
func rates(cur *proto.Engine, prev proto.Engine) {
	if !cur.Up || !prev.Up {
		return
	}
	rate := func(tok, sec, ptok, psec float64) float64 {
		if dt, ds := tok-ptok, sec-psec; dt > 0 && ds > 0 {
			return dt / ds
		}
		return 0
	}
	m, p := cur.Metrics, prev.Metrics
	// Engines without busy-seconds counters (vLLM, SGLang, LocalAI,
	// FreeToken) get tokens over the wall time between reads, as Speculum
	// does: a throughput, lower than decode speed when the engine idles.
	wall := cur.At.Sub(prev.At).Seconds()
	outSec, pOutSec, promptSec, pPromptSec := m.OutputSeconds, p.OutputSeconds, m.PromptSeconds, p.PromptSeconds
	if outSec == 0 && pOutSec == 0 && wall > 0 {
		outSec = wall
	}
	if promptSec == 0 && pPromptSec == 0 && wall > 0 {
		promptSec = wall
	}
	cur.DecodeTPS = rate(m.OutputTokens, outSec, p.OutputTokens, pOutSec)
	cur.PrefillTPS = rate(m.PromptTokens, promptSec, p.PromptTokens, pPromptSec)
}

// heldRates are the decode and prefill rates an engine's counters last
// measured.
type heldRates struct {
	decode, prefill float64
}

// hold keeps an engine's last measured rates on show while it works.
// llama.cpp adds a request's tokens to its counters only when the request
// ends (issue 147), so every read inside a generation sees no change and
// rates gives 0 t/s. While a request is in flight and a counter has not
// moved, the engine shows the rate that counter last measured. An idle
// engine still reads 0, and a restart or a down engine forgets the rates.
func (w *Watcher) hold(cur *proto.Engine, prev proto.Engine) {
	if w.held == nil {
		w.held = map[string]heldRates{}
	}
	m, p := cur.Metrics, prev.Metrics
	if !cur.Up || m.OutputTokens < p.OutputTokens || m.PromptTokens < p.PromptTokens {
		delete(w.held, cur.Name)
		return
	}
	h := w.held[cur.Name]
	busy := m.Processing > 0
	for _, s := range cur.Slots {
		busy = busy || s.Processing
	}
	keep := func(rate *float64, last *float64, frozen bool) {
		switch {
		case *rate > 0:
			*last = *rate
		case busy && frozen:
			*rate = *last
		}
	}
	keep(&cur.DecodeTPS, &h.decode, m.OutputTokens == p.OutputTokens)
	keep(&cur.PrefillTPS, &h.prefill, m.PromptTokens == p.PromptTokens)
	w.held[cur.Name] = h
}

func (w *Watcher) read(ctx context.Context, e config.Engine) proto.Engine {
	out := proto.Engine{Name: e.Name, Type: e.Type, URL: e.URL, At: time.Now(), Models: []proto.Model{}}
	base := strings.TrimRight(e.URL, "/")
	var keyErr error
	if e.APIKeyFile != "" {
		var key string
		key, keyErr = e.APIKey()
		ctx = context.WithValue(ctx, credentialKey{}, credential{key, keyErr})
	}
	var err error
	switch e.Type {
	case "llamacpp", "ninfer":
		out.Metrics, err = w.metrics(ctx, base+"/metrics", llamacpp)
		// Best-effort, and only when the metrics read that alone decides
		// up/down succeeded: the context window and the live KV slot pool.
		// A build without /props, or that 501s on /slots (KoboldCpp), stays
		// up with a zero window or no slots; a down engine fetches neither.
		if err == nil {
			out.MetricsReported = true
			if b, perr := w.get(ctx, base+"/props"); perr == nil {
				var p struct {
					NCtx  int    `json:"n_ctx"`
					Alias string `json:"model_alias"`
				}
				if json.Unmarshal(b, &p) == nil {
					out.Window = p.NCtx
					if p.Alias != "" {
						out.Models = append(out.Models, proto.Model{Name: p.Alias, State: "loaded", Window: p.NCtx})
					}
				}
			}
			if ss, serr := w.slots(ctx, base+"/slots", "", ""); serr == nil {
				out.Slots = ss
			}
		}
	case "custom":
		err = w.custom(ctx, base, e, &out)
	case "llama-swap":
		err = w.llamaSwap(ctx, base, &out)
		if err == nil {
			// The request log is best effort: an older llama-swap without
			// /api/metrics/activity still reports models and metrics.
			_ = w.activity(ctx, e.Name, base)
		}
	case "ollama":
		err = w.ollama(ctx, base, &out)
	case "strata":
		err = w.strata(ctx, base, e.APIKeyEnv, &out)
	case "vllm":
		err = w.openAIWithProm(ctx, base, e.APIKeyEnv, "vllm:", vllmNames, &out)
	case "sglang":
		err = w.openAIWithProm(ctx, base, e.APIKeyEnv, "sglang:", sglangNames, &out)
	case "localai":
		err = w.openAIWithProm(ctx, base, e.APIKeyEnv, "localai_", localaiNames, &out)
	case "openai":
		err = w.openAIModels(ctx, base, e.APIKeyEnv, &out)
	case "koboldcpp":
		err = w.koboldcpp(ctx, base, e.APIKeyEnv, &out)
	case "tabbyapi":
		err = w.tabbyapi(ctx, base, e.APIKeyEnv, &out)
	case "lmstudio":
		err = w.lmstudio(ctx, base, e.APIKeyEnv, &out)
	case "unsloth":
		err = w.unsloth(ctx, base, e.APIKeyEnv, &out)
	case "freetoken":
		err = w.freetoken(ctx, base, e.APIKeyEnv, &out)
	case "comfyui":
		err = w.comfyui(ctx, base, e.APIKeyEnv, &out)
	case "kokoro":
		err = w.kokoro(ctx, base, e.APIKeyEnv, &out)
	default:
		err = fmt.Errorf("no reader for type %q", e.Type)
	}
	out.Up = err == nil
	if !out.Up {
		switch {
		case isAuthStatus(err):
			// A 401/403 is the engine's own answer: it is alive and
			// refusing the credential. The answer proves liveness; the
			// credential problem stays reported as the engine's error.
			out.Up = true
		case keyErr != nil:
			// The credential never made it onto a request, so no answer
			// arrived through the reader. Ask the engine's primary
			// endpoint with no credential: any HTTP answer, whatever its
			// status, proves the server is alive.
			out.Up = w.answered(ctx, base+livenessPath(e))
		}
	}
	if err != nil {
		out.Error = err.Error()
	}
	if keyErr != nil {
		out.Error = keyErr.Error()
	}
	out.Kind = kindOf(e.Type, e.Kind)
	return out
}

// livenessPath is the endpoint that says whether an engine of this type is
// answering: the one its reader asks first.
func livenessPath(e config.Engine) string {
	switch e.Type {
	case "llamacpp", "ninfer":
		return "/metrics"
	case "custom":
		switch {
		case e.Health != "":
			return e.Health
		case e.Metrics != "" || len(e.Map) > 0:
			if e.Metrics != "" {
				return e.Metrics
			}
			return "/metrics"
		case e.Models != "":
			return e.Models
		case e.Slots != "":
			return e.Slots
		}
		return "/"
	case "llama-swap":
		return "/running"
	case "ollama":
		return "/api/ps"
	case "strata":
		return "/health"
	case "vllm", "sglang", "localai", "openai":
		return "/v1/models"
	case "koboldcpp":
		return "/api/v1/model"
	case "tabbyapi":
		return "/tabby/config"
	case "lmstudio":
		return "/api/v0/models"
	case "unsloth":
		return "/api/health"
	case "freetoken":
		return "/v1/stats"
	case "comfyui":
		return "/queue"
	case "kokoro":
		return "/health"
	}
	return "/"
}

// answered reports whether the endpoint returns an HTTP answer of any
// status. A refusal is still an answer from a live server; only a
// transport failure (nothing listening, timeout) means the server is not
// answering.
func (w *Watcher) answered(ctx context.Context, url string) bool {
	_, err := w.get(ctx, url)
	var status *httpStatusError
	return err == nil || errors.As(err, &status)
}

// custom reads a config-described engine: health for up/down when set,
// then any of metrics (through Map), an OpenAI-style models list and a
// llama.cpp-style slots list. Only the first configured read decides up.
func (w *Watcher) custom(ctx context.Context, base string, e config.Engine, out *proto.Engine) error {
	metricsPath := e.Metrics
	if metricsPath == "" && len(e.Map) > 0 {
		metricsPath = "/metrics"
	}
	switch {
	case e.Health != "":
		if _, err := w.getKey(ctx, base+e.Health, e.APIKeyEnv); err != nil {
			return err
		}
	case metricsPath == "" && e.Models == "" && e.Slots == "":
		if _, err := w.getKey(ctx, base+"/", e.APIKeyEnv); err != nil {
			return err
		}
	}
	gate := e.Health == ""
	var authErr error // a credential refusal on an optional read, reported not fatal
	if metricsPath != "" {
		b, err := w.getKey(ctx, base+metricsPath, e.APIKeyEnv)
		if err != nil && gate {
			return err
		}
		gate = false
		if err != nil {
			if isAuthStatus(err) {
				authErr = err
			}
		} else {
			vals := ParseProm(string(b))
			f := func(field string) float64 { return vals[e.Map[field]] }
			out.Metrics = proto.EngineMetrics{
				PromptTokens: f("prompt_tokens"), OutputTokens: f("output_tokens"),
				PromptSeconds: f("prompt_seconds"), OutputSeconds: f("output_seconds"),
				Processing: f("processing"), Waiting: f("waiting"),
			}
			out.MetricsReported = true
		}
	}
	if e.Models != "" {
		b, err := w.getKey(ctx, base+e.Models, e.APIKeyEnv)
		if err != nil && gate {
			return err
		}
		gate = false
		if err != nil {
			if isAuthStatus(err) {
				authErr = err
			}
		} else {
			var r struct {
				Data []openAIModel `json:"data"`
			}
			if json.Unmarshal(b, &r) == nil {
				for _, m := range r.Data {
					out.Models = append(out.Models, proto.Model{Name: m.ID, State: "loaded", Window: m.window()})
					out.Window = max(out.Window, m.window())
				}
			}
		}
	}
	if e.Slots != "" {
		ss, err := w.slots(ctx, base+e.Slots, "", "")
		if err != nil && gate {
			return err
		}
		if err != nil {
			if isAuthStatus(err) {
				authErr = err
			}
		} else {
			out.Slots = ss
		}
	}
	if authErr != nil {
		out.Error = authErr.Error()
	}
	return nil
}

// httpStatusError preserves the response status without parsing error text.
type httpStatusError struct {
	code int
	msg  string
}

func (e *httpStatusError) Error() string { return e.msg }

func (w *Watcher) get(ctx context.Context, url string) ([]byte, error) {
	return w.getKey(withoutCredential(ctx), url, "")
}

// getKey is get with a bearer token read from the environment variable
// keyEnv, when one is named and set.
func (w *Watcher) getKey(ctx context.Context, url, keyEnv string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c, ok := ctx.Value(credentialKey{}).(credential); ok {
		if c.err != nil {
			return nil, c.err
		}
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
	} else if keyEnv != "" {
		if tok := os.Getenv(keyEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{code: resp.StatusCode, msg: fmt.Sprintf("GET %s: %s", url, resp.Status)}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func (w *Watcher) metrics(ctx context.Context, url string, names map[string]string) (proto.EngineMetrics, error) {
	var m proto.EngineMetrics
	b, err := w.get(ctx, url)
	if err != nil {
		return m, err
	}
	vals := ParseProm(string(b))
	f := func(field string) float64 { return vals[names[field]] }
	m.PromptTokens, m.OutputTokens = f("prompt_tokens"), f("output_tokens")
	m.PromptSeconds, m.OutputSeconds = f("prompt_seconds"), f("output_seconds")
	m.Processing, m.Waiting = f("processing"), f("waiting")
	m.DraftTokens, m.DraftAccepted = vals["ninfer:draft_tokens_total"], vals["ninfer:draft_accepted_tokens_total"]
	return m, nil
}

// llamaSwap lists the running models and sums each upstream's llama.cpp
// metrics, so the engine card shows what is actually loaded behind it.
func (w *Watcher) llamaSwap(ctx context.Context, base string, out *proto.Engine) error {
	b, err := w.get(ctx, base+"/running")
	if err != nil {
		return err
	}
	var r struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
			Proxy string `json:"proxy"`
			Cmd   string `json:"cmd"`
		} `json:"running"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("/running: %w", err)
	}
	for _, m := range r.Running {
		out.Models = append(out.Models, proto.Model{Name: m.Model, State: m.State, File: models.ModelArg(m.Cmd)})
		if m.State != "ready" || m.Proxy == "" {
			continue
		}
		px := strings.TrimRight(m.Proxy, "/")
		mm, err := w.metrics(ctx, px+"/metrics", llamacpp)
		if err == nil {
			// an upstream without /metrics still counts as loaded
			out.Metrics = out.Metrics.Add(mm)
			out.MetricsReported = true
		}
		// Best-effort per model: the proxy's context window and its live KV
		// slots, tagged with the model they sit on. A 404/501 on either
		// leaves the engine up with what it has.
		if b, perr := w.get(ctx, px+"/props"); perr == nil {
			var p struct {
				NCtx int `json:"n_ctx"`
			}
			if json.Unmarshal(b, &p) == nil {
				out.Models[len(out.Models)-1].Window = p.NCtx
			}
		}
		if ss, serr := w.slots(ctx, px+"/slots", m.Model, ""); serr == nil {
			out.Slots = append(out.Slots, ss...)
		}
	}
	for _, md := range out.Models {
		if md.State == "ready" && md.Window > out.Window {
			out.Window = md.Window
		}
	}
	return nil
}

// SlotBusy reports whether one /slots entry is processing a request.
// llama.cpp reports a slot's state in "state"; NInfer builds serve an
// "is_processing" bool instead, so a slot counts as processing on either.
func SlotBusy(state string, isProcessing bool) bool {
	return state == "processing" || isProcessing
}

// slots fetches an engine's /slots endpoint (the llama.cpp family) and maps
// its sessions to the wire shape. llama.cpp reports a slot's state in
// "state"; NInfer builds serve an "is_processing" bool instead, so a slot
// counts as processing on either. Any error - a 404 or 501 from a build
// without the endpoint, a timeout - is the caller's to swallow, so an engine
// without /slots stays up with no slot data.
func (w *Watcher) slots(ctx context.Context, url, model, keyEnv string) ([]proto.Slot, error) {
	b, err := w.getKey(ctx, url, keyEnv)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID            int    `json:"id"`
		State         string `json:"state"`
		IsProcessing  bool   `json:"is_processing"`
		NCtx          int    `json:"n_ctx"`
		NPromptTokens int    `json:"n_prompt_tokens"`
		NCache        int    `json:"n_prompt_tokens_cache"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("/slots: %w", err)
	}
	out := make([]proto.Slot, 0, len(raw))
	for _, s := range raw {
		out = append(out, proto.Slot{
			ID:         "s" + strconv.Itoa(s.ID),
			Model:      model,
			Window:     s.NCtx,
			Used:       s.NPromptTokens,
			Cached:     s.NCache,
			Processing: SlotBusy(s.State, s.IsProcessing),
		})
	}
	return out, nil
}

func (w *Watcher) ollama(ctx context.Context, base string, out *proto.Engine) error {
	b, err := w.get(ctx, base+"/api/ps")
	if err != nil {
		return err
	}
	var r struct {
		Models []struct {
			Name     string `json:"name"`
			SizeVRAM uint64 `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("/api/ps: %w", err)
	}
	for _, m := range r.Models {
		out.Models = append(out.Models, proto.Model{Name: m.Name, State: "loaded", ReportedVRAM: m.SizeVRAM})
	}
	return nil
}

// strata reads /health for the model (open on every install) and /metrics
// for the counters the server totals since it started. Metrics and slots
// use api_key_env when configured and remain best effort: health alone
// decides up/down. Its prompt seconds count only fresh reads while
// prompt tokens include cached prefixes, as in the llama.cpp-based readers,
// so a cache-heavy workload reads a high prefill tok/s.
func (w *Watcher) strata(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.get(ctx, base+"/health")
	if err != nil {
		return err
	}
	var h struct {
		Model      string `json:"model"`
		Loaded     bool   `json:"loaded"`
		MaxContext int    `json:"max_context"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return fmt.Errorf("/health: %w", err)
	}
	state := "unloaded"
	if h.Loaded {
		state = "loaded"
	}
	out.Models = append(out.Models, proto.Model{Name: h.Model, State: state, Window: h.MaxContext})
	out.Window = h.MaxContext
	if slots, err := w.slots(ctx, base+"/slots", h.Model, keyEnv); err == nil {
		out.Slots = slots
	}
	b, err = w.getKey(ctx, base+"/metrics", keyEnv)
	if err != nil {
		return nil
	}
	var m struct {
		Live struct {
			State   string `json:"state"`
			Running *int   `json:"running"`
			Queued  int    `json:"queued"`
		} `json:"live"`
		Totals struct {
			PromptTokens float64 `json:"prompt_tokens"`
			OutputTokens float64 `json:"output_tokens"`
			PromptMS     float64 `json:"prompt_ms"`
			DecodeMS     float64 `json:"decode_ms"`
		} `json:"totals"`
		Requests []struct {
			Time           float64 `json:"time"`
			DurationS      float64 `json:"duration_s"`
			Finish         string  `json:"finish"`
			PromptTokens   float64 `json:"prompt_tokens"`
			Reused         float64 `json:"reused"`
			OutputTokens   float64 `json:"output_tokens"`
			PromptMS       float64 `json:"prompt_ms"`
			DecodeTokS     float64 `json:"decode_tok_s"`
			DraftsOffered  float64 `json:"drafts_offered"`
			DraftsAccepted float64 `json:"drafts_accepted"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	// Strata keeps its last requests in /metrics. Each becomes a row of the
	// request log under the model /health names, so the ledger and the
	// leaderboard tell two Strata quants apart on any port.
	rows := make([]proto.Request, 0, len(m.Requests))
	for _, q := range m.Requests {
		status := http.StatusOK
		switch q.Finish {
		case "cancel":
			status = 499
		case "error":
			status = http.StatusInternalServerError
		}
		r := proto.Request{
			ID: out.Name + "/" + strconv.FormatFloat(q.Time, 'f', 6, 64), Engine: out.Name, Model: h.Model,
			T: time.UnixMicro(int64(math.Round(q.Time * 1e6))), Status: status,
			Prompt: q.PromptTokens, Cache: q.Reused, Output: q.OutputTokens,
			Draft: q.DraftsOffered, DraftAcc: q.DraftsAccepted,
			DecodeTPS: q.DecodeTokS, DurationMS: q.DurationS * 1000, PrefillMS: q.PromptMS,
		}
		if fresh := q.PromptTokens - q.Reused; fresh > 0 && q.PromptMS > 0 {
			r.PrefillTPS = fresh / (q.PromptMS / 1000)
		}
		rows = append(rows, r)
	}
	w.record(rows)
	out.MetricsReported = true
	switch {
	case m.Live.Running != nil:
		out.Metrics.Processing = float64(*m.Live.Running)
	case m.Live.State == "reading" || m.Live.State == "generating":
		out.Metrics.Processing = 1
	}
	out.Metrics.Waiting = float64(m.Live.Queued)
	out.Metrics.PromptTokens = m.Totals.PromptTokens
	out.Metrics.OutputTokens = m.Totals.OutputTokens
	out.Metrics.PromptSeconds = m.Totals.PromptMS / 1000
	out.Metrics.OutputSeconds = m.Totals.DecodeMS / 1000
	return nil
}

// ParseProm reads Prometheus text exposition into name -> value, summing
// series that differ only by labels.
func ParseProm(text string) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		rest := line[len(name):]
		if strings.HasPrefix(rest, "{") {
			j := strings.LastIndexByte(rest, '}')
			if j < 0 {
				continue
			}
			rest = rest[j+1:]
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		out[name] += v
	}
	return out
}
