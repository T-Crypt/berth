// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// sat1200 is Saturday 2026-11-14 12:00 on a fixed clock.
var sat1200 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func at(h, m int) time.Time { return time.Date(2026, 11, 14, h, m, 0, 0, time.UTC) }

func run(who string, from, to time.Time) proto.Booking {
	return proto.Booking{Card: "gpu0", Kind: proto.BookRun, Who: who, Engine: who, Start: from, End: to}
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "schedule.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

// A booking survives a restart, keeps its ID, and a second booking that
// claims the same hours on the card is refused with what it clashes with.
func TestBookPersistsAndRefusesAClash(t *testing.T) {
	s, p := open(t)
	made, err := s.Book(run("pi-pocket", at(23, 0), at(23, 0).Add(5*time.Hour)), 1, false, sat1200)
	if err != nil || len(made) != 1 || made[0].ID != "b1" {
		t.Fatalf("book = %+v, %v; want one booking b1", made, err)
	}
	_, err = s.Book(run("train", at(22, 0), at(23, 30)), 1, false, sat1200)
	if err == nil || !strings.Contains(err.Error(), "clashes with b1 (run for pi-pocket, Sat 23:00-Sun 04:00)") {
		t.Fatalf("clash err = %v", err)
	}
	again, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.List(sat1200); len(got) != 1 || got[0].ID != "b1" || got[0].Who != "pi-pocket" {
		t.Fatalf("after reopen = %+v, want b1 for pi-pocket", got)
	}
}

// A priority change claims no time: it books over a run, but not over
// another priority change for the same engine.
func TestPriorityBookingsOverlapRunsNotEachOther(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Book(run("train", at(13, 0), at(16, 0)), 1, false, sat1200); err != nil {
		t.Fatal(err)
	}
	p := proto.Booking{Card: "gpu0", Kind: proto.BookPriority, Who: "op", Engine: "dirk", Priority: "batch", Start: at(13, 0), End: at(16, 0)}
	if _, err := s.Book(p, 1, false, sat1200); err != nil {
		t.Fatalf("priority over a run: %v", err)
	}
	if _, err := s.Book(p, 1, false, sat1200); err == nil {
		t.Fatal("a second priority change for dirk in the same hours was booked")
	}
}

// Earliest moves a booking past every clash to the first free hours with
// its length; days books the same hours on each day, all or none.
func TestEarliestAndDays(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Book(run("op", at(13, 0), at(18, 0)), 1, false, sat1200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Book(run("pi", at(18, 0), at(19, 0)), 1, false, sat1200); err != nil {
		t.Fatal(err)
	}
	got, err := s.Book(run("train", at(14, 0), at(17, 0)), 1, true, sat1200)
	if err != nil || !got[0].Start.Equal(at(19, 0)) || !got[0].End.Equal(at(22, 0)) {
		t.Fatalf("earliest = %+v, %v; want 19:00-22:00", got, err)
	}
	nightly, err := s.Book(run("pocket", at(23, 0), at(23, 0).Add(5*time.Hour)), 7, false, sat1200)
	if err != nil || len(nightly) != 7 || !nightly[6].Start.Equal(at(23, 0).AddDate(0, 0, 6)) {
		t.Fatalf("7 nights = %d, %v", len(nightly), err)
	}
	before := len(s.List(sat1200))
	if _, err := s.Book(run("x", at(1, 0).AddDate(0, 0, 3), at(2, 0).AddDate(0, 0, 3)), 2, false, sat1200); err == nil {
		t.Fatal("a repeat that clashes on one day was booked")
	}
	if after := len(s.List(sat1200)); after != before {
		t.Fatalf("a refused repeat left %d bookings, want %d", after, before)
	}
}

// Rebook moves a booking and keeps its ID; unbook takes it off; an ended
// booking is no longer listed.
func TestRebookUnbookAndEnd(t *testing.T) {
	s, _ := open(t)
	made, _ := s.Book(run("pi", at(13, 0), at(14, 0)), 1, false, sat1200)
	moved := made[0]
	moved.Start, moved.End, moved.Priority = at(15, 0), at(17, 0), "batch"
	if _, err := s.Rebook(moved, sat1200); err != nil {
		t.Fatal(err)
	}
	if got := s.List(sat1200); len(got) != 1 || got[0].ID != "b1" || !got[0].Start.Equal(at(15, 0)) || got[0].Priority != "batch" {
		t.Fatalf("after rebook = %+v", got)
	}
	if got := s.List(at(17, 0)); len(got) != 0 {
		t.Fatalf("an ended booking is still listed: %+v", got)
	}
	if _, err := s.Unbook("b1", sat1200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unbook("b1", sat1200); err == nil {
		t.Fatal("unbooked twice")
	}
}

// A browser sends UTC; the schedule keeps and names the node's own clock, so
// a 23:00 booking at UTC-7 clashes "Sat 23:00", not "Sun 06:00".
func TestBookingsReadTheNodesClock(t *testing.T) {
	s, _ := open(t)
	mst := time.FixedZone("MST", -7*3600)
	now := sat1200.In(mst)
	night := time.Date(2026, 11, 14, 23, 0, 0, 0, mst)
	made, err := s.Book(run("pi", night.UTC(), night.Add(5*time.Hour).UTC()), 1, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := made[0].Start.Format("Mon 15:04 MST"); got != "Sat 23:00 MST" {
		t.Errorf("start = %s, want Sat 23:00 MST", got)
	}
	_, err = s.Book(run("x", night.UTC(), night.Add(time.Hour).UTC()), 1, false, now)
	if err == nil || !strings.Contains(err.Error(), "run Sat 23:00-Sun 00:00 clashes with b1 (run for pi, Sat 23:00-Sun 04:00)") {
		t.Errorf("clash = %v", err)
	}
}

func TestCheckRefusesBadBookings(t *testing.T) {
	for _, tc := range []struct {
		b    proto.Booking
		want string
	}{
		{proto.Booking{Kind: "nap", Who: "a", Start: at(13, 0), End: at(14, 0)}, "use run, priority or down"},
		{proto.Booking{Kind: proto.BookRun, Who: "a", Start: at(13, 0), End: at(14, 0)}, "names its engine"},
		{proto.Booking{Kind: proto.BookDown, Who: "a", Start: at(10, 0), End: at(11, 0)}, "ends in the past"},
		{proto.Booking{Kind: proto.BookDown, Who: "a", Start: at(13, 0), End: at(13, 0).AddDate(0, 0, 8)}, "at most"},
		{proto.Booking{Kind: proto.BookPriority, Who: "a", Engine: "e", Start: at(13, 0), End: at(14, 0)}, "names its priority"},
		{proto.Booking{Kind: proto.BookRun, Who: "a", Engine: "e", Priority: "hold", Start: at(13, 0), End: at(14, 0)}, `priority "hold"`},
	} {
		if err := tc.b.Check(sat1200); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want %q", tc.b, err, tc.want)
		}
	}
}

func TestParseAt(t *testing.T) {
	for in, want := range map[string]time.Time{
		"now":              sat1200,
		"22:00":            at(22, 0),
		"09:30":            at(9, 30).AddDate(0, 0, 1),
		"tue 22:00":        at(22, 0).AddDate(0, 0, 3),
		"sat 11:00":        at(11, 0).AddDate(0, 0, 7),
		"sat 13:00":        at(13, 0),
		"2026-11-15 06:00": at(6, 0).AddDate(0, 0, 1),
		"2026-11-15T06:00": at(6, 0).AddDate(0, 0, 1),
	} {
		got, err := ParseAt(in, sat1200)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseAt(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseAt("soon", sat1200); err == nil {
		t.Error("ParseAt(soon) took it")
	}
}
