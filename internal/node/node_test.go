// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

const gib = 1 << 30

// card is one fake device: what the driver reports for it.
type card struct {
	mu    sync.Mutex
	index int
	total uint64
	procs []gpu.Proc
}

func (c *card) Read(context.Context) (gpu.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var used uint64
	for _, p := range c.procs {
		used += p.Used
	}
	return gpu.Snapshot{Index: c.index, Total: c.total, Used: used, Free: c.total - used,
		Procs: append([]gpu.Proc(nil), c.procs...)}, nil
}

type quiet struct{}

func (quiet) Squatter(bool, string, gpu.Proc)      {}
func (quiet) Alert(string, string, string, string) {}

// twoCards is a node over gpu0 (0000:01:00.0) and gpu1 (0000:02:00.0), 24 GiB
// each, with the shared event log, lease counter and parking ledger.
func twoCards(t *testing.T) *Node {
	t.Helper()
	events, ids, seq, ledger := Shared()
	var cards []*Card
	for i, addr := range []string{"0000:01:00.0", "0000:02:00.0"} {
		name := []string{"gpu0", "gpu1"}[i]
		b := broker.New(broker.Config{
			Card: name, Events: events, IDs: ids, Seq: seq, Ledger: ledger, Reporter: quiet{},
			ReleaseGrace: time.Hour, DefaultWait: 2 * time.Second, JuggleWindow: -1,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, &card{index: i, total: 24 * gib}, procs.Map{})
		b.Tick(context.Background())
		cards = append(cards, &Card{Name: name, Addr: addr, B: b})
	}
	n, err := New(events, ids, cards...)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func grant(t *testing.T, n *Node, cardKey, engine string) *broker.Lease {
	t.Helper()
	c, err := n.Card(cardKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l, err := c.B.Acquire(ctx, broker.Request{Engine: engine, Priority: broker.Foreground}, nil)
	if err != nil {
		t.Fatalf("acquire %s on %s: %v", engine, cardKey, err)
	}
	return l
}

// A request names its card by name or by PCI address in either form; no
// card means the first, as every request meant on a one-card berthd.
func TestCardIsFoundByNameOrAddress(t *testing.T) {
	n := twoCards(t)
	for key, want := range map[string]string{
		"": "gpu0", "gpu1": "gpu1", "0000:02:00.0": "gpu1", "00000000:02:00.0": "gpu1", "0000:01:00.0": "gpu0",
	} {
		c, err := n.Card(key)
		if err != nil || c.Name != want {
			t.Fatalf("Card(%q) = %v, %v; want %s", key, c, err, want)
		}
	}
	if _, err := n.Card("gpu7"); err == nil || err.Error() != "no card gpu7 on this node (cards: gpu0, gpu1)" {
		t.Fatalf("Card(gpu7) error = %v, want the node's cards named", err)
	}
}

// Two cards each grant a whole-card foreground lease at once: one card's
// holder never makes the other card's request wait. The two leases of one
// engine get different IDs, so a stop or a reclaim names one of them.
func TestCardsScheduleApartAndLeaseIDsStayUnique(t *testing.T) {
	n := twoCards(t)
	a := grant(t, n, "gpu0", "strata")
	b := grant(t, n, "gpu1", "strata")
	if a.ID() == b.ID() {
		t.Fatalf("both cards' leases are %s; want one ID per lease on the node", a.ID())
	}
	st := n.Status()
	got := map[string]string{}
	for _, l := range st.Leases {
		got[l.ID] = l.Card
	}
	if got[a.ID()] != "gpu0" || got[b.ID()] != "gpu1" || len(got) != 2 {
		t.Fatalf("status leases = %v, want %s on gpu0 and %s on gpu1", got, a.ID(), b.ID())
	}
}

// Every card writes to one event stream: the sequence numbers run in order
// across cards, and each event names its card.
func TestEventsAreOneStreamNamedByCard(t *testing.T) {
	n := twoCards(t)
	grant(t, n, "gpu0", "comfyui")
	grant(t, n, "gpu1", "vllm")

	evs, _, _ := n.Events.Events(0, false)
	cards := map[string]string{}
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d; want one sequence for the node", i, e.Seq)
		}
		if e.Kind == proto.EvGranted {
			cards[e.Engine] = e.Card
		}
	}
	if cards["comfyui"] != "gpu0" || cards["vllm"] != "gpu1" {
		t.Fatalf("granted events by card = %v, want comfyui on gpu0 and vllm on gpu1", cards)
	}
	if st := n.Status(); st.LastEvent != uint64(len(evs)) {
		t.Fatalf("status last event = %d, want %d", st.LastEvent, len(evs))
	}
}

// The machine's status lists each card's GPU once, by card name.
func TestStatusListsEachCardOnce(t *testing.T) {
	n := twoCards(t)
	st := n.Status()
	var cards []string
	for _, g := range st.GPUs {
		cards = append(cards, g.Card)
	}
	if strings.Join(cards, ",") != "gpu0,gpu1" {
		t.Fatalf("status GPUs = %v, want gpu0,gpu1", cards)
	}
}

// berth stop and berth resume name an engine, not a card: the node finds the
// card it runs on, and a name no card knows gets the one-card answer.
func TestStopAndResumeFindTheEngineOnItsCard(t *testing.T) {
	n := twoCards(t)
	l := grant(t, n, "gpu1", "vllm")
	l.Attach(4242)
	ids, err := n.Stop("vllm")
	if err != nil || len(ids) != 1 || ids[0] != l.ID() {
		t.Fatalf("Stop(vllm) = %v, %v; want %s on gpu1", ids, err, l.ID())
	}
	if _, err := n.Stop("nope"); err == nil || err.Error() != "no running engine named nope under a lease" {
		t.Fatalf("Stop(nope) error = %v", err)
	}
	if err := n.Resume("nope", false); err == nil || err.Error() != "nope is not parked" {
		t.Fatalf("Resume(nope) error = %v", err)
	}
}

// A card in a VM at startup is named by its address, and that is one card,
// not two cards with the same name.
func TestNewTakesACardNamedByItsAddress(t *testing.T) {
	events, ids, _, _ := Shared()
	b := broker.New(broker.Config{Events: events, IDs: ids}, &card{total: 24 * gib}, procs.Map{})
	n, err := New(events, ids, &Card{Name: "0000:02:00.0", Addr: "0000:02:00.0", B: b})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := n.Card("00000000:02:00.0"); err != nil || c.Name != "0000:02:00.0" {
		t.Fatalf("card by nvidia-smi address = %v, %v", c, err)
	}
}

// A request names several cards with a comma list, by name or address, for
// one lease across them; a card named twice is refused.
func TestPickNamesEveryCardOfAList(t *testing.T) {
	n := twoCards(t)
	for key, want := range map[string]string{
		"": "gpu0", "gpu1": "gpu1", "gpu0,gpu1": "gpu0,gpu1", "gpu1,0000:01:00.0": "gpu1,gpu0",
	} {
		cs, err := n.Pick(key)
		if err != nil {
			t.Fatalf("Pick(%q): %v", key, err)
		}
		var names []string
		for _, c := range cs {
			names = append(names, c.Name)
		}
		if got := strings.Join(names, ","); got != want {
			t.Errorf("Pick(%q) = %s, want %s", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"gpu0,0000:01:00.0": "gpu0 is named twice in gpu0,0000:01:00.0",
		"gpu0,gpu7":         "no card gpu7 on this node (cards: gpu0, gpu1)",
	} {
		if _, err := n.Pick(key); err == nil || err.Error() != want {
			t.Errorf("Pick(%q) error = %v, want %q", key, err, want)
		}
	}
}

// berth stop of an engine with one lease on two cards drains both shares
// and names the lease once.
func TestStopNamesALeaseOnSeveralCardsOnce(t *testing.T) {
	n := twoCards(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cs, err := n.Pick("gpu0,gpu1")
	if err != nil {
		t.Fatal(err)
	}
	ls, err := broker.AcquireAll(ctx, []*broker.Broker{cs[0].B, cs[1].B}, broker.Request{Engine: "maya", Priority: broker.Foreground}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		l.Attach(4242)
	}
	ids, err := n.Stop("maya")
	if err != nil || len(ids) != 1 || ids[0] != ls[0].ID() {
		t.Fatalf("Stop(maya) = %v, %v; want [%s]", ids, err, ls[0].ID())
	}
	for _, l := range ls {
		select {
		case <-l.Drains():
		case <-time.After(time.Second):
			t.Fatal("a share of maya was not drained")
		}
	}
}

// View narrows a status to the cards a key names, by name or by PCI address
// in either form, and keeps what holds every card.
func TestViewIsTheCardsAKeyNames(t *testing.T) {
	st := &proto.Status{
		GPUs: []proto.GPU{
			{Card: "gpu0", Addr: "0000:01:00.0", Scheduled: true},
			{Card: "gpu1", Addr: "0000:09:00.0", Scheduled: true},
		},
		Leases:    []proto.Lease{{Engine: "chat", Card: "gpu0"}, {Engine: "maya", Card: "gpu1"}},
		Waiting:   []proto.Lease{{Engine: "voice", Card: "gpu1"}},
		Squatters: []proto.Squatter{{PID: 7, Card: "gpu0"}},
		Claims: []proto.Claim{{Name: "steam-game", Active: true}, {Name: "sim", Active: true, Lease: "l-4", Cards: []string{"gpu1"}},
			{Name: "blender", Action: "track"}},
	}
	engines := func(ls []proto.Lease) string {
		var out []string
		for _, l := range ls {
			out = append(out, l.Engine)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		key, gpus, leases, waiting, active string
		squatters                          int
	}{
		{"", "gpu0", "chat", "", "steam-game", 1},
		{"gpu1", "gpu1", "maya", "voice", "steam-game,sim", 0},
		{"0000:09:00.0", "gpu1", "maya", "voice", "steam-game,sim", 0},
		{"00000000:09:00.0", "gpu1", "maya", "voice", "steam-game,sim", 0},
		{"gpu1,0000:01:00.0", "gpu0,gpu1", "chat,maya", "voice", "steam-game,sim", 1},
	} {
		v := View(st, tc.key)
		var gpus []string
		for _, g := range v.GPUs {
			gpus = append(gpus, g.Card)
		}
		if got := strings.Join(gpus, ","); got != tc.gpus {
			t.Errorf("%q: cards %s, want %s", tc.key, got, tc.gpus)
		}
		if got := engines(v.Leases); got != tc.leases {
			t.Errorf("%q: leases %s, want %s", tc.key, got, tc.leases)
		}
		if got := engines(v.Waiting); got != tc.waiting {
			t.Errorf("%q: waiting %s, want %s", tc.key, got, tc.waiting)
		}
		if len(v.Squatters) != tc.squatters {
			t.Errorf("%q: %d squatters, want %d", tc.key, len(v.Squatters), tc.squatters)
		}
		var active []string
		for _, cl := range v.Claims {
			if cl.Active {
				active = append(active, cl.Name)
			}
		}
		if got := strings.Join(active, ","); len(v.Claims) != 3 || got != tc.active {
			t.Errorf("%q: active claims %s of %d, want %s of 3", tc.key, got, len(v.Claims), tc.active)
		}
	}
	if v := View(st, "gpu9"); v != nil {
		t.Errorf("gpu9: %+v, want nil for a card the node lacks", v)
	}
	if len(st.Leases) != 2 || len(st.Squatters) != 1 || !st.Claims[1].Active {
		t.Errorf("View changed the status it read: %+v", st)
	}
}

// A card that cannot be read is that card's problem. A view of another
// card reads clean, so the gateway still starts engines there; a view of
// the failing card, or of the whole machine, names it.
func TestProbeErrorIsPerCard(t *testing.T) {
	gpu0 := proto.Status{GPUs: []proto.GPU{{Index: 0, Scheduled: true}}}
	gpu1 := proto.Status{GPUs: []proto.GPU{{Index: 1, Scheduled: true}}, ProbeErr: "unable to determine the device handle"}
	named(&gpu0, &Card{Name: "gpu0"})
	named(&gpu1, &Card{Name: "gpu1"})
	st := &proto.Status{GPUs: []proto.GPU{gpu0.GPUs[0], gpu1.GPUs[0]}, ProbeErr: "gpu1: unable to determine the device handle"}
	for key, want := range map[string]string{
		"gpu0":      "",
		"gpu1":      "gpu1: unable to determine the device handle",
		"gpu0,gpu1": "gpu1: unable to determine the device handle",
	} {
		if got := View(st, key).ProbeErr; got != want {
			t.Errorf("%q: probe error %q, want %q", key, got, want)
		}
	}
}

// One card in a VM is that card's state, not the machine's. A view of the
// other card still serves; a view of the card in the VM, or of every card
// when each is in a VM, reports the passthrough.
func TestPassthroughIsPerCard(t *testing.T) {
	vm := &proto.Passthrough{State: proto.CardPassthrough, Driver: "vfio-pci", Addr: "0000:09:00.0"}
	gpu0 := proto.Status{GPUs: []proto.GPU{{Index: 0, Scheduled: true}}}
	gpu1 := proto.Status{GPUs: []proto.GPU{{Index: 1, Scheduled: true}}, Passthrough: vm}
	named(&gpu0, &Card{Name: "gpu0", Addr: "0000:01:00.0"})
	named(&gpu1, &Card{Name: "gpu1", Addr: "0000:09:00.0"})
	if gpu0.GPUs[0].Passthrough != nil || gpu1.GPUs[0].Passthrough != vm {
		t.Fatalf("stamped %+v and %+v, want only gpu1 in the VM", gpu0.GPUs[0].Passthrough, gpu1.GPUs[0].Passthrough)
	}

	st := &proto.Status{GPUs: []proto.GPU{gpu0.GPUs[0], gpu1.GPUs[0]}, Passthrough: vm}
	for key, want := range map[string]*proto.Passthrough{"gpu0": nil, "gpu1": vm, "gpu0,gpu1": nil} {
		if got := View(st, key).Passthrough; got != want {
			t.Errorf("%q: passthrough %+v, want %+v", key, got, want)
		}
	}
	st.GPUs[0].Passthrough = vm
	if got := View(st, "gpu0,gpu1").Passthrough; got != vm {
		t.Errorf("every card in a VM: passthrough %+v, want %+v", got, vm)
	}
}

// A claim tracked on gpu1 keeps its memory and its card in the node's
// status, and the engine ranking adds up the hours from every card.
func TestClaimsAndPriorityMergeAcrossCards(t *testing.T) {
	g0 := proto.Status{
		Claims: []proto.Claim{{Name: "blender", Action: "track", Active: true, Used: 2 * gib, Procs: "blender"},
			{Name: "game", Action: "hold"}},
		Priority: &proto.PriorityStatus{Source: "usage", Engines: []proto.EngineRank{
			{Engine: "maya", Rank: 1, Hours7d: 3}, {Engine: "voice", Rank: 2, Hours7d: 1}}},
	}
	g1 := proto.Status{
		Claims: []proto.Claim{{Name: "blender", Action: "track", Active: true, Used: 3 * gib, GTTUsed: gib},
			{Name: "game", Action: "hold"}},
		Priority: &proto.PriorityStatus{Source: "usage", Engines: []proto.EngineRank{
			{Engine: "voice", Rank: 1, Hours7d: 4}, {Engine: "comfy", Rank: 2, Hours7d: 2}}},
	}
	named(&g0, &Card{Name: "gpu0"})
	named(&g1, &Card{Name: "gpu1"})
	claims := mergeClaims(g0.Claims, g1.Claims)
	b := claims[0]
	if b.Used != 5*gib || b.GTTUsed != gib || !b.Active || strings.Join(b.Cards, ",") != "gpu0,gpu1" {
		t.Errorf("blender = %+v, want 5 GiB + 1 GiB GTT on gpu0,gpu1", b)
	}
	if g := claims[1]; g.Active || len(g.Cards) != 0 {
		t.Errorf("idle game claim = %+v, want no cards", g)
	}
	if len(g0.Claims[0].Cards) != 1 {
		t.Errorf("merge changed gpu0's own claim: %+v", g0.Claims[0])
	}
	var got []string
	for _, e := range mergePriority(g0.Priority, g1.Priority).Engines {
		got = append(got, fmt.Sprintf("%s=%g#%d", e.Engine, e.Hours7d, e.Rank))
	}
	if want := "maya=3#1 voice=5#2 comfy=2#3"; strings.Join(got, " ") != want {
		t.Errorf("engines = %v, want %s", got, want)
	}
}
