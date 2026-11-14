// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package contract is one engine's terms as a single view: what berth.toml
// declares for it, joined with what Berth measured, every number tagged with
// where it came from. It is read only and keeps no file of its own, so it
// cannot drift from the config it reads.
package contract

import (
	"fmt"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/swapcost"
)

// Source is where a number came from.
type Source string

const (
	Declared Source = "declared" // berth.toml says so
	Default  Source = "default"  // berth.toml is silent; Berth's default applies
	Measured Source = "measured" // Berth saw it on the card, in a switch or on the last run's lease
	Unknown  Source = "unknown"  // nothing declares it and nothing measured it
)

// besideHeadroom is the broker's margin on a learned size (broker.besideHeadroom).
const besideHeadroom = 512 << 20

// Bytes is a size and its source. Bytes is 0 when the source is unknown, or
// when the size is the whole card.
type Bytes struct {
	Bytes  uint64 `json:"bytes,omitempty" yaml:"bytes,omitempty"`
	Source Source `json:"source" yaml:"source"`
	Why    string `json:"why,omitempty" yaml:"why,omitempty"`
}

// Duration is a time and its source.
type Duration struct {
	Seconds float64 `json:"seconds" yaml:"seconds"`
	Source  Source  `json:"source" yaml:"source"`
	Why     string  `json:"why,omitempty" yaml:"why,omitempty"`
}

// Text is a setting and its source.
type Text struct {
	Value  string `json:"value" yaml:"value"`
	Source Source `json:"source" yaml:"source"`
	Why    string `json:"why,omitempty" yaml:"why,omitempty"`
}

// Switch is how long a switch to the engine takes from one tier: the median of
// the gateway's measured switches, or unknown with no samples.
type Switch struct {
	Tier    swapcost.Tier `json:"tier" yaml:"tier"`
	Took    Duration      `json:"took" yaml:"took"`
	Samples int           `json:"samples,omitempty" yaml:"samples,omitempty"`
}

