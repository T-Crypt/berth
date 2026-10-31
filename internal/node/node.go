// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package node is one machine's cards under one berthd. Each card keeps its
// own broker (its leases, queue, drains, parks and lends), and the node routes
// a request to the card it names, searches every card for an engine or a
// lease named by the user, and answers status and events for the machine as a
// whole. Leases never cross a machine (docs/design.md, "Many machines"), so the
// node is the one authority for its cards and the status it builds is the
// machine view a peer reads.
package node

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/eventlog"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/proto"
)

// Card is one governed card: its name on the node (gpu0, gpu1), its PCI
// address in sysfs form when known, its NVIDIA UUID when known (what
// CUDA_VISIBLE_DEVICES takes), and the broker that schedules it.
type Card struct {
	Name string
	Addr string
	UUID string
	B    *broker.Broker
}

// Node is a machine's cards. The first card is the default: a request that
// names no card goes there, as every request did when a berthd had one card.
type Node struct {
	cards  []*Card
	Events *eventlog.Log  // the machine's one event stream, shared by every card's broker
	IDs    *atomic.Uint64 // the machine's lease numbers, shared by every card's broker
}

// Shared returns the event log, lease counter, arrival order and parking
// ledger a node's brokers must be built with, so the machine has one ordered
// stream, one ID space, one queue order across its cards and one budget of
// RAM for parks.
func Shared() (*eventlog.Log, *atomic.Uint64, *atomic.Uint64, *park.Ledger) {
	return eventlog.New(), &atomic.Uint64{}, &atomic.Uint64{}, &park.Ledger{}
}

// New is a node over cards, which must have distinct names and addresses.
func New(events *eventlog.Log, ids *atomic.Uint64, cards ...*Card) (*Node, error) {
	if len(cards) == 0 {
		return nil, errors.New("a node needs at least one card")
	}
	seen := map[string]bool{}
	for _, c := range cards {
		for _, k := range slices.Compact([]string{c.Name, c.Addr}) {
			if k == "" {
				continue
			}
			if seen[k] {
				return nil, fmt.Errorf("two cards are named %s", k)
			}
			seen[k] = true
		}
	}
	return &Node{cards: cards, Events: events, IDs: ids}, nil
}

// One is a node of one card, gpu0, over a broker built on its own: the node a
// one-card berthd serves, and the one tests serve a lone broker through.
func One(b *broker.Broker) *Node {
	return &Node{cards: []*Card{{Name: "gpu0", B: b}}, Events: b.EventLog(), IDs: b.IDs()}
}

// Cards is the node's cards in order.
func (n *Node) Cards() []*Card { return n.cards }

// Card is the card a request names: its name, or its PCI address in either
// sysfs or nvidia-smi form. "" is the first card.
func (n *Node) Card(key string) (*Card, error) {
	if key == "" {
		return n.cards[0], nil
	}
	for _, c := range n.cards {
		if answers(c.Name, c.Addr, key) {
			return c, nil
		}
	}
	names := make([]string, len(n.cards))
	for i, c := range n.cards {
		names[i] = c.Name
	}
	return nil, fmt.Errorf("no card %s on this node (cards: %s)", key, strings.Join(names, ", "))
}

// answers is whether the card named name at PCI address addr is the one key
// names: its name, or its address in sysfs or nvidia-smi form.
func answers(name, addr, key string) bool {
	if name == key {
		return true
	}
	return addr != "" && strings.Contains(key, ":") && gpu.SysfsAddr(key) == addr
}

// View is a node's status narrowed to the cards key picks, as Pick picks
// them: their readings, leases, waiters and squatters. A claim stays listed,
// active only when it is active on one of them (a claim with no cards, from
// an older berthd, counts as on every card). What holds every card (a
// passthrough, a probe error) stays. nil when st is nil or key names a card
// st does not have.
func View(st *proto.Status, key string) *proto.Status {
	if st == nil {
		return nil
	}
	cards := CardsOf(st, key)
	if cards == nil {
		return nil
	}
	on := func(card string) bool { return card == "" || slices.Contains(cards, card) }
	v := *st
	v.GPUs = slices.DeleteFunc(slices.Clone(st.GPUs), func(g proto.GPU) bool { return !g.Scheduled || !on(g.Card) })
	v.Leases = slices.DeleteFunc(slices.Clone(st.Leases), func(l proto.Lease) bool { return !on(l.Card) })
	v.Waiting = slices.DeleteFunc(slices.Clone(st.Waiting), func(l proto.Lease) bool { return !on(l.Card) })
	v.Squatters = slices.DeleteFunc(slices.Clone(st.Squatters), func(s proto.Squatter) bool { return !on(s.Card) })
	v.Claims = slices.Clone(st.Claims)
	for i, cl := range v.Claims {
		if len(cl.Cards) > 0 && !slices.ContainsFunc(cl.Cards, on) {
			v.Claims[i].Active, v.Claims[i].Lease = false, ""
		}
	}
	v.Passthrough = inVM(v.GPUs)
	// A view of some cards carries only their probe errors. One that keeps
	// every card keeps the status's own, which a berthd older than per-card
	// errors fills in alone.
	if len(v.GPUs) < scheduled(st.GPUs) {
		var errs []string
		for _, g := range v.GPUs {
			if g.ProbeErr != "" {
				errs = append(errs, g.Card+": "+g.ProbeErr)
			}
		}
		v.ProbeErr = strings.Join(errs, "; ")
	}
	return &v
}

