// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package engines

// Readers for the engines beyond the llama.cpp family: vLLM, SGLang,
// KoboldCpp, TabbyAPI, LocalAI, LM Studio, Unsloth Studio, FreeToken, any
// OpenAI-compatible server, and ComfyUI. Each reads only what the engine
// serves unconditionally to decide up/down; optional endpoints (a /metrics
// that is off by default, a models list behind a key) add detail when they
// answer and are never a liveness signal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/T-Crypt/berth/internal/proto"
)

// openAIModel is one entry of an OpenAI-compatible /v1/models list. Servers
// name the context window differently; window takes whichever is set.
type openAIModel struct {
	ID            string `json:"id"`
	MaxModelLen   int    `json:"max_model_len"`
	ContextLength int    `json:"context_length"`
}

func (m openAIModel) window() int {
	if m.MaxModelLen > 0 {
		return m.MaxModelLen
	}
	return m.ContextLength
}

// openAIModels reads /v1/models into out.Models (all loaded) and the widest
// window into out.Window. It is the liveness read for most of these engines.
func (w *Watcher) openAIModels(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.getKey(ctx, base+"/v1/models", keyEnv)
	if err != nil {
		return err
	}
	var r struct {
		Data []openAIModel `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("/v1/models: %w", err)
	}
	for _, m := range r.Data {
		out.Models = append(out.Models, proto.Model{Name: m.ID, State: "loaded", Window: m.window()})
		out.Window = max(out.Window, m.window())
	}
	return nil
}

// promMetrics reads an optional Prometheus /metrics and maps it onto the
// engine's counters. ok is false when the endpoint is absent or serves none
// of the engine's own names, which leaves the card up without rates.
func (w *Watcher) promMetrics(ctx context.Context, url, keyEnv, prefix string, names map[string]string) (proto.EngineMetrics, bool) {
	b, err := w.getKey(ctx, url, keyEnv)
	if err != nil {
		return proto.EngineMetrics{}, false
	}
	vals := ParseProm(string(b))
	found := false
	for k := range vals {
		if strings.HasPrefix(k, prefix) {
			found = true
			break
		}
	}
	if !found {
		return proto.EngineMetrics{}, false
	}
	f := func(field string) float64 { return vals[names[field]] }
	return proto.EngineMetrics{
		PromptTokens: f("prompt_tokens"), OutputTokens: f("output_tokens"),
		PromptSeconds: f("prompt_seconds"), OutputSeconds: f("output_seconds"),
		Processing: f("processing"), Waiting: f("waiting"),
		InFlightReported: inFlight(vals, names),
	}, true
}

// vLLM and SGLang: /metrics is off by default on SGLang and can be turned
// off on vLLM, so /v1/models alone decides up. Neither exports busy-seconds
// counters, so their tok/s is tokens over wall time between reads.
var vllmNames = map[string]string{
	"prompt_tokens": "vllm:prompt_tokens_total",
	"output_tokens": "vllm:generation_tokens_total",
	"processing":    "vllm:num_requests_running",
	"waiting":       "vllm:num_requests_waiting",
}

var sglangNames = map[string]string{
	"prompt_tokens": "sglang:prompt_tokens_total",
	"output_tokens": "sglang:generation_tokens_total",
	"processing":    "sglang:num_requests_running",
	"waiting":       "sglang:num_requests_waiting",
}

var localaiNames = map[string]string{
	"prompt_tokens": "localai_prompt_tokens_total",
	"output_tokens": "localai_completion_tokens_total",
}

func (w *Watcher) openAIWithProm(ctx context.Context, base, keyEnv, prefix string, names map[string]string, out *proto.Engine) error {
	if err := w.openAIModels(ctx, base, keyEnv, out); err != nil {
		return err
	}
	if m, ok := w.promMetrics(ctx, base+"/metrics", keyEnv, prefix, names); ok {
		out.Metrics = m
		out.MetricsReported = true
	}
	return nil
}

// koboldcpp reads /api/v1/model (its own shape, {"result": name}) and the
// live context budget from /api/v1/config/max_length. It has no /metrics,
// and its /slots answers 501.
func (w *Watcher) koboldcpp(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.getKey(ctx, base+"/api/v1/model", keyEnv)
	if err != nil {
		return err
	}
	var r struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("/api/v1/model: %w", err)
	}
	if b, err := w.getKey(ctx, base+"/api/v1/config/max_length", keyEnv); err == nil {
		var ml struct {
			Value int `json:"value"`
		}
		if json.Unmarshal(b, &ml) == nil {
			out.Window = ml.Value
		}
	}
	if r.Result != "" {
		out.Models = append(out.Models, proto.Model{Name: r.Result, State: "loaded", Window: out.Window})
	}
	return nil
}

// tabbyapi reads /tabby/config (only TabbyAPI serves it) for the context
// length, then the OpenAI model list for what is loaded.
func (w *Watcher) tabbyapi(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.getKey(ctx, base+"/tabby/config", keyEnv)
	if err != nil {
		return err
	}
	var cfg struct {
		ContextLength int `json:"context_length"`
	}
	_ = json.Unmarshal(b, &cfg)
	if w.openAIModels(ctx, base, keyEnv, out) == nil {
		for i := range out.Models {
			if out.Models[i].Window == 0 {
				out.Models[i].Window = cfg.ContextLength
			}
		}
	}
	out.Window = max(out.Window, cfg.ContextLength)
	return nil
}

// lmstudio reads LM Studio's /api/v0/models, which lists every model with
// its state; only the loaded ones count toward the window.
func (w *Watcher) lmstudio(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.getKey(ctx, base+"/api/v0/models", keyEnv)
	if err != nil {
		return err
	}
	var r struct {
		Data []struct {
			ID            string `json:"id"`
			State         string `json:"state"`
			ContextLength int    `json:"max_context_length"`
			LoadedContext int    `json:"loaded_context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("/api/v0/models: %w", err)
	}
	for _, m := range r.Data {
		if m.State != "loaded" {
			continue
		}
		win := m.LoadedContext
		if win == 0 {
			win = m.ContextLength
		}
		out.Models = append(out.Models, proto.Model{Name: m.ID, State: "loaded", Window: win})
		out.Window = max(out.Window, win)
	}
	return nil
}

