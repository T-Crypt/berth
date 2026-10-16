package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/runner"
)

// --keep-comfyui-up without --drain-comfyui is a silent no-op downstream
// (KeepComfyUI never fires without DrainComfyUI), so the CLI rejects it
// like the berth.toml validation does.
func TestCheckRunFlagsKeepNeedsDrain(t *testing.T) {
	if err := checkRunFlags(true, ""); err == nil {
		t.Fatal("keep without drain should error")
	}
	if err := checkRunFlags(true, "http://127.0.0.1:8188"); err != nil {
		t.Fatalf("keep with drain should pass, got %v", err)
	}
	if err := checkRunFlags(false, ""); err != nil {
		t.Fatalf("plain run should pass, got %v", err)
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
