// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// A lease's window is the hours its engine entry, or its berth run line,
// holds it to. Inside them it is scheduled like any other lease. A waiter
// outside them steps out of line, holding up nobody, and is told when they open, and
// a holder is asked back when they close: a window close is a drain, so it
// waits for the boundary the drain names and never cuts a request off.

import (
	"fmt"
	"time"
)

// closeWindows acts on the granted leases whose window no longer holds
// now: an active holder drains, a parked one is evicted as its TTL expiry
// evicts it. A lease already draining, parking or released is on its way
// out by its own path, so no lease is asked twice. Called with b.mu held,
// from Tick.
func (b *Broker) closeWindows(now time.Time) {
	for _, l := range b.leases {
		if l.window.Contains(now) { // no window contains every hour
			continue
		}
		why := fmt.Sprintf("its window %s closed", l.window)
		switch l.state {
		case active:
			b.drain(l, why)
		case parked:
			b.evict(l, why)
		}
	}
}

// opensIn is how long a window waits to open, as its waiter reads it:
// whole minutes, "45m", "3h12m", "2h".
func opensIn(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int64(d/time.Hour), int64((d%time.Hour)/time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}
