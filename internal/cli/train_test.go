// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/runner"
)

// berth train sets the lease's shape itself, so a flag that would change it
// is refused before berthd is dialed.
func TestTrainRefusesTheFlagsItSets(t *testing.T) {
	isolate(t)
	for _, flag := range [][]string{{"--priority", "batch"}, {"--needs-mib", "8000"}, {"--wait", "5m"}, {"--window", "23:00-07:00"}} {
		args := append([]string{"train", "--as", "x"}, flag...)
		code, _, errOut := runCLI(t, append(args, "--", "true")...)
		if code != ExitSystem || !strings.Contains(errOut, "berth train sets "+flag[0]+" itself") {
			t.Errorf("%v: exit %d, stderr %q", flag, code, errOut)
		}
	}
}

// The lock lives in berthd, so a training never starts unprotected.
func TestTrainRefusesToStartWithoutADaemon(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.sock")
	code, out, _ := runCLI(t, "train", "--as", "x", "--socket", missing, "--", "true")
	if code != runner.ExitRefused {
		t.Fatalf("exit = %d, want %d", code, runner.ExitRefused)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}
