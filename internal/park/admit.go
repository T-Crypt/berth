// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package park

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Budget is the RAM picture one park decision runs on. Available is what
// the kernel reports available now and Parked what parked engines hold:
// both are readings, not reservations, and neither is promised to anyone.
// The RAM parked engines hold is already gone from Available, so Parked
// only counts toward Max.
type Budget struct {
	Available uint64 // the kernel's available RAM, in bytes
	Floor     uint64 // the RAM the machine must keep free, in bytes
	Parked    uint64 // the RAM parked engines already hold, in bytes
	Max       uint64 // the cap on Parked plus a new footprint, in bytes; 0 = no cap
}

// Parked is one parked engine: what stopping it again would give back.
type Parked struct {
	Name  string
	Bytes uint64
	Since time.Time
	TTL   time.Duration // its own ttl; 0 = Evict's
}

const gib = uint64(1) << 30

func gib1(n uint64) string {
	return strconv.FormatFloat(float64(n)/float64(gib), 'f', 1, 64)
}

// Admit says whether footprint may park on top of b: it must fit in
// Available above Floor, and Parked plus footprint must stay within Max.
// It refuses rather than underflow, and its refusal lines say what is
// available, never what is reserved.
func Admit(b Budget, footprint uint64) (bool, string) {
	var above uint64
	if b.Available >= b.Floor {
		above = b.Available - b.Floor
	}
	if above >= footprint {
		if b.Max == 0 || b.Parked+footprint <= b.Max {
			return true, ""
		}
		return false, fmt.Sprintf("park refused: %s GiB parked would pass the %s GiB cap", gib1(b.Parked+footprint), gib1(b.Max))
	}
	return false, fmt.Sprintf("park refused: needs %s GiB, %s GiB available above the floor", gib1(footprint), gib1(above))
}

// Evict names the parked engines to stop, in the order to stop them in.
// First every entry older than ttl. Then, if the RAM the kernel reports
// available sits below the floor, the least recently parked, until it is
// back at the floor: stopping an engine gives its Bytes back to the
// kernel, and the RAM a ttl entry just gave back counts toward the floor
// too. Then, if the memory pressure figure is over threshold, one more,
// the least recently parked, even when the RAM is fine; a call evicts at
// most one this way, so pressure drains the pool a step at a time.
func Evict(parked []Parked, b Budget, pressure, threshold float64, now time.Time, ttl time.Duration) []string {
	byAge := slices.Clone(parked)
	slices.SortFunc(byAge, func(x, y Parked) int { return x.Since.Compare(y.Since) })
	taken := map[string]bool{}
	var out []string
	stop := func(p Parked) {
		out = append(out, p.Name)
		taken[p.Name] = true
	}
	avail := b.Available
	for _, p := range byAge {
		if now.Sub(p.Since) > cmp.Or(p.TTL, ttl) {
			stop(p)
			avail += p.Bytes
		}
	}
	for _, p := range byAge {
		if taken[p.Name] || avail >= b.Floor {
			break
		}
		stop(p)
		avail += p.Bytes
	}
	if pressure > threshold {
		for _, p := range byAge {
			if !taken[p.Name] {
				stop(p)
				break
			}
		}
	}
	return out
}

// Ledger is the machine's parking RAM, shared by every card's broker: what
// each card has parking (admitted, not yet out of the kernel's available
// RAM) and parked. Admit reads the other cards' figures and records the
// admitted park under one lock, so two cards never admit parks against the
// same free RAM. A nil Ledger is a machine of one card.
type Ledger struct {
	mu    sync.Mutex
	cards map[string]Totals
}

// Totals is one card's parking RAM, in bytes.
type Totals struct {
	Parking uint64 // admitted parks that have not landed yet
	Parked  uint64 // what parked engines hold
	ToCome  uint64 // what running and waiting engines are expected to climb back to
}

// Set records card's figures; the card's broker calls it every tick.
func (l *Ledger) Set(card string, t Totals) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cards == nil {
		l.cards = map[string]Totals{}
	}
	l.cards[card] = t
}

// Others is the other cards' figures added up.
func (l *Ledger) Others(card string) Totals {
	if l == nil {
		return Totals{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.others(card)
}

func (l *Ledger) others(card string) Totals {
	var t Totals
	for c, ct := range l.cards {
		if c != card {
			t.Parking += ct.Parking
			t.Parked += ct.Parked
			t.ToCome += ct.ToCome
		}
	}
	return t
}

// Admit decides a park of cost on card, whose own figures are mine, against
// b with the other cards added: their parking RAM and the RAM their engines
// climb back to come off Available, and their parked RAM goes onto Parked.
// An admitted park counts as parking on card at once.
func (l *Ledger) Admit(card string, mine Totals, b Budget, cost uint64) (bool, string) {
	if l == nil {
		return Admit(b, cost)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	o := l.others(card)
	b.Available -= min(b.Available, o.Parking+o.ToCome)
	b.Parked += o.Parked
	ok, why := Admit(b, cost)
	if ok {
		if l.cards == nil {
			l.cards = map[string]Totals{}
		}
		mine.Parking += cost
		l.cards[card] = mine
	}
	return ok, why
}
