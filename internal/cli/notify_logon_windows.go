// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// The tray runs in the user's session, so it starts at logon from the
// user's Run key, never as a service: a service runs in session 0, which
// has no desktop.
const (
	logonRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	notifyValue = "BerthNotify"
)

// notifyAtLogon starts berth notify at the user's logon, or stops starting
// it when on is false. It returns the change in words for the report.
func notifyAtLogon(exe string, on, dryRun bool) (string, error) {
	if !on {
		line := `reg delete HKCU\` + logonRunKey + ` /v ` + notifyValue + ` /f`
		if dryRun {
			return line, nil
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
	cmd := `"` + exe + `" notify`
	line := `reg add HKCU\` + logonRunKey + ` /v ` + notifyValue + ` /d "` + cmd + `"`
	if dryRun {
		return line, nil
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, logonRunKey, registry.SET_VALUE)
	if err != nil {
		return line, err
	}
	defer k.Close()
	return line, k.SetStringValue(notifyValue, cmd)
}
