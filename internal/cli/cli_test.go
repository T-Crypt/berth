package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// exec runs the CLI in process with isolated streams, the way the binary runs
// it, so an exit code and a stream can be asserted together.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Env{Args: args, Out: &out, Err: &errOut, Version: "test"})
	return code, out.String(), errOut.String()
}

// isolate points every path the CLI reads at a temp dir, so a test never
// touches the operator's config, socket or key.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	t.Setenv("BERTH_SOCKET", filepath.Join(home, "run", "berth.sock"))
	t.Setenv("BERTH_CONFIG", "")
	t.Setenv("BERTH_API_KEY_FILE", filepath.Join(home, "config", "berth", "api.key"))
	t.Setenv("NO_COLOR", "1")
	return home
}

func TestNoArgsIsUsageError(t *testing.T) {
	isolate(t)
	code, out, errOut := runCLI(t)
	if code != ExitSystem {
		t.Errorf("exit = %d, want %d", code, ExitSystem)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "berth <command> [flags]") {
		t.Errorf("stderr = %q, want the command list", errOut)
	}
}

func TestHelpGoesToStdoutAndPasses(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		code, out, errOut := runCLI(t, args...)
		if code != ExitOK {
			t.Errorf("%v: exit = %d, want 0", args, code)
		}
		if errOut != "" {
			t.Errorf("%v: stderr = %q, want empty", args, errOut)
		}
		for _, want := range []string{"config", "status", "Global flags"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: stdout %q lacks %q", args, out, want)
			}
		}
	}
}

func TestHelpOneCommand(t *testing.T) {
	isolate(t)
	code, out, errOut := runCLI(t, "help", "status")
	if code != ExitOK || errOut != "" {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	for _, want := range []string{"berth status [--socket P]", "collisions", "Flags:"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
}

func TestHelpAGroup(t *testing.T) {
	isolate(t)
	code, out, _ := runCLI(t, "help", "config")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"berth config <command>", "check", "show"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
}

func TestUnknownCommandExitSystem(t *testing.T) {
	isolate(t)
	cases := [][]string{{"nope"}, {"help", "nope"}, {"config", "nope"}}
	for _, args := range cases {
		code, out, errOut := runCLI(t, args...)
		if code != ExitSystem {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitSystem)
		}
		if out != "" {
			t.Errorf("%v: stdout = %q, want empty", args, out)
		}
		if !strings.Contains(errOut, "[ERROR] unknown command") {
			t.Errorf("%v: stderr = %q", args, errOut)
		}
	}
}

func TestUnknownLeadingFlag(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "--nope", "status")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, `unknown flag "--nope"`) {
		t.Errorf("stderr = %q", errOut)
	}
}

// A global flag works before the command name and after it, and --quiet leaves
// the exit code as the only answer either way.
func TestQuietInBothPositions(t *testing.T) {
	isolate(t)
	before, outBefore, _ := runCLI(t, "-q", "version")
	after, outAfter, _ := runCLI(t, "version", "-q")
	if before != ExitOK || after != ExitOK {
		t.Fatalf("exit = %d / %d, want 0", before, after)
	}
	if outBefore != "" || outAfter != "" {
		t.Errorf("stdout = %q / %q, want empty", outBefore, outAfter)
	}
}

func TestVersionFormats(t *testing.T) {
	isolate(t)
	code, out, _ := runCLI(t, "version")
	if code != ExitOK || out != "berth test\n" {
		t.Fatalf("human = %q (exit %d), want %q", out, code, "berth test\n")
	}

	_, out, _ = runCLI(t, "version", "--format", "json")
	var doc versionDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if doc.Version != "test" || doc.API == 0 || doc.Platform == "" {
		t.Errorf("doc = %+v", doc)
	}

	_, out, _ = runCLI(t, "version", "--format", "yaml")
	if !strings.Contains(out, "version: test") {
		t.Errorf("yaml = %q", out)
	}
}

