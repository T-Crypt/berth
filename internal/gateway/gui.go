// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

// The gui plane. A browser cannot use the gateway API (it needs the key and
// refuses any Origin), so a second loopback listener serves the plane: GET
// /gui/ lists every configured engine with its live state, one button per
// engine. GET /gui/<name> for an engine with a gui URL in berth.toml sends
// the browser to the engine's web UI when it answers, or shows a page that
// names who holds the card and offers a Launch button: a start runs under a
// lease, like any gateway start, and the page follows the engine there once
// it answers. A bookmark or a home-page tile can point there instead of at an
// engine that may be stopped.
//
// It holds no key: it can only start an engine that has a start command in
// berth.toml, which is what the operator asked for by writing one. The index
// launches any such engine, with a web UI or without one (Strata, a
// llama-server), so a phone can switch the card. A start is a POST that must
// come from the page itself (same origin), so another site cannot make a
// visitor's browser drain the card.

//go:embed gui
var guiFS embed.FS

var guiPage = template.Must(template.ParseFS(guiFS, "gui/page.html"))

// wake is the last start the wake page asked for, per engine.
type wake struct {
	starting bool
	err      error
}

// GUIHandler serves the gui plane: the index, the per-engine wake pages and
// their assets.
func (g *Gateway) GUIHandler() http.Handler {
	assets, _ := fs.Sub(guiFS, "gui")
	mux := http.NewServeMux()
	mux.Handle("GET /_wake/", http.StripPrefix("/_wake/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /gui/{$}", g.guiIndex)
	// The static route beats GET /gui/{name} in Go's ServeMux, so an engine
	// named "state" loses that one URL to the plane's state endpoint.
	mux.HandleFunc("GET /gui/state", func(w http.ResponseWriter, r *http.Request) {
		rows := g.guiRows(r.Context(), g.nodeStatus())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("GET /gui/{name}", g.guiShow)
	mux.HandleFunc("GET /gui/{name}/state", g.guiState)
	mux.HandleFunc("POST /gui/{name}/wake", g.guiWake)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/gui/", http.StatusFound)
	})
	return g.guiGuard(mux)
}

// guiGuard keeps the page on loopback (or a name the operator allowed for a
// same-machine proxy), loads nothing from elsewhere, and admits a POST only
// from this origin.
func (g *Gateway) guiGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		switch {
		case !loopbackHost(r.Host) && !g.allowedHost(r.Host):
			apiError(w, http.StatusForbidden, "open the wake page at 127.0.0.1 or localhost")
		case r.Method == http.MethodPost && !sameOrigin(r):
			apiError(w, http.StatusForbidden, "a start must come from the wake page itself")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// sameOrigin is true when a browser says the request came from this origin.
// A client that sends neither header is not a browser and cannot be tricked
// into a cross-site request.
func sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin"
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// guiEngine is the configured engine name, if it has a web UI to wake.
func (g *Gateway) guiEngine(name string) (*config.Engine, bool) {
	for i := range g.Engines {
		e := &g.Engines[i]
		if e.Name == name && e.GUI != "" && len(e.Start) > 0 {
			return e, true
		}
	}
	return nil, false
}

// launchable is the configured engine name, if Berth can start it.
func (g *Gateway) launchable(name string) (*config.Engine, bool) {
	for i := range g.Engines {
		e := &g.Engines[i]
		if e.Name == name && len(e.Start) > 0 {
			return e, true
		}
	}
	return nil, false
}

func (g *Gateway) probe(ctx context.Context, e *config.Engine) bool {
	if e.Type == "comfyui" {
		return g.comfyAnswers(ctx, e.URL)
	}
	return g.answers(ctx, e.URL)
}

// guiRow is one engine's row on the GUI plane: what it is (web UI or API
// only) and what it is doing on the card right now.
type guiRow struct {
	Name    string    `json:"name"`
	GUI     string    `json:"gui,omitempty"` // the engine's web UI; empty = No UI
	API     string    `json:"api,omitempty"` // the engine's own address, shown when there is no web UI
	Launch  bool      `json:"launch"`        // true when the engine has a start command
	State   string    `json:"state"`         // up, offline, waiting, starting, failed
	Message string    `json:"message,omitempty"`
	Detail  string    `json:"detail,omitempty"` // the start's last log line, or its error
	Code    string    `json:"code,omitempty"`
	Card    []guiHold `json:"card,omitempty"`
	// Cards are the cards the engine's entry runs on, by name; empty while
	// berthd does not answer.
	Cards []string `json:"cards,omitempty"`
}

// guiRowFor is the static part of an engine's row: what it is. The state
// fields are filled in once, per request.
func guiRowFor(e *config.Engine) guiRow {
	row := guiRow{Name: e.Name, GUI: e.GUI, Launch: len(e.Start) > 0}
	if e.GUI == "" {
		row.API = e.URL
	}
	return row
}

type guiView struct {
	Title   string
	Engine  string // empty on the index
	Engines []guiRow
}

func (g *Gateway) guiIndex(w http.ResponseWriter, _ *http.Request) {
	v := guiView{Title: "berth", Engines: make([]guiRow, 0, len(g.Engines))}
	for i := range g.Engines {
		v.Engines = append(v.Engines, guiRowFor(&g.Engines[i]))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = guiPage.Execute(w, v)
}

// guiRows is the plane's state: one row per engine in config order.
// Up rows come from the probe; everything else reuses cardView, exactly like
// the per-engine state handler.
func (g *Gateway) guiRows(ctx context.Context, status *proto.Status) []guiRow {
	rows := make([]guiRow, 0, len(g.Engines))
	for i := range g.Engines {
		e := &g.Engines[i]
		row := guiRowFor(e)
		if status != nil {
			row.Cards = node.CardsOf(status, e.GPU)
		}
		if g.probe(ctx, e) {
			row.State = "up"
			row.Message = e.Name + " is ready"
		} else {
			g.mu.Lock()
			wk := g.wakes[e.Name]
			var last string
			if s, ok := g.running[e.Name]; ok {
				last = lastLine(s.stderr.String())
			}
			g.mu.Unlock()
			st := cardView(e.Name, node.View(status, e.GPU), wk)
			if st.State != "failed" {
				st.Detail = last
			}
			row.State = st.State
			row.Message = st.Message
			row.Detail = st.Detail
			row.Code = st.Code
			row.Card = st.Card
		}
		rows = append(rows, row)
	}
	return rows
}

// guiShow sends the browser on when the engine answers; otherwise it shows
// the page, which says who holds the card and offers a start.
func (g *Gateway) guiShow(w http.ResponseWriter, r *http.Request) {
	e, ok := g.guiEngine(r.PathValue("name"))
	if !ok {
		apiError(w, http.StatusNotFound, "no engine of that name has a gui and a start command in berth.toml")
		return
	}
	if g.probe(r.Context(), e) {
		http.Redirect(w, r, e.GUI, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = guiPage.Execute(w, guiView{Title: e.Name + " - offline", Engine: e.Name})
}

type guiStatus struct {
	// State is up, offline (no start asked), waiting (a start waits for the
	// card), starting (the card is ours, the engine is loading) or failed.
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"` // one line for people
	Detail  string    `json:"detail,omitempty"`  // the start's last log line, or its error
	Code    string    `json:"code,omitempty"`    // a failed start's reason code
	URL     string    `json:"url,omitempty"`     // where the browser goes once up
	Card    []guiHold `json:"card,omitempty"`    // who holds the card besides this engine
}

// guiHold is one lease on the card, as the page names it.
type guiHold struct {
	Name   string `json:"name"`
	State  string `json:"state"` // active, draining, releasing, stuck
	Pinned bool   `json:"pinned,omitempty"`
}

func (g *Gateway) guiState(w http.ResponseWriter, r *http.Request) {
	e, ok := g.guiEngine(r.PathValue("name"))
	if !ok {
		apiError(w, http.StatusNotFound, "no engine of that name has a gui and a start command in berth.toml")
		return
	}
	var st guiStatus
	if g.probe(r.Context(), e) {
		st = guiStatus{State: "up", URL: e.GUI, Message: e.Name + " is ready"}
	} else {
		g.mu.Lock()
		wk := g.wakes[e.Name]
		var last string
		if s, ok := g.running[e.Name]; ok {
			last = lastLine(s.stderr.String())
		}
		g.mu.Unlock()
		st = cardView(e.Name, node.View(g.nodeStatus(), e.GPU), wk)
		if st.State != "failed" {
			st.Detail = last
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(st)
}

// cardView is the page's state for a stopped engine: whether a start is under
// way, who holds the card, and what the start waits for. Berth never runs two
// engines on the card at once, and a drain waits for the holder's request (or
// ComfyUI job) to finish, so the page names that wait instead of hiding it.
func cardView(engine string, node *proto.Status, wk *wake) guiStatus {
	st := guiStatus{State: "offline"}
	ours := false
	if node != nil {
		for _, l := range node.Leases {
			if l.Engine == engine {
				ours = true
				continue
			}
			st.Card = append(st.Card, guiHold{Name: holderName(l), State: l.State, Pinned: l.Pinned})
		}
	}
	switch {
	case wk != nil && wk.starting && ours:
		st.State = "starting"
		st.Message = "The card is " + engine + "'s; loading " + engine
	case wk != nil && wk.starting:
		st.State = "waiting"
		st.Message = waitLine(st.Card)
	case wk != nil && wk.err != nil:
		st.State = "failed"
		st.Message = engine + " did not start"
		st.Detail = wk.err.Error()
		if se, ok := wk.err.(*startError); ok {
			st.Code = se.code
		}
	case node == nil:
		st.Message = engine + " is offline; berthd is not answering, so a start would fail"
	case len(st.Card) == 0:
		st.Message = engine + " is offline and the card is free"
	default:
		st.Message = engine + " is offline; the card is in use by " + names(st.Card) +
			". Start asks it to release the card once its current request finishes"
		for _, h := range st.Card {
			if h.Pinned {
				st.Message = engine + " is offline; " + h.Name + " holds the card and Berth never stops it. Start waits until it ends"
			}
		}
	}
	return st
}

// waitLine says what a start is waiting on.
func waitLine(card []guiHold) string {
	if len(card) == 0 {
		return "Waiting for the card"
	}
	h := card[0]
	switch {
	case h.Pinned:
		return "Waiting for " + h.Name + " to end (Berth never stops it)"
	case h.State == "draining":
		return "Waiting for " + h.Name + " release: finishing its current request"
	case h.State == "releasing":
		return "Waiting for " + h.Name + " release: freeing its memory"
	case h.State == "stuck":
		return "Waiting for " + h.Name + " release: it exited but its memory is still held"
	}
	return "Waiting for " + h.Name + " release"
}

// holderName names a lease for people: llama-swap's models by their family.
func holderName(l proto.Lease) string {
	if l.Group != "" && l.Group != l.Engine {
		return l.Group + " (" + l.Engine + ")"
	}
	return l.Engine
}

func names(card []guiHold) string {
	out := make([]string, len(card))
	for i, h := range card {
		out[i] = h.Name
	}
	return strings.Join(out, " and ")
}

// guiWake is the page's Launch: a switch to the engine. Only the button
// asks; opening the page never takes the card.
func (g *Gateway) guiWake(w http.ResponseWriter, r *http.Request) {
	e, ok := g.launchable(r.PathValue("name"))
	if !ok {
		apiError(w, http.StatusNotFound, "no engine of that name has a start command in berth.toml")
		return
	}
	g.switchTo(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(guiStatus{State: "waiting", Message: "Asking berth for the card"})
}
