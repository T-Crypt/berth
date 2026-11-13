// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package fit ranks where a request can run: which engine, by what path
// (resume from RAM, a free card, a drain of the holder, or a wait for a card
// something keeps), and what the start costs, each cost with its source. It
// reads a status and the engines' contracts and does no I/O, so a recorded
// status replays the same answer. Fit advises; the broker decides.
package fit

import (
	"fmt"
	"slices"
	"strings"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/contract"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/swapcost"
)

// Path is how an engine gets its card, cheapest first.
type Path int

const (
	Resume  Path = iota // parked in RAM: a resume puts it back
	Free                // nothing holds, uses or waits for the card
	Drain               // the holder is drained first
	Blocked             // a VM, a cordon or a pinned lease keeps the card
)

var pathNames = [...]string{Resume: "resume", Free: "free", Drain: "drain", Blocked: "blocked"}

func (p Path) String() string { return pathNames[p] }

// MarshalText writes the path's name.
func (p Path) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// Candidate is one engine and what it takes to bring it up.
type Candidate struct {
	Engine string `json:"engine" yaml:"engine"`
	Path   Path   `json:"path" yaml:"path"`
	Why    string `json:"why" yaml:"why"`
	// Tier is where the engine's weights sit before the start.
	Tier swapcost.Tier `json:"tier" yaml:"tier"`
	// Start is the measured switch time from Tier, unknown without samples
	// or while the card is out of reach.
	Start contract.Duration `json:"start" yaml:"start"`

	Entry *config.Engine `json:"-" yaml:"-"`
}

// Rank orders engines by path, cheapest first; engines on one path keep
// their order (berth.toml's). book is the gateway's switch book, nil for
// none: every start is then unknown.
func Rank(st *proto.Status, engines []*config.Engine, book *swapcost.Book) []Candidate {
	out := make([]Candidate, 0, len(engines))
	for _, e := range engines {
		out = append(out, candidate(st, e, book))
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return int(a.Path) - int(b.Path) })
	return out
}

func candidate(st *proto.Status, e *config.Engine, book *swapcost.Book) Candidate {
	c := Candidate{Engine: e.Name, Entry: e, Tier: swapcost.Cold}
	v := node.View(st, e.GPU)
	switch {
	case Parked(st, e.Name):
		c.Path, c.Tier, c.Why = Resume, swapcost.Warm, "parked in RAM"
	case CardFree(v):
		c.Path, c.Why = Free, "the card is free"
	case v != nil && OutOfReach(v) == "":
		c.Path, c.Why = Drain, "drains "+holders(v)
	default:
		c.Path, c.Why = Blocked, OutOfReach(v)
		if c.Why == "" {
			c.Why = "berthd did not report the card"
		}
	}
	if c.Path == Blocked {
		c.Start = contract.Duration{Source: contract.Unknown, Why: "the card is out of reach"}
		return c
	}
	for _, s := range contract.Build(e.Name, e, st, book).Switch {
		if s.Tier == c.Tier {
			c.Start = s.Took
		}
	}
	return c
}

// holders names what holds or waits for the card, for a drain's reason.
func holders(st *proto.Status) string {
	var names []string
	for _, l := range st.Leases {
		names = append(names, l.Holder())
	}
	switch {
	case len(names) > 0:
		return strings.Join(names, " and ")
	case len(st.Squatters) > 0:
		return "memory no lease accounts for"
	default:
		return "the waiters ahead"
	}
}

// Parked is whether the engine's own lease sits parked in RAM. A parked model
// behind a router (a llama-swap model under the llama-swap group) has its own
// front, which wakes it when the router forwards a request, so the router is
// never parked on its behalf.
func Parked(st *proto.Status, name string) bool {
	if st == nil {
		return false
	}
	for _, l := range st.Leases {
		if l.Engine == name && l.State == "parked" {
			return true
		}
	}
	return false
}

// CardFree is whether nothing holds, uses or waits for the card and the card
// serves this machine. A parked lease holds it: a batch job must not evict an
// engine someone parked in RAM to come back to. A stowaway or a tracked app
// (Blender) holds memory no lease accounts for, and a start beside it is the
// collision Berth exists to prevent.
func CardFree(st *proto.Status) bool {
	if st == nil || st.Passthrough != nil || st.ProbeErr != "" ||
		len(st.Leases) > 0 || len(st.Waiting) > 0 || len(st.Squatters) > 0 || Cordoned(st) {
		return false
	}
	// Status lists every claim rule; only one matching right now is in use.
	return !slices.ContainsFunc(st.Claims, func(c proto.Claim) bool { return c.Active })
}

// OutOfReach says why nothing can start on the card now, "" when something
// can: the card is in a VM, cordoned, or a pinned lease (a game, a hold, a
// training) has it.
func OutOfReach(st *proto.Status) string {
	if st == nil {
		return ""
	}
	if st.Passthrough != nil {
		return "the card is passed through to a VM"
	}
	if Cordoned(st) {
		return "the card is cordoned"
	}
	for _, l := range st.Leases {
		if l.Pinned {
			return fmt.Sprintf("%s holds the card", l.Holder())
		}
	}
	return ""
}

// Cordoned is whether a scheduled card in the status is cordoned.
func Cordoned(st *proto.Status) bool {
	return slices.ContainsFunc(st.GPUs, func(g proto.GPU) bool { return g.Scheduled && g.Cordoned })
}
