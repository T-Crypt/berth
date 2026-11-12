// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// runFootprint runs engine once under the broker: it takes the card, holds
// proc 101 (a child of the attached 100) with the given VRAM while the
// driver sees it, then gives the berth back and the card shows it gone.
func runFootprint(t *testing.T, b *Broker, c *card, engine string, used uint64) {
	t.Helper()
	l := must(t, acquire(b, Request{Engine: engine, Priority: Foreground}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "llama-server", Used: used})
	b.Tick(context.Background())
	l.Release()
	c.set()
	b.Tick(context.Background())
}

// A freed lease folds its peak into the engine's footprint: Peak holds the
// most memory seen, Last the most recent run's peak. A lease that never
// held memory is not a run.
func TestFootprintFoldsOnFree(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100})
	runFootprint(t, b, c, "llama", 12*GiB)
	runFootprint(t, b, c, "llama", 8*GiB)

	last, ok := b.Footprint("llama")
	if !ok || last != 8*GiB {
		t.Fatalf("Footprint(llama) = %d, %v; want %d, true", last, ok, 8*GiB)
	}
	b.mu.Lock()
	f := b.footprints["llama"]
	peak, runs, at := uint64(0), 0, time.Time{}
	if f != nil {
		peak, runs, at = f.Peak, f.Runs, f.At
	}
	b.mu.Unlock()
	if peak != 12*GiB || runs != 2 || at.IsZero() {
		t.Fatalf("footprint = peak %d over %d runs at %v, want %d over 2 with a time", peak, runs, at, 12*GiB)
	}

	// A run that held no memory measures nothing and records no run.
	l := must(t, acquire(b, Request{Engine: "empty", Priority: Foreground}))
	l.Attach(100)
	b.Tick(context.Background())
	l.Release()
	b.Tick(context.Background())
	if _, ok := b.Footprint("empty"); ok {
		t.Fatal("a lease that held no memory was counted as a run")
	}
}

// The table survives a restart through the footprint file, and a missing
// file is an empty table, not an error.
func TestFootprintPersistsAcrossRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "footprint.json")
	b, c, _ := setup(t, procs.Map{101: 100})
	b.cfg.FootprintFile = file
	runFootprint(t, b, c, "llama", 12*GiB)
	runFootprint(t, b, c, "llama", 8*GiB)
	b.persistFootprints()

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "llama") {
		t.Fatalf("file %q does not name the engine", string(data))
	}

	b2 := New(Config{FootprintFile: file, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &card{total: 24 * GiB}, procs.Map{})
	last, ok := b2.Footprint("llama")
	if !ok || last != 8*GiB {
		t.Fatalf("restored Footprint(llama) = %d, %v; want %d, true", last, ok, 8*GiB)
	}
	b2.mu.Lock()
	if f := b2.footprints["llama"]; f == nil || f.Peak != 12*GiB || f.Runs != 2 {
		t.Fatalf("restored footprint = %+v, want peak %d over 2 runs", f, 12*GiB)
	}
	b2.mu.Unlock()

	if _, err := loadFootprints(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a missing file was not an empty table")
	}
}

// A corrupt file is logged and ignored: the broker starts with an empty
// table instead of failing.
func TestFootprintIgnoresACorruptFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "footprint.json")
	if err := os.WriteFile(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := New(Config{FootprintFile: file, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &card{total: 24 * GiB}, procs.Map{})
	b.Tick(context.Background())
	if _, ok := b.Footprint("llama"); ok {
		t.Fatal("the corrupt file was read as a footprint")
	}
	if n := len(b.Status().Leases); n != 0 {
		t.Fatalf("leases = %d, want 0", n)
	}
}

// A LearnSize lease takes the engine's measured peak plus the load headroom,
// not the whole card, so a sized engine is granted beside it and the holder
// is never drained. An engine never measured still takes the whole card.
func TestLearnSizeSharesTheCard(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100})
	runFootprint(t, b, c, "llama", 12*GiB)

	llama := granted(t, b, "llama", acquire(b, Request{Engine: "llama", Priority: Foreground, LearnSize: true}))
	llama.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "llama-server", Used: 12 * GiB})
	b.Tick(context.Background())
	if d := grantDetail(t, b, "llama"); !strings.Contains(d, "sized from its measured peak 12.0 GiB") {
		t.Fatalf("grant detail = %q, want the learned size named", d)
	}

	granted(t, b, "whisper", acquire(b, Request{Engine: "whisper", Priority: Foreground, Needs: 8 * GiB}))
	select {
	case why := <-llama.Drains():
		t.Fatalf("llama was drained for a waiter that fits beside its learned size: %s", why)
	default:
	}

	fresh := acquire(b, Request{Engine: "fresh", Priority: Batch, LearnSize: true})
	pending(t, fresh)
	if w := lastWaiting(b); w == "" {
		t.Fatal("an unmeasured LearnSize engine was not left waiting for the whole card")
	}
}

// A run that holds far more than the learned footprint warns once, once
// the engine has three runs to learn from.
func TestFootprintWarnsOnceOnADrift(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100})
	for i := 0; i < 3; i++ {
		runFootprint(t, b, c, "llama", 10*GiB)
	}
	runFootprint(t, b, c, "llama", 18*GiB)

	ev, _, _ := b.Events(0, false)
	var warns []proto.Event
	for _, e := range ev {
		if e.Kind == proto.EvFootprint {
			warns = append(warns, e)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("footprint warnings = %d, want 1: %+v", len(warns), warns)
	}
	for _, want := range []string{"llama uses 18.0 GiB", "its last runs used about 10.0 GiB"} {
		if !strings.Contains(warns[0].Detail, want) {
			t.Errorf("detail %q lacks %q", warns[0].Detail, want)
		}
	}
}

// With fewer than three learned runs the broker has nothing to compare
// against and stays quiet.
func TestFootprintStaysQuietBeforeThreeRuns(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100})
	runFootprint(t, b, c, "llama", 10*GiB)
	runFootprint(t, b, c, "llama", 10*GiB)
	runFootprint(t, b, c, "llama", 18*GiB)

	ev, _, _ := b.Events(0, false)
	for _, e := range ev {
		if e.Kind == proto.EvFootprint {
			t.Fatalf("warned with only 2 learned runs: %+v", e)
		}
	}
}

// The fold happens once, at the free: a release that is still holding
// memory records nothing until the card shows the memory gone.
func TestFootprintFoldsAtTheFreeNotTheRelease(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100})
	l := must(t, acquire(b, Request{Engine: "llama", Priority: Foreground}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "llama-server", Used: 9 * GiB})
	b.Tick(context.Background())
	l.Release()

	if _, ok := b.Footprint("llama"); ok {
		t.Fatal("the release was folded before the driver showed the memory free")
	}
	c.set()
	b.Tick(context.Background())
	last, ok := b.Footprint("llama")
	if !ok || last != 9*GiB {
		t.Fatalf("Footprint(llama) = %d, %v after the free; want %d, true", last, ok, 9*GiB)
	}
}
