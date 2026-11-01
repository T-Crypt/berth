// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/runner"
)

// --keep-comfyui-up without --drain-comfyui is a silent no-op downstream
// (KeepComfyUI never fires without DrainComfyUI), so the CLI rejects it
// like the berth.toml validation does.
func TestCheckRunFlagsKeepNeedsDrain(t *testing.T) {
	if err := checkRunFlags(runFlags{keepComfyUI: true}); err == nil {
		t.Fatal("keep without drain should error")
	}
	if err := checkRunFlags(runFlags{keepComfyUI: true, drainComfyUI: "http://127.0.0.1:8188"}); err != nil {
		t.Fatalf("keep with drain should pass, got %v", err)
	}
	if err := checkRunFlags(runFlags{}); err != nil {
		t.Fatalf("plain run should pass, got %v", err)
	}
}

// A Compose engine is one Compose file, never also a container, and the path
// has to outlive the daemon's working directory.
func TestCheckRunFlagsCompose(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "compose.yaml")
	if err := checkRunFlags(runFlags{compose: abs}); err != nil {
		t.Fatalf("compose alone should pass, got %v", err)
	}
	if err := checkRunFlags(runFlags{compose: abs, container: "tagger"}); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("compose with container = %v, want it refused", err)
	}
	err := checkRunFlags(runFlags{compose: "compose.yaml"})
	if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("relative compose = %v, want it refused", err)
	}
}

func TestRunNeedsAnEngineAndACommand(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "run", "--", "true")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "--as NAME and the engine's command are required") {
		t.Errorf("stderr = %q", errOut)
	}
	if !strings.Contains(errOut, "usage: berth run --as NAME") {
		t.Errorf("stderr %q lacks the usage line", errOut)
	}
}

func TestRunRejectsABadPriority(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "run", "--as", "x", "--priority", "urgent", "--", "true")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "urgent") {
		t.Errorf("stderr = %q", errOut)
	}
}

// A slice that cannot name a unit fails at the CLI, before berthd is
// dialed and the engine is touched.
func TestRunRejectsABadSlice(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "run", "--as", "x", "--slice", "nope", "--", "true")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "must name a .slice unit") {
		t.Errorf("stderr = %q", errOut)
	}
}

// --fail-closed with berthd down refuses to start the engine: EX_TEMPFAIL, so
// a service manager can retry instead of running unprotected.
func TestRunFailClosedRefusesWithoutADaemon(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.sock")
	code, out, _ := runCLI(t, "run", "--as", "x", "--fail-closed", "--socket", missing, "--", "true")
	if code != runner.ExitRefused {
		t.Fatalf("exit = %d, want %d", code, runner.ExitRefused)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty: the engine owns stdout", out)
	}
}

// Fail-open is the invariant: with berthd down the engine still starts, the run
// is recorded as unprotected, and --quiet never hides that warning.
func TestRunFailOpenStartsWithAWarningEvenQuiet(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.sock")
	code, _, errOut := runCLI(t, "-q", "run", "--as", "x", "--socket", missing, "--", "true")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "UNPROTECTED: berthd unreachable") {
		t.Errorf("stderr = %q, want the unprotected warning", errOut)
	}
}

