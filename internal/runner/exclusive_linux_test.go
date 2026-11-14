// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSMI answers the exclusive mode's nvidia-smi calls: apps is what the
// compute-app query prints, sudoErr fails the mode change. It records every
// argv.
type fakeSMI struct {
	apps    string
	sudoErr error
	calls   [][]string
}

func (f *fakeSMI) cli(argv []string, _ time.Duration) (string, error) {
	f.calls = append(f.calls, argv)
	if argv[0] == "sudo" {
		return "", f.sudoErr
	}
	return f.apps, nil
}

// On an empty card the mode goes to exclusive-process before the run, and
// back to the default when it ends, on the granted cards only.
func TestExclusiveSetsAndRestoresTheMode(t *testing.T) {
	f := &fakeSMI{}
	restore, err := exclusive(Options{CLI: f.cli}, "GPU-a,GPU-b")
	if err != nil {
		t.Fatal(err)
	}
	set := []string{"sudo", "-n", "nvidia-smi", "-i", "GPU-a,GPU-b", "-c", "EXCLUSIVE_PROCESS"}
	if len(f.calls) != 2 || !slices.Equal(f.calls[1], set) {
		t.Fatalf("calls = %q, want the query then %q", f.calls, set)
	}
	restore(func(string, ...any) {})
	back := []string{"sudo", "-n", "nvidia-smi", "-i", "GPU-a,GPU-b", "-c", "DEFAULT"}
	if len(f.calls) != 3 || !slices.Equal(f.calls[2], back) {
		t.Fatalf("calls = %q, want %q last", f.calls, back)
	}
}

// A compute process already on the card would own its one context and lock
// the training out, so the run is refused and the mode is left alone.
func TestExclusiveRefusesWhenAProcessIsOnTheCard(t *testing.T) {
	f := &fakeSMI{apps: "4242, python3\n"}
	_, err := exclusive(Options{CLI: f.cli}, "GPU-a")
	if err == nil || !strings.Contains(err.Error(), "4242, python3") {
		t.Fatalf("err = %v, want a refusal naming the process", err)
	}
	for _, c := range f.calls {
		if c[0] == "sudo" {
			t.Fatalf("the mode was changed beside a running process: %q", c)
		}
	}
}

// Without a sudo rule the run is refused instead of starting without the
// guard the operator asked for.
func TestExclusiveRefusesWithoutSudo(t *testing.T) {
	f := &fakeSMI{sudoErr: errors.New("a password is required")}
	_, err := exclusive(Options{CLI: f.cli}, "GPU-a")
	if err == nil || !strings.Contains(err.Error(), "sudo rule") {
		t.Fatalf("err = %v, want it to name the sudo rule", err)
	}
}

// A card with no NVIDIA UUID (an AMD card) has no exclusive mode to set.
func TestExclusiveNeedsAnNVIDIACard(t *testing.T) {
	f := &fakeSMI{}
	if _, err := exclusive(Options{CLI: f.cli}, ""); err == nil {
		t.Fatal("exclusive mode set on a card with no UUID")
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %q, want none", f.calls)
	}
}
