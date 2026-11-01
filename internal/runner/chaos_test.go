// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

// Scenario 2 from contrib/chaos: an engine that ignores SIGTERM. The drain
// sends SIGTERM to the engine's own process group, waits out the grace, and
// then SIGKILLs that group; a bystander in another group is never touched
// and nothing else is signalled.

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/park"
)

// startStubborn starts an engine that swallows SIGTERM across the group:
// the shell traps it, and the sleep it waits in inherits the ignored
// disposition. The returned cmd is ready to drain only after the ready
// line arrives, which the shell prints once the trap is set.
func startStubborn(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `trap "" TERM; echo ready; sleep 30`)
	isolate(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		b := make([]byte, 16)
		_, _ = stdout.Read(b)
		close(ready)
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("the stubborn engine never set its trap")
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, waited
}

func TestChaosScenario2IgnoresTerm(t *testing.T) {
	hog, died := startStubborn(t)
	bystander, _ := startSleep(t)

	done := make(chan struct{})
	var logs strings.Builder
	drain(Options{Engine: "hog", Grace: 300 * time.Millisecond}, hog, newLine(nil), done, "test drain", sayTo(&logs))

	select {
	case err := <-died:
		if out := logs.String(); !strings.Contains(out, "ignored terminate for 300ms; killing") {
			t.Fatalf("the kill was not the escalation of an ignored stop; log:\n%s", out)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("exit error = %v", err)
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("the hog exited on %v, want SIGKILL", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the hog outlived its own drain; log:\n%s", &logs)
	}

	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the bystander did not survive the drain: %v; log:\n%s", err, &logs)
	}
}

// A checkpointed engine's CUDA calls block until a restore, so its own
// SIGTERM shutdown never finishes; the stubborn engine stands in for it.
// llama-swap's unload or ttl sends the wrapper SIGTERM and SIGKILLs it a
// few seconds later, so the forwarded stop has to end the engine at once
// or it outlives its lease holding its weights in RAM.
func TestStopSignalKillsCheckpointedEngine(t *testing.T) {
	hog, died := startStubborn(t)
	done := make(chan struct{})
	defer close(done)
	go forwardSignals(hog, done, func() bool { return true }, func() {})
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-died:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("exit error = %v", err)
		}
		ws, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("the parked engine exited on %v, want SIGKILL", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the parked engine outlived the stop signal")
	}
}

// berth stop or an eviction of a checkpointed engine kills it at once
// rather than sending a SIGTERM it cannot act on and waiting out the grace.
func TestDrainKillsParkedCheckpointedEngine(t *testing.T) {
	hog, died := startStubborn(t)
	line := newLine(nil)
	line.parked = true
	o := Options{Engine: "hog", Grace: 5 * time.Second, Checkpoint: true, Park: &park.Adapter{}}
	var logs strings.Builder
	start := time.Now()
	drain(o, hog, line, make(chan struct{}), "berth stop", sayTo(&logs))

	select {
	case <-died:
		if took := time.Since(start); took > time.Second {
			t.Fatalf("the parked engine took %v to die, want no grace; log:\n%s", took, &logs)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the parked engine outlived its drain; log:\n%s", &logs)
	}
}
