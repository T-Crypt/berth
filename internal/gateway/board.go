// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"strings"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

// The switchboard's read side: one node as GET /v1/berth/board serves it.
// The gateway is the only process that knows a start in flight, so a node's
// board is built here; the harbor reads one per node. Cards is a list and
// every action names its engine, so more cards (v0.5) and more nodes (v0.6)
// add rows, not a new shape.

type boardNode struct {
	Name      string        `json:"name"`
	Reachable bool          `json:"reachable"` // berthd answered; false = cards and holders unknown
	Cards     []boardCard   `json:"cards"`
	Engines   []boardEngine `json:"engines"`
}

type boardCard struct {
	Card  string `json:"card,omitempty"` // its name on the node (gpu0)
	Index int    `json:"index"`
	Name  string `json:"name"`
	// State is free, held, draining (a holder is giving it back),
	// passthrough (in a VM) or probe-failed.
	State string `json:"state"`
	// Cordoned: berth cordon closed the card to new work but holds.
	Cordoned bool `json:"cordoned,omitempty"`
	// Base is the card's own gateway base URL, a path on this gateway.
	Base    string    `json:"base,omitempty"`
	Total   uint64    `json:"total"`
	Used    uint64    `json:"used"`
	Holders []guiHold `json:"holders"`
}

type boardEngine struct {
	guiRow
	Stoppable bool         `json:"stoppable"` // holds the card under an active lease that is not pinned
	Models    []boardModel `json:"models"`
}

type boardModel struct {
	ID     string `json:"id"`
	Loaded bool   `json:"loaded,omitempty"` // the engine's reader reports it loaded
	// Loading: a switchboard load is under way, or the engine reports the
	// model starting.
	Loading bool `json:"loading,omitempty"`
}

func (g *Gateway) board(w http.ResponseWriter, r *http.Request) {
	status := g.nodeStatus()
	rows := g.guiRows(r.Context(), status)
	listed := map[string][]string{}
	live := map[string]map[string]string{}
	for i := range g.Engines {
		e := &g.Engines[i]
		if rows[i].State == "up" {
			listed[e.Name] = g.listed(r.Context(), e.URL)
			if e.Type == "llama-swap" {
				if states := g.swapRunning(r.Context(), e.URL); states != nil {
					live[e.Name] = states
				}
			}
		}
		if len(listed[e.Name]) == 0 {
			listed[e.Name] = e.Serves
		}
	}
	host, _ := os.Hostname()
	g.mu.Lock()
	inFlight := maps.Clone(g.loads)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(boardOf(host, status, rows, listed, live, inFlight))
}

// swapRunning is llama-swap's own /running, model -> state, read at once:
// berthd's reader reads it every few seconds, and a load the board shows
// must not read cold again in the gap between llama-swap's answer and that
// read. nil when llama-swap does not answer.
func (g *Gateway) swapRunning(ctx context.Context, base string) map[string]string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/running", nil)
	if err != nil {
		return nil
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil
	}
	states := map[string]string{}
	for _, m := range r.Running {
		states[m.Model] = m.State
	}
	return states
}

// boardOf assembles the board from berthd's status (nil = not answering),
// the engines' rows in config order, each engine's model list, the model
// states llama-swap reported just now (over berthd's older read) and the
// switchboard loads under way.
func boardOf(host string, status *proto.Status, rows []guiRow, listed map[string][]string, live map[string]map[string]string, inFlight map[loadKey]bool) boardNode {
	n := boardNode{Name: host, Reachable: status != nil, Cards: []boardCard{}, Engines: make([]boardEngine, 0, len(rows))}
	loaded := map[string]map[string]bool{}
	starting := map[string]map[string]bool{}
	if status != nil {
		for _, gp := range status.GPUs {
			if !gp.Scheduled {
				continue
			}
			c := boardCard{Card: gp.Card, Index: gp.Index, Name: gp.Name, State: "free", Cordoned: gp.Cordoned,
				Total: gp.Total, Used: gp.Used, Holders: []guiHold{}}
			if gp.Card != "" { // a berthd older than card names has no per-card URL
				c.Base = "/cards/" + gp.Card + "/v1"
			}
			for _, l := range node.View(status, gp.Card).Leases {
				c.Holders = append(c.Holders, guiHold{Name: holderName(l), State: l.State, Pinned: l.Pinned})
				if c.State == "free" {
					c.State = "held"
				}
				if l.State != "active" {
					c.State = "draining"
				}
			}
			switch {
			case gp.Passthrough != nil || status.Passthrough != nil:
				c.State = "passthrough"
			case status.ProbeErr != "":
				c.State = "probe-failed"
			}
			n.Cards = append(n.Cards, c)
		}
		for _, e := range status.Engines {
			loaded[e.Name] = map[string]bool{}
			starting[e.Name] = map[string]bool{}
			for _, m := range e.Models {
				loaded[e.Name][m.Name] = m.State == "loaded" || m.State == "ready"
				starting[e.Name][m.Name] = m.State == "starting"
			}
		}
	}
	for name, states := range live {
		loaded[name], starting[name] = map[string]bool{}, map[string]bool{}
		for m, s := range states {
			loaded[name][m] = s == "ready"
			starting[name][m] = s == "starting"
		}
	}
	for _, row := range rows {
		be := boardEngine{guiRow: row, Models: []boardModel{}}
		if status != nil {
			for _, l := range status.Leases {
				if (l.Engine == row.Name || l.Group == row.Name) && !l.Pinned && l.State == "active" {
					be.Stoppable = true
				}
			}
		}
		for _, id := range listed[row.Name] {
			l := loaded[row.Name][id]
			be.Models = append(be.Models, boardModel{ID: id, Loaded: l,
				Loading: !l && (starting[row.Name][id] || inFlight[loadKey{row.Name, id}])})
		}
		n.Engines = append(n.Engines, be)
	}
	return n
}
