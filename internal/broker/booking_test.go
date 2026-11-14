// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// book is a booking on b's card from turnBase+from for length.
func book(_ *Broker, id string, k proto.BookingKind, engine, prio string, from, length time.Duration) proto.Booking {
	return proto.Booking{ID: id, Card: "gpu0", Kind: k, Who: "test", Engine: engine, Priority: prio,
		Start: turnBase.Add(from), End: turnBase.Add(from + length)}
}

func leaseRow(t *testing.T, b *Broker, engine string) proto.Lease {
	t.Helper()
	st := b.Status()
	for _, l := range append(st.Leases, st.Waiting...) {
		if l.Engine == engine {
			return l
		}
	}
	t.Fatalf("no lease for %s in %+v", engine, st)
	return proto.Lease{}
}

// A run booking is the engine's turn: its batch waiter runs at foreground,
// so an interactive holder drains for it, and status names the booking.
func TestRunBookingLiftsItsWaiterAndDrainsALowerHolder(t *testing.T) {
	b, _ := startTurnBroker(t, procs.Map{})
	b.SetBookings([]proto.Booking{book(b, "b1", proto.BookRun, "pi-pocket", "", -time.Minute, 5*time.Hour)})
	holder := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive, Needs: 20 * GiB}))

	w := acquireW(b, Request{Engine: "pi-pocket", Priority: Batch, Needs: 20 * GiB})
	pending(t, w.ch)
	if got, want := drained(t, holder), "pi-pocket (foreground) needs the card"; got != want {
		t.Errorf("drain = %q, want %q", got, want)
	}
	if row := leaseRow(t, b, "pi-pocket"); row.Priority != "foreground" || row.Booking != "b1" {
		t.Errorf("waiter = %s booking %q, want foreground booking b1", row.Priority, row.Booking)
	}
	holder.Release()
	b.Tick(context.Background())
	granted(t, b, "pi-pocket", w.ch)
}

// A tie is the holder's, except against the waiter's booked turn: a
// foreground holder with no booking drains for a run lifted to foreground.
func TestRunBookingWinsATie(t *testing.T) {
	b, _ := startTurnBroker(t, procs.Map{})
	b.cfg.JuggleWindow = time.Hour
	b.SetBookings([]proto.Booking{book(b, "b1", proto.BookRun, "sft", "", 0, time.Hour)})
	holder := must(t, acquire(b, Request{Engine: "chat", Priority: Foreground, Needs: 20 * GiB}))
	w := acquireW(b, Request{Engine: "sft", Priority: Batch, Needs: 20 * GiB})
	pending(t, w.ch)
	if got, want := drained(t, holder), "sft (foreground) needs the card"; got != want {
		t.Errorf("drain = %q, want %q", got, want)
	}
}

// Inside its run booking a holder keeps the card against a foreground
// waiter; once the booking ends it is back to its own class and drains.
func TestRunBookingKeepsItsHolderUntilItEnds(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	b.SetBookings([]proto.Booking{book(b, "b1", proto.BookRun, "train", "", 0, 3*time.Hour)})
	holder := must(t, acquire(b, Request{Engine: "train", Priority: Batch, Needs: 20 * GiB}))

	w := acquireW(b, Request{Engine: "game", Priority: Foreground, Needs: 20 * GiB})
	pending(t, w.ch)
	select {
	case why := <-holder.Drains():
		t.Fatalf("a booked holder was drained inside its booking: %s", why)
	default:
	}
	if got, want := firstReason(t, w), "held by train [foreground, active]"; !strings.HasSuffix(got, want) {
		t.Errorf("reason = %q, want it to end %q", got, want)
	}

	clk.set(turnBase.Add(3 * time.Hour))
	b.Tick(context.Background())
	if got, want := drained(t, holder), "game (foreground) needs the card"; got != want {
		t.Errorf("drain = %q, want %q", got, want)
	}
	if row := leaseRow(t, b, "train"); row.Priority != "batch" || row.Booking != "" {
		t.Errorf("holder after the booking = %s booking %q, want batch and none", row.Priority, row.Booking)
	}
}

