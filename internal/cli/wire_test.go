package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wireConfig is a minimal llama-swap config: one wrappable model, one proxied.
const wireConfig = `models:
  "A":
    cmd: >
      ${server} -m /a.gguf
  "B":
    proxy: http://127.0.0.1:8000
`

func wireFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(wireConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWirePlanWritesNothing(t *testing.T) {
	isolate(t)
	p := wireFile(t)
	code, out, errOut := runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	for _, want := range []string{"1 of 2 models", "add A", "--write"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != wireConfig {
		t.Errorf("plan mode wrote the file: err=%v", err)
	}
}

func TestWireCheckReportsDrift(t *testing.T) {
	isolate(t)
	p := wireFile(t)
	code, out, errOut := runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--check")
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitFail, errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	for _, want := range []string{markError, "1 of 2 models", "A (add)", "--write"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks %q", errOut, want)
		}
	}
}

func TestWireWriteAppliesAndCheckPasses(t *testing.T) {
	isolate(t)
	p := wireFile(t)
	code, out, errOut := runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--write")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "Wired 1 of 2 models") {
		t.Errorf("stdout %q lacks the write summary", out)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "/usr/bin/berth run --as A --group llama-swap --priority foreground") {
		t.Errorf("wrapper missing:\n%s", got)
	}
	backups, err := filepath.Glob(p + ".bak-wire-*")
	if err != nil || len(backups) != 1 {
		t.Errorf("backups = %v (err %v), want exactly one", backups, err)
	}

	code, _, _ = runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--check")
	if code != ExitOK {
		t.Errorf("check after write: exit = %d, want 0", code)
	}
	code, out, _ = runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth")
	if code != ExitOK || !strings.Contains(out, "1 wired, 1 skipped") {
		t.Errorf("plan after write: exit %d, stdout %q", code, out)
	}
}

func TestWireExclude(t *testing.T) {
	isolate(t)
	p := wireFile(t)
	code, out, _ := runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--exclude", "A")
	if code != ExitOK || !strings.Contains(out, "2 skipped") {
		t.Errorf("exit %d, stdout %q; want both models skipped", code, out)
	}
}

func TestWireMachineDocument(t *testing.T) {
	isolate(t)
	p := wireFile(t)
	code, out, _ := runCLI(t, "wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--check", "--format", "json")
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d", code, ExitFail)
	}
	var doc wireDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	if doc.Status != "drift" || doc.Mode != "check" || doc.Add != 1 || doc.Wired != 0 {
		t.Errorf("doc = %+v", doc)
	}
	if len(doc.Models) != 2 || doc.Models[0].Name != "A" || doc.Models[0].Action != "add" {
		t.Errorf("models = %+v", doc.Models)
	}
}

func TestWireFlagErrors(t *testing.T) {
	isolate(t)
	p := wireFile(t)
	cases := [][]string{
		{"wire", "llama-swap", "--bin", "/usr/bin/berth"},                                                      // no --config
		{"wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--write", "--check"},                 // exclusive
		{"wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--priority", "urgent"},               // bad value
		{"wire", "llama-swap", "--config", p, "--bin", "/usr/bin/berth", "--exclude", "[bad"},                  // bad pattern
		{"wire", "llama-swap", "--config", filepath.Join(t.TempDir(), "nope.yaml"), "--bin", "/usr/bin/berth"}, // missing file
	}
	for _, args := range cases {
		code, _, errOut := runCLI(t, args...)
		if code != ExitSystem {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitSystem)
		}
		if !strings.Contains(errOut, markError) {
			t.Errorf("%v: stderr %q lacks %q", args, errOut, markError)
		}
	}
}

func TestIsTempBuild(t *testing.T) {
	if isTempBuild("/usr/local/bin/berth") {
		t.Error("installed path flagged as temporary")
	}
	if !isTempBuild(filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "cli")) {
		t.Error("go run path not flagged")
	}
}
