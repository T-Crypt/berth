// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// The card's schedule. berthd's booking file says who has the card when,
// and the broker reads it on every tick. A run booking is an engine's turn:
// its waiter goes first, it runs at the booking's priority, and while it
// holds the card nothing but a hold drains it. A priority booking moves an
// engine to another class for its hours. Downtime takes no new work and
// drains its holders at the start. An hour nobody booked runs as it always
// did, by priority then arrival, so an unused booking leaves the card idle
// for nobody.

import (
	"fmt"
	"slices"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// SetBookings replaces the card's schedule with bs, and schedules the queue
// under it at once.
func (b *Broker) SetBookings(bs []proto.Booking) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bookings = slices.Clone(bs)
	now := b.now()
	b.closeForDowntime(now)
	b.schedule(now)
}

// booked is the booking that sets l's priority at now: a run, then a
// priority change, for its engine or its group. A hold, a training and a
// class the operator set by hand are not moved.
func (b *Broker) booked(l *lease, now time.Time) (proto.Booking, bool) {
	if l.basePrio == Hold || l.training || l.operatorPrio {
		return proto.Booking{}, false
	}
	for _, k := range []proto.BookingKind{proto.BookRun, proto.BookPriority} {
		for _, name := range []string{l.engine, l.group} {
			if name == "" {
				continue
			}
			if x, ok := proto.BookingAt(b.bookings, k, name, now); ok {
				return x, true
			}
		}
	}
	return proto.Booking{}, false
}

// inRun is whether a run booking holds for l at now.
func (b *Broker) inRun(l *lease, now time.Time) bool {
	x, ok := b.booked(l, now)
	return ok && x.Kind == proto.BookRun
}

// bookedPrio is the class a booking gives: its own, and foreground for a
// run that names none.
func bookedPrio(x proto.Booking) Priority {
	if x.Priority == "" {
		return Foreground
	}
	p, err := ParsePriority(x.Priority)
	if err != nil {
		return Foreground
	}
	return p
}

// applyBookings sets every lease's class from the schedule at now: the
// booking's while one holds, its own when none does. Each move is an event.
// Called with b.mu held.
func (b *Broker) applyBookings(now time.Time) {
	moved := false
	for _, l := range slices.Concat(b.queue, mapValues(b.leases)) {
		p, id := l.basePrio, ""
		if x, ok := b.booked(l, now); ok {
			p, id = bookedPrio(x), x.ID
		}
		if p == l.prio && id == l.booking {
			continue
		}
		detail := fmt.Sprintf("priority %s -> %s: booking %s", l.prio, p, id)
		if id == "" {
			detail = fmt.Sprintf("priority %s -> %s: booking %s ended", l.prio, p, l.booking)
		}
		l.prio, l.booking, moved = p, id, true
		b.emit(proto.Event{Kind: proto.EvBookingPrio, Lease: l.id, Engine: l.engine, Detail: detail})
		b.log.Info("booking priority", "lease", l.id, "engine", l.engine, "change", detail)
	}
	if moved {
		b.sortQueue()
	}
}

// closeForDowntime drains the active holders, and evicts the parked ones,
// when downtime holds at now. A hold and a training keep the card. A lease
// already on its way out is not asked twice. Called with b.mu held.
func (b *Broker) closeForDowntime(now time.Time) {
	d, ok := proto.BookingAt(b.bookings, proto.BookDown, "", now)
	if !ok {
		return
	}
	for _, l := range b.leases {
		if l.prio == Hold || l.training {
			continue
		}
		switch l.state {
		case active:
			b.drain(l, downWhy(d))
		case parked:
			b.evict(l, downWhy(d))
		}
	}
}

// downWhy is what a waiter and a drained holder read during downtime.
func downWhy(d proto.Booking) string {
	why := fmt.Sprintf("the card is down until %s (booking %s by %s", d.End.Format("Mon 15:04"), d.ID, d.Who)
	if d.Note != "" {
		why += ": " + d.Note
	}
	return why + ")"
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