// A setting no flag sets comes from the engine's entry; a set flag wins, and
// each override is named with both values. A flag equal to the entry is not
// an override.
func TestApplyEngineEntry(t *testing.T) {
	e := &config.Engine{Name: "comfyui", Priority: "batch", NeedsMiB: 8192,
		DrainPost: "http://127.0.0.1:1/free", DrainSlots: "http://127.0.0.1:2",
		DrainComfyUI: "http://127.0.0.1:3", DrainMax: config.Duration{Duration: time.Minute},
		MinTurn: config.Duration{Duration: 5 * time.Minute}}

	var errBuf bytes.Buffer
	c := &Context{setFlags: map[string]bool{}, Err: &errBuf}
	prio, keep := "interactive", false
	needs, dp, ds, dc := uint64(0), "", "", ""
	var dm, mt time.Duration
	cont, comp := "", ""
	// The flags ride as pointers, so the local changes below reach the same
	// set every call passes.
	f := engineFlags{priority: &prio, needsMiB: &needs, drainPost: &dp, drainSlots: &ds,
		drainComfyUI: &dc, keepComfyUI: &keep, drainMax: &dm, minTurn: &mt,
		container: &cont, compose: &comp}
	applyEngineEntry(c, e, f)
	if e.Priority != "batch" || e.NeedsMiB != 8192 || e.DrainPost != "http://127.0.0.1:1/free" ||
		e.DrainSlots != "http://127.0.0.1:2" || e.DrainComfyUI != "http://127.0.0.1:3" ||
		e.DrainMax.Duration != time.Minute || e.MinTurn.Duration != 5*time.Minute {
		t.Errorf("defaults from the entry = %q %d %q %q %q %s %s", e.Priority, e.NeedsMiB, e.DrainPost, e.DrainSlots, e.DrainComfyUI, e.DrainMax, e.MinTurn)
	}
	if errBuf.String() != "" {
		t.Errorf("no flag set, but a notice was printed: %q", errBuf.String())
	}

	errBuf.Reset()
	c = &Context{setFlags: map[string]bool{"priority": true, "needs-mib": true, "drain-post": true,
		"drain-slots": true, "drain-comfyui": true, "drain-max": true, "min-turn": true}, Err: &errBuf}
	prio, needs, dp, ds, dc = "foreground", 4096, "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9"
	dm = 2 * time.Minute
	mt = time.Minute
	applyEngineEntry(c, e, f)
	if e.Priority != "foreground" || e.NeedsMiB != 4096 || e.DrainPost != "http://127.0.0.1:9" ||
		e.DrainMax.Duration != 2*time.Minute || e.MinTurn.Duration != time.Minute {
		t.Errorf("explicit flags lost to the entry: %q %d %q %s %s", e.Priority, e.NeedsMiB, e.DrainPost, e.DrainMax, e.MinTurn)
	}
	for _, want := range []string{
		"berth: --priority overrides berth.toml priority = batch for comfyui",
		"berth: --needs-mib overrides berth.toml needs_mib = 8192 for comfyui",
		"berth: --drain-post overrides berth.toml drain_post = http://127.0.0.1:1/free for comfyui",
		"berth: --drain-slots overrides berth.toml drain_slots = http://127.0.0.1:2 for comfyui",
		"berth: --drain-comfyui overrides berth.toml drain_comfyui = http://127.0.0.1:3 for comfyui",
		"berth: --drain-max overrides berth.toml drain_max = 1m0s for comfyui",
		"berth: --min-turn overrides berth.toml min_turn = 5m0s for comfyui",
	} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errBuf.String())
		}
	}

	errBuf.Reset()
	c = &Context{setFlags: map[string]bool{"priority": true}, Err: &errBuf}
	prio = "batch"
	same := &config.Engine{Name: "comfyui", Priority: "batch"}
	applyEngineEntry(c, same, f)
	if errBuf.String() != "" {
		t.Errorf("a flag equal to the entry printed a notice: %q", errBuf.String())
	}

	on := &config.Engine{Name: "comfyui", KeepComfyUI: true, DrainComfyUI: "http://127.0.0.1:3"}
	errBuf.Reset()
	c = &Context{setFlags: map[string]bool{}, Err: &errBuf}
	keep = false
	applyEngineEntry(c, on, f)
	if !on.KeepComfyUI {
		t.Error("the entry's keep_comfyui_up = true was not kept")
	}

	off := &config.Engine{Name: "comfyui"}
	errBuf.Reset()
	c = &Context{setFlags: map[string]bool{"keep-comfyui-up": true}, Err: &errBuf}
	keep = false
	applyEngineEntry(c, off, f)
	if !strings.Contains(errBuf.String(), "berth: --keep-comfyui-up overrides berth.toml keep_comfyui_up = false for comfyui") {
		t.Errorf("stderr = %q, want the keep-comfyui-up notice", errBuf.String())
	}
}

func TestRunOverrideNoticeIsPrinted(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	toml := filepath.Join(home, "berth.toml")
	file := "" +
		"[[engine]]\n" +
		"name = \"comfyui\"\n" +
		"type = \"comfyui\"\n" +
		"url = \"http://127.0.0.1:8188\"\n" +
		"drain_comfyui = \"http://127.0.0.1:8188\"\n" +
		"keep_comfyui_up = false\n"
	if err := os.WriteFile(toml, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "nope.sock")
	code, _, errOut := runCLI(t, "run", "--as", "comfyui", "--keep-comfyui-up",
		"--socket", missing, "--config", toml, "--", "true")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "berth: --keep-comfyui-up overrides berth.toml keep_comfyui_up = false for comfyui") {
		t.Errorf("stderr = %q, want the override notice", errOut)
	}
	if !strings.Contains(errOut, "UNPROTECTED: berthd unreachable") {
		t.Errorf("stderr = %q, want the unprotected warning", errOut)
	}
}

