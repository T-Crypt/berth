package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeConfig puts a berth.toml in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "berth.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const validConfig = `
[metrics]
listen = "127.0.0.1:9797"

[engines]
every = "3s"

[[engine]]
name = "llama-swap"
type = "llama-swap"
url  = "http://127.0.0.1:9090"

[alerts.queue]
warn = 4
crit = 10
`

func TestConfigCheckHuman(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, out, errOut := runCLI(t, "config", "check", "--config", path)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "[OK] Config valid: "+path {
		t.Errorf("first line = %q", lines[0])
	}
	for _, want := range []string{
		"     - Engines: 1 configured",
		"     - Metrics: 127.0.0.1:9797 (/metrics)",
		"     - Alerts: queue warn 4 crit 10, clear polls 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "nothing was applied") {
		t.Errorf("a read-only check carries no disclaimer: %q", out)
	}
}

func TestConfigCheckVerbose(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, out, _ := runCLI(t, "config", "check", "--config", path, "-v")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"     - Engines (1):",
		"       * llama-swap (type: llama-swap, url: http://127.0.0.1:9090)",
		"     - Metrics Listener:",
		"       * Address: 127.0.0.1:9797",
		"       * Path:    /metrics",
		"       * Enabled: true",
		"     - Discovery: on, every 3s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
}

func TestConfigCheckJSON(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, out, errOut := runCLI(t, "config", "check", "--config", path, "--format", "json")
	if code != ExitOK || errOut != "" {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	var doc configCheckDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if doc.Status != "ok" || doc.Path != path || doc.Source != "file" || doc.Engines != 1 {
		t.Errorf("doc = %+v", doc)
	}
	if !doc.Metrics.Enabled || doc.Metrics.Address != "127.0.0.1:9797" || doc.Metrics.Endpoint != "/metrics" {
		t.Errorf("metrics = %+v", doc.Metrics)
	}
	if doc.Applied {
		t.Error("applied = true, want false: check never applies")
	}
	if doc.Warnings != nil || doc.Errors != nil {
		t.Errorf("warnings/errors = %v / %v, want none", doc.Warnings, doc.Errors)
	}
}

func TestConfigCheckYAML(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, out, errOut := runCLI(t, "config", "check", "--config", path, "--format", "yaml")
	if code != ExitOK || errOut != "" {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	var doc configCheckDoc
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("yaml %q: %v", out, err)
	}
	if doc.Status != "ok" || doc.Engines != 1 || doc.Metrics.Address != "127.0.0.1:9797" {
		t.Errorf("doc = %+v", doc)
	}
}

func TestConfigCheckInvalidHuman(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "[engines]\nevery = 5\n")
	code, out, errOut := runCLI(t, "config", "check", "--config", path)
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d", code, ExitFail)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "[ERROR] config invalid: "+path) {
		t.Errorf("stderr = %q", errOut)
	}
	if !strings.Contains(errOut, "missing unit in duration") {
		t.Errorf("stderr %q does not name the problem", errOut)
	}
}

func TestConfigCheckInvalidJSON(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "[engines]\nevery = 5\n")
	code, out, errOut := runCLI(t, "config", "check", "--config", path, "--format", "json")
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d", code, ExitFail)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty: the document carries the failure", errOut)
	}
	var doc configCheckDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if doc.Status != "invalid" || len(doc.Errors) == 0 {
		t.Errorf("doc = %+v", doc)
	}
}

// No file at the default path is a valid setup: the defaults and discovery.
func TestConfigCheckNoFile(t *testing.T) {
	isolate(t)
	code, out, errOut := runCLI(t, "config", "check")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, want := range []string{"[OK] Config valid:", "No file: berth runs on its built-in defaults", "Engines: 0 configured, discovery on", "Metrics: off"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
}

func TestConfigCheckExplicitMissingFile(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.toml")
	code, out, errOut := runCLI(t, "config", "check", "--config", missing)
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "config not found: "+missing) {
		t.Errorf("stderr = %q", errOut)
	}
}

// The path used to be a positional argument; saying so is kinder than reading
// the wrong file.
func TestConfigCheckPositionalPathPointsAtTheFlag(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "config", "check", "some/berth.toml")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	for _, want := range []string{`unexpected argument "some/berth.toml"`, "use --config some/berth.toml"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks %q", errOut, want)
		}
	}
}

func TestConfigCheckWarnsToStderr(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "[engines]\ndiscover = false\n")
	code, out, errOut := runCLI(t, "config", "check", "--config", path)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: a warning alone does not fail", code)
	}
	if !strings.Contains(errOut, "[WARN] engines.discover:") {
		t.Errorf("stderr = %q, want the warning", errOut)
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("stdout %q carries the warning", out)
	}
}

// --strict turns the same warning into a failed check, and the [OK] line is
// not printed at all.
func TestConfigCheckStrictFailsOnAWarning(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "[engines]\ndiscover = false\n")
	code, out, errOut := runCLI(t, "config", "check", "--config", path, "--strict")
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d", code, ExitFail)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	for _, want := range []string{"[ERROR] config valid with 1 warning and --strict is set:", "engines.discover: discovery is off"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q lacks %q", errOut, want)
		}
	}
}

func TestConfigCheckStrictPassesACleanConfig(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, _, errOut := runCLI(t, "config", "check", "--config", path, "--strict")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
}

// The evaluated config is what show prints: the file's values with every
// default materialised.
func TestConfigShowJSON(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "[engines]\n")
	code, out, errOut := runCLI(t, "config", "show", "--config", path, "--format", "json")
	if code != ExitOK || errOut != "" {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	var doc struct {
		Path   string `json:"path"`
		Source string `json:"source"`
		Config struct {
			Engines struct {
				Every    string `json:"every"`
				Discover bool   `json:"discover"`
			} `json:"engines"`
			Priority struct {
				JuggleWindow string `json:"juggle_window"`
			} `json:"priority"`
			Alerts struct {
				ClearPolls int `json:"clear_polls"`
			} `json:"alerts"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if doc.Path != path || doc.Source != "file" {
		t.Errorf("path/source = %q/%q", doc.Path, doc.Source)
	}
	if doc.Config.Engines.Every != "5s" || !doc.Config.Engines.Discover {
		t.Errorf("engines = %+v, want the defaults materialised", doc.Config.Engines)
	}
	if doc.Config.Priority.JuggleWindow != "15s" || doc.Config.Alerts.ClearPolls != 3 {
		t.Errorf("priority/alerts = %+v / %+v", doc.Config.Priority, doc.Config.Alerts)
	}
}

