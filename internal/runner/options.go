// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"io"
	"log/slog"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/lend"
	"github.com/T-Crypt/berth/internal/park"
)

// Over is what the caller of OptionsFor supplies itself: the values a
// flag carries on `berth run`, and the writers and windows an internal
// caller owns. The zero value defers to the engine's entry.
type Over struct {
	Socket string
	// Front is berth run --front: the engine's door, for park =
	// "checkpoint". It is a run's own address, so no entry carries it.
	Front      string
	Group      string
	Pinned     bool
	Switched   bool
	Wait       time.Duration
	Grace      time.Duration
	FailClosed bool
	Argv       []string
	Stderr     io.Writer
	Output     io.Writer
	Log        *slog.Logger
}

// OptionsFor is the options one run of a configured engine takes: every
// setting the engine's berth.toml entry carries, with the caller's
// overrides where a flag or the caller owns the value. The defaults the
// entry leaves open are the shared ones: interactive priority, a 30 s
// drain grace, the engine's own start command.
func OptionsFor(e *config.Engine, o Over) Options {
	prio := e.Priority
	switch {
	case prio == "" && e.Background:
		prio = "batch"
	case prio == "":
		prio = "interactive"
	}
	grace := o.Grace
	if grace == 0 {
		grace = 30 * time.Second
	}
	argv := o.Argv
	if argv == nil {
		argv = e.Start
	}
	probe := "/v1/models"
	if e.Type == "comfyui" {
		probe = "/system_stats"
	}
	opts := Options{
		Socket: o.Socket, Engine: e.Name, Card: e.GPU, Group: o.Group, Priority: prio,
		Needs: e.NeedsMiB << 20, Pinned: o.Pinned, Switched: o.Switched, Wait: o.Wait,
		DrainPost: e.DrainPost, DrainSlots: e.DrainSlots, DrainComfyUI: e.DrainComfyUI,
		DrainMax: e.DrainMax.Duration, MinTurn: e.MinTurn.Duration, ParkTTL: e.ParkTTL.Duration, Container: e.Container, Compose: e.Compose,
		APIKeyEnv: e.APIKeyEnv, APIKeyFile: e.APIKeyFile,
		ExitComfyUIIdle: e.ExitComfyUIIdle, ExitComfyUIIdleAfter: e.ExitComfyUIIdleAfter.Duration,
		Grace: grace, FailClosed: o.FailClosed,
		Slice: e.Slice,
		Argv:  argv, Stderr: o.Stderr, Output: o.Output, Log: o.Log,
	}
	// kv_snapshots: the drain's snapshot step. The slot calls go to the
	// entry's drain_slots base when it names one, else the drain_post
	// origin, else the engine URL; the budget takes the default when the
	// entry left it open, and the save guard keeps the library floor.
	if e.KVSnapshots != "" {
		mib := e.KVSnapshotsMiB
		if mib == 0 {
			mib = config.DefaultKVSnapshotsMiB
		}
		base := e.DrainSlots
		if base == "" {
			base = drainPostBase(e.DrainPost)
		}
		if base == "" {
			base = e.URL
		}
		opts.KVDir, opts.KVBudget, opts.KVBase, opts.KVFloor = e.KVSnapshots, int64(mib<<20), base, config.DefaultFreeFloor
	}
	// park = "hook" hands the drain the engine's own offload calls; when
	// the entry carries no URLs either, the drain probes Berth's park
	// protocol on the engine's URL instead. keep_comfyui_up is the same
	// alias for a ComfyUI: its unload stays the park, with the base the
	// drain URL names.
	if e.Park == "hook" || e.KeepComfyUI {
		adapter, ok := park.For(e.Type, e.ParkURL, e.ResumeURL)
		base := e.URL
		if e.Park != "hook" {
			base = e.DrainComfyUI
		}
		if ok {
			opts.Park = &adapter
		} else {
			opts.Hook = true
		}
		opts.ParkBase, opts.ParkProbe = base, probe
	}
	// park = "checkpoint" moves the engine's device memory into its RAM
	// with cuda-checkpoint. The front picks the engine's base; the probe
	// says when a restore serves again.
	if e.Park == "checkpoint" {
		opts.Checkpoint, opts.Front, opts.ParkProbe = true, o.Front, probe
	}
	// lend = "hook" gives the run its engine's lend calls. berthd asks for a
	// lend only from a run whose entry has them: the protocol base is
	// lend_url, and a kind with built-in calls (strata) uses its own URL.
	opts.WholeMachine = e.WholeMachine
	if e.Lend == "hook" {
		if adapter, ok := lend.For(e.Type, e.URL, e.LendURL); ok {
			opts.Lend = &adapter
		}
	}
	return opts
}
