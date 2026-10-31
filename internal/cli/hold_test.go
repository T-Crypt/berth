// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"strings"
	"testing"
)

// berth hold --gpu holds the card it names on a node of several, and leaves
// the others to their engines.
func TestHoldTakesTheCardItNames(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		gpu  string
		want []bool // held, per card
	}{
		{"gpu1", []bool{false, true, false}},
		{"gpu0,gpu2", []bool{true, false, true}},
		{"", []bool{true, false, false}},
	} {
		sock, brokers := serveNode(t, "gpu0", "gpu1", "gpu2")
		args := []string{"hold", "--socket", sock, "--for", "1s", "--reason", "game"}
		if tc.gpu != "" {
			args = append(args, "--gpu", tc.gpu)
		}
		done := make(chan string, 1)
		go func() { _, _, errOut := runCLI(t, args...); done <- errOut }()
		held := func(b int) bool { return slices.Contains(leaseNames(brokers[b]), "hold:game") }
		waitUntil(t, func() bool {
			for i, want := range tc.want {
				if want && !held(i) {
					return false
				}
			}
			return true
		})
		for i, want := range tc.want {
			if !want && held(i) {
				t.Errorf("--gpu %q: gpu%d is held too", tc.gpu, i)
			}
		}
		if errOut := <-done; !strings.Contains(errOut, "hold released") {
			t.Errorf("--gpu %q: the hold did not end cleanly: %q", tc.gpu, errOut)
		}
	}
}

// berth hold refuses a --gpu no card can be, before berthd is asked.
func TestHoldRefusesABadGPU(t *testing.T) {
	isolate(t)
	sock, brokers := serveNode(t, "gpu0", "gpu1")
	for _, bad := range []string{"card one", "gpu1,gpu1"} {
		code, _, errOut := runCLI(t, "hold", "--socket", sock, "--gpu", bad)
		if code == ExitOK || !strings.Contains(errOut, "gpu") {
			t.Errorf("--gpu %q: exit = %d, stderr %q; want a refusal naming the card", bad, code, errOut)
		}
	}
	for i, b := range brokers {
		if n := leaseNames(b); len(n) > 0 {
			t.Errorf("gpu%d holds %v after a refused hold", i, n)
		}
	}
}

// berth hold --gpu names a card berthd does not have: refused, nothing held.
func TestHoldRefusesACardTheNodeLacks(t *testing.T) {
	isolate(t)
	sock, _ := serveNode(t, "gpu0", "gpu1")
	code, _, errOut := runCLI(t, "hold", "--socket", sock, "--gpu", "gpu7", "--wait", "1s", "--for", "1s")
	if code == ExitOK || !strings.Contains(errOut, "no card gpu7") {
		t.Errorf("exit = %d, stderr %q; want a refusal that names gpu7", code, errOut)
	}
}
