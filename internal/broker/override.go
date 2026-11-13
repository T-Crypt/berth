// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// The operator's hand on the queue. Berth orders waiters by priority, then
// arrival, and nobody has to manage it; berth queue is for the times the
// operator wants it otherwise. Every change is an event, so the log shows
// that a person moved the line, not the scheduler.

import (
	"fmt"

	"github.com/T-Crypt/berth/internal/proto"
)

// QueueAction is one operator change to a waiter.
type QueueAction string

const (
	// QueueBump moves the waiter to the front of its priority class.
	QueueBump QueueAction = "bump"
	// QueuePrio moves the waiter to another priority class.
	QueuePrio QueueAction = "prio"
	// QueueDrop refuses the waiter, the way a timeout would.
	QueueDrop QueueAction = "drop"
)

// dropReason is what a dropped waiter is told.
const dropReason = "dropped from the queue by the operator (berth queue drop)"

// Queue applies one operator change to the waiter with lease ID id. prio is
// the new class for QueuePrio: batch, interactive or foreground. A hold is a
// pin, not a queue position, and a training's place is fixed by the lock, so
// neither can be set here. The returned line says what changed.
func (b *Broker) Queue(id string, action QueueAction, prio string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var w *lease
	for _, q := range b.queue {
		if q.id == id {
			w = q
		}
	}
	if w == nil {
		return "", notFound(fmt.Sprintf("no waiter %s on this card", id))
	}
	var detail string
	switch action {
	case QueueBump:
		w.bumpedN = b.order()
		b.sortQueue()
		detail = "moved to the front of its class by the operator"
	case QueuePrio:
		if w.training {
			return "", fmt.Errorf("%s is a training: its place is fixed by the card's lock", id)
		}
		p, err := ParsePriority(prio)
		if err != nil || p == Hold {
			return "", fmt.Errorf("priority %q: use batch, interactive or foreground", prio)
		}
		detail = fmt.Sprintf("priority %s -> %s by the operator", w.prio, p)
		w.prio = p
		b.sortQueue()
	case QueueDrop:
		select {
		case w.lockCh <- dropReason:
		default:
		}
		detail = "dropped by the operator"
	default:
		return "", fmt.Errorf("unknown queue action %q (bump, prio, drop)", action)
	}
	b.emit(proto.Event{Kind: proto.EvQueueOverride, Lease: w.id, Engine: w.engine, Detail: detail})
	b.log.Info("queue override", "lease", w.id, "engine", w.engine, "change", detail)
	return detail, nil
}
