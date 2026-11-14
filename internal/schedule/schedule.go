// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package schedule is the cards' booking file. berthd is the only process
// that writes it, so the CLI, the gateway and the harbor reach it through
// berthd's socket and no file lock is needed. A booking that has ended
// leaves the file on the next write.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/fsx"
	"github.com/T-Crypt/berth/internal/proto"
)

// MaxDays is how many consecutive days one request may book at once.
const MaxDays = 14

type file struct {
	Next     int             `json:"next"`
	Bookings []proto.Booking `json:"bookings"`
}

// Store is the booking file, held in memory once loaded.
type Store struct {
	path string
	mu   sync.Mutex
	f    file
}

// Open loads the file at path; a missing file is an empty schedule.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// List is every booking that has not ended at now, by start.
func (s *Store) List(now time.Time) []proto.Booking {
	s.mu.Lock()
	defer s.mu.Unlock()
	return live(s.f.Bookings, now)
}

func live(bs []proto.Booking, now time.Time) []proto.Booking {
	out := slices.DeleteFunc(slices.Clone(bs), func(b proto.Booking) bool { return !b.End.After(now) })
	for i := range out {
		out[i] = onClock(out[i], now)
	}
	slices.SortStableFunc(out, func(a, b proto.Booking) int { return a.Start.Compare(b.Start) })
	return out
}

// Book puts b on the schedule on days consecutive days (0 = 1), all of
// them or none. With earliest, each copy starts at the first hours at or
// after its own start where it clashes with nothing, keeping its length.
func (s *Store) Book(b proto.Booking, days int, earliest bool, now time.Time) ([]proto.Booking, error) {
	if days <= 0 {
		days = 1
	}
	if days > MaxDays {
		return nil, fmt.Errorf("book at most %d days at once", MaxDays)
	}
	b = onClock(b, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	all := live(s.f.Bookings, now)
	next := s.f.Next
	var made []proto.Booking
	for d := range days {
		c := b
		c.Start, c.End = b.Start.AddDate(0, 0, d), b.End.AddDate(0, 0, d)
		if earliest {
			c = firstFree(c, all)
		}
		next++
		c.ID = "b" + strconv.Itoa(next)
		if err := fits(c, all, now); err != nil {
			return nil, err
		}
		all = append(all, c)
		made = append(made, c)
	}
	return made, s.save(file{Next: next, Bookings: all})
}

// Unbook takes the booking id off the schedule.
func (s *Store) Unbook(id string, now time.Time) (proto.Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := live(s.f.Bookings, now)
	i := slices.IndexFunc(all, func(b proto.Booking) bool { return b.ID == id })
	if i < 0 {
		return proto.Booking{}, fmt.Errorf("no booking %s on the schedule", id)
	}
	gone := all[i]
	return gone, s.save(file{Next: s.f.Next, Bookings: slices.Delete(all, i, i+1)})
}

// Rebook replaces the booking with b.ID by b: new hours, priority, engine
// or note. It keeps its ID, and is checked as a new booking would be
// against every other one.
func (s *Store) Rebook(b proto.Booking, now time.Time) (proto.Booking, error) {
	b = onClock(b, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	all := live(s.f.Bookings, now)
	i := slices.IndexFunc(all, func(o proto.Booking) bool { return o.ID == b.ID })
	if i < 0 {
		return proto.Booking{}, fmt.Errorf("no booking %s on the schedule", b.ID)
	}
	rest := slices.Delete(slices.Clone(all), i, i+1)
	if err := fits(b, rest, now); err != nil {
		return proto.Booking{}, err
	}
	all[i] = b
	return b, s.save(file{Next: s.f.Next, Bookings: all})
}

// fits is whether b may join others: a valid booking that clashes with none.
func fits(b proto.Booking, others []proto.Booking, now time.Time) error {
	if err := b.Check(now); err != nil {
		return err
	}
	for _, o := range others {
		if b.Clashes(o) {
			return fmt.Errorf("%s %s-%s clashes with %s (%s for %s, %s-%s)", b.Kind, clock(b.Start), clock(b.End),
				o.ID, o.Kind, o.Who, clock(o.Start), clock(o.End))
		}
	}
	return nil
}

// firstFree moves b later, keeping its length, until it clashes with none
// of others. Each clash it steps past ends later than the last, so it stops.
func firstFree(b proto.Booking, others []proto.Booking) proto.Booking {
	length := b.End.Sub(b.Start)
	for {
		i := slices.IndexFunc(others, b.Clashes)
		if i < 0 {
			return b
		}
		b.Start = others[i].End
		b.End = b.Start.Add(length)
	}
}

func clock(t time.Time) string { return t.Format("Mon 15:04") }

// onClock is b with its hours read on now's clock, the node's: a browser
// sends UTC, and the file, the status and every message read local time.
func onClock(b proto.Booking, now time.Time) proto.Booking {
	b.Start, b.End = b.Start.In(now.Location()), b.End.In(now.Location())
	return b
}

func (s *Store) save(f file) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteFile(s.path, b, 0o600); err != nil {
		return err
	}
	s.f = f
	return nil
}
