// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package sysd

// ScopeReady has no user manager to answer to on this platform.
func ScopeReady() bool { return false }

// ScopeArgv has no user manager to answer to on this platform.
func ScopeArgv(prefix, name, slice string, argv []string) []string { return argv }
