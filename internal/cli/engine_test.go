package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEngine is an OpenAI-compatible list, which is all the openai reader reads.
func fakeEngine(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"qwen3-30b"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// enginesConfig writes a berth.toml with one openai engine per URL, named e0,
// e1, and turns discovery off so the machine's real ports are not probed.
func enginesConfig(t *testing.T, urls ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[engines]\ndiscover = false\n")
	for i, u := range urls {
		fmt.Fprintf(&b, "[[engine]]\nname = \"e%d\"\ntype = \"openai\"\nurl = %q\n", i, u)
	}
	p := filepath.Join(t.TempDir(), "berth.toml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEngineCheckAllUp(t *testing.T) {
	isolate(t)
	path := enginesConfig(t, fakeEngine(t))
	code, out, errOut := runCLI(t, "engine", "check", "--config", path)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "[OK] 1 of 1 engine answer") {
		t.Errorf("stdout = %q, want the verdict", out)
	}
	if !strings.Contains(out, "up, 1 model (qwen3-30b)") {
		t.Errorf("stdout %q does not name the model", out)
	}
}

func TestEngineCheckOneDown(t *testing.T) {
	isolate(t)
	path := enginesConfig(t, fakeEngine(t), "http://127.0.0.1:1")
	code, out, errOut := runCLI(t, "engine", "check", "--config", path)
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitFail, errOut)
	}
	if !strings.Contains(out, ": DOWN") {
		t.Errorf("stdout = %q, want the down engine", out)
	}
	if !strings.Contains(errOut, "[ERROR] 1 of 2 engines did not answer") {
		t.Errorf("stderr = %q, want the verdict", errOut)
	}
	if !strings.Contains(errOut, errIndent+"e1: ") {
		t.Errorf("stderr %q does not name the engine", errOut)
	}
	// The reason is reported once, in the [ERROR] block, not twice.
	if strings.Count(out+errOut, "connection refused") != 1 {
		t.Errorf("the reason appears %d times:\n%s\n%s", strings.Count(out+errOut, "connection refused"), out, errOut)
	}
}

func TestEngineCheckJSON(t *testing.T) {
	isolate(t)
	path := enginesConfig(t, fakeEngine(t), "http://127.0.0.1:1")
	code, out, errOut := runCLI(t, "engine", "check", "--config", path, "--format", "json")
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d", code, ExitFail)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty: the document carries the failure", errOut)
	}
	var doc engineCheckDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if doc.Status != "failed" || doc.Checked != 2 || doc.Answered != 1 {
		t.Errorf("doc = +%v", doc)
	}
	if len(doc.Engines) != 2 || !doc.Engines[0].Up || doc.Engines[1].Up {
		t.Errorf("engines = %+v", doc.Engines)
	}
	if doc.Engines[1].Error == "" {
		t.Error("the down engine has no error in the document")
	}
}

func TestEngineCheckNoEngines(t *testing.T) {
	isolate(t)
	path := enginesConfig(t)
	code, out, _ := runCLI(t, "engine", "check", "--config", path)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"[OK] No engines configured", "berth engine check --discover"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout %q lacks %q", out, want)
		}
	}
}

func TestEngineCheckUnknownName(t *testing.T) {
	isolate(t)
	path := enginesConfig(t, fakeEngine(t))
	code, _, errOut := runCLI(t, "engine", "check", "--config", path, "nope")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, `no engine named "nope"`) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestEngineCheckVerboseNamesWhatItRead(t *testing.T) {
	isolate(t)
	path := enginesConfig(t, fakeEngine(t))
	code, out, _ := runCLI(t, "engine", "check", "--config", path, "-v")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "       * metrics: none reported") {
		t.Errorf("stdout = %q, want the verbose block", out)
	}
}
