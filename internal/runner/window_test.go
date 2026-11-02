// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// A run carries the hours it named to berthd on the acquire: nothing else
// tells the daemon them. The run that is outside them never starts its
// command, and the reason it comes back with says when the window opens.
func TestRunWaitsOutsideItsWindow(t *testing.T) {
	sock, _ := twoCardDaemon(t)
	// The next hour and the one after it: the minute the test runs in is
	// outside them whatever the clock says.
	m := time.Now().Hour()*60 + time.Now().Minute()
	win := proto.Window{Start: (m + 60) % minutesDay, End: (m + 120) % minutesDay}

	var logs syncBuf
	code := Run(Options{Socket: sock, Engine: "nightly", Priority: "batch", Card: "gpu0",
		Wait: 300 * time.Millisecond, Window: win, Grace: time.Second,
		Argv: []string{"sleep", "30"}, Stderr: &logs})
	want := "outside its window " + win.String() + "; opens in"
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d, a run outside its hours is refused: %s", code, ExitRefused, logs.String())
	}
	if !strings.Contains(logs.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, logs.String())
	}
}

const minutesDay = 24 * 60
