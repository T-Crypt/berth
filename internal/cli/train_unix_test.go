// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cli

import (
	"strings"
	"testing"
	"time"
)

// Through the real wire: while berth train runs, a berth run for the same
// card is refused at once, though it asked to wait an hour, and its command
// never starts.
func TestTrainLocksTheCardAgainstBerthRun(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	done := make(chan int, 1)
	go func() {
		code, _, _ := runCLI(t, "train", "--as", "train-a", "--socket", sock, "--", "sleep", "3")
		done <- code
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, out, _ := runCLI(t, "status", "--socket", sock)
		if strings.Contains(out, "train-a") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("train-a never held the card:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}

	start := time.Now()
	code, out, errOut := runCLI(t, "run", "--as", "chat", "--priority", "foreground", "--wait", "1h", "--socket", sock, "--", "echo", "started")
	if code == ExitOK || strings.Contains(out, "started") {
		t.Fatalf("berth run beside a training: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "locked for training by train-a") {
		t.Errorf("stderr %q does not name the training", errOut)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the refusal took %v, want it at once", took)
	}
	if code := <-done; code != ExitOK {
		t.Errorf("berth train exit = %d, want 0", code)
	}
}

// --exclusive is part of the run: on a card the driver names no UUID for,
// berth train --exclusive is refused after the grant and its command never
// starts, so it never runs without the guard it asked for.
func TestTrainExclusiveRefusesWithoutAnNVIDIACard(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "train", "--exclusive", "--as", "train-a", "--socket", sock, "--", "echo", "started")
	if code == ExitOK || strings.Contains(out, "started") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want a refusal and no start", code, out, errOut)
	}
	if !strings.Contains(errOut, "--exclusive") {
		t.Errorf("stderr %q does not name --exclusive", errOut)
	}
}

// --exclusive belongs to berth train alone.
func TestRunRefusesExclusive(t *testing.T) {
	isolate(t)
	code, _, errOut := runCLI(t, "run", "--exclusive", "--as", "x", "--", "true")
	if code != ExitSystem || !strings.Contains(errOut, "--exclusive is for berth train") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}