// A run with --drain-max set names the engine's drain_max as the flag
// overrides it, like the other drain flags.
func TestRunDrainMaxOverrideNoticeIsPrinted(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	toml := filepath.Join(home, "berth.toml")
	file := "" +
		"[[engine]]\n" +
		"name = \"strata\"\n" +
		"type = \"llamacpp\"\n" +
		"url = \"http://127.0.0.1:8080\"\n" +
		"drain_slots = \"http://127.0.0.1:8080\"\n" +
		"drain_max = \"10m\"\n"
	if err := os.WriteFile(toml, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "nope.sock")
	code, _, errOut := runCLI(t, "run", "--as", "strata", "--drain-max", "5m",
		"--socket", missing, "--config", toml, "--", "true")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "berth: --drain-max overrides berth.toml drain_max = 10m0s for strata") {
		t.Errorf("stderr = %q, want the override notice", errOut)
	}
}

// A run with --min-turn set names the engine's min_turn as the flag
// overrides it, like the other drain flags.
func TestRunMinTurnOverrideNoticeIsPrinted(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	toml := filepath.Join(home, "berth.toml")
	file := "" +
		"[[engine]]\n" +
		"name = \"comfyui\"\n" +
		"type = \"comfyui\"\n" +
		"url = \"http://127.0.0.1:8188\"\n" +
		"min_turn = \"10m\"\n"
	if err := os.WriteFile(toml, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "nope.sock")
	code, _, errOut := runCLI(t, "run", "--as", "comfyui", "--min-turn", "5m",
		"--socket", missing, "--config", toml, "--", "true")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "berth: --min-turn overrides berth.toml min_turn = 10m0s for comfyui") {
		t.Errorf("stderr = %q, want the override notice", errOut)
	}
}

// A run that names a model reads the settings of the entry its --group names.
func TestRunMatchesTheGroupEntry(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	toml := filepath.Join(home, "berth.toml")
	file := "" +
		"[[engine]]\n" +
		"name = \"llama-swap\"\n" +
		"type = \"llamacpp\"\n" +
		"url = \"http://127.0.0.1:1234\"\n" +
		"priority = \"batch\"\n"
	if err := os.WriteFile(toml, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "nope.sock")
	code, _, errOut := runCLI(t, "run", "--as", "NInfer-27B", "--group", "llama-swap",
		"--priority", "foreground", "--socket", missing, "--config", toml, "--", "true")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "berth: --priority overrides berth.toml priority = batch for llama-swap") {
		t.Errorf("stderr = %q, want the override notice for the group's entry", errOut)
	}
}

// A config that cannot be read fails the run instead of ignoring it.
func TestRunFailsOnAnInvalidConfig(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	toml := filepath.Join(home, "berth.toml")
	if err := os.WriteFile(toml, []byte("priority = \"urgent\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "run", "--as", "x", "--config", toml, "--", "true")
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d", code, ExitFail)
	}
	if !strings.Contains(errOut, "config invalid") {
		t.Errorf("stderr = %q, want the config failure", errOut)
	}
}

// A checkpointed engine sleeps only behind a front (docs/design.md, rule 3).
func TestCheckFront(t *testing.T) {
	engine := []string{"llama-server", "--port", "{{BERTH_PORT}}"}
	for _, tc := range []struct {
		park, front string
		argv        []string
		want        string
	}{
		{"checkpoint", "127.0.0.1:5800", engine, ""},
		{"", "", []string{"llama-server"}, ""},
		{"hook", "", []string{"llama-server"}, ""},
		{"checkpoint", "", engine, "--park checkpoint needs --front: a checkpointed engine may only sleep behind a door that holds its requests"},
		{"hook", "127.0.0.1:5800", engine, "--front is for --park checkpoint"},
		{"checkpoint", "127.0.0.1:5800", []string{"llama-server", "--port", "5801"}, "--front needs {{BERTH_PORT}} in the engine's command, where its port goes"},
		{"checkpoint", "0.0.0.0:5800", engine, "--front: "},
		{"sleep", "", engine, `--park "sleep" is not off, hook or checkpoint`},
	} {
		err := checkFront(tc.park, tc.front, tc.argv)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("checkFront(%q, %q) = %v, want nil", tc.park, tc.front, err)
		case tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.want)):
			t.Errorf("checkFront(%q, %q) = %v, want %q", tc.park, tc.front, err, tc.want)
		}
	}
}

// A card that no node could name fails at the CLI, before berthd is dialed.
func TestRunRejectsABadCard(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "run", "--as", "x", "--gpu", "1", "--", "true")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, `gpu "1" must be a card name like gpu1`) {
		t.Errorf("stderr = %q", errOut)
	}
}
