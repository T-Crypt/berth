// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/swapcost"
)

const mib = 1 << 20

func card(fps ...proto.EngineFootprint) *proto.Status {
	return &proto.Status{
		GPUs:       []proto.GPU{{Scheduled: true, Total: 24564 * mib, Baseline: 10 * mib}},
		Footprints: fps,
	}
}

// The VRAM a lease asks for comes out the way the broker sizes it, and says
// where it came from.
func TestBuildVRAM(t *testing.T) {
	cases := []struct {
		name string
		e    *config.Engine
		st   *proto.Status
		want Bytes
	}{
		{"needs_mib", &config.Engine{NeedsMiB: config.MiBs{14000}}, card(),
			Bytes{Bytes: 14000 * mib, Source: Declared, Why: "needs_mib"}},
		{"needs_mib per card", &config.Engine{NeedsMiB: config.MiBs{9000, 12000}}, card(),
			Bytes{Bytes: 12000 * mib, Source: Declared, Why: "needs_mib, the largest of 2 cards"}},
		{"learned", &config.Engine{LearnSize: true}, card(proto.EngineFootprint{Engine: "e", HotPeakBytes: 10000 * mib}),
			Bytes{Bytes: 10512 * mib, Source: Measured, Why: "learn_size: measured peak 10000 MiB + 512 MiB"}},
		{"learned peak fills the card", &config.Engine{LearnSize: true}, card(proto.EngineFootprint{Engine: "e", HotPeakBytes: 24100 * mib}),
			Bytes{Source: Measured, Why: "learn_size, its measured peak fills the card, so the whole card"}},
		{"learn_size never measured", &config.Engine{LearnSize: true}, card(),
			Bytes{Source: Default, Why: "learn_size, not measured yet, so the whole card"}},
		{"learn_size without berthd", &config.Engine{LearnSize: true}, nil,
			Bytes{Source: Unknown, Why: "learn_size, but berthd did not answer"}},
		{"nothing declared", &config.Engine{}, card(proto.EngineFootprint{Engine: "e", HotPeakBytes: 10000 * mib}),
			Bytes{Source: Default, Why: "the whole card"}},
		{"not in berth.toml", nil, card(),
			Bytes{Source: Default, Why: "the whole card"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Build("e", tc.e, tc.st, nil).VRAM; got != tc.want {
				t.Fatalf("VRAM = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Measured figures come from the footprint, declared ones win where
// berth.toml names them, and without berthd every measured figure is unknown.
func TestBuildMeasuredAndDeclared(t *testing.T) {
	fp := proto.EngineFootprint{Engine: "e", Bytes: 13000 * mib, Runs: 4, RAMPeakBytes: 9000 * mib, ParkedRAMBytes: 20000 * mib}
	e := &config.Engine{Type: "llamacpp", Priority: "foreground", MinTurn: config.Duration{Duration: 5 * time.Minute}}

	c := Build("e", e, card(fp), nil)
	if c.Type != "llamacpp" || !c.InConfig || c.Priority != (Text{"foreground", Declared}) {
		t.Fatalf("type %q in_config %v priority %+v", c.Type, c.InConfig, c.Priority)
	}
	if c.LastPeak != (Bytes{Bytes: 13000 * mib, Source: Measured, Why: "4 runs measured"}) {
		t.Fatalf("LastPeak = %+v", c.LastPeak)
	}
	if c.RAM != (Bytes{Bytes: 9000 * mib, Source: Measured, Why: "the most a run held while active"}) {
		t.Fatalf("RAM = %+v", c.RAM)
	}
	if c.ParkedRAM != (Bytes{Bytes: 20000 * mib, Source: Measured}) {
		t.Fatalf("ParkedRAM = %+v", c.ParkedRAM)
	}
	if c.MinTurn != (Duration{Seconds: 300, Source: Declared}) || c.Window != (Text{"always", Default}) {
		t.Fatalf("MinTurn %+v Window %+v", c.MinTurn, c.Window)
	}

	e.NeedsRAMMiB = 16000
	if got := Build("e", e, card(fp), nil).RAM; got != (Bytes{Bytes: 16000 * mib, Source: Declared, Why: "needs_ram_mib"}) {
		t.Fatalf("declared RAM = %+v", got)
	}

	off := Build("e", nil, nil, nil)
	if off.LastPeak != (Bytes{Source: Unknown, Why: "berthd did not answer"}) || off.Priority != (Text{"interactive", Default}) {
		t.Fatalf("without berthd: LastPeak %+v Priority %+v", off.LastPeak, off.Priority)
	}
}

// A switch time is the book's median for the tier, hot is free by rule, and
// a tier with no samples is unknown.
func TestBuildSwitch(t *testing.T) {
	book, err := swapcost.NewBook(filepath.Join(t.TempDir(), "swapcost.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []time.Duration{6 * time.Second, 8 * time.Second, 7 * time.Second} {
		book.Record(swapcost.Sample{Engine: "e", Tier: swapcost.Cold, Took: s, At: time.Now()})
	}
	want := []Switch{
		{Tier: swapcost.Hot, Took: Duration{Source: Default, Why: "already on the card, no switch"}},
		{Tier: swapcost.Warm, Took: Duration{Source: Unknown, Why: "no switch measured"}},
		{Tier: swapcost.Cool, Took: Duration{Source: Unknown, Why: "no switch measured"}},
		{Tier: swapcost.Cold, Took: Duration{Seconds: 7, Source: Measured}, Samples: 3},
	}
	got := Build("e", nil, nil, book).Switch
	if len(got) != len(want) {
		t.Fatalf("switch = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("switch[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