// A run booking puts its waiter ahead of an earlier foreground one.
func TestRunBookingGoesFirstInLine(t *testing.T) {
	b, _ := startTurnBroker(t, procs.Map{})
	b.SetBookings([]proto.Booking{book(b, "b1", proto.BookRun, "pi-pocket", "batch", 0, time.Hour)})
	holder := must(t, acquire(b, Request{Engine: "hold", Priority: Hold, Needs: 20 * GiB}))
	early := acquireW(b, Request{Engine: "chat", Priority: Foreground, Needs: 20 * GiB})
	pending(t, early.ch)
	booked := acquireW(b, Request{Engine: "pi-pocket", Priority: Batch, Needs: 20 * GiB})
	pending(t, booked.ch)
	if st := b.Status(); len(st.Waiting) != 2 || st.Waiting[0].Engine != "pi-pocket" {
		t.Fatalf("queue = %+v, want pi-pocket first", st.Waiting)
	}
	holder.Release()
	b.Tick(context.Background())
	granted(t, b, "pi-pocket", booked.ch)
}

// A priority booking moves an engine's class for its hours and back after,
// with an event each way; the operator's own berth queue prio wins over it.
func TestPriorityBookingMovesTheClassAndBack(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	b.SetBookings([]proto.Booking{book(b, "b2", proto.BookPriority, "dirk", "batch", time.Hour, time.Hour)})
	must(t, acquire(b, Request{Engine: "hold", Priority: Hold, Needs: 20 * GiB}))
	w := acquireW(b, Request{Engine: "dirk", Priority: Foreground, Needs: 20 * GiB})
	pending(t, w.ch)
	if row := leaseRow(t, b, "dirk"); row.Priority != "foreground" {
		t.Fatalf("before the booking = %s, want foreground", row.Priority)
	}
	clk.set(turnBase.Add(90 * time.Minute))
	b.Tick(context.Background())
	if row := leaseRow(t, b, "dirk"); row.Priority != "batch" || row.Booking != "b2" {
		t.Errorf("inside the booking = %s booking %q, want batch b2", row.Priority, row.Booking)
	}
	clk.set(turnBase.Add(2 * time.Hour))
	b.Tick(context.Background())
	if row := leaseRow(t, b, "dirk"); row.Priority != "foreground" || row.Booking != "" {
		t.Errorf("after the booking = %s booking %q, want foreground and none", row.Priority, row.Booking)
	}
	evs, _, _ := b.Events(0, false)
	var moves []string
	for _, e := range evs {
		if e.Kind == proto.EvBookingPrio {
			moves = append(moves, e.Detail)
		}
	}
	want := []string{"priority foreground -> batch: booking b2", "priority batch -> foreground: booking b2 ended"}
	if len(moves) != 2 || moves[0] != want[0] || moves[1] != want[1] {
		t.Errorf("events = %q, want %q", moves, want)
	}

	clk.set(turnBase.Add(90 * time.Minute))
	id := leaseRow(t, b, "dirk").ID
	if _, err := b.Queue(id, QueuePrio, "interactive"); err != nil {
		t.Fatal(err)
	}
	b.Tick(context.Background())
	if row := leaseRow(t, b, "dirk"); row.Priority != "interactive" {
		t.Errorf("after berth queue prio inside the booking = %s, want interactive", row.Priority)
	}
}

// Downtime drains its holder at the start, refuses new work with the
// booking's reason, and still lets a hold in.
func TestDowntimeDrainsAndTakesNoNewWork(t *testing.T) {
	b, clk := startTurnBroker(t, procs.Map{})
	d := book(b, "b3", proto.BookDown, "", "", time.Hour, 30*time.Minute)
	d.Who, d.Note = "operator", "driver update"
	b.SetBookings([]proto.Booking{d})
	holder := must(t, acquire(b, Request{Engine: "chat", Priority: Interactive, Needs: 8 * GiB}))

	clk.set(turnBase.Add(time.Hour))
	b.Tick(context.Background())
	want := "the card is down until " + d.End.Format("Mon 15:04") + " (booking b3 by operator: driver update)"
	if got := drained(t, holder); got != want {
		t.Errorf("drain = %q, want %q", got, want)
	}
	holder.Release()
	b.Tick(context.Background())

	w := acquireW(b, Request{Engine: "chat", Priority: Foreground, Needs: 8 * GiB})
	pending(t, w.ch)
	if got := firstReason(t, w); got != "next in line: "+want {
		t.Errorf("waiter reason = %q, want %q", got, want)
	}
	must(t, acquire(b, Request{Engine: "game", Priority: Hold, Needs: 8 * GiB}))

	clk.set(turnBase.Add(90 * time.Minute))
	b.Tick(context.Background())
	granted(t, b, "chat", w.ch)
}
