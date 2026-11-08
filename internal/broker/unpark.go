// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Unpark and the park switch: a person's own say over RAM parking. Parking
// stays automatic; berth unpark drops a parked engine from RAM now, and
// berth park off stops parking altogether (every drain stops its engine, as
// before parking existed) until berth park on. The switch lives in the
// daemon: a restart starts with parking on.

import (
	"sort"
)

// Unpark evicts the parked and parking leases of engine, by its name or
// its group ("" = every one): each stops the way a ttl eviction stops it,
// through its own runner, and the RAM it held is freed. The reply names
// the leases asked; none parked is no error, so a job can unpark before it
// starts without first asking what is parked.
func (b *Broker) Unpark(engine string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unparkLocked(engine, "unparked by berth unpark")
}

// SetParkOff turns parking off (true) or back on (false) for this card.
// Turning it off also evicts every lease parked here now. Returns the
// leases evicted.
func (b *Broker) SetParkOff(off bool) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.parkOff = off
	if !off {
		return nil
	}
	return b.unparkLocked("", "parking switched off by berth park off")
}

// ParkOff is whether parking is switched off on this card.
func (b *Broker) ParkOff() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.parkOff
}

// unparkLocked evicts the matching parked or parking leases. A parking
// lease hears the drain once its park call ends, as berth stop's does.
// Called with b.mu held.
func (b *Broker) unparkLocked(engine, why string) []string {
	ids := []string{}
	for _, l := range b.leases {
		if l.state != parked && l.state != parking {
			continue
		}
		if engine != "" && l.engine != engine && l.group != engine {
			continue
		}
		b.evict(l, why)
		ids = append(ids, l.id)
	}
	sort.Strings(ids)
	return ids
}
