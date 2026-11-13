// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Footprints: the VRAM each engine's leases held at their peak, as the
// driver measured it, kept across restarts. They are shown as an estimate
// of what a run needs; Berth never treats them as a reservation.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/T-Crypt/berth/internal/fsx"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/proto"
)

// engineFootprint is one engine's record of its runs: the most memory seen
// across them, and the last run's peak, which a next run most likely needs.
type engineFootprint struct {
	Peak      uint64    // the most memory any run was measured holding
	Last      uint64    // the last run's peak
	Runs      int       // runs measured holding memory
	At        time.Time // when Last was recorded
	ParkedRAM uint64    // the RAM a parked run last held (its scope's anon + shmem); 0 = never measured
	ParkBase  uint64    // what that run held hot, just before it parked
	RAMPeak   uint64    // the most RAM any run held while active (its scope's anon + shmem); 0 = never measured
	// HotPeak is the most VRAM a run of this engine name held, kept on the
	// name's entry: a group's Peak is its largest model's, and one model of
	// a llama-swap may fit beside a parked engine while another does not.
	HotPeak uint64
}

// besideHeadroom is the free card memory a whole-card engine needs past its
// measured peak to load beside a parked engine. Measured on an RTX 4090: a
// 23710 MiB model served alone, and died loading with 398 MiB left beside a
// 456 MiB residual. The 2 s card read misses the load's last allocations.
const besideHeadroom = 512 << 20

// hotPeak is the most VRAM a run of engine has held, 0 until one was measured.
// b.mu must be held.
func (b *Broker) hotPeak(engine string) uint64 {
	if f := b.footprints[engine]; f != nil {
		return f.HotPeak
	}
	return 0
}

// learnSize is the size a request takes, and for a LearnSize request the
// grant's line saying where the size came from: the most VRAM a run of the
// engine has held plus besideHeadroom. A request with a size, for the whole
// machine or on an APU keeps what it asked. An engine never measured, or one
// whose peak leaves the card nothing to share, takes the whole card. b.mu
// must be held.
func (b *Broker) learnSize(r Request) (uint64, string) {
	if !r.LearnSize || r.Needs > 0 || r.WholeMachine || b.snap.APU {
		return r.Needs, ""
	}
	peak := b.hotPeak(r.Engine)
	if peak == 0 {
		return 0, "not measured yet, so the whole card"
	}
	if peak+besideHeadroom >= b.ceiling() {
		return 0, fmt.Sprintf("its measured peak %s fills the card", sizeHuman(peak))
	}
	return peak + besideHeadroom, fmt.Sprintf("sized from its measured peak %s", sizeHuman(peak))
}

// footprints is the table keyed by engine key: the lease's group, else its
// engine name. A park record (ParkedRAM, ParkBase) is keyed by the engine
// name alone, since the models of one group park different amounts.
type footprints map[string]*engineFootprint

// loadFootprints reads the table from its file. A missing file is an empty
// table; a corrupt one is reported by the caller and ignored.
func loadFootprints(path string) (footprints, error) {
	f := footprints{}
	data, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(data, &f)
}

// foldFootprint records the peak a freed lease measured, under the key it
// counted for. A lease that never held memory measured nothing and is not a
// run. Once the engine has a learned figure (three runs), a run that holds
// far more than it warns once, so a grown engine is seen instead of
// silently mis-sized. b.mu must be held.
func (b *Broker) foldFootprint(l *lease, now time.Time) {
	if l.peak == 0 {
		return
	}
	k := l.key()
	f := b.footprints[k]
	if f == nil {
		f = &engineFootprint{}
		b.footprints[k] = f
	}
	if f.Last > 0 && f.Runs >= 3 && 2*l.peak > 3*f.Last {
		// The estimate is kept per key, so the key names the engine in the
		// message: for a grouped engine (llama-swap) that is the family.
		msg := fmt.Sprintf("%s uses %s, its last runs used about %s", k, sizeHuman(l.peak), sizeHuman(f.Last))
		b.log.Warn("a run's VRAM is far from the engine's learned footprint", "lease", l.id, "engine", l.engine, "detail", msg)
		b.emit(proto.Event{Kind: proto.EvFootprint, Lease: l.id, Engine: l.engine, Bytes: l.peak, Detail: msg})
	}
	if l.peak > f.Peak {
		f.Peak = l.peak
	}
	f.Last = l.peak
	f.Runs++
	f.At = now
	n := b.footprints[l.engine]
	if n == nil {
		n = &engineFootprint{}
		b.footprints[l.engine] = n
	}
	n.HotPeak = max(n.HotPeak, l.peak)
	b.footprintDirty = true
}

// sizeHuman is a byte count the way a person reads it: GiB with one decimal
// at and above a GiB, whole MiB below.
func sizeHuman(n uint64) string {
	if n >= 1024*gpu.MiB {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1024*gpu.MiB))
	}
	return fmt.Sprintf("%d MiB", n/gpu.MiB)
}

// footprintStatus is the table as the status reply carries it: per engine
// key, the last run's peak and the peaks learn_size and RAM figures read,
// sorted by name. A llama-swap model's entry may carry only its own peaks;
// the last-run figure sits on its group's. b.mu must be held.
func (b *Broker) footprintStatus() []proto.EngineFootprint {
	out := []proto.EngineFootprint{}
	for k, f := range b.footprints {
		if f == nil || f.Last == 0 && f.HotPeak == 0 && f.RAMPeak == 0 && f.ParkedRAM == 0 {
			continue
		}
		out = append(out, proto.EngineFootprint{Engine: k, Bytes: f.Last, Runs: f.Runs, At: f.At,
			HotPeakBytes: f.HotPeak, RAMPeakBytes: f.RAMPeak, ParkedRAMBytes: f.ParkedRAM})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Engine < out[j].Engine })
	return out
}

// Footprint reports the VRAM a run of the engine last used: the most recent
// measured peak, what a next run most likely needs. ok is false until the
// broker has measured the engine. The engine is named by its key: the
// lease's group, else its engine name.
func (b *Broker) Footprint(engine string) (bytes uint64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if f := b.footprints[engine]; f != nil && f.Last > 0 {
		return f.Last, true
	}
	return 0, false
}

// persistFootprints writes the table when a fold changed it since the last
// write, the way persist does for the board: a temp file renamed over the
// old one, outside b.mu.
func (b *Broker) persistFootprints() {
	if b.cfg.FootprintFile == "" {
		return
	}
	b.footSaveMu.Lock()
	defer b.footSaveMu.Unlock()
	b.mu.Lock()
	if !b.footprintDirty {
		b.mu.Unlock()
		return
	}
	data, err := json.Marshal(b.footprints)
	b.footprintDirty = false
	b.mu.Unlock()
	if err == nil {
		err = fsx.WriteFile(b.cfg.FootprintFile, data, 0o600)
	}
	if err != nil {
		if !b.footSaveFailed {
			b.log.Warn("engine footprints not saved; the estimates will not survive a restart", "file", b.cfg.FootprintFile, "err", err)
			b.footSaveFailed = true
		}
		return
	}
	b.footSaveFailed = false
}
