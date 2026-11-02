// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package engines

// Discovery finds engines nobody configured: every minute it probes the
// well-known ports on this machine (and on any extra hosts) and asks each
// answer who it is, by response, never by port alone. A port already read
// by a configured engine is never probed. The table and fingerprints are
// Speculum's, plus llama-swap, Strata and ComfyUI.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// DiscoveryEvery is how often discovery runs; DiscoveredTTL is how long a
// discovered engine may stay down before its card is dropped.
const (
	DiscoveryEvery = time.Minute
	DiscoveredTTL  = 5 * time.Minute
)

// discoveryPorts lists, per port, the readers to try there in order. Where
// several engines share a default port the order disambiguates: the most
// specific fingerprint first, the generic OpenAI list last.
var discoveryPorts = []struct {
	port  int
	types []string
}{
	{11434, []string{"ollama"}},
	{8080, []string{"llama-swap", "strata", "llamacpp", "localai", "openai"}},
	{8000, []string{"vllm", "openai"}},
	{8188, []string{"comfyui"}},
	{1234, []string{"lmstudio"}},
	{8888, []string{"unsloth"}},
	{1919, []string{"freetoken"}},
	{30000, []string{"sglang"}},
	{5000, []string{"tabbyapi"}},
	{5001, []string{"koboldcpp"}},
	{8880, []string{"kokoro"}},
}

// probeOrder is the order every reader is tried on an extra port.
var probeOrder = []string{"llama-swap", "strata", "comfyui", "kokoro", "ollama", "koboldcpp", "llamacpp", "vllm", "sglang",
	"tabbyapi", "localai", "lmstudio", "unsloth", "freetoken", "openai"}

// Labels are the display names of discovered engines.
var Labels = map[string]string{
	"llama-swap": "llama-swap", "strata": "Strata", "llamacpp": "llama.cpp", "ollama": "Ollama",
	"vllm": "vLLM", "sglang": "SGLang", "koboldcpp": "KoboldCpp", "tabbyapi": "TabbyAPI",
	"localai": "LocalAI", "lmstudio": "LM Studio", "unsloth": "Unsloth", "freetoken": "FreeToken",
	"openai": "OpenAI-compatible", "comfyui": "ComfyUI", "kokoro": "Kokoro",
}

type target struct {
	host  string
	port  int
	types []string
}

func targets(hosts []string, ports []int) []target {
	all := []string{"127.0.0.1"}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h != "" && h != "127.0.0.1" && h != "localhost" && h != "::1" {
			all = append(all, h)
		}
	}
	var out []target
	for _, h := range all {
		known := map[int]bool{}
		for _, d := range discoveryPorts {
			out = append(out, target{h, d.port, d.types})
			known[d.port] = true
		}
		for _, p := range ports {
			if !known[p] {
				out = append(out, target{h, p, probeOrder})
				known[p] = true
			}
		}
	}
	return out
}

func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443"}[u.Scheme]
		if port == "" {
			port = "80"
		}
	}
	host := u.Hostname()
	if host == "localhost" || host == "::1" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// discover runs one pass and adds what it finds. Called without w.mu.
func (w *Watcher) discover(ctx context.Context) {
	w.mu.Lock()
	claimed := map[string]bool{}
	for _, e := range w.all() {
		claimed[hostPort(e.URL)] = true
	}
	names := map[string]bool{}
	for _, e := range w.all() {
		names[e.Name] = true
	}
	w.mu.Unlock()

	c := &http.Client{Timeout: 500 * time.Millisecond}
	for _, t := range targets(w.Hosts, w.Ports) {
		base := "http://" + net.JoinHostPort(t.host, strconv.Itoa(t.port))
		if claimed[hostPort(base)] || ctx.Err() != nil {
			continue
		}
		for _, typ := range t.types {
			if !fingerprint(ctx, c, base, typ) {
				continue
			}
			name := Labels[typ]
			if t.host != "127.0.0.1" {
				name += " @" + t.host
			}
			if names[name] {
				name += fmt.Sprintf(" :%d", t.port)
			}
			names[name] = true
			w.mu.Lock()
			w.found = append(w.found, config.Engine{Name: name, Type: typ, URL: base})
			w.mu.Unlock()
			break
		}
	}
}

