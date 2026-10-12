package store

import (
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

func board(since time.Time, collisions, drains uint64) proto.Counters {
	return proto.Counters{Since: since, CollisionsAvoided: collisions, Drains: drains}
}

// A window is the live totals minus the totals stored at its start.
func TestCountersOverWindow(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 10, 12, 12, 0, 30, 0, time.UTC)
	since := now.Add(-48 * time.Hour)
	// One reading a minute for two hours: collisions climb by one every 10 min.
	for m := 120; m >= 1; m-- {
		at := now.Add(-time.Duration(m) * time.Minute)
		if err := s.IngestCounters(at, board(since, uint64(100+(120-m)/10), 7)); err != nil {
			t.Fatal(err)
		}
	}
	live := board(since, 113, 9)
	for _, c := range []struct {
		name             string
		d                time.Duration
		collisions, drns uint64
	}{
		{"5m", 5 * time.Minute, 2, 2},   // 111 stored at -5m (to the minute) -> 113 live
		{"15m", 15 * time.Minute, 3, 2}, // 110 at -15m
		{"1h", time.Hour, 7, 2},         // 106 at -60m
	} {
		w, err := s.CountersOver(c.name, c.d, now, live)
		if err != nil {
			t.Fatal(err)
		}
		if w.Counts.CollisionsAvoided != c.collisions || w.Counts.Drains != c.drns || w.Partial {
			t.Errorf("%s: %+v", c.name, w)
		}
	}
	// 24h reaches before the first reading: what is stored, marked partial.
	w, err := s.CountersOver("24h", 24*time.Hour, now, live)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Partial || w.Counts.CollisionsAvoided != 13 || !w.From.Equal(now.Add(-120*time.Minute).Truncate(time.Minute)) {
		t.Errorf("24h: %+v", w)
	}
}

// A node that started over (an older berthd, or a lost board file) counts
// from zero again; the window adds what it counted after the restart
// instead of going negative.
func TestCountersOverRestart(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 10, 12, 12, 0, 30, 0, time.UTC)
	old, fresh := now.Add(-24*time.Hour), now.Add(-20*time.Minute)
	for _, r := range []struct {
		ago time.Duration
		c   proto.Counters
	}{
		{40 * time.Minute, board(old, 50, 5)},
		{30 * time.Minute, board(old, 52, 5)},  // +2
		{19 * time.Minute, board(fresh, 1, 0)}, // restart: +1
		{10 * time.Minute, board(fresh, 3, 1)}, // +2, drains +1
	} {
		if err := s.IngestCounters(now.Add(-r.ago), r.c); err != nil {
			t.Fatal(err)
		}
	}
	w, err := s.CountersOver("1h", time.Hour, now, board(fresh, 4, 1)) // live: +1
	if err != nil {
		t.Fatal(err)
	}
	if w.Counts.CollisionsAvoided != 6 || w.Counts.Drains != 1 {
		t.Fatalf("across a restart: %+v", w.Counts)
	}
}

// With nothing stored, a window shows the live board when the board began
// inside it, and nothing when it began before (no reading to subtract).
func TestCountersOverEmptyStore(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	w, err := s.CountersOver("1h", time.Hour, now, board(now.Add(-10*time.Minute), 3, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Partial || w.Counts.CollisionsAvoided != 3 {
		t.Fatalf("board began in the window: %+v", w)
	}
	w, err = s.CountersOver("5m", 5*time.Minute, now, board(now.Add(-10*time.Minute), 3, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Partial || w.Counts.CollisionsAvoided != 0 {
		t.Fatalf("board began before the window: %+v", w)
	}
}

func TestCountersPruned(t *testing.T) {
	s := openStore(t)
	now := time.Now()
	since := now.Add(-60 * 24 * time.Hour)
	if err := s.IngestCounters(now.Add(-40*24*time.Hour), board(since, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestCounters(now.Add(-time.Hour), board(since, 2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(now, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM counters_1m`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after prune = %d, %v", n, err)
	}
}
