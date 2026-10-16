package wire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a small llama-swap config with the shapes the walker must handle:
// a folded block, a literal block, a quoted name with a space, a proxied model
// (no cmd), a single-line cmd, comments everywhere, and a top-level key after
// models that must keep its distance.
const fixture = `# llama-swap config
macros:
  server: /usr/local/bin/llama-server
models:
  # a note that stays
  "Alpha-7B":
    aliases: ["alpha"]
    cmd: >
      ${server} -m /models/alpha.gguf
      --ctx-size 4096
    ttl: 300
  "Beta 9B":
    cmd: |
      ${server} -m /models/beta.gguf
  "Gamma":
    proxy: http://127.0.0.1:8000
  "Delta":
    cmd: ${server} -m /models/delta.gguf
after:
  key: 1
`

// fixtureWired is fixture after one wire with options() below.
const fixtureWired = `# llama-swap config
macros:
  server: /usr/local/bin/llama-server
models:
  # a note that stays
  "Alpha-7B":
    aliases: ["alpha"]
    cmd: >
      /usr/bin/berth run --as Alpha-7B --group llama-swap --priority foreground --drain-post http://127.0.0.1:9090/api/models/unload/Alpha-7B --
      ${server} -m /models/alpha.gguf
      --ctx-size 4096
    ttl: 300
  "Beta 9B":
    cmd: |
      /usr/bin/berth run --as 'Beta 9B' --group llama-swap --priority foreground --drain-post http://127.0.0.1:9090/api/models/unload/Beta%209B --
      ${server} -m /models/beta.gguf
  "Gamma":
    proxy: http://127.0.0.1:8000
  "Delta":
    cmd: ${server} -m /models/delta.gguf
after:
  key: 1
`

