// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The switchboard's third action: load one model behind llama-swap. Berth
// does not swap models itself; llama-swap does, and has no load call of its
// own. Any request under /upstream/MODEL/ makes it start that model, so a
// load is a GET of the model's /health there. llama-swap's model command runs
// under berth run, so the card is leased the usual way.

type loadKey struct{ engine, model string }

// apiLoad is POST /v1/berth/load: 202 once the load is under way. The board
// shows the model loading until llama-swap reports it ready.
func (g *Gateway) apiLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine string `json:"engine"`
		Model  string `json:"model"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Engine == "" || req.Model == "" {
		apiError(w, http.StatusBadRequest, `the body must be JSON like {"engine":"NAME","model":"MODEL"}`)
		return
	}
	e := g.engineByName(req.Engine)
	switch {
	case e == nil:
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine %q in berth.toml", req.Engine))
		return
	case e.Type != "llama-swap":
		apiError(w, http.StatusConflict, req.Engine+" is not llama-swap; only llama-swap loads one model on request")
		return
	case !slices.Contains(g.listed(r.Context(), e.URL), req.Model):
		apiError(w, http.StatusNotFound, fmt.Sprintf("%s does not list a model %q", req.Engine, req.Model))
		return
	}
	g.load(e.Name, e.URL, req.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Engine string `json:"engine"`
		Model  string `json:"model"`
	}{e.Name, req.Model})
}

// load asks llama-swap for the model in the background; a second ask while
// one is under way joins it.
func (g *Gateway) load(engine, base, model string) {
	k := loadKey{engine, model}
	g.mu.Lock()
	if g.loads == nil {
		g.loads = map[loadKey]bool{}
	}
	if g.loads[k] {
		g.mu.Unlock()
		return
	}
	g.loads[k] = true
	g.mu.Unlock()
	g.log().Info("loading a model", "engine", engine, "model", model)
	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.loads, k)
			g.mu.Unlock()
		}()
		// The load outlives the request that asked for it, bounded by the
		// gateway's ready.
		ctx, cancel := context.WithTimeout(context.Background(), g.ready())
		defer cancel()
		u := strings.TrimRight(base, "/") + "/upstream/" + escapeSegments(model) + "/health"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			var resp *http.Response
			if resp, err = http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 300 {
					err = fmt.Errorf("llama-swap answered %s", resp.Status)
				}
			}
		}
		if err != nil {
			g.log().Warn("load failed", "engine", engine, "model", model, "err", err)
		}
	}()
}

// escapeSegments escapes a model ID for a URL path but keeps its slashes:
// llama-swap finds the model in /upstream/ by its path segments, and an ID
// such as org/model must reach it as two of them.
func escapeSegments(id string) string {
	parts := strings.Split(id, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
