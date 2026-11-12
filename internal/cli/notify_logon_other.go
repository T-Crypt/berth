// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package cli

// notifyAtLogon has nothing to do here: on Linux berth notify is a user
// unit, written with the others.
func notifyAtLogon(string, bool, bool) (string, error) { return "", nil }
