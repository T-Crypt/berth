// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"fmt"
	"sort"
	"time"
)

// Yield tells the run of every leased engine named engine (by name or group)
// that its job has let go of the card from inside, for d, or that it has
// taken it back when d is not positive (#299). A ComfyUI job that waits on an
// LLM never reaches its job boundary, so a drain for that LLM waits on the
// job while the job waits on the drain. While the yield lasts, a drain finds
// the job at its boundary and parks or stops the engine as it would there.
// The broker's books do not move on the yield: the card read after the park
// still decides what is free (rule 1). It answers the leases it told.
func (b *Broker) Yield(engine string, d time.Duration) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, l := range b.leases {
		// A parked or parking lease hears the end too: its run must stop
		// counting the job as at its boundary before the job runs again.
		if l.pid == 0 || (l.state != active && l.state != draining && l.state != parking && l.state != parked) ||
			(l.engine != engine && l.group != engine) {
			continue
		}
		if l.gang != nil {
			// A share cannot park yet, so a drain that skipped the job
			// boundary would stop the engine mid-job.
			return nil, fmt.Errorf("%w: %s runs on several cards, and a lease on several cards cannot park, so it cannot yield", ErrRefused, engine)
		}
		select {
		case <-l.yieldCh:
		default:
		}
		l.yieldCh <- d
		ids = append(ids, l.id)
	}
	if len(ids) == 0 {
		return nil, notFound(fmt.Sprintf("no running engine named %s under a lease", engine))
	}
	sort.Strings(ids)
	if d > 0 {
		b.log.Info("yield", "engine", engine, "for", d, "leases", ids)
	} else {
		b.log.Info("yield ended", "engine", engine, "leases", ids)
	}
	return ids, nil
}

// Yields carries each yield for this lease's run, newest first: how long it
// lasts, or 0 when it ended.
func (l *Lease) Yields() <-chan time.Duration { return l.l.yieldCh }
