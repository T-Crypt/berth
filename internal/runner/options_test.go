// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"reflect"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/proto"
)

// OptionsFor carries the engine entry's settings, fills the defaults it
// leaves open, and defers every zero override to the entry.
func TestOptionsFor(t *testing.T) {
	e := &config.Engine{Name: "strata", Type: "strata", Priority: "foreground", NeedsMiB: config.MiBs{8192},
		Start: []string{"python", "serve.py"}, DrainPost: "http://127.0.0.1:1/unload",
		DrainSlots: "http://127.0.0.1:2", DrainComfyUI: "http://127.0.0.1:3",
		DrainMax: config.Duration{Duration: time.Minute}, MinTurn: config.Duration{Duration: 5 * time.Minute},
		ParkTTL: config.Duration{Duration: 10 * time.Minute}, Window: proto.Window{Start: 23 * 60, End: 7 * 60},
		WholeMachine: true}

	o := OptionsFor(e, Over{Socket: "s", Group: "strata", Wait: 10 * time.Second, Argv: []string{"true"}})
	want := Options{
		Socket: "s", Engine: "strata", Group: "strata", Priority: "foreground", Needs: []uint64{8192 << 20},
		Wait: 10 * time.Second, Grace: 30 * time.Second, DrainPost: e.DrainPost, DrainSlots: e.DrainSlots,
		DrainComfyUI: e.DrainComfyUI, DrainMax: e.DrainMax.Duration, MinTurn: 5 * time.Minute, ParkTTL: 10 * time.Minute, Argv: []string{"true"},
		WholeMachine: true, Window: e.Window,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("entry settings = %+v, want %+v", o, want)
	}

	o = OptionsFor(&config.Engine{Name: "e"}, Over{Socket: "s"})
	if o.Priority != "interactive" || o.Grace != 30*time.Second || len(o.Argv) != 0 {
		t.Errorf("bare entry = %+v, want the shared defaults", o)
	}

	// A zero override defers to the entry, an unset one to the default.
	o = OptionsFor(&config.Engine{Name: "e", Start: []string{"x"}}, Over{Socket: "s", Grace: 5 * time.Second, Argv: nil})
	if o.Grace != 5*time.Second || o.Argv[0] != "x" {
		t.Errorf("overrides = %+v", o)
	}

	// park = hook with URLs: the hook's adapter over the engine's base.
	hook := &config.Engine{Name: "mine", Type: "custom", URL: "http://127.0.0.1:9000",
		Park: "hook", ParkURL: "http://127.0.0.1:9000/offload", ResumeURL: "http://127.0.0.1:9000/reload"}
	o = OptionsFor(hook, Over{Socket: "s"})
	if o.Park == nil || o.Hook || o.ParkBase != hook.URL || o.ParkProbe != "/v1/models" {
		t.Errorf("hook options = %+v", o)
	}

	// keep_comfyui_up is the park alias: the ComfyUI adapter over the
	// drain URL, and the device stats as the readiness probe.
	keep := &config.Engine{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:8188",
		DrainComfyUI: "http://127.0.0.1:8188", KeepComfyUI: true}
	o = OptionsFor(keep, Over{Socket: "s"})
	if o.Park == nil || o.Park.Park == nil || o.ParkBase != keep.DrainComfyUI || o.ParkProbe != "/system_stats" {
		t.Errorf("keep-up alias options = %+v", o)
	}

	// A hook that names no URLs probes Berth's park protocol.
	bare := &config.Engine{Name: "v", Type: "llamacpp", URL: "http://127.0.0.1:8080", Park: "hook"}
	o = OptionsFor(bare, Over{Socket: "s"})
	if o.Park != nil || !o.Hook || o.ParkBase != bare.URL {
		t.Errorf("bare hook options = %+v", o)
	}
}

// A run of a parked-capable engine holds the adapter in its options;
// nothing in the entry may drop it.
func TestParkAdapterForBuiltIn(t *testing.T) {
	for _, kind := range park.BuiltInKinds() {
		a, ok := park.For(kind, "", "")
		if !ok || a.Park == nil || a.Resume == nil {
			t.Errorf("the %s adapter is unusable: %+v %v", kind, a, ok)
		}
	}
}
