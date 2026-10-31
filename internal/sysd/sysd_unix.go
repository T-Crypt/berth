// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package sysd

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ScopeReady reports whether a systemd user manager can take a scope:
// systemd-run on PATH, and the manager's private socket in the runtime
// directory.
func ScopeReady() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "systemd", "private"))
	return err == nil
}

// ScopeArgv runs the command through the user manager's scope when one is
// reachable, so the command leaves the caller's cgroup and survives a
// restart or upgrade of whoever started it: the unit is named
// <prefix>-<name>-<rand>, --collect drops the scope when the run ends,
// --quiet keeps systemd-run's own notice out of the output. With --scope
// systemd-run execs the command in place, so its PID is unchanged.
// slice, when set, is a validated .slice unit name: the scope is placed
// under it, so the slice unit's resource policy (CPU, memory and swap
// limits) applies to the engine and everything it forks.
func ScopeArgv(prefix, name, slice string, argv []string) []string {
	// Tests start fake engines: never in the developer's own user manager.
	if testing.Testing() || !ScopeReady() {
		return argv
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	return scopeArgv(fmt.Sprintf("%s-%s-%x", prefix, UnitName(name), rnd), slice, argv)
}

// scopeArgv is the argv builder ScopeArgv runs when a manager is
// reachable: tests drive it directly, so the shape (and the slice
// placement) is asserted without touching a real manager.
func scopeArgv(unit, slice string, argv []string) []string {
	args := []string{"systemd-run", "--user", "--scope", "--unit", unit, "--collect", "--quiet"}
	if slice != "" {
		args = append(args, "--slice", slice)
	}
	return append(args, append([]string{"--"}, argv...)...)
}
