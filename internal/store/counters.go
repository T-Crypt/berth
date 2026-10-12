package store

import (
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// IngestCounters stores the node's running board totals for the minute of
// now. A later read in the same minute replaces the earlier one.
func (s *Store) IngestCounters(now time.Time, c proto.Counters) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO counters_1m
		(minute, since, collisions, handed_off, drains, stuck, squatters, contested, refused, juggles)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now.Truncate(time.Minute).Unix(), c.Since.Unix(), c.CollisionsAvoided, c.VRAMHandedOff,
		c.Drains, c.StuckCaught, c.SquattersCaught, c.ContestedGrants, c.Refused, c.JugglesStopped)
	return err
}

// CounterWindow is what the board counted over one range.
type CounterWindow struct {
	Range   string         `json:"range"`
	From    time.Time      `json:"from"`    // where the counting starts: now - range, or the first stored minute
	To      time.Time      `json:"to"`      // now
	Counts  proto.Counters `json:"counts"`  // counted in [from, to]; Since = From
	Partial bool           `json:"partial"` // the store holds less than the range
}

// CountersOver counts what the board counted in the d before now: the live
// totals minus the totals stored at the window's start. Between two readings
// the increase is summed; a reading whose board start (since) moved or whose
// count dropped is a node that started over, and its count is all new. The
// stored readings are a minute apart, so the window's start is exact to the
// minute.
func (s *Store) CountersOver(name string, d time.Duration, now time.Time, live proto.Counters) (CounterWindow, error) {
	w := CounterWindow{Range: name, From: now.Add(-d), To: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT minute, since, collisions, handed_off, drains, stuck, squatters, contested, refused, juggles
		FROM counters_1m
		WHERE minute >= COALESCE((SELECT MAX(minute) FROM counters_1m WHERE minute <= ?), 0)
		ORDER BY minute`, w.From.Unix())
	if err != nil {
		return w, err
	}
	defer rows.Close()
	var pts []proto.Counters
	var first int64
	for rows.Next() {
		var minute, since int64
		var c proto.Counters
		if err := rows.Scan(&minute, &since, &c.CollisionsAvoided, &c.VRAMHandedOff, &c.Drains,
			&c.StuckCaught, &c.SquattersCaught, &c.ContestedGrants, &c.Refused, &c.JugglesStopped); err != nil {
			return w, err
		}
		if len(pts) == 0 {
			first = minute
		}
		c.Since = time.Unix(since, 0)
		pts = append(pts, c)
	}
	if err := rows.Err(); err != nil {
		return w, err
	}
	if len(pts) == 0 {
		// Nothing stored yet: only the live board, which says when it began.
		w.Partial = true
		if live.Since.After(w.From) {
			w.From = live.Since
			w.Counts = live
		}
		w.Counts.Since = w.From
		return w, nil
	}
	if f := time.Unix(first, 0); f.After(w.From) {
		w.From, w.Partial = f, true
	}
	pts = append(pts, live)
	var sum proto.Counters
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		reset := b.Since.Unix() != a.Since.Unix()
		sum.CollisionsAvoided += step(a.CollisionsAvoided, b.CollisionsAvoided, reset)
		sum.VRAMHandedOff += step(a.VRAMHandedOff, b.VRAMHandedOff, reset)
		sum.Drains += step(a.Drains, b.Drains, reset)
		sum.StuckCaught += step(a.StuckCaught, b.StuckCaught, reset)
		sum.SquattersCaught += step(a.SquattersCaught, b.SquattersCaught, reset)
		sum.ContestedGrants += step(a.ContestedGrants, b.ContestedGrants, reset)
		sum.Refused += step(a.Refused, b.Refused, reset)
		sum.JugglesStopped += step(a.JugglesStopped, b.JugglesStopped, reset)
	}
	sum.Since = w.From
	w.Counts = sum
	return w, nil
}

func step(a, b uint64, reset bool) uint64 {
	if reset || b < a {
		return b
	}
	return b - a
}

// ParseCounterRange maps the board's window names to durations.
func ParseCounterRange(s string) (time.Duration, bool) {
	switch s {
	case "5m":
		return 5 * time.Minute, true
	case "15m":
		return 15 * time.Minute, true
	case "1h":
		return time.Hour, true
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}
