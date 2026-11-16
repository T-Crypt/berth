// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// The tray runs in the user's session, so it starts at logon from the
// user's Run key, never as a service: a service runs in session 0, which
// has no desktop.
const (
	logonRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	notifyValue = "BerthNotify"
)

// notifyLauncher is the Run value that starts berth notify at logon, and
// the stub it runs. berth.exe is a console program, so the bare exe in the
// Run key opened a console at every logon, and closing that window ended
// the tray (issue 344). The stub is the one the other units start through:
// wscript opens no console, runs berth notify hidden and appends its
// output to the unit's log.
func notifyLauncher(exe string) (run, stub string, err error) {
	u := notifyUnit(exe)
	stub, err = unitStub(u)
	return strings.Join(taskAction(u), " "), stub, err
}

// notifyAtLogon starts berth notify at the user's logon, or stops starting
// it when on is false. It returns the change in words for the report.
func notifyAtLogon(exe string, on, dryRun bool) (string, error) {
	stubPath := unitStubPath(notifyUnit("").Name)
	if !on {
		line := `reg delete HKCU\` + logonRunKey + ` /v ` + notifyValue + ` /f`
		if dryRun {
			return line, nil
		}
		if err := os.Remove(stubPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return line, err
		}
		k, err := registry.OpenKey(registry.CURRENT_USER, logonRunKey, registry.SET_VALUE)
		if err != nil {
			return line, nil // no Run key: nothing starts it
		}
		defer k.Close()
		if err := k.DeleteValue(notifyValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return line, err
		}
		return line, nil
	}
	run, stub, err := notifyLauncher(exe)
	line := `reg add HKCU\` + logonRunKey + ` /v ` + notifyValue + ` /d "` + run + `"`
	if err != nil || dryRun {
		return line, err
	}
	// UTF-16 with a BOM is a script wscript reads, whatever the paths hold;
	// the log's directory exists before the first start, since the
	// redirection creates the file but not the path to it.
	for _, dir := range []string{filepath.Dir(stubPath), filepath.Dir(unitLogPath(notifyUnit("").Name))} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return line, err
		}
	}
	if err := os.WriteFile(stubPath, utf16le(stub), 0o644); err != nil {
		return line, err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, logonRunKey, registry.SET_VALUE)
	if err != nil {
		return line, err
	}
	defer k.Close()
	return line, k.SetStringValue(notifyValue, run)
}
