// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// berth fit for a model ranks every engine that serves it in berth.toml's
// order, against a real berthd; without berthd every card is out of reach;
// a name nothing serves is refused with the engines that exist.
func TestFitAgainstADaemon(t *testing.T) {
	home := isolate(t)
	sock := startDaemon(t)
	cfg := filepath.Join(home, "berth.toml")
	toml := "[[engine]]\nname = \"a\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nserves = [\"m\"]\ngpu = \"gpu1\"\n" +
		"[[engine]]\nname = \"b\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8081\"\nserves = [\"m\"]\ngpu = \"gpu0\"\n"
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "fit", "m", "--config", cfg, "--socket", sock, "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var doc struct {
		Pick struct {
			Engine, Path, Why string
		} `json:"pick"`
		Candidates []struct {
			Engine, Path string
			Start        struct{ Source string }
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// a comes first in berth.toml, but its card is not one berthd reports:
	// b on the free card wins, and a waits.
	if doc.Pick.Engine != "b" || doc.Pick.Path != "free" || doc.Pick.Why != "the card is free" ||
		len(doc.Candidates) != 2 || doc.Candidates[1].Engine != "a" || doc.Candidates[1].Path != "blocked" ||
		doc.Candidates[0].Start.Source != "unknown" {
		t.Fatalf("fit = %s", out)
	}

	code, out, _ = runCLI(t, "fit", "m", "--config", cfg, "--socket", sock)
	if code != 0 || !strings.HasPrefix(out, "m: b, free, the card is free\n") || !strings.Contains(out, "ENGINE") {
		t.Fatalf("human (exit %d):\n%s", code, out)
	}

	code, out, _ = runCLI(t, "fit", "a", "--config", cfg, "--socket", filepath.Join(home, "none.sock"))
	if code != 0 || !strings.HasPrefix(out, "a: blocked, berthd did not report the card\n") || !strings.Contains(out, "berthd unreachable") {
		t.Fatalf("offline (exit %d):\n%s", code, out)
	}

	code, _, errOut = runCLI(t, "fit", "nope", "--config", cfg, "--socket", sock)
	if code == 0 || !strings.Contains(errOut, `no engine named "nope", and none serves it`) || !strings.Contains(errOut, "engines: a, b") {
		t.Fatalf("unknown (exit %d): %s", code, errOut)
	}
}

// A model no berth.toml engine names or serves is still found the way the
// gateway finds it: a live llama-swap lists it on /v1/models, so berth fit
// ranks the model itself on llama-swap's card.
func TestFitFindsAModelALiveEngineLists(t *testing.T) {
	home := isolate(t)
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"Defiance-27B"},{"id":"Dirk-27B"}]}`)
	}))
	t.Cleanup(ls.Close)
	sock := startDaemonWith(t, server.Extras{Engines: func() []proto.Engine {
		return []proto.Engine{{Name: "llama-swap", Type: "llama-swap", URL: ls.URL, Up: true, State: "up"}}
	}})
	cfg := filepath.Join(home, "berth.toml")
	if err := os.WriteFile(cfg, []byte("[[engine]]\nname = \"llama-swap\"\ntype = \"llama-swap\"\nurl = \""+ls.URL+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "fit", "Dirk-27B", "--config", cfg, "--socket", sock)
	if code != 0 || !strings.HasPrefix(out, "Dirk-27B: free, the card is free\n") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}

	code, _, errOut = runCLI(t, "fit", "nope", "--config", cfg, "--socket", sock)
	if code == 0 || !strings.Contains(errOut, `no engine named "nope", and none serves it`) {
		t.Fatalf("unknown (exit %d): %s", code, errOut)
	}
}
