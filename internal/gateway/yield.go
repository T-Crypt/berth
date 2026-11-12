// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// A yield lasts this long when the caller names no length, and never longer
// than the cap: a node that dies mid-yield must not leave its engine's next
// drains skipping the job boundary for long.
const (
	defaultYield = 5 * time.Minute
	maxYield     = time.Hour
)

// apiYield is POST /v1/berth/yield, for a ComfyUI node that calls an LLM the
// card's lease would otherwise keep waiting (#299). The node first frees its
// models in-process. {"engine":"comfyui","seconds":120} says the running job
// is at a safe point, so a drain for the LLM parks ComfyUI as it would between
// jobs. {"engine":"comfyui","end":true} takes the card back: the answer comes
// once ComfyUI holds it again, so the job's next node can load its models.
func (g *Gateway) apiYield(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine  string  `json:"engine"`
		Seconds float64 `json:"seconds"`
		End     bool    `json:"end"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &req) != nil || req.Engine == "" {
		apiError(w, http.StatusBadRequest, `the body must be JSON like {"engine":"NAME","seconds":120} or {"engine":"NAME","end":true}`)
		return
	}
	if req.Seconds < 0 || req.Seconds > maxYield.Seconds() {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("seconds must be between 0 and %d", int(maxYield.Seconds())))
		return
	}
	e := g.engineByName(req.Engine)
	if e == nil {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine %q in berth.toml", req.Engine))
		return
	}
	d := req.Seconds
	switch {
	case req.End:
		d = 0
	case d == 0:
		d = defaultYield.Seconds()
	}
	conn, raw, err := server.Dial(g.Socket)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; no yield was set")
		return
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpYield, Engine: req.Engine, Yield: d}); err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd: "+err.Error())
		return
	}
	m, err := conn.Recv()
	switch {
	case err != nil:
		apiError(w, http.StatusServiceUnavailable, "no yield reply from berthd: "+err.Error())
		return
	case m.Op == proto.OpRefused:
		apiError(w, http.StatusConflict, m.Reason)
		return
	}
	if req.End {
		if err := g.ensure(r.Context(), e, false); err != nil {
			g.startFailed(w, e.Name, e.Name, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Engine  string  `json:"engine"`
		Yield   bool    `json:"yield"`
		Seconds float64 `json:"seconds"`
	}{req.Engine, !req.End, d})
}
