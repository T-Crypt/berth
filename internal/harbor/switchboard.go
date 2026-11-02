// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/proto"
)

// The SWITCH tab's server side. The board and its actions belong to the
// gateway, which owns engine starts and knows a start in flight; the harbor
// only forwards them with the node's key. Anyone who can open the harbor may
// read the board. An action needs a paired device, asked from the harbor's
// own page.

const gatewayTimeout = 5 * time.Second

// writeOK is whether a write may go ahead, and when not, the reply says why.
// Cross-site first, so another site learns nothing about pairing.
func (h *Harbor) writeOK(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case crossSite(r):
		apiError(w, http.StatusForbidden, "cross_site", "the harbor takes actions only from its own page")
	case r.Header.Get("X-Berth") != "1":
		apiError(w, http.StatusForbidden, "no_x_berth", "an action needs the X-Berth: 1 header")
	case !h.paired(r):
		apiError(w, http.StatusUnauthorized, "not_paired", "this device is not paired; run berth pair on the machine")
	default:
		return true
	}
	return false
}

// boardRead is GET /v1/board: the gateway's board for this node.
func (h *Harbor) boardRead(w http.ResponseWriter, r *http.Request) {
	h.toGateway(w, r, http.MethodGet, "/v1/berth/board", nil)
}

// boardAction is POST /v1/board/switch, /v1/board/stop and /v1/board/load.
// The body is the gateway's own, {"engine":"NAME"}, plus "model" for a load,
// checked here so a bad one never leaves the harbor.
func (h *Harbor) boardAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.writeOK(w, r) {
			return
		}
		var req struct {
			Engine string `json:"engine"`
			Model  string `json:"model,omitempty"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		load := action == "load"
		if err != nil || json.Unmarshal(body, &req) != nil || req.Engine == "" || load && req.Model == "" {
			want := `{"engine":"NAME"}`
			if load {
				want = `{"engine":"NAME","model":"MODEL"}`
			}
			apiError(w, http.StatusBadRequest, "bad_body", "the body must be JSON like "+want)
			return
		}
		if !load {
			req.Model = ""
		}
		out, _ := json.Marshal(req)
		h.toGateway(w, r, http.MethodPost, "/v1/berth/"+action, out)
	}
}

// toGateway sends one request to the gateway with the node's key and hands
// its status and body back to the page.
func (h *Harbor) toGateway(w http.ResponseWriter, r *http.Request, method, path string, body []byte) {
	if h.GatewayKey == "" {
		apiError(w, http.StatusServiceUnavailable, "no_gateway_key", "the harbor found no node API key; start berth gateway once to create it")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gatewayTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.Gateway, "/")+path, rd)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "bad_gateway_url", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.GatewayKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		apiError(w, http.StatusBadGateway, "gateway_down", "the gateway is not answering at "+h.Gateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20))
}

// pairCode is POST /v1/pair/code: a paired device mints a code for another
// one and gets the link, and the link as a QR code, to show it.
func (h *Harbor) pairCode(w http.ResponseWriter, r *http.Request) {
	if !h.writeOK(w, r) {
		return
	}
	m, err := h.ask(proto.Msg{Op: proto.OpPairCode})
	if err != nil {
		apiError(w, http.StatusBadGateway, "node_down", "berthd is not answering: "+err.Error())
		return
	}
	if m.Op == proto.OpRefused {
		apiError(w, http.StatusConflict, "refused", m.Reason)
		return
	}
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	link := scheme + "://" + r.Host + "/pair?code=" + m.Code
	svg, err := pair.QRSVG(link)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "qr", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Link string `json:"link"`
		QR   string `json:"qr"` // a data: URL the page shows as an image
	}{link, "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))})
}
