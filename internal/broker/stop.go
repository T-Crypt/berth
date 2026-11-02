// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Stop is the broker's half of berth stop: a person decided the named
// engine is done, so every lease of it gives the card back through the
// usual drain. The broker only asks; the lease's wrapper runs the stop
// ladder, and the lease frees only after a card read shows its memory gone.

import (
	"fmt"
	"sort"

	"github.com/T-Crypt/berth/internal/proto"
)

// stopReason is what the drain says, so the wrapper's output and the event
// log show who asked.
const stopReason = "stopped by berth stop"

// Stop asks every active, draining, parking or parked lease of the named engine (by its name
// or its group) to give the berth back, the way a higher-priority waiter
// would: one reason on the lease's drain channel, nothing signalled from
// here. A pinned lease is asked too: the pin keeps the scheduler off it,
// and a person who says stop means stop. A lease without an attached run
// (no recorded PID) is not Berth's to stop, and neither is an engine the
// broker never leased.
func (b *Broker) Stop(engine string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, l := range b.leases {
		if l.pid == 0 || (l.state != active && l.state != draining && l.state != parking && l.state != parked) {
			continue
		}
		if l.engine != engine && l.group != engine {
			continue
		}
		if l.state != draining {
			// A parked or parking engine is stopped too: its run hears the
			// drain once its park call ends and stops the engine. A lease
			// already draining keeps the story the drain that ordered it
			// told; only this transition says "stopped on command".
			l.state = draining
			if b.cfg.OnStopOrdered != nil {
				b.cfg.OnStopOrdered(l.engine, "Stopped on command")
			}
		}
		ids = append(ids, l.id)
		b.askDrain(l, stopReason)
	}
	if len(ids) == 0 {
		return nil, notFound(fmt.Sprintf("no running engine named %s under a lease", engine))
	}
	sort.Strings(ids)
	return ids, nil
}

// askDrain puts one reason on the lease's drain channel. When a reason is
// already on its way the ask is dropped, as in the scheduler, and nothing
// is booked twice.
func (b *Broker) askDrain(l *lease, reason string) {
	select {
	case l.drainCh <- reason:
	default:
		return
	}
	b.log.Info("draining", "lease", l.id, "engine", l.engine, "for", "berth stop")
	b.counters.Drains++
	b.emit(proto.Event{Kind: proto.EvDraining, Lease: l.id, Engine: l.engine, Detail: reason})
}
