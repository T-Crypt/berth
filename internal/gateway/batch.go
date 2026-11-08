// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// apiBatch is POST /v1/berth/batch, `berth batch` on the keyed API:
// {"engine":"comfyui","grace":30,"max":14400} starts or renews the batch and,
// for an engine with a start command, switches the card to it the way
// /v1/berth/switch does, so an agent sends one call before its jobs.
// {"engine":"comfyui","end":true} ends it. Grace and max are seconds; 0 or
// absent takes berthd's defaults.
func (g *Gateway) apiBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine string  `json:"engine"`
		Grace  float64 `json:"grace"`
		Max    float64 `json:"max"`
		End    bool    `json:"end"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Engine == "" {
		apiError(w, http.StatusBadRequest, `the body must be JSON like {"engine":"NAME","grace":30}`)
		return
	}
	if req.Grace < 0 || req.Max < 0 {
		apiError(w, http.StatusBadRequest, "grace and max must not be negative")
		return
	}
	if g.engineByName(req.Engine) == nil {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine %q in berth.toml", req.Engine))
		return
	}
	conn, raw, err := server.Dial(g.Socket)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; no batch was set")
		return
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpBatch, Engine: req.Engine, Grace: req.Grace, Max: req.Max, End: req.End}); err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd: "+err.Error())
		return
	}
	m, err := conn.Recv()
	switch {
	case err != nil:
		apiError(w, http.StatusServiceUnavailable, "no batch reply from berthd: "+err.Error())
		return
	case m.Op == proto.OpRefused:
		apiError(w, http.StatusConflict, m.Reason)
		return
	}
	if !req.End {
		if e, ok := g.launchable(req.Engine); ok {
			g.switchTo(e)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		Engine string `json:"engine"`
		Batch  bool   `json:"batch"`
	}{req.Engine, !req.End})
}