func options() Options {
	return Options{
		Bin:      "/usr/bin/berth",
		Group:    "llama-swap",
		Priority: "foreground",
		APIBase:  "http://127.0.0.1:9090",
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLlamaSwapWiresTheWholeRoster(t *testing.T) {
	r, err := LlamaSwap(writeTemp(t, fixture), options())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Alpha-7B": "add",
		"Beta 9B":  "add",
		"Gamma":    "skip",
		"Delta":    "skip",
	}
	for _, m := range r.Models {
		if want[m.Name] != m.Action {
			t.Errorf("%s: action = %q, want %q (reason %q)", m.Name, m.Action, want[m.Name], m.Reason)
		}
	}
	if !r.Changed {
		t.Error("Changed = false, want true")
	}
	if got := r.NewContent(); got != fixtureWired {
		t.Errorf("content mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, fixtureWired)
	}
	if r.Count("add") != 2 || r.Count("skip") != 2 {
		t.Errorf("counts = add %d, skip %d; want 2, 2", r.Count("add"), r.Count("skip"))
	}
}

func TestLlamaSwapIsIdempotent(t *testing.T) {
	r, err := LlamaSwap(writeTemp(t, fixtureWired), options())
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed {
		t.Errorf("Changed = true on already wired input (models: %+v)", r.Models)
	}
	if r.NewContent() != fixtureWired {
		t.Errorf("content changed on second pass:\n%s", r.NewContent())
	}
	for _, m := range r.Models {
		if m.Action != "wired" && m.Action != "skip" {
			t.Errorf("%s: action = %q, want wired or skip", m.Name, m.Action)
		}
	}
}

func TestLlamaSwapUpdateRealignsFlags(t *testing.T) {
	// A hand-wrapped line with different flags: without Update it counts as
	// wired and stays; with Update it is rewritten to the requested flags.
	in := strings.Replace(fixtureWired,
		"/usr/bin/berth run --as Alpha-7B --group llama-swap --priority foreground --drain-post http://127.0.0.1:9090/api/models/unload/Alpha-7B --",
		"/opt/old/berth run --as Alpha-7B --priority interactive --drain-post http://127.0.0.1:9090/api/models/unload/Alpha-7B --", 1)
	o := options()

	r, err := LlamaSwap(writeTemp(t, in), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.Models {
		if m.Name == "Alpha-7B" && m.Action != "wired" {
			t.Errorf("without Update: Alpha-7B action = %q, want wired", m.Action)
		}
	}

	o.Update = true
	r, err = LlamaSwap(writeTemp(t, in), o)
	if err != nil {
		t.Fatal(err)
	}
	var updated *Model
	for i, m := range r.Models {
		if m.Name == "Alpha-7B" {
			updated = &r.Models[i]
		}
	}
	if updated == nil || updated.Action != "update" {
		t.Fatalf("with Update: Alpha-7B = %+v, want update", updated)
	}
	out := r.NewContent()
	if strings.Contains(out, "/opt/old/berth") {
		t.Error("old wrapper line survived --update")
	}
	if !strings.Contains(out, "/usr/bin/berth run --as Alpha-7B --group llama-swap --priority foreground") {
		t.Errorf("updated line missing:\n%s", out)
	}
	// And the update is stable.
	r2, err := LlamaSwap(writeTemp(t, out), o)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Changed {
		t.Error("--update is not idempotent")
	}
}

func TestLlamaSwapExclude(t *testing.T) {
	o := options()
	o.Exclude = []string{"Beta *", "Delta"}
	r, err := LlamaSwap(writeTemp(t, fixture), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.Models {
		switch m.Name {
		case "Beta 9B":
			if m.Action != "skip" || !strings.Contains(m.Reason, "excluded") {
				t.Errorf("Beta 9B = %+v, want excluded skip", m)
			}
		case "Delta":
			if m.Action != "skip" || !strings.Contains(m.Reason, "excluded") {
				t.Errorf("Delta = %+v, want exclusion to win over the cmd shape", m)
			}
		}
	}
	if strings.Contains(r.NewContent(), "run --as 'Beta 9B'") {
		t.Error("excluded model was wired")
	}
}

func TestLlamaSwapKeepsCRLF(t *testing.T) {
	in := strings.ReplaceAll(fixture, "\n", "\r\n")
	r, err := LlamaSwap(writeTemp(t, in), options())
	if err != nil {
		t.Fatal(err)
	}
	out := r.NewContent()
	if strings.Contains(out, "\n\n") || strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\r") {
		t.Errorf("mixed line endings after edit:\n%q", out)
	}
	if !strings.Contains(out, "--drain-post http://127.0.0.1:9090/api/models/unload/Alpha-7B --\r\n") {
		t.Error("inserted line does not carry the file's CRLF ending")
	}
	r2, err := LlamaSwap(writeTemp(t, out), options())
	if err != nil || r2.Changed {
		t.Errorf("CRLF pass is not idempotent: changed=%v err=%v", r2 != nil && r2.Changed, err)
	}
}

func TestLlamaSwapErrors(t *testing.T) {
	if _, err := LlamaSwap(filepath.Join(t.TempDir(), "missing.yaml"), options()); err == nil {
		t.Error("missing file: want error")
	}
	if _, err := LlamaSwap(writeTemp(t, "a: [}\n"), options()); err == nil {
		t.Error("bad YAML: want error")
	}
	if _, err := LlamaSwap(writeTemp(t, "macros:\n  server: x\n"), options()); err == nil {
		t.Error("no models key: want error")
	}
	if _, err := LlamaSwap(writeTemp(t, "models: {}\n"), options()); err == nil {
		t.Error("empty models: want error")
	}
}

func TestOptionsValidate(t *testing.T) {
	bad := []Options{
		{Bin: ""},
		{Bin: "x", APIBase: "http://127.0.0.1:9090", Priority: "urgent"},
		{Bin: "x", APIBase: "not a url", Priority: "foreground"},
		{Bin: "x", APIBase: "http://127.0.0.1:9090", Priority: "", Exclude: []string{"[bad"}},
	}
	for i, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
	ok := Options{Bin: "x", APIBase: "http://127.0.0.1:9090", Priority: "foreground", Group: "g", Exclude: []string{"a*"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
}

func TestWriteBacksUpAndReplaces(t *testing.T) {
	p := writeTemp(t, fixture)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LlamaSwap(p, options())
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Write(r)
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" || !strings.Contains(backup, ".bak-wire-") {
		t.Fatalf("backup = %q, want a .bak-wire path", backup)
	}
	old, err := os.ReadFile(backup)
	if err != nil || string(old) != fixture {
		t.Errorf("backup content: err=%v, matches=%v", err, string(old) == fixture)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != fixtureWired {
		t.Errorf("written content: err=%v, matches=%v", err, string(got) == fixtureWired)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 preserved", fi.Mode().Perm())
	}

	// A result with nothing to change writes nothing.
	clean, err := LlamaSwap(p, options())
	if err != nil {
		t.Fatal(err)
	}
	if b, err := Write(clean); err != nil || b != "" {
		t.Errorf("Write on a clean result = (%q, %v), want (\"\", nil)", b, err)
	}
}
