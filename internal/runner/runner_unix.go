// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/T-Crypt/berth/internal/sysd"
)

var stopSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP}

// isolate puts the engine in its own process group, so a drain reaches the
// processes it forks (Strata's server spawns the engine that holds the memory).
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// afterStart has nothing to do: the process group was set at start.
func afterStart(*exec.Cmd) {}

// afterWait has nothing to do: the process group needs no handle.
func afterWait(*exec.Cmd) {}

// scopeToChild starts the engine in its own transient scope, so the
// lease can name every process under it by cgroup even when one escapes
// the process tree (chaos scenario 3: a child that forks and whose
// parent exits before any tick records it). The engine gets a scope of
// its own even when this run already sits in one (a gateway start, or a
// `berth run` inside another lease's engine, as llama-swap runs its
// models): two leases never share a scope. Without a user manager the
// child runs in this cgroup, as always.
func scopeToChild(as, slice string, argv []string, log *slog.Logger) []string {
	// Tests start fake engines: never in the developer's own user manager.
	if testing.Testing() {
		return argv
	}
	return pickScopeArgv(scopeAllowed(os.Getenv), sysd.ScopeReady(), as, slice, argv, log)
}

// pickScopeArgv places the engine in the scope. An operator who set a
// slice for its limits gets an engine that can swap when the scope is
// dropped, so a slice is never dropped quietly: the Warn names the
// engine, the slice and the reason. A plain run with no slice reads as
// always, at Debug.
func pickScopeArgv(allowed, ready bool, as, slice string, argv []string, log *slog.Logger) []string {
	if !allowed {
		logScopeDrop(log, as, slice, "inside a Type=notify service; the engine stays in its cgroup")
		return argv
	}
	if !ready {
		logScopeDrop(log, as, slice, "no systemd user manager; the engine runs in this run's cgroup")
		return argv
	}
	return sysd.ScopeArgv("berth-run", as, slice, argv)
}

// logScopeDrop tells the operator why the engine keeps the caller's
// cgroup, at Warn when a slice made that placement matter.
func logScopeDrop(log *slog.Logger, as, slice, reason string) {
	if log == nil {
		return
	}
	if slice == "" {
		log.Debug(reason, "engine", as)
		return
	}
	log.Warn(reason+"; the slice is dropped", "engine", as, "slice", slice)
}

// scopeAllowed is false inside a Type=notify service (NOTIFY_SOCKET set):
// systemd drops a readiness message sent from outside the service's
// cgroup, so the unit would never leave "activating".
func scopeAllowed(getenv func(string) string) bool {
	return getenv("NOTIFY_SOCKET") == ""
}

func signalGroup(cmd *exec.Cmd, s os.Signal) {
	if sig, ok := s.(syscall.Signal); ok {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
	}
}

func terminate(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
func kill(cmd *exec.Cmd)      { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

func exitCode(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}
