// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// A lease on several cards. Each card's broker queues one share of it with
// the same lease ID and the same place in the node's arrival order, and
// schedules that share as it schedules any waiter: it drains holders the
// share outranks. A share that fits takes nothing. It keeps the head of its
// card's queue and pokes its gang, and the gang grants every share at once,
// under every card's lock, only when each one still fits. So no card is held
// idle while another is awaited. Two gangs never wait on each other: the one
// that arrived first stands ahead of the other on every card, and a gang
// takes the cards' locks in card-name order.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// shareQueued, when set, runs after each share is queued: tests use it to
// interleave two gangs.
var shareQueued func(engine, card string)

type share struct {
	b *Broker
	l *lease
}

type gang struct {
	shares []share // in card-name order
	kick   chan struct{}
}

// poke tells the gang a share fits. Called with that share's b.mu held, so
// it never blocks.
func (g *gang) poke() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// commit grants every share if every one is at the head of its card's queue
// and fits there now, and reports whether it did.
func (g *gang) commit(now time.Time) bool {
	for _, s := range g.shares {
		s.b.mu.Lock()
	}
	defer func() {
		for i := len(g.shares) - 1; i >= 0; i-- {
			g.shares[i].b.mu.Unlock()
		}
	}()
	for _, s := range g.shares {
		b := s.b
		if b.cardState != "" || b.observationWait(now) != "" || b.reattaching(now) > 0 {
			return false
		}
		b.sortQueue()
		if len(b.queue) == 0 || b.queue[0] != s.l {
			return false
		}
		ok, _, _ := b.evaluate(s.l, now)
		if !ok || (b.snap.APU && b.apuAdmissionWait() != "") {
			return false
		}
	}
	for _, s := range g.shares {
		s.b.grant(s.l, now)
	}
	for _, s := range g.shares {
		s.b.schedule(now)
	}
	return true
}

// AcquireAll asks for one lease on every card in cards, granted on all of
// them at once or on none, and returns its share on each card in the order
// given. One card is a plain Acquire. needs is the size on each card, in
// the order of cards; nil gives every share r.Needs. The cards must share
// the node's IDs and Seq.
func AcquireAll(ctx context.Context, cards []*Broker, needs []uint64, r Request, onWait func(string)) ([]*Lease, error) {
	per := shareRequests(cards, needs, r)
	if len(cards) == 1 {
		l, err := cards[0].Acquire(ctx, per[cards[0]], onWait)
		if err != nil {
			return nil, err
		}
		return []*Lease{l}, nil
	}
	sorted := slices.Clone(cards)
	slices.SortFunc(sorted, func(a, b *Broker) int { return strings.Compare(a.cfg.Card, b.cfg.Card) })
	for _, b := range sorted {
		b.mu.Lock()
		err := b.refuse(per[b])
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	id := fmt.Sprintf("%s-%d", r.Engine, sorted[0].cfg.IDs.Add(1))
	arrived := sorted[0].cfg.Seq.Add(1)
	g := &gang{kick: make(chan struct{}, 1)}
	waits := make(chan string, 8)
	done := make(chan struct{})
	defer close(done)
	for _, b := range sorted {
		b.mu.Lock()
		l := newLease(id, per[b], b.now())
		l.arrivedN, l.gang = arrived, g
		b.queue = append(b.queue, l)
		b.log.Info("acquire", "lease", l.id, "engine", l.engine, "priority", l.prio, "needs_mib", l.needs>>20, "cards", len(sorted))
		b.emit(proto.Event{Kind: proto.EvRequested, Lease: l.id, Engine: l.engine, Bytes: l.needs, Detail: l.prio.String()})
		g.shares = append(g.shares, share{b, l})
		b.mu.Unlock()
		if shareQueued != nil {
			shareQueued(l.engine, b.cfg.Card)
		}
		go func() {
			for {
				select {
				case why := <-l.waitCh:
					select {
					case waits <- b.cfg.Card + ": " + why:
					case <-done:
						return
					}
				case <-done:
					return
				}
			}
		}()
	}
	for _, s := range g.shares {
		s.b.mu.Lock()
		s.b.schedule(s.b.now())
		s.b.mu.Unlock()
	}

	wait := sorted[0].waitFor(r)
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	giveUp := func(why string) error {
		var first error
		for _, s := range g.shares {
			if err := s.b.abandon(s.l, why); first == nil {
				first = err
			}
		}
		return first
	}
	for {
		select {
		case <-g.kick:
			if g.commit(sorted[0].now()) {
				out := make([]*Lease, len(cards))
				for i, c := range cards {
					for _, s := range g.shares {
						if s.b == c {
							out[i] = &Lease{b: s.b, l: s.l}
						}
					}
				}
				return out, nil
			}
		case why := <-waits:
			if onWait != nil {
				onWait(why)
			}
		case <-ctx.Done():
			return nil, giveUp(ctx.Err().Error())
		case <-timeout:
			return nil, giveUp(fmt.Sprintf("gave up after %s", wait))
		}
	}
}

// ReattachAll leases every card in cards again, under one lease ID, to an
// engine that held a lease on all of them and outlived its daemon. It is
// all or none: a card that refuses hands back the shares already taken.
func ReattachAll(cards []*Broker, needs []uint64, r Request, pid int, start uint64) ([]*Lease, error) {
	per := shareRequests(cards, needs, r)
	id := fmt.Sprintf("%s-%d", r.Engine, cards[0].cfg.IDs.Add(1))
	out := make([]*Lease, 0, len(cards))
	for _, b := range cards {
		l, err := b.reattach(id, per[b], pid, start)
		if err != nil {
			for _, got := range out {
				got.Release()
			}
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// shareRequests is the request each card's share is made from: r, with that
// card's size from needs (in the order of cards) when needs is given.
func shareRequests(cards []*Broker, needs []uint64, r Request) map[*Broker]Request {
	per := make(map[*Broker]Request, len(cards))
	for i, b := range cards {
		rr := r
		if needs != nil {
			rr.Needs = needs[i]
		}
		per[b] = rr
	}
	return per
}
