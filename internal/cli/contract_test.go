// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without berthd, berth contract still prints what berth.toml declares and
// marks every measured number unknown; a name nobody knows is refused with
// the names that exist.
func TestContractWithoutBerthd(t *testing.T) {
	home := isolate(t)
	cfg := filepath.Join(home, "berth.toml")
	if err := os.WriteFile(cfg, []byte("[[engine]]\nname = \"qwen\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\npriority = \"foreground\"\nneeds_mib = 14000\nmin_turn = \"5m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runCLI(t, "contract", "qwen", "--config", cfg, "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got struct {
		InConfig bool `json:"in_config"`
		VRAM     struct {
			Bytes  uint64 `json:"bytes"`
			Source string `json:"source"`
		} `json:"vram"`
		LastPeak struct {
			Source string `json:"source"`
			Why    string `json:"why"`
		} `json:"last_peak"`
		MinTurn struct {
			Seconds float64 `json:"seconds"`
			Source  string  `json:"source"`
		} `json:"min_turn"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !got.InConfig || got.VRAM.Bytes != 14000<<20 || got.VRAM.Source != "declared" ||
		got.LastPeak.Source != "unknown" || got.LastPeak.Why != "berthd did not answer" ||
		got.MinTurn.Seconds != 300 || got.MinTurn.Source != "declared" {
		t.Fatalf("contract = %+v", got)
	}

	code, out, _ = runCLI(t, "contract", "qwen", "--config", cfg)
	if code != 0 || !strings.Contains(out, "berthd unreachable") || !strings.Contains(out, "14000 MiB") {
		t.Fatalf("human (exit %d):\n%s", code, out)
	}

	code, _, errOut = runCLI(t, "contract", "nope", "--config", cfg)
	if code == 0 || !strings.Contains(errOut, `no engine named "nope"`) || !strings.Contains(errOut, "known: qwen") {
		t.Fatalf("unknown name (exit %d): %s", code, errOut)
	}
}