func scheduled(gpus []proto.GPU) int {
	n := 0
	for _, g := range gpus {
		if g.Scheduled {
			n++
		}
	}
	return n
}

// inVM is the first scheduled card's VM state when every scheduled card in
// gpus is in a VM, nil while one of them serves.
func inVM(gpus []proto.GPU) *proto.Passthrough {
	var first *proto.Passthrough
	for _, g := range gpus {
		if !g.Scheduled {
			continue
		}
		if g.Passthrough == nil {
			return nil
		}
		if first == nil {
			first = g.Passthrough
		}
	}
	return first
}

// CardsOf is the names of the cards key picks in a node's status: "" is
// the first card. nil when a name in key is no card in st.
func CardsOf(st *proto.Status, key string) []string {
	var cards []proto.GPU
	for _, g := range st.GPUs {
		if g.Scheduled {
			cards = append(cards, g)
		}
	}
	if len(cards) == 0 {
		return nil
	}
	if key == "" {
		return []string{cards[0].Card}
	}
	var out []string
	for _, k := range strings.Split(key, ",") {
		i := slices.IndexFunc(cards, func(g proto.GPU) bool { return answers(g.Card, g.Addr, k) })
		if i < 0 {
			return nil
		}
		out = append(out, cards[i].Card)
	}
	return out
}

// Pick is the cards a request names: one card as Card takes it, or a comma
// list of them for one lease on several cards. "" is the first card.
func (n *Node) Pick(key string) ([]*Card, error) {
	var out []*Card
	for _, k := range strings.Split(key, ",") {
		c, err := n.Card(k)
		if err != nil {
			return nil, err
		}
		if slices.Contains(out, c) {
			return nil, fmt.Errorf("%s is named twice in %s", c.Name, key)
		}
		out = append(out, c)
	}
	return out, nil
}

// Stop drains every lease of engine on every card.
func (n *Node) Stop(engine string) ([]string, error) {
	var ids []string
	var first error
	for _, c := range n.cards {
		got, err := c.B.Stop(engine)
		if err != nil {
			first = cmpErr(first, err)
			continue
		}
		ids = append(ids, got...)
	}
	if len(ids) == 0 {
		return nil, first
	}
	// A lease on several cards is drained on each of them, under one ID.
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// Resume puts engine's parked lease back in line on the card it parked on.
func (n *Node) Resume(engine string, switched bool) error {
	var first error
	for _, c := range n.cards {
		err := c.B.Resume(engine, switched)
		if err == nil || !broker.IsNotFound(err) {
			return err
		}
		first = cmpErr(first, err)
	}
	return first
}

// Switch marks the engine's queued lease on whichever card holds it as a
// person's switch.
func (n *Node) Switch(engine string) error {
	var first error
	for _, c := range n.cards {
		err := c.B.Switch(engine)
		if err == nil || !broker.IsNotFound(err) {
			return err
		}
		first = cmpErr(first, err)
	}
	return first
}

// Reclaim lists what a stuck lease recorded: the one named id on whichever
// card holds it, or, for "", every stuck lease on every card.
func (n *Node) Reclaim(id string) ([]proto.Holder, error) {
	var hs []proto.Holder
	var first error
	for _, c := range n.cards {
		got, err := c.B.Reclaim(id)
		if err != nil {
			if id != "" && !broker.IsNotFound(err) {
				return nil, err
			}
			first = cmpErr(first, err)
			continue
		}
		if id != "" {
			return got, nil
		}
		hs = append(hs, got...)
	}
	if id != "" {
		return nil, first
	}
	return hs, nil
}

// StillAlive is whether any card still sees the process.
func (n *Node) StillAlive(pid int, start uint64) bool {
	for _, c := range n.cards {
		if c.B.StillAlive(pid, start) {
			return true
		}
	}
	return false
}

// cmpErr keeps the first error it was given.
func cmpErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}

