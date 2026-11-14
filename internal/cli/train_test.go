// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/runs"
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

// With berthd down, a run refuses to start while a training that lost the
// daemon still runs on the node, instead of starting unprotected beside it.
func TestRunRefusesWithoutADaemonWhileATrainingRuns(t *testing.T) {
	isolate(t)
	sock := filepath.Join(t.TempDir(), "berth.sock")
	self := os.Getpid()
	if _, err := runs.Write(runs.Dir(sock), runs.Record{
		Engine: "train-a", PID: self, Start: procs.System{}.StartTime(self), Started: time.Now(),
		Reason: runs.ReasonLost, Training: true,
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "run", "--as", "chat", "--socket", sock, "--", "echo", "started")
	if code != runner.ExitRefused || strings.Contains(out, "started") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want a refusal and no start", code, out, errOut)
	}
	if !strings.Contains(errOut, "train-a is training") {
		t.Errorf("stderr %q does not name the training", errOut)
	}
}
