// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"slices"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// schedules is every engine held to hours at now, one row per engine by
// name: the configured windows, with a lease's own window in place of its
// engine's, since that is the one the broker enforces.
func schedules(configured map[string]proto.Window, st *proto.Status, now time.Time) []proto.Schedule {
	hours := make(map[string]proto.Window, len(configured))
	for name, w := range configured {
		if w.Set() {
			hours[name] = w
		}
	}
	for _, l := range slices.Concat(st.Leases, st.Waiting) {
		if w, err := proto.ParseWindow(l.Window); err == nil && w.Set() {
			hours[l.Engine] = w
		}
	}
	out := make([]proto.Schedule, 0, len(hours))
	for name, w := range hours {
		out = append(out, proto.ScheduleAt(name, w, now))
	}
	slices.SortFunc(out, func(a, b proto.Schedule) int { return strings.Compare(a.Engine, b.Engine) })
	return out
}
