// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/suite"
	"github.com/T-Crypt/berth/internal/sysd"
)

func runRun(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	engine := fs.String("as", "", "engine name shown in status and drain messages (required)")
	group := fs.String("group", "", "engine family for juggles and the usage ranking, e.g. llama-swap for each of its models (default: --as)")
	prio := fs.String("priority", "interactive", "batch, interactive, foreground or hold")
	pinned := fs.Bool("pinned", false, "never drained: the card stays CMD's until it exits (an app like Blender, or a training run; not an engine a request should swap out)")
	switched := fs.Bool("switched", false, "a person's switch (the gateway sets it for the wake page's Launch and /v1/berth/switch): wins a juggle")
	needs := fs.Uint64("needs-mib", 0, "memory this engine needs; 0 = the whole card")
	wait := fs.Duration("wait", 0, "how long to wait for a berth; 0 = berthd's default; negative = no deadline")
	drainPost := fs.String("drain-post", "", "URL to POST when asked to drain, before signalling (e.g. llama-swap's unload)")
	drainSlots := fs.String("drain-slots", "", "engine base URL whose /slots must show idle before a drain unloads or stops it (Strata, or a llama-swap model's /upstream/<model>)")
	drainComfy := fs.String("drain-comfyui", "", "ComfyUI base URL: on drain, wait until its queue is empty before stopping it (never cuts a job off)")
	keepComfy := fs.Bool("keep-comfyui-up", false, "with --drain-comfyui: unload its models via POST /free and leave the server up instead of stopping it")
	drainMax := fs.Duration("drain-max", 0, "how long a drain may wait at a boundary (a busy slot, a running ComfyUI job) before it stops the engine, or refuses with --fail-closed; 0 = unbounded except fail-closed safety checks")
	minTurn := fs.Duration("min-turn", 0, "minimum turn: after the grant the card stays this run's for this long; no request drains it meanwhile, only a hold does (a game, a call, berth hold); 0 = off")
	keyFile := fs.String("api-key-file", "", "absolute owner-private Unix file holding the bearer token; exclusive with --api-key-env")
	idle := fs.Bool("exit-comfyui-idle", false, "release a ComfyUI session after its first completed job; with it set, --drain-max no longer bounds the wait: a session with no completed job holds the card")
	after := fs.Duration("exit-comfyui-idle-after", 0, "with --exit-comfyui-idle: release an unused session after this interval; 0 = no deadline")
	keyEnv := fs.String("api-key-env", "", "environment variable holding the bearer token for engine drain requests")
	parkMode := fs.String("park", "", "off, hook or checkpoint (cuda-checkpoint moves the engine's VRAM into RAM on a drain; needs --front), over the entry's park")
	frontAddr := fs.String("front", "", "loopback address the engine's requests come to (llama-swap's ${PORT}); the engine takes a free port at "+runner.PortPlaceholder+" in its command, and requests wait here while it sleeps")
	container := fs.String("container", "", "NAME of the one container CMD runs in: the drain stops it via docker/podman stop instead of signalling the wrapper")
	card := fs.String("gpu", "", "card to run on: its name on the node (gpu1) or its PCI address, or a comma list of them for one lease on several cards; the lease goes there and the engine gets CUDA_VISIBLE_DEVICES set to the cards' UUIDs unless its environment sets it; over the entry's gpu; default the node's first card")
	composeFile := fs.String("compose", "", "absolute PATH of the Compose file CMD's project runs from: the lease covers every container of the project and the drain stops it with compose stop, never down; exclusive with --container")
	sliceName := fs.String("slice", "", "NAME.slice to start the engine's scope under (systemd user manager, Linux): the slice unit's CPU, memory and swap policy applies to the engine and everything it forks; over the entry's slice")
	grace := fs.Duration("grace", 30*time.Second, "time each drain step gets before the next")
	failClosed := fs.Bool("fail-closed", false, "do not start CMD without berthd or stop it without a safe boundary")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml: the engine entry matching --as (or --group) supplies the settings a flag does not set; a set flag wins and says so; missing = no defaults ($BERTH_CONFIG)")
	if code, done := c.Parse(fs); done {
		return code
	}
	required := Systemf("--as NAME and the engine's command are required").
		WithHint("usage: berth run --as NAME [flags] -- CMD [ARGS...], or give NAME's berth.toml entry a start")
	if *engine == "" {
		return c.fail(required)
	}
	// The engine's berth.toml entry is the default for every setting a flag
	// does not set, so a run with no flags behaves like a gateway start. A
	// flag the operator set wins and says so: the file and the flags can
	// never silently disagree.
	merged, _, err := suite.Merged(*cfgPath)
	if err != nil {
		return c.fail(Failf("config invalid: %s", *cfgPath).Detail("%v", err))
	}
	e := config.Engine{Name: *engine}
	if entry := engineEntry(merged, *engine, *group); entry != nil {
		e = *entry
	}
	// With no command, the entry's own start runs, as the gateway starts it.
	// A start found through --group is that group's engine, not this one.
	argv := c.Args
	if len(argv) == 0 && e.Name == *engine {
		argv = e.Start
	}
	if len(argv) == 0 {
		return c.fail(required)
	}
	applyEngineEntry(c, &e, engineFlags{
		priority: prio, needsMiB: needs, drainPost: drainPost, drainSlots: drainSlots,
		drainComfyUI: drainComfy, keepComfyUI: keepComfy, drainMax: drainMax, minTurn: minTurn,
		container: container, compose: composeFile,
	})
	if c.Changed("api-key-env") {
		e.APIKeyEnv = *keyEnv
	}
	if c.Changed("api-key-file") {
		e.APIKeyFile = *keyFile
	}
	if c.Changed("slice") {
		if e.Slice != "" && e.Slice != *sliceName {
			fmt.Fprintf(c.Err, "berth: --slice overrides berth.toml slice = %s for %s\n", e.Slice, e.Name)
		}
		e.Slice = *sliceName
	}
	if e.Slice != "" {
		if err := sysd.CheckSlice(e.Slice); err != nil {
			return c.fail(Systemf("%v", err))
		}
	}
	if e.APIKeyEnv != "" && e.APIKeyFile != "" {
		return c.fail(Systemf("api_key_env and api_key_file are mutually exclusive"))
	}
	if c.Changed("exit-comfyui-idle") {
		e.ExitComfyUIIdle = *idle
	}
	if c.Changed("exit-comfyui-idle-after") {
		e.ExitComfyUIIdleAfter.Duration = *after
	}
	if e.ExitComfyUIIdle && (e.DrainComfyUI == "" || e.KeepComfyUI) {
		return c.fail(Systemf("exit_comfyui_idle needs drain_comfyui and cannot use keep_comfyui_up"))
	}
	if e.ExitComfyUIIdleAfter.Duration < 0 || (e.ExitComfyUIIdleAfter.Duration > 0 && !e.ExitComfyUIIdle) {
		return c.fail(Systemf("exit_comfyui_idle_after needs exit_comfyui_idle and must not be negative"))
	}
	if c.Changed("park") {
		e.Park = *parkMode
	}
	if c.Changed("gpu") {
		if e.GPU != "" && e.GPU != *card {
			fmt.Fprintf(c.Err, "berth: --gpu overrides berth.toml gpu = %s for %s\n", e.GPU, *engine)
		}
		e.GPU = *card
	}
	if e.GPU != "" {
		if err := gpu.CheckCard(e.GPU); err != nil {
			return c.fail(Systemf("%v", err))
		}
	}
	if err := checkFront(e.Park, *frontAddr, argv); err != nil {
		return c.fail(Systemf("%v", err))
	}
	e.Name = *engine
	if _, err := broker.ParsePriority(e.Priority); err != nil {
		return c.fail(Systemf("%v", err))
	}
	if err := checkRunFlags(runFlags{
		keepComfyUI: e.KeepComfyUI, drainComfyUI: e.DrainComfyUI, container: e.Container, compose: e.Compose,
	}); err != nil {
		return c.fail(Systemf("%v", err))
	}
	// Nothing here goes to stdout: the engine owns it. berth's own notices are
	// diagnostics on stderr, and --quiet never hides the unprotected warning.
	return runner.Run(runner.OptionsFor(&e, runner.Over{
		Socket: *sock, Front: *frontAddr, Group: *group, Pinned: *pinned, Switched: *switched, Wait: *wait, Grace: *grace,
		FailClosed: *failClosed, Argv: argv, Stderr: c.Err, Log: c.Log(),
	}))
}