// unsloth reads Unsloth Studio's /api/health (no key) for up/down; its
// /v1/models needs a Studio key (api_key_env, UNSLOTH_STUDIO_AUTH_TOKEN by
// default), and without one the card is up and says a key is needed.
func (w *Watcher) unsloth(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.get(ctx, base+"/api/health")
	if err != nil {
		return err
	}
	var h struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return fmt.Errorf("/api/health: %w", err)
	}
	if h.Status != "healthy" {
		return fmt.Errorf("/api/health: status %q", h.Status)
	}
	if keyEnv == "" {
		keyEnv = "UNSLOTH_STUDIO_AUTH_TOKEN"
	}
	if err := w.openAIModels(ctx, base, keyEnv, out); isAuthStatus(err) {
		out.Error = "API key needed for the model list (" + keyEnv + ")"
	}
	return nil
}

func isAuthStatus(err error) bool {
	var status *httpStatusError
	return errors.As(err, &status) && (status.code == http.StatusUnauthorized || status.code == http.StatusForbidden)
}

// freetoken reads FreeToken's JSON /v1/stats (it serves no Prometheus).
// The body has grown over releases, so each figure is looked up under the
// names it has carried and left out when none is present.
func (w *Watcher) freetoken(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	b, err := w.getKey(ctx, base+"/v1/stats", keyEnv)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		return fmt.Errorf("/v1/stats: %w", err)
	}
	out.MetricsReported = true
	if inner, ok := body["stats"].(map[string]any); ok {
		body = inner
	}
	pick := func(keys ...string) float64 {
		for _, k := range keys {
			if v, ok := body[k].(float64); ok {
				return v
			}
		}
		return 0
	}
	out.Metrics.OutputTokens = pick("tokens_generated", "generated_tokens", "output_tokens", "tokens_predicted")
	out.Metrics.PromptTokens = pick("tokens_prompt", "prompt_tokens", "input_tokens")
	out.Metrics.Waiting = pick("queue", "requests_waiting", "pending_requests")
	for _, k := range []string{"generating", "busy", "active"} {
		if v, ok := body[k].(bool); ok {
			out.Metrics.InFlightReported = true
			if v {
				out.Metrics.Processing = 1
			}
		}
	}
	return nil
}

// comfyui reads ComfyUI's /queue (jobs running and pending) and
// /system_stats (the version). It has no token counters; the queue is what
// decides a safe handoff point (berth run --drain-comfyui).
func (w *Watcher) comfyui(ctx context.Context, base, keyEnv string, out *proto.Engine) error {
	running, pending, err := w.comfyQueue(ctx, base, keyEnv)
	if err != nil {
		return err
	}
	out.Metrics.Processing, out.Metrics.Waiting = float64(running), float64(pending)
	out.Metrics.InFlightReported = true
	if b, err := w.getKey(ctx, base+"/system_stats", keyEnv); err == nil {
		var s struct {
			System struct {
				Version string `json:"comfyui_version"`
			} `json:"system"`
		}
		if json.Unmarshal(b, &s) == nil && s.System.Version != "" {
			out.Models = append(out.Models, proto.Model{Name: "ComfyUI " + s.System.Version, State: "loaded"})
		}
	}
	return nil
}

func (w *Watcher) comfyQueue(ctx context.Context, base, keyEnv string) (running, pending int, err error) {
	b, err := w.getKey(ctx, base+"/queue", keyEnv)
	if err != nil {
		return 0, 0, err
	}
	var q struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := json.Unmarshal(b, &q); err != nil {
		return 0, 0, fmt.Errorf("/queue: %w", err)
	}
	return len(q.Running), len(q.Pending), nil
}