// prune drops discovered engines that have stayed down past DiscoveredTTL.
// Called with w.mu held.
func (w *Watcher) prune(now time.Time) {
	kept := w.found[:0]
	for _, e := range w.found {
		s, ok := w.state[e.Name]
		if ok && !s.Up && now.Sub(w.lastUp[e.Name]) > DiscoveredTTL {
			delete(w.state, e.Name)
			delete(w.latched, e.Name)
			delete(w.stopped, e.Name)
			delete(w.lastUp, e.Name)
			continue
		}
		kept = append(kept, e)
	}
	w.found = kept
}

func probeJSON(ctx context.Context, c *http.Client, url string, v any) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if v == nil {
		return resp.StatusCode, true
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, false
	}
	return resp.StatusCode, json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(v) == nil
}

// fingerprint is true when the server at base is the named engine. Each
// check reads something only that engine serves; the generic OpenAI check
// is tried last on any port.
func fingerprint(ctx context.Context, c *http.Client, base, typ string) bool {
	var m map[string]any
	get := func(path string) bool { m = nil; _, ok := probeJSON(ctx, c, base+path, &m); return ok && m != nil }
	isList := func(k string) bool { _, ok := m[k].([]any); return ok }
	switch typ {
	case "llama-swap":
		return get("/running") && isList("running")
	case "strata":
		return get("/health") && m["service"] == "strata"
	case "comfyui":
		if !get("/system_stats") {
			return false
		}
		_, sys := m["system"].(map[string]any)
		return sys && isList("devices")
	case "kokoro":
		// {"status":"healthy"} is common; a voice list is Kokoro-FastAPI's own.
		return get("/health") && m["status"] == "healthy" && get("/v1/audio/voices") && isList("voices")
	case "ollama":
		return get("/api/version") && m["version"] != nil
	case "llamacpp":
		if !get("/props") {
			return false
		}
		// Strata and KoboldCpp serve a llama.cpp-shaped /props too.
		if bi, _ := m["build_info"].(string); strings.HasPrefix(bi, "Strata") {
			return false
		}
		if _, kobold := m["total_slots"]; kobold {
			return false
		}
		_, ok := m["default_generation_settings"]
		return ok
	case "localai":
		code, ok := probeJSON(ctx, c, base+"/healthz", nil)
		return ok && code == http.StatusNoContent
	case "vllm":
		if !get("/version") {
			return false
		}
		if _, ok := m["version"].(string); !ok {
			return false
		}
		return get("/v1/models") && isList("data")
	case "sglang":
		return get("/get_model_info") && get("/v1/models") && isList("data")
	case "koboldcpp":
		if !get("/api/v1/model") {
			return false
		}
		_, ok := m["result"].(string)
		return ok
	case "tabbyapi":
		code, ok := probeJSON(ctx, c, base+"/tabby/config", nil)
		return ok && code == http.StatusOK
	case "lmstudio":
		if !get("/api/v0/models") {
			return false
		}
		data, ok := m["data"].([]any)
		if !ok || len(data) == 0 {
			return false
		}
		for _, d := range data {
			dm, ok := d.(map[string]any)
			if !ok {
				return false
			}
			if _, ok := dm["state"]; !ok {
				return false
			}
		}
		return true
	case "unsloth":
		if !get("/api/health") {
			return false
		}
		s, _ := m["service"].(string)
		return strings.Contains(strings.ToLower(s), "unsloth")
	case "freetoken":
		return get("/health") && get("/v1/stats")
	case "openai":
		return get("/v1/models") && isList("data")
	}
	return false
}
