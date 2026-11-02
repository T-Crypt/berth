// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

// berth unpark and berth park against a live daemon: usage errors, nothing
// parked is no error, and the park switch round trip in human and JSON.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUnparkUsage(t *testing.T) {
	isolate(t)
	sock, _, _ := startStopDaemon(t, time.Hour)
	for _, args := range [][]string{{"unpark"}, {"unpark", "a", "--all"}, {"unpark", "a", "b"}} {
		if code, _, _ := runCLI(t, append(args, "--socket", sock)...); code != ExitFail {
			t.Fatalf("%v exit = %d, want %d", args, code, ExitFail)
		}
	}
}

func TestUnparkNothingParked(t *testing.T) {
	isolate(t)
	sock, b, c := startStopDaemon(t, time.Hour)
	grantEngine(t, b, c, "ninfer")
	code, out, errOut := runCLI(t, "unpark", "--all", "--socket", sock)
	if code != ExitOK || !strings.Contains(out, "nothing parked") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
}

func TestParkSwitchRoundTrip(t *testing.T) {
	isolate(t)
	sock, b, _ := startStopDaemon(t, time.Hour)
	code, out, errOut := runCLI(t, "park", "off", "--socket", sock)
	if code != ExitOK || !strings.Contains(out, "parking is off") || !b.ParkOff() {
		t.Fatalf("park off: exit %d out %q err %q", code, out, errOut)
	}
	code, out, _ = runCLI(t, "park", "--socket", sock, "--format", "json")
	var v struct {
		Parking string   `json:"parking"`
		Evicted []string `json:"evicted"`
	}
	if code != ExitOK || json.Unmarshal([]byte(out), &v) != nil || v.Parking != "off" {
		t.Fatalf("park status json: exit %d out %q", code, out)
	}
	if code, out, _ = runCLI(t, "park", "on", "--socket", sock); code != ExitOK || !strings.Contains(out, "parking is on") || b.ParkOff() {
		t.Fatalf("park on: exit %d out %q", code, out)
	}
	if code, _, _ = runCLI(t, "park", "sideways", "--socket", sock); code != ExitFail {
		t.Fatalf("bad arg exit = %d", code)
	}
}
