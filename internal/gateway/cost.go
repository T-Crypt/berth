// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// The gateway runs a start, so it is the one witness that sees both ends
// of it: the grant its run printed, which is when the card showed free,
// and the engine's first answer. That is the pair swapcost measures, and
// it is a pair, not a request's wait: the time behind the grant belongs
// to whoever held the card, not to the engine loading.

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/fit"
	"github.com/T-Crypt/berth/internal/pagecache"
	"github.com/T-Crypt/berth/internal/swapcost"
)

// costBook returns the gateway's book of measured switch times, read from
// CostFile once per gateway; an empty CostFile keeps it in memory only.
// A file that is not a book is one WARN and an empty book: a figure the
// gateway gives is one it measured after that, not a guess carried over.
func (g *Gateway) costBook() *swapcost.Book {
	g.costOnce.Do(func() {
		b, err := swapcost.NewBook(g.CostFile)
		if err != nil {
			g.log().Warn("gateway could not read its switch-cost book; starting empty", "path", g.CostFile, "err", err)
		}
		g.costs = b
	})
	return g.costs
}

// position is where an engine sat before a start: whether its weights
// were parked in host RAM, and what share of its files the kernel page
// cache held. The tier a sample is recorded under is read from it.
type position struct {
	parked bool
	cached float64
}

// readPosition takes the engine's position now, at the moment its start
// is decided and before its prefetch reads begin: the node's lease table
// says whether the engine is parked, and the page cache says how much of
// its files it holds. No prefetch list, or a file whose pages cannot be
// checked, leaves the share 0: a part of the answer would be a wrong
// fraction, and cold is the honest reading of not knowing.
func (g *Gateway) readPosition(e *config.Engine) *position {
	pos := &position{parked: fit.Parked(g.nodeStatus(), e.Name)}
	files, _ := prefetchFiles(e.Prefetch)
	if len(files) == 0 {
		return pos
	}
	var cached, size int64
	for _, f := range files {
		c, sz, err := pagecache.Resident(f)
		if err != nil {
			return pos
		}
		cached += c
		size += sz
	}
	if size > 0 {
		pos.cached = float64(cached) / float64(size)
	}
	return pos
}

// stampGrant notes the moment the run printed its grant, the point the
// card was free. Engines this gateway adopted carry no position: their
// starts were run by a previous gateway, and grants the follower sees on
// them complete the engine record but are never measured.
func (g *Gateway) stampGrant(s *started) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s.pos != nil && s.grantAt.IsZero() {
		s.grantAt = time.Now()
	}
}

// markAnswered closes a start whose engine just answered: the failure
// cooldown clears, and when this gateway ran the start and saw its grant,
// the switch from that grant to this answer goes into the book once,
// whichever waiting request noticed the answer first.
func (g *Gateway) markAnswered(e *config.Engine, s *started) {
	var sample *swapcost.Sample
	g.mu.Lock()
	s.answered = true
	if !s.recorded && s.pos != nil && !s.grantAt.IsZero() {
		s.recorded = true
		now := time.Now()
		sample = &swapcost.Sample{
			Engine: e.Name,
			Tier:   swapcost.TierFor(s.pos.parked, false, s.pos.cached),
			Took:   now.Sub(s.grantAt),
			At:     now,
		}
	}
	delete(g.failed, e.Name)
	g.mu.Unlock()
	if sample != nil {
		g.saveSwitch(*sample)
	}
}

// saveSwitch adds one measured switch to the book and writes the book to
// its file. A failed write is one WARN line: the gateway keeps serving,
// and the next recorded switch carries this one too.
func (g *Gateway) saveSwitch(s swapcost.Sample) {
	book := g.costBook()
	g.costMu.Lock()
	defer g.costMu.Unlock()
	book.Record(s)
	if err := book.Save(); err != nil {
		g.log().Warn("gateway could not save its switch-cost book", "path", g.CostFile, "err", err)
	}
}

// tierOrder is the order /v1/berth/cost answers tiers in: the book's
// four positions, nearest the card first.
var tierOrder = []swapcost.Tier{swapcost.Hot, swapcost.Warm, swapcost.Cool, swapcost.Cold}

