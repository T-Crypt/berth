// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/contract"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/swapcost"
)

// recorded is a status berthd answered on aphotic on 2026-11-13, with a
// training lease holding the card pinned. Footprints of jobs Fit never ranks
// were cut and the training's name generalised; nothing else was changed.
func recorded(t *testing.T) *proto.Status {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "aphotic-training-pinned.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st proto.Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func engines(names ...string) []*config.Engine {
	out := make([]*config.Engine, len(names))
	for i, n := range names {
		out[i] = &config.Engine{Name: n}
	}
	return out
}

type got struct {
	Engine string
	Path   Path
	Why    string
	Tier   swapcost.Tier
	Start  contract.Duration
}

func summary(cs []Candidate) []got {
	out := make([]got, len(cs))
	for i, c := range cs {
		out[i] = got{c.Engine, c.Path, c.Why, c.Tier, c.Start}
	}
	return out
}

func check(t *testing.T, cs []Candidate, want []got) {
	t.Helper()
	g := summary(cs)
	if len(g) != len(want) {
		t.Fatalf("candidates = %+v, want %+v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("candidate %d = %+v\nwant          %+v", i, g[i], want[i])
		}
	}
}

var unknownCold = contract.Duration{Source: contract.Unknown, Why: "no switch measured"}

// The recorded training holds the card pinned: every engine waits for it,
// no start is estimated, and the reason names the holder.
func TestRankRecordedTraining(t *testing.T) {
	blocked := contract.Duration{Source: contract.Unknown, Why: "the card is out of reach"}
	check(t, Rank(recorded(t), engines("strata", "comfyui"), nil), []got{
		{"strata", Blocked, "training holds the card", swapcost.Cold, blocked},
		{"comfyui", Blocked, "training holds the card", swapcost.Cold, blocked},
	})
}

// Paths rank running, resume, free, drain, blocked; engines on one path keep
// berth.toml's order. Starts come from the switch book by tier, unknown
// where it has no samples.
func TestRankOrdersByPath(t *testing.T) {
	st := recorded(t)
	st.Leases = nil
	book, err := swapcost.NewBook(filepath.Join(t.TempDir(), "swapcost.json"))
	if err != nil {
		t.Fatal(err)
	}
	book.Record(swapcost.Sample{Engine: "comfyui", Tier: swapcost.Cold, Took: 8 * time.Second, At: time.Now()})
	book.Record(swapcost.Sample{Engine: "strata", Tier: swapcost.Warm, Took: 4 * time.Second, At: time.Now()})

	check(t, Rank(st, engines("comfyui", "acestep"), book), []got{
		{"comfyui", Free, "the card is free", swapcost.Cold, contract.Duration{Seconds: 8, Source: contract.Measured}},
		{"acestep", Free, "the card is free", swapcost.Cold, unknownCold},
	})

	st.Leases = []proto.Lease{
		{Engine: "strata", State: "parked"},
		{Engine: "dirk", Group: "llama-swap", State: "active"},
	}
	check(t, Rank(st, engines("comfyui", "strata"), book), []got{
		{"strata", Resume, "parked in RAM", swapcost.Warm, contract.Duration{Seconds: 4, Source: contract.Measured}},
		{"comfyui", Drain, "drains strata and llama-swap (dirk)", swapcost.Cold, contract.Duration{Seconds: 8, Source: contract.Measured}},
	})

	// The model that holds the card, and the router whose model holds it,
	// are already there: no drain of themselves, no switch.
	hot := contract.Duration{Source: contract.Default, Why: "already on the card, no switch"}
	check(t, Rank(st, engines("comfyui", "dirk", "llama-swap"), book), []got{
		{"dirk", Running, "already on the card", swapcost.Hot, hot},
		{"llama-swap", Running, "already on the card", swapcost.Hot, hot},
		{"comfyui", Drain, "drains strata and llama-swap (dirk)", swapcost.Cold, contract.Duration{Seconds: 8, Source: contract.Measured}},
	})
}

// The card is free only when nothing holds, uses or waits for it and it
// serves this machine.
func TestCardFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *proto.Status
		want bool
	}{
		{"no reading", nil, false},
		{"empty card", &proto.Status{}, true},
		{"a lease", &proto.Status{Leases: []proto.Lease{{Engine: "chat"}}}, false},
		{"a waiter", &proto.Status{Waiting: []proto.Lease{{Engine: "chat"}}}, false},
		{"a stowaway", &proto.Status{Squatters: []proto.Squatter{{PID: 7}}}, false},
		{"claim rules, none matching", &proto.Status{Claims: []proto.Claim{{Name: "steam-game"}, {Name: "blender"}}}, true},
		{"a tracked app running", &proto.Status{Claims: []proto.Claim{{Name: "steam-game"}, {Name: "blender", Active: true}}}, false},
		{"in a VM", &proto.Status{Passthrough: &proto.Passthrough{State: proto.CardPassthrough}}, false},
		{"probe failing", &proto.Status{ProbeErr: "nvidia-smi: exit 9"}, false},
		{"cordoned", &proto.Status{GPUs: []proto.GPU{{Scheduled: true, Cordoned: true}}}, false},
	} {
		if got := CardFree(tc.st); got != tc.want {
			t.Errorf("%s: CardFree = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The card is out of reach while it is in a VM, cordoned, or a pinned lease
// has it; an ordinary holder is drained the usual way.
func TestOutOfReach(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *proto.Status
		want string
	}{
		{"no reading", nil, ""},
		{"free card", &proto.Status{}, ""},
		{"an ordinary holder", &proto.Status{Leases: []proto.Lease{{Engine: "llama-swap"}}}, ""},
		{"a game", &proto.Status{Leases: []proto.Lease{{Engine: "claim:steam-game", Pinned: true}}}, "claim:steam-game holds the card"},
		{"in a VM", &proto.Status{Passthrough: &proto.Passthrough{State: proto.CardPassthrough}}, "the card is passed through to a VM"},
		{"cordoned", &proto.Status{GPUs: []proto.GPU{{Scheduled: true, Cordoned: true}}}, "the card is cordoned"},
	} {
		if got := OutOfReach(tc.st); got != tc.want {
			t.Errorf("%s: OutOfReach = %q, want %q", tc.name, got, tc.want)
		}
	}
}