// checkFront holds a run to rule 3 of docs/design.md: a checkpointed
// engine sleeps only behind a front, and a front needs the engine's port
// placeholder and a loopback address.
func checkFront(parkMode, frontAddr string, argv []string) error {
	switch parkMode {
	case "", "off", "hook", "checkpoint":
	default:
		return fmt.Errorf("--park %q is not off, hook or checkpoint", parkMode)
	}
	if parkMode == "checkpoint" && frontAddr == "" {
		return errors.New("--park checkpoint needs --front: a checkpointed engine may only sleep behind a door that holds its requests")
	}
	if frontAddr == "" {
		return nil
	}
	if parkMode != "checkpoint" {
		return errors.New("--front is for --park checkpoint")
	}
	if err := config.Loopback(frontAddr); err != nil {
		return fmt.Errorf("--front: %w", err)
	}
	if !slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, runner.PortPlaceholder) }) {
		return fmt.Errorf("--front needs %s in the engine's command, where its port goes", runner.PortPlaceholder)
	}
	return nil
}

// engineEntry is the [[engine]] block a run reads its defaults from: the block
// named by --as, or, for a run that names a model (llama-swap), the block
// named by --group.
func engineEntry(cfg config.Config, as, group string) *config.Engine {
	for i := range cfg.Engine {
		if cfg.Engine[i].Name == as {
			return &cfg.Engine[i]
		}
	}
	for i := range cfg.Engine {
		if group != "" && cfg.Engine[i].Name == group {
			return &cfg.Engine[i]
		}
	}
	return nil
}