// A TOML document is the config's own file format: what show prints can be
// read back as berth.toml, with the same values.
func TestConfigShowTOMLRoundTrips(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, out, errOut := runCLI(t, "config", "show", "--config", path, "--format", "toml")
	if code != ExitOK || errOut != "" {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, `every = "3s"`) || !strings.Contains(out, `warn = 4.0`) {
		t.Errorf("toml %q lacks the file's own values", out)
	}
	roundTrip := filepath.Join(t.TempDir(), "berth.toml")
	if err := os.WriteFile(roundTrip, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "config", "check", "--config", roundTrip); code != ExitOK {
		t.Fatalf("the printed document does not validate: exit %d, %q", code, errOut)
	}
	_, before, _ := runCLI(t, "config", "show", "--config", path, "--format", "json")
	_, after, _ := runCLI(t, "config", "show", "--config", roundTrip, "--format", "json")
	// The path differs, every other value must not.
	strip := func(s, p string) string { return strings.ReplaceAll(s, p, "PATH") }
	if strip(before, path) != strip(after, roundTrip) {
		t.Errorf("round trip differs:\nbefore %s\nafter  %s", before, after)
	}
}

func TestConfigShowHuman(t *testing.T) {
	isolate(t)
	path := writeConfig(t, validConfig)
	code, out, _ := runCLI(t, "config", "show", "--config", path)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "[OK] Config: "+path) {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(out, "     - Engines: 1 configured, discovery on") {
		t.Errorf("stdout %q lacks the engine count", out)
	}
}

func TestConfigShowNoFileIsTheDefaults(t *testing.T) {
	isolate(t)
	code, out, _ := runCLI(t, "config", "show")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "(built-in defaults, no file)") {
		t.Errorf("stdout = %q", out)
	}
}