// Contract is one engine's terms.
type Contract struct {
	Engine string `json:"engine" yaml:"engine"`
	// Type is the engine type from berth.toml or discovery, "" for a name
	// Berth knows only from its leases (a llama-swap model).
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`
	InConfig bool   `json:"in_config" yaml:"in_config"`
	Priority Text   `json:"priority" yaml:"priority"`
	// VRAM is what a lease asks for: needs_mib, the learned size, or the
	// whole card.
	VRAM Bytes `json:"vram" yaml:"vram"`
	// LastPeak is the most VRAM the last run held, what a next run most
	// likely needs. A reading, never a reservation.
	LastPeak Bytes `json:"last_peak" yaml:"last_peak"`
	// RAM is the host RAM a run needs: needs_ram_mib, else the most a run
	// held while active.
	RAM Bytes `json:"ram" yaml:"ram"`
	// ParkedRAM is the host RAM the engine last held parked.
	ParkedRAM Bytes    `json:"parked_ram" yaml:"parked_ram"`
	MinTurn   Duration `json:"min_turn" yaml:"min_turn"`
	Window    Text     `json:"window" yaml:"window"`
	Switch    []Switch `json:"switch" yaml:"switch"`
}

// Tiers is the switch tiers in the order a contract lists them, fastest first.
var Tiers = []swapcost.Tier{swapcost.Hot, swapcost.Warm, swapcost.Cool, swapcost.Cold}

// Build joins an engine's berth.toml entry (nil when it has none) with what
// st measured for it and the switch book (nil when there is none). st is nil
// when berthd did not answer; every measured number is then unknown.
func Build(name string, e *config.Engine, st *proto.Status, book *swapcost.Book) Contract {
	c := Contract{Engine: name, InConfig: e != nil}
	var fp *proto.EngineFootprint
	var card uint64
	if st != nil {
		for i := range st.Footprints {
			if st.Footprints[i].Engine == name {
				fp = &st.Footprints[i]
			}
		}
		for _, en := range st.Engines {
			if en.Name == name {
				c.Type = en.Type
			}
		}
		for _, g := range st.GPUs {
			if g.Scheduled {
				card = g.Total - min(g.Total, g.Baseline)
				break
			}
		}
	}
	if e != nil && e.Type != "" {
		c.Type = e.Type
	}

	// A llama-swap model has no berth.toml entry: its terms are the flags on
	// its berth run line, which the last run's lease carried.
	ran := fp != nil && fp.Priority != ""
	c.Priority = Text{Value: "interactive", Source: Default}
	switch {
	case e != nil && e.Priority != "":
		c.Priority = Text{Value: e.Priority, Source: Declared}
	case ran:
		c.Priority = Text{Value: fp.Priority, Source: Measured, Why: "its last run's lease"}
	}

	c.VRAM = vram(e, fp, card, st != nil)

	c.LastPeak = measured(fp, func(f *proto.EngineFootprint) uint64 { return f.Bytes }, st != nil)
	switch {
	case fp != nil && fp.Bytes > 0:
		c.LastPeak.Why = fmt.Sprintf("%d runs measured", fp.Runs)
	case fp != nil && fp.HotPeakBytes > 0:
		// A model of a group: its last-run figure sits on the group's entry.
		c.LastPeak = Bytes{Bytes: fp.HotPeakBytes, Source: Measured, Why: "the most a run under this name held"}
	}

	c.RAM = measured(fp, func(f *proto.EngineFootprint) uint64 { return f.RAMPeakBytes }, st != nil)
	if c.RAM.Source == Measured {
		c.RAM.Why = "the most a run held while active"
	}
	if e != nil && e.NeedsRAMMiB > 0 {
		c.RAM = Bytes{Bytes: e.NeedsRAMMiB << 20, Source: Declared, Why: "needs_ram_mib"}
	}
	c.ParkedRAM = measured(fp, func(f *proto.EngineFootprint) uint64 { return f.ParkedRAMBytes }, st != nil)

	c.MinTurn = Duration{Source: Default, Why: "off"}
	switch {
	case e != nil && e.MinTurn.Duration > 0:
		c.MinTurn = Duration{Seconds: e.MinTurn.Seconds(), Source: Declared}
	case ran:
		c.MinTurn = Duration{Seconds: fp.MinTurnS, Source: Measured, Why: "its last run's lease"}
	}
	c.Window = Text{Value: "always", Source: Default}
	if e != nil && e.Window.Set() {
		c.Window = Text{Value: e.Window.String(), Source: Declared}
	}

	for _, t := range Tiers {
		s := Switch{Tier: t, Took: Duration{Source: Unknown, Why: "no switch measured"}}
		if book != nil {
			if est := book.Estimate(name, t); est.Known && est.Samples == 0 {
				s.Took = Duration{Source: Default, Why: "already on the card, no switch"}
			} else if est.Known {
				s.Took = Duration{Seconds: est.Took.Round(100 * time.Millisecond).Seconds(), Source: Measured}
				s.Samples = est.Samples
			}
		}
		c.Switch = append(c.Switch, s)
	}
	return c
}

// vram is the size a lease of the engine asks for, the way the broker sizes
// it: needs_mib as declared, else the learned size under learn_size, else
// the whole card.
func vram(e *config.Engine, fp *proto.EngineFootprint, card uint64, read bool) Bytes {
	if e != nil {
		if b := e.NeedsMiB.Bytes(); len(b) > 0 {
			why := "needs_mib"
			if len(b) > 1 {
				why = fmt.Sprintf("needs_mib, the largest of %d cards", len(b))
			}
			return Bytes{Bytes: largest(b), Source: Declared, Why: why}
		}
		if e.LearnSize {
			switch {
			case !read:
				return Bytes{Source: Unknown, Why: "learn_size, but berthd did not answer"}
			case fp == nil || fp.HotPeakBytes == 0:
				return Bytes{Source: Default, Why: "learn_size, not measured yet, so the whole card"}
			case card > 0 && fp.HotPeakBytes+besideHeadroom >= card:
				return Bytes{Source: Measured, Why: "learn_size, its measured peak fills the card, so the whole card"}
			default:
				return Bytes{Bytes: fp.HotPeakBytes + besideHeadroom, Source: Measured,
					Why: fmt.Sprintf("learn_size: measured peak %d MiB + 512 MiB", fp.HotPeakBytes>>20)}
			}
		}
	}
	return Bytes{Source: Default, Why: "the whole card"}
}

// measured is one footprint figure, unknown when berthd did not answer or
// never measured it.
func measured(fp *proto.EngineFootprint, get func(*proto.EngineFootprint) uint64, read bool) Bytes {
	if !read {
		return Bytes{Source: Unknown, Why: "berthd did not answer"}
	}
	if fp == nil || get(fp) == 0 {
		return Bytes{Source: Unknown, Why: "not measured yet"}
	}
	return Bytes{Bytes: get(fp), Source: Measured}
}

func largest(b []uint64) uint64 {
	var m uint64
	for _, v := range b {
		m = max(m, v)
	}
	return m
}