// Status is the machine's status: every card's GPU, leases, waiters and
// stowaways named by card, the board's counters added up, and what every
// card shares (the event sequence, claims, priorities, unprotected runs)
// once. Each card's status is read once, so a status costs one read per card
// however many cards the node has. With one card it is that card's broker's
// status with the card named.
func (n *Node) Status() proto.Status {
	all := make([]proto.Status, len(n.cards))
	governed := map[int]bool{}
	for i, c := range n.cards {
		all[i] = c.B.Status()
		named(&all[i], c)
		for _, g := range all[i].GPUs {
			if g.Scheduled {
				governed[g.Index] = true
			}
		}
	}
	st := all[0]
	if len(n.cards) == 1 {
		return st
	}
	// Every broker lists every device the probe sees; each card's own entry
	// comes from its broker, and a device no card governs is listed once.
	st.GPUs = slices.DeleteFunc(st.GPUs, func(g proto.GPU) bool { return !g.Scheduled && governed[g.Index] })
	runs := map[int]bool{}
	for _, r := range st.Unprotected {
		runs[r.PID] = true
	}
	foot := map[string]bool{}
	for _, f := range st.Footprints {
		foot[f.Engine] = true
	}
	var probeErrs []string
	if st.ProbeErr != "" {
		probeErrs = append(probeErrs, n.cards[0].Name+": "+st.ProbeErr)
	}
	for i, cs := range all[1:] {
		for _, g := range cs.GPUs {
			if g.Scheduled {
				st.GPUs = append(st.GPUs, g)
			}
		}
		st.Leases = append(st.Leases, cs.Leases...)
		st.Waiting = append(st.Waiting, cs.Waiting...)
		st.Squatters = append(st.Squatters, cs.Squatters...)
		for _, r := range cs.Unprotected {
			if !runs[r.PID] {
				runs[r.PID] = true
				st.Unprotected = append(st.Unprotected, r)
			}
		}
		for _, f := range cs.Footprints {
			if !foot[f.Engine] {
				foot[f.Engine] = true
				st.Footprints = append(st.Footprints, f)
			}
		}
		st.Claims = mergeClaims(st.Claims, cs.Claims)
		st.Priority = mergePriority(st.Priority, cs.Priority)
		st.Counters = add(st.Counters, cs.Counters)
		st.ParkedBytes += cs.ParkedBytes
		if cs.ProbeErr != "" {
			probeErrs = append(probeErrs, n.cards[i+1].Name+": "+cs.ProbeErr)
		}
	}
	st.Passthrough = inVM(st.GPUs)
	st.ProbeErr = strings.Join(probeErrs, "; ")
	st.LastEvent = n.Events.Last()
	return st
}

// named stamps the card's name on everything in st that lives on one card.
func named(st *proto.Status, c *Card) {
	for i := range st.GPUs {
		if st.GPUs[i].Scheduled {
			st.GPUs[i].Card, st.GPUs[i].Addr = c.Name, c.Addr
			st.GPUs[i].ProbeErr = st.ProbeErr
			st.GPUs[i].Passthrough = st.Passthrough
		}
	}
	for _, ls := range []([]proto.Lease){st.Leases, st.Waiting} {
		for i := range ls {
			ls[i].Card = c.Name
		}
	}
	for i := range st.Squatters {
		st.Squatters[i].Card = c.Name
	}
	for i, cl := range st.Claims {
		if cl.Active || cl.Used > 0 || cl.GTTUsed > 0 {
			st.Claims[i].Cards = []string{c.Name}
		}
	}
}

// mergeClaims adds another card's claim states to a: memory is summed, a
// claim is active if it is on any card, and its cards are the union.
func mergeClaims(a, b []proto.Claim) []proto.Claim {
	a = slices.Clone(a)
	for _, cl := range b {
		i := slices.IndexFunc(a, func(x proto.Claim) bool { return x.Name == cl.Name })
		if i < 0 {
			a = append(a, cl)
			continue
		}
		a[i].Active = a[i].Active || cl.Active
		a[i].Used += cl.Used
		a[i].GTTUsed += cl.GTTUsed
		a[i].Cards = append(slices.Clone(a[i].Cards), cl.Cards...)
		if a[i].Lease == "" {
			a[i].Lease = cl.Lease
		}
	}
	return a
}

// mergePriority sums each engine's 7 day hours over the cards. The engines
// keep the first card's order; one only another card has goes last.
func mergePriority(a, b *proto.PriorityStatus) *proto.PriorityStatus {
	if a == nil || b == nil {
		if a == nil {
			return b
		}
		return a
	}
	m := *a
	m.Engines = slices.Clone(a.Engines)
	for _, e := range b.Engines {
		i := slices.IndexFunc(m.Engines, func(x proto.EngineRank) bool { return x.Engine == e.Engine })
		if i < 0 {
			e.Rank = len(m.Engines) + 1
			m.Engines = append(m.Engines, e)
			continue
		}
		m.Engines[i].Hours7d += e.Hours7d
	}
	return &m
}

// add is the board's counters of two cards together.
func add(a, b proto.Counters) proto.Counters {
	a.CollisionsAvoided += b.CollisionsAvoided
	a.VRAMHandedOff += b.VRAMHandedOff
	a.Drains += b.Drains
	a.StuckCaught += b.StuckCaught
	a.SquattersCaught += b.SquattersCaught
	a.ContestedGrants += b.ContestedGrants
	a.Refused += b.Refused
	a.JugglesStopped += b.JugglesStopped
	if !b.Since.IsZero() && (a.Since.IsZero() || b.Since.Before(a.Since)) {
		a.Since = b.Since
	}
	return a
}
