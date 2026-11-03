// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Peer is another node's gateway, reached over the tailnet with that
// node's key. Its own berthd leases its own cards; this gateway only
// sends it the requests for models no engine here serves.
type Peer struct {
	Name string
	URL  string
	Key  string
}

// hopHeader marks a model list one gateway asks another for. The answer
// is the peer's own engines only, never its peers', so a node forwards a
// request only to a node that serves the model itself, and two nodes that
// name each other cannot pass a request back and forth.
const hopHeader = "X-Berth-Peer"

// peerList is what a peer's /v1/models lists, cached as an engine's list
// is. An unreachable peer lists nothing, and is asked again on the next
// request after the cache runs out.
func (g *Gateway) peerList(ctx context.Context, p Peer) []string {
	key := "peer " + p.URL
	g.mu.Lock()
	if c, ok := g.lists[key]; ok && time.Since(c.at) < 30*time.Second {
		g.mu.Unlock()
		return c.models
	}
	g.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+"/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+p.Key)
	req.Header.Set(hopHeader, "1")
	var out []string
	if resp, err := g.client().Do(req); err != nil {
		g.log().Warn("peer did not answer", "peer", p.Name, "err", err)
	} else {
		var r struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&r) != nil {
			g.log().Warn("peer listed no models", "peer", p.Name, "status", resp.StatusCode)
		}
		resp.Body.Close()
		for _, d := range r.Data {
			out = append(out, d.ID)
		}
	}
	g.mu.Lock()
	if g.lists == nil {
		g.lists = map[string]cached{}
	}
	g.lists[key] = cached{time.Now(), out}
	g.mu.Unlock()
	return out
}

// peerFor is the first peer, in berth.toml's order, that lists model.
func (g *Gateway) peerFor(ctx context.Context, model string) (Peer, bool) {
	for _, p := range g.Peers {
		if slices.Contains(g.peerList(ctx, p), model) {
			return p, true
		}
	}
	return Peer{}, false
}

// toPeer sends the request on to p as it came, under p's key: the
// client's key is this node's and goes no further.
func (g *Gateway) toPeer(w http.ResponseWriter, r *http.Request, p Peer, body []byte) {
	u, err := url.Parse(p.URL)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "peer url: "+err.Error())
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		g.log().Warn("peer request failed", "peer", p.Name, "err", err)
		apiError(w, http.StatusBadGateway, "peer "+p.Name+" did not answer: "+err.Error())
	}
	r.Header.Set("Authorization", "Bearer "+p.Key)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Host = u.Host
	proxy.ServeHTTP(w, r)
}
