// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Batches. An agent that queues forty images wants the card for all of
// them, not one switch per image: every chat turn in between would drain
// ComfyUI at its job boundary and cost a model reload each way. A batch
// names an engine; while it lasts, that engine's lease keeps the card
// against any waiter but a hold (as a minimum turn does), and its waiting
// lease takes the card the way a person's switch does. The batch ends by
// itself once the engine has been idle (no job running or queued) for the
// grace, so a single image still goes back and forth as before.

import (
	"fmt"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

const (
	// DefaultBatchGrace is how long the engine must stay idle before a batch
	// ends: long enough for a script to submit its next job.
	DefaultBatchGrace = 30 * time.Second
	// DefaultBatchMax caps a batch nobody ends.
	DefaultBatchMax = 4 * time.Hour
)

type batch struct {
	grace    time.Duration
	deadline time.Time
	idleFrom time.Time // zero while the engine is busy or unread
}

// Batch starts a batch for engine (a lease's engine or group name), or
// renews one: the grace and the cap restart from now. Zero durations take
// the defaults.
func (b *Broker) Batch(engine string, grace, max time.Duration) {
	if grace <= 0 {
		grace = DefaultBatchGrace
	}
	if max <= 0 {
		max = DefaultBatchMax
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	_, renewed := b.batches[engine]
	b.batches[engine] = &batch{grace: grace, deadline: now.Add(max)}
	if !renewed {
		b.log.Info("batch started", "engine", engine, "grace", grace, "max", max)
		b.emit(proto.Event{Kind: proto.EvBatchStarted, Engine: engine,
			Detail: fmt.Sprintf("%s keeps the card until it has been idle %s (at most %s)", engine, grace, max)})
	}
	b.schedule(now)
}

// EndBatch ends engine's batch now; false when it had none.
func (b *Broker) EndBatch(engine string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.batches[engine]; !ok {
		return false
	}
	b.endBatch(engine, "ended by hand")
	b.schedule(b.now())
	return true
}

func (b *Broker) endBatch(engine, why string) {
	delete(b.batches, engine)
	b.log.Info("batch ended", "engine", engine, "why", why)
	b.emit(proto.Event{Kind: proto.EvBatchEnded, Engine: engine, Detail: engine + ": " + why})
}

// batchOf is the batch l runs under, keyed by its engine or its group.
func (b *Broker) batchOf(l *lease) (string, *batch) {
	for _, k := range []string{l.engine, l.group} {
		if bt, ok := b.batches[k]; k != "" && ok {
			return k, bt
		}
	}
	return "", nil
}

// pruneBatches ends the batches whose engine stayed idle for the grace, or
// that ran into their cap. An engine nobody can read counts as busy while a
// lease of it holds or waits for the card, and as idle once none does.
func (b *Broker) pruneBatches(now time.Time) {
	for k, bt := range b.batches {
		if !now.Before(bt.deadline) {
			b.endBatch(k, "reached its cap")
			continue
		}
		if b.engineBusy(k) {
			bt.idleFrom = time.Time{}
			continue
		}
		if bt.idleFrom.IsZero() {
			bt.idleFrom = now
		}
		if now.Sub(bt.idleFrom) >= bt.grace {
			b.endBatch(k, fmt.Sprintf("idle for %s", bt.grace))
		}
	}
}

func (b *Broker) engineBusy(k string) bool {
	if b.cfg.Busy != nil {
		if busy, known := b.cfg.Busy(k); known {
			return busy
		}
	}
	for _, l := range b.leases {
		if l.engine == k || l.group == k {
			return true
		}
	}
	for _, l := range b.queue {
		if l.engine == k || l.group == k {
			return true
		}
	}
	return false
}

// inBatch is whether holder h keeps the card from waiter w for its batch.
// A second lease of the batch's own engine queues behind it as before.
func (b *Broker) inBatch(h, w *lease) bool {
	k, bt := b.batchOf(h)
	return bt != nil && k != w.engine && k != w.group
}

func batchWhy(engine string, bt *batch) string {
	return fmt.Sprintf("%s runs a batch; it keeps the card until it has been idle %s", engine, bt.grace)
}
