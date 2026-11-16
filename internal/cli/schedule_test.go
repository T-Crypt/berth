// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/schedule"
	"github.com/T-Crypt/berth/internal/server"
)

// Booking, listing, moving and cancelling on a real berthd: the board shows
// the booking, a clash is refused with what it hits, and a booked waiter is
// queued first at the booking's class.
func TestScheduleAgainstADaemon(t *testing.T) {
	isolate(t)
	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.json"))
	if err != nil {
		t.Fatal(err)
	}
	sock := startDaemonWith(t, server.Extras{Schedule: s})

	code, out, errOut := runCLI(t, "schedule", "book", "--who", "pi-pocket", "--engine", "pocket", "--priority", "batch",
		"--at", "now", "--for", "5h", "--socket", sock)
	if code != 0 || !strings.Contains(out, "booked b1: run for pi-pocket") {
		t.Fatalf("book (exit %d): %s %s", code, out, errOut)
	}
	code, _, errOut = runCLI(t, "schedule", "book", "--who", "op", "--kind", "down", "--at", "now", "--for", "1h", "--socket", sock)
	if code != 1 || !strings.Contains(errOut, "clashes with b1 (run for pi-pocket") {
		t.Fatalf("clash (exit %d): %s", code, errOut)
	}

	holder := ask(t, sock, proto.Msg{Engine: "holder", Priority: "foreground", Pinned: true})
	if m, err := holder.Recv(); err != nil || m.Op != proto.OpGranted {
		t.Fatalf("holder: %+v, %v", m, err)
	}
	ask(t, sock, proto.Msg{Engine: "chat", Priority: "foreground", Wait: 30})
	waitUntil(t, func() bool { return len(queueJSON(t, sock)) == 1 })
	ask(t, sock, proto.Msg{Engine: "pocket", Priority: "interactive", Wait: 30})
	waitUntil(t, func() bool { return len(queueJSON(t, sock)) == 2 })

	code, out, errOut = runCLI(t, "schedule", "--socket", sock, "--format", "json")
	if code != 0 {
		t.Fatalf("schedule (exit %d): %s", code, errOut)
	}
	var board proto.ScheduleBoard
	if err := json.Unmarshal([]byte(out), &board); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(board.Running) != 1 || board.Running[0].Engine != "holder" {
		t.Errorf("running = %+v, want holder", board.Running)
	}
	if len(board.Queued) != 2 || board.Queued[0].Engine != "pocket" || board.Queued[0].Priority != "batch" || board.Queued[0].Booking != "b1" {
		t.Errorf("queued = %+v, want pocket first at batch under b1", board.Queued)
	}
	if len(board.Bookings) != 1 || board.Bookings[0].ID != "b1" {
		t.Errorf("bookings = %+v, want b1", board.Bookings)
	}

	code, out, errOut = runCLI(t, "schedule", "move", "b1", "--at", "23:59", "--socket", sock)
	if code != 0 || !strings.Contains(out, "moved b1") {
		t.Fatalf("move (exit %d): %s %s", code, out, errOut)
	}
	waitUntil(t, func() bool { return queueJSON(t, sock)[0].Engine == "chat" })

	code, out, errOut = runCLI(t, "schedule", "cancel", "b1", "--socket", sock)
	if code != 0 || !strings.Contains(out, "cancelled b1") {
		t.Fatalf("cancel (exit %d): %s %s", code, out, errOut)
	}
	code, out, _ = runCLI(t, "schedule", "--socket", sock)
	if code != 0 || !strings.Contains(out, "nothing booked") || !strings.Contains(out, "RUNNING") {
		t.Fatalf("board after cancel (exit %d): %s", code, out)
	}

	code, out, errOut = runCLI(t, "schedule", "book", "--who", "op", "--engine", "strata", "--priority", "batch", "--claim",
		"--at", "now", "--for", "1h", "--socket", sock)
	if code != 0 || !strings.Contains(out, "booked b2") {
		t.Fatalf("book --claim (exit %d): %s %s", code, out, errOut)
	}
	if got := s.List(time.Now()); len(got) != 1 || !got[0].Claim {
		t.Errorf("schedule after --claim = %+v, want one claiming run", got)
	}
	if code, out, _ = runCLI(t, "schedule", "--socket", sock); code != 0 || !strings.Contains(out, "run (claim)") {
		t.Errorf("board (exit %d) does not show the claim: %s", code, out)
	}
	code, _, errOut = runCLI(t, "schedule", "book", "--who", "op", "--kind", "down", "--claim", "--at", "23:00", "--for", "1h", "--socket", sock)
	if code != 1 || !strings.Contains(errOut, "only a run booking claims") {
		t.Errorf("down --claim (exit %d): %s", code, errOut)
	}
}
