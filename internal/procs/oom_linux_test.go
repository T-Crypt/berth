// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package procs

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Berth raises a parked engine's score and gives it back, unprivileged.
func TestSetOOMScoreAdjRaisesAndRestores(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	read := func() int {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/oom_score_adj")
		if err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	was := read()
	old, err := SetOOMScoreAdj(pid, 1000)
	if err != nil || old != was || read() != 1000 {
		t.Fatalf("raise: old %d (was %d) err %v now %d", old, was, err, read())
	}
	if old, err = SetOOMScoreAdj(pid, was); err != nil || old != 1000 || read() != was {
		t.Fatalf("restore: old %d err %v now %d, want %d", old, err, read(), was)
	}
}
