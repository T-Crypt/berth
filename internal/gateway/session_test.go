// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"slices"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/runner"
)

func TestRunArgvCarriesSessionPolicy(t *testing.T) {
	e := config.Engine{Name: "fixture", ExitComfyUIIdle: true, ExitComfyUIIdleAfter: config.Duration{Duration: time.Minute}}
	o := runner.OptionsFor(&e, runner.Over{FailClosed: true})
	args := runArgv(o)
	if !slices.Contains(args, "--exit-comfyui-idle") || !slices.Contains(args, "--fail-closed") {
		t.Fatalf("lost session policy: %q", args)
	}
	i := slices.Index(args, "--exit-comfyui-idle-after")
	if i < 0 || i+1 >= len(args) || args[i+1] != "1m0s" {
		t.Fatalf("lost idle deadline: %q", args)
	}
}

// A gateway start of an entry with gpu = runs on that card.
func TestRunArgvCarriesTheCard(t *testing.T) {
	args := runArgv(runner.OptionsFor(&config.Engine{Name: "maya", GPU: "gpu1"}, runner.Over{}))
	i := slices.Index(args, "--gpu")
	if i < 0 || i+1 >= len(args) || args[i+1] != "gpu1" {
		t.Fatalf("lost the card: %q", args)
	}
	if bare := runArgv(runner.OptionsFor(&config.Engine{Name: "plain"}, runner.Over{})); slices.Contains(bare, "--gpu") {
		t.Fatalf("an entry with no card gained one: %q", bare)
	}
}

// A detached gateway launch carries the entry's slice: the engine scope
// the run creates takes it, so the slice's policy covers the engine.
func TestRunArgvCarriesSlice(t *testing.T) {
	e := config.Engine{Name: "fixture", Slice: "strata.slice"}
	o := runner.OptionsFor(&e, runner.Over{})
	args := runArgv(o)
	i := slices.Index(args, "--slice")
	if i < 0 || i+1 >= len(args) || args[i+1] != "strata.slice" {
		t.Fatalf("lost slice: %q", args)
	}
	bare := runArgv(runner.OptionsFor(&config.Engine{Name: "plain"}, runner.Over{}))
	if slices.Contains(bare, "--slice") {
		t.Fatalf("an entry with no slice gained one: %q", bare)
	}
}