// costReply is one engine's estimates, one per tier, times in ms. An
// estimate with known false has no samples: its took_ms is 0 and is not
// a prediction.
type costReply struct {
	Engine    string          `json:"engine"`
	Estimates []estimateReply `json:"estimates"`
}

type estimateReply struct {
	Tier    string `json:"tier"`
	TookMs  int64  `json:"took_ms"`
	Samples int    `json:"samples"`
	Known   bool   `json:"known"`
}

// cost answers what the gateway measured about starting one engine from
// each tier. An engine berth.toml does not name is a 404: the gateway
// starts what it is configured to start and has measured nothing else.
func (g *Gateway) cost(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("engine")
	if name == "" {
		apiError(w, http.StatusBadRequest, "name the engine to price: /v1/berth/cost?engine=NAME")
		return
	}
	if g.engineByName(name) == nil {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine %q in the config", name))
		return
	}
	book := g.costBook()
	out := costReply{Engine: name, Estimates: make([]estimateReply, 0, len(tierOrder))}
	g.costMu.Lock()
	for _, tier := range tierOrder {
		es := book.Estimate(name, tier)
		out.Estimates = append(out.Estimates, estimateReply{
			Tier: string(es.Tier), TookMs: es.Took.Milliseconds(), Samples: es.Samples, Known: es.Known})
	}
	g.costMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// planReply is swapcost.Plan as the gateway serves it: one step per
// chain entry, and total_ms the sum of the known steps only. With
// unmeasured above 0 the total is a lower bound, "at least this, N
// steps unmeasured", never a promise. prefetch is per step: the engine
// to warm while that step holds the card, empty where the next step is
// the same engine or the chain ends; keep_parked lists the engines the
// chain comes back to, so the caller parks them instead of paying a
// reload.
type planReply struct {
	Steps      []stepReply `json:"steps"`
	TotalMs    int64       `json:"total_ms"`
	Unmeasured int         `json:"unmeasured"`
	Prefetch   []string    `json:"prefetch"`
	KeepParked []string    `json:"keep_parked"`
}

type stepReply struct {
	Engine  string `json:"engine"`
	Tier    string `json:"tier"`
	TookMs  int64  `json:"took_ms"`
	Samples int    `json:"samples"`
	Known   bool   `json:"known"`
}

// plan works a declared chain out against the book: what each switch is
// likely to take, what the total is a lower bound on, which engine to
// prefetch at each step, and which engines to keep parked.
func (g *Gateway) plan(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		apiError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		Chain []struct {
			Engine string `json:"engine"`
			Tier   string `json:"tier"`
		} `json:"chain"`
	}
	if json.Unmarshal(body, &req) != nil {
		apiError(w, http.StatusBadRequest, `the body must be JSON like {"chain":[{"engine":"NAME","tier":"cold"}]}`)
		return
	}
	if len(req.Chain) == 0 {
		apiError(w, http.StatusBadRequest, "the chain is empty: name at least one step")
		return
	}
	steps := make([]swapcost.Step, 0, len(req.Chain))
	for i, s := range req.Chain {
		switch swapcost.Tier(s.Tier) {
		case swapcost.Hot, swapcost.Warm, swapcost.Cool, swapcost.Cold:
		default:
			apiError(w, http.StatusBadRequest, fmt.Sprintf("chain step %d names tier %q: it must be hot, warm, cool or cold", i+1, s.Tier))
			return
		}
		if g.engineByName(s.Engine) == nil {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("chain step %d names engine %q, which is not in the config", i+1, s.Engine))
			return
		}
		steps = append(steps, swapcost.Step{Engine: s.Engine, Tier: swapcost.Tier(s.Tier)})
	}
	book := g.costBook()
	g.costMu.Lock()
	p := book.Plan(steps, time.Now())
	g.costMu.Unlock()
	out := planReply{
		Steps:      make([]stepReply, 0, len(p.Steps)),
		TotalMs:    p.Total.Milliseconds(),
		Unmeasured: p.Unmeasured,
		Prefetch:   p.Prefetch,
		KeepParked: p.KeepParked,
	}
	for _, sc := range p.Steps {
		out.Steps = append(out.Steps, stepReply{
			Engine: sc.Step.Engine, Tier: string(sc.Step.Tier),
			TookMs: sc.Estimate.Took.Milliseconds(), Samples: sc.Estimate.Samples, Known: sc.Estimate.Known})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
