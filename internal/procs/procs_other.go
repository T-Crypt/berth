// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package procs

import "errors"

// System has no parent lookup on this platform, so only a
// wrapper's direct child PID is attributed to its lease.
type System struct{}

func (System) Parent(int) int       { return 0 }
func (System) StartTime(int) uint64 { return 0 }

// List has no process table on this platform, so no descendants are
// recorded.
func List() []Proc { return nil }

// Container has no cgroups on this platform, so no process is ever in a
// container: engines run on the host.
func Container(int) (string, bool) { return "", false }

// Scope has no cgroups on this platform, so no process runs in a Berth
// scope.
func Scope(int) (string, bool) { return "", false }

// ScopeMem has no cgroups on this platform, so no scope is ever measured.
func ScopeMem(string) (Mem, bool) { return Mem{}, false }

// SetOOMScoreAdj has no OOM score on this platform.
func SetOOMScoreAdj(int, int) (int, error) { return 0, errors.ErrUnsupported }