// engineFlags are the `berth run` flags that fold onto an engine's entry.
type engineFlags struct {
	priority     *string
	needsMiB     *uint64
	drainPost    *string
	drainSlots   *string
	drainComfyUI *string
	keepComfyUI  *bool
	drainMax     *time.Duration
	minTurn      *time.Duration
	container    *string
	compose      *string
}

// applyEngineEntry folds the run's flags into the engine's entry copy e:
// a flag the operator set wins and says so, in the file's key and value;
// an unset flag defers to the entry.
func applyEngineEntry(c *Context, e *config.Engine, f engineFlags) {
	prio, needs, drainPost := f.priority, f.needsMiB, f.drainPost
	drainSlots, drainComfy, keepComfy := f.drainSlots, f.drainComfyUI, f.keepComfyUI
	drainMax, minTurn := f.drainMax, f.minTurn
	container, composeFile := f.container, f.compose
	notice := func(flag, key, toml string) {
		fmt.Fprintf(c.Err, "berth: --%s overrides berth.toml %s = %s for %s\n", flag, key, toml, e.Name)
	}
	if c.Changed("priority") {
		if e.Priority != "" && e.Priority != *prio {
			notice("priority", "priority", e.Priority)
		}
		e.Priority = *prio
	} else if e.Priority == "" {
		e.Priority = *prio
	}
	if c.Changed("needs-mib") {
		if e.NeedsMiB > 0 && e.NeedsMiB != *needs {
			notice("needs-mib", "needs_mib", strconv.FormatUint(e.NeedsMiB, 10))
		}
		e.NeedsMiB = *needs
	}
	drains := []struct {
		flag, key string
		toml      string
		got       *string
		set       func(string)
	}{
		{"drain-post", "drain_post", e.DrainPost, drainPost, func(s string) { e.DrainPost = s }},
		{"drain-slots", "drain_slots", e.DrainSlots, drainSlots, func(s string) { e.DrainSlots = s }},
		{"drain-comfyui", "drain_comfyui", e.DrainComfyUI, drainComfy, func(s string) { e.DrainComfyUI = s }},
		{"container", "container", e.Container, container, func(s string) { e.Container = s }},
		{"compose", "compose", e.Compose, composeFile, func(s string) { e.Compose = s }},
	}
	for _, d := range drains {
		if !c.Changed(d.flag) {
			if d.toml != "" {
				d.set(d.toml)
			}
			continue
		}
		if d.toml != "" && d.toml != *d.got {
			notice(d.flag, d.key, d.toml)
		}
		d.set(*d.got)
	}
	if c.Changed("keep-comfyui-up") {
		if !e.KeepComfyUI {
			notice("keep-comfyui-up", "keep_comfyui_up", "false")
		}
		e.KeepComfyUI = *keepComfy
	}
	if c.Changed("drain-max") {
		if e.DrainMax.Duration > 0 && e.DrainMax.Duration != *drainMax {
			notice("drain-max", "drain_max", e.DrainMax.String())
		}
		e.DrainMax.Duration = *drainMax
	}
	if c.Changed("min-turn") {
		if e.MinTurn.Duration > 0 && e.MinTurn.Duration != *minTurn {
			notice("min-turn", "min_turn", e.MinTurn.String())
		}
		e.MinTurn.Duration = *minTurn
	}
}

// runFlags are the `berth run` settings that cannot stand on their own or
// cannot stand together.
type runFlags struct {
	keepComfyUI  bool
	drainComfyUI string
	container    string
	compose      string
}

// checkRunFlags rejects flag combinations that can never work, mirroring
// the berth.toml validation, before anything starts.
func checkRunFlags(f runFlags) error {
	if f.keepComfyUI && f.drainComfyUI == "" {
		return fmt.Errorf("--keep-comfyui-up needs --drain-comfyui, the ComfyUI base URL to unload on drain")
	}
	if f.container != "" && f.compose != "" {
		return fmt.Errorf("--container and --compose are mutually exclusive: a Compose project names its own containers")
	}
	if f.compose != "" && !filepath.IsAbs(f.compose) {
		return fmt.Errorf("--compose %s must be an absolute path: engines start from the daemon's working directory", f.compose)
	}
	return nil
}
