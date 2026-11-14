// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package eventlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

func open(t *testing.T, path string) *Log {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func kinds(evs []proto.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

// A restarted berthd shows the events from before it, and numbers its own
// after them, so `berth events --since N` and `berth why` read across a
// deploy.
func TestOpenKeepsTheStreamAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l := open(t, path)
	l.Emit(proto.Event{Kind: proto.EvRequested, Lease: "strata-7", At: time.Unix(10, 0)})
	l.Emit(proto.Event{Kind: proto.EvGranted, Lease: "strata-7", At: time.Unix(11, 0)})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2 := open(t, path)
	if got := l2.Last(); got != 2 {
		t.Fatalf("Last after the restart = %d, want 2", got)
	}
	l2.Emit(proto.Event{Kind: proto.EvDraining, Lease: "strata-7"})
	evs, _, _ := l2.Events(0, false)
	if got := kinds(evs); len(got) != 3 || got[0] != proto.EvRequested || got[2] != proto.EvDraining {
		t.Fatalf("events after the restart = %v", got)
	}
	if evs[2].Seq != 3 {
		t.Fatalf("the first event after the restart is seq %d, want 3", evs[2].Seq)
	}
	if got := l2.LastLease(); got != 7 {
		t.Fatalf("LastLease = %d, want 7", got)
	}
}

// A line cut short by a crash is skipped, not the whole file.
func TestOpenSkipsATornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := `{"seq":1,"kind":"lease.requested","lease":"a-1"}` + "\n" + `{"seq":2,"kind":"lease.gr`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	l := open(t, path)
	l.Emit(proto.Event{Kind: proto.EvGranted, Lease: "a-1"})
	evs, _, _ := l.Events(0, false)
	if got := kinds(evs); len(got) != 2 || got[1] != proto.EvGranted || evs[1].Seq != 2 {
		t.Fatalf("events = %v (%+v)", got, evs)
	}
	l.Close()
	l3 := open(t, path)
	if got := l3.Last(); got != 2 {
		t.Fatalf("Last after a torn line and a restart = %d, want 2", got)
	}
}

// The file is moved aside once it passes its cap, so it never grows without
// bound, and the events in the moved file still come back.
func TestOpenRotatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l := open(t, path)
	l.maxFile = 200
	for range 10 {
		l.Emit(proto.Event{Kind: proto.EvRequested, Lease: "comfyui-1", Detail: "a detail line"})
	}
	l.Close()
	if fi, err := os.Stat(path); err != nil || fi.Size() > 400 {
		t.Fatalf("the live file was not rotated: %v %v", fi, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	l2 := open(t, path)
	if got := l2.Last(); got != 10 {
		t.Fatalf("Last after rotation = %d, want 10", got)
	}
}

// A first start has no state directory yet; Open makes it, as the other
// state files do.
func TestOpenMakesTheStateDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "berth", "events.jsonl")
	l := open(t, path)
	l.Emit(proto.Event{Kind: proto.EvNodeStarted})
	l.Close()
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("no events written: %v %v", fi, err)
	}
}