// --format before the command name is resolved against the command's own list
// once the command is known.
func TestFormatBeforeTheCommand(t *testing.T) {
	isolate(t)
	code, out, _ := runCLI(t, "--format", "json", "version")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("stdout = %q, want a json document", out)
	}

	code, _, errOut := runCLI(t, "--format", "json", "key")
	if code != ExitSystem {
		t.Errorf("key: exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "--format does not apply to berth key") {
		t.Errorf("key: stderr = %q", errOut)
	}
}

func TestBadFormatValue(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "version", "--format", "csv")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "human, json, yaml") {
		t.Errorf("stderr = %q, want the valid values", errOut)
	}
}

// A command that takes no --format says so rather than silently ignoring it.
func TestRunHasNoFormatFlag(t *testing.T) {
	isolate(t)
	code, out, errOut := runCLI(t, "run", "--format", "json", "--", "true")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if strings.Count(errOut, "flag provided but not defined") != 1 {
		t.Errorf("stderr = %q, want the reason once", errOut)
	}
}

func TestUnexpectedArgument(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "version", "extra")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, `unexpected argument "extra"`) {
		t.Errorf("stderr = %q", errOut)
	}
}

// stubView lets the Write contract be tested on its own: what goes to stdout,
// what goes to stderr, and which exit code comes back.
type stubView struct{}

func (stubView) Data() any { return map[string]string{"status": "failed"} }

func (stubView) Human(c *Context, ok bool) error {
	c.Printf("human report, ok=%v\n", ok)
	return nil
}

func testContext(machine, quiet bool) (*Context, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
	if machine {
		c.Format = FormatJSON
	}
	if quiet {
		c.Out = io.Discard
	}
	return c, &out, &errOut
}

func TestWriteMachineKeepsStderrCleanOnFailure(t *testing.T) {
	c, out, errOut := testContext(true, false)
	e := Failf("the check failed")
	code := c.Write(stubView{}, e)
	if code != ExitFail {
		t.Errorf("exit = %d, want %d", code, ExitFail)
	}
	if !strings.Contains(out.String(), `"status": "failed"`) {
		t.Errorf("stdout = %q, want the document", out.String())
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty: the document carries the facts", errOut.String())
	}
}

func TestWriteMachineReportsStderrWhenThereIsNoDocument(t *testing.T) {
	c, out, errOut := testContext(true, false)
	code := c.Write(nil, Systemf("berthd unreachable"))
	if code != ExitSystem {
		t.Errorf("exit = %d, want %d", code, ExitSystem)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "[ERROR] berthd unreachable") {
		t.Errorf("stderr = %q, want the failure", errOut.String())
	}
}

func TestWriteHumanSplitsReportAndFailure(t *testing.T) {
	c, out, errOut := testContext(false, false)
	code := c.Write(stubView{}, Failf("one engine did not answer").Detail("gone: no route"))
	if code != ExitFail {
		t.Errorf("exit = %d, want %d", code, ExitFail)
	}
	if !strings.Contains(out.String(), "human report, ok=false") {
		t.Errorf("stdout = %q, want the report", out.String())
	}
	for _, want := range []string{"[ERROR] one engine did not answer", errIndent + "gone: no route"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q lacks %q", errOut.String(), want)
		}
	}
}

func TestWriteQuietPrintsNothingOnStdout(t *testing.T) {
	c, _, errOut := testContext(true, true)
	if code := c.Write(stubView{}, nil); code != ExitOK {
		t.Errorf("exit = %d, want 0", code)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

// A view that prints nothing on failure (config check) must not leave a
// half-written report on stdout.
func TestWriteHumanSilentViewOnFailure(t *testing.T) {
	c, out, errOut := testContext(false, false)
	if code := c.Write(nil, Failf("config invalid: x")); code != ExitFail {
		t.Errorf("exit = %d, want %d", code, ExitFail)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "[ERROR] config invalid: x") {
		t.Errorf("stderr = %q", errOut.String())
	}
}
