// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package proto

import (
	"fmt"
	"slices"
	"time"
)

// BookingKind is what a booking does to its card while it holds.
type BookingKind string

const (
	// BookRun is an engine's turn on the card: its waiter goes first, it
	// runs at the booking's priority, and its holder keeps the card against
	// everything but a hold until the booking ends.
	BookRun BookingKind = "run"
	// BookPriority moves an engine to another priority class for the
	// booking's hours. It claims no time on the card.
	BookPriority BookingKind = "priority"
	// BookDown is downtime: the card takes no new work, and its holders
	// drain when it starts. A hold and a training stay.
	BookDown BookingKind = "down"
)

// Booking is one entry on a card's schedule, [Start, End) on the node's
// clock.
type Booking struct {
	ID       string      `json:"id" yaml:"id"`
	Card     string      `json:"card" yaml:"card"`
	Kind     BookingKind `json:"kind" yaml:"kind"`
	Who      string      `json:"who" yaml:"who"`
	Engine   string      `json:"engine,omitempty" yaml:"engine,omitempty"`
	Priority string      `json:"priority,omitempty" yaml:"priority,omitempty"`
	Start    time.Time   `json:"start" yaml:"start"`
	End      time.Time   `json:"end" yaml:"end"`
	Note     string      `json:"note,omitempty" yaml:"note,omitempty"`
	// Claim lets a run's waiter drain any holder but a hold or a pinned
	// one (a training, berth run --pinned), whatever the holder's class.
	Claim bool `json:"claim,omitempty" yaml:"claim,omitempty"`
}

// Holds is whether t falls inside the booking.
func (b Booking) Holds(t time.Time) bool { return !t.Before(b.Start) && t.Before(b.End) }

// Claims is whether the booking takes the card's time, so two of them may
// not overlap: a run or a downtime does, a priority change does not.
func (b Booking) Claims() bool { return b.Kind == BookRun || b.Kind == BookDown }

// Clashes is whether b and o may not share any hour on one card: two that
// claim the card, or two priority changes for the same engine.
func (b Booking) Clashes(o Booking) bool {
	if b.Card != o.Card || !b.Start.Before(o.End) || !o.Start.Before(b.End) {
		return false
	}
	if b.Claims() && o.Claims() {
		return true
	}
	return b.Kind == BookPriority && o.Kind == BookPriority && b.Engine == o.Engine
}

// MaxBookingAhead is how far ahead a booking may start, and MaxBookingLen
// how long one may run.
const (
	MaxBookingAhead = 14 * 24 * time.Hour
	MaxBookingLen   = 7 * 24 * time.Hour
)

var priorities = []string{"batch", "interactive", "foreground"}

// Check is what a booking must be before it goes on a schedule at now: a
// known kind, a name for who asked, an engine for a run or a priority
// change, a priority class where it takes one, and hours that end in the
// future, start inside the horizon and run no longer than a week.
func (b Booking) Check(now time.Time) error {
	switch b.Kind {
	case BookRun, BookPriority, BookDown:
	default:
		return fmt.Errorf("kind %q: use run, priority or down", b.Kind)
	}
	switch {
	case b.Who == "":
		return fmt.Errorf("name who the booking is for")
	case b.Kind != BookDown && b.Engine == "":
		return fmt.Errorf("a %s booking names its engine", b.Kind)
	case b.Kind == BookPriority && b.Priority == "":
		return fmt.Errorf("a priority booking names its priority: batch, interactive or foreground")
	case b.Kind == BookDown && b.Priority != "":
		return fmt.Errorf("downtime takes no priority")
	case b.Claim && b.Kind != BookRun:
		return fmt.Errorf("only a run booking claims its hours")
	case b.Priority != "" && !slices.Contains(priorities, b.Priority):
		return fmt.Errorf("priority %q: use batch, interactive or foreground", b.Priority)
	case !b.End.After(b.Start):
		return fmt.Errorf("the booking ends before it starts")
	case !b.End.After(now):
		return fmt.Errorf("the booking ends in the past")
	case b.Start.Sub(now) > MaxBookingAhead:
		return fmt.Errorf("a booking starts at most %s ahead", MaxBookingAhead)
	case b.End.Sub(b.Start) > MaxBookingLen:
		return fmt.Errorf("a booking runs at most %s", MaxBookingLen)
	}
	return nil
}

// ScheduleBoard is the scheduler's view of a node, as berth schedule and
// GET /v1/berth/schedule give it: what holds a card, the waiters in grant
// order, and every booking that has not ended.
type ScheduleBoard struct {
	Now      time.Time `json:"now" yaml:"now"`
	Running  []Lease   `json:"running" yaml:"running"`
	Queued   []Lease   `json:"queued" yaml:"queued"`
	Bookings []Booking `json:"bookings" yaml:"bookings"`
}

// BoardOf is st's scheduler view at now, with empty lists, never null.
func BoardOf(st *Status, now time.Time) ScheduleBoard {
	b := ScheduleBoard{Now: now, Running: st.Leases, Queued: st.Waiting, Bookings: st.Bookings}
	if b.Running == nil {
		b.Running = []Lease{}
	}
	if b.Queued == nil {
		b.Queued = []Lease{}
	}
	if b.Bookings == nil {
		b.Bookings = []Booking{}
	}
	return b
}

// BookingAt is the booking of kind k for engine that holds at t, if any.
// An empty engine matches only downtime, which names none.
func BookingAt(bs []Booking, k BookingKind, engine string, t time.Time) (Booking, bool) {
	for _, b := range bs {
		if b.Kind == k && b.Engine == engine && b.Holds(t) {
			return b, true
		}
	}
	return Booking{}, false
}
