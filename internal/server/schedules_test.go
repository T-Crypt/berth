// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"slices"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// An engine with hours shows whether or not it runs, and a lease's own
// window stands in for its engine's.
func TestSchedulesJoinConfigAndLeases(t *testing.T) {
	now := time.Date(2026, 11, 12, 20, 0, 0, 0, time.Local)
	configured := map[string]proto.Window{
		"nightly": {Start: 23 * 60, End: 7 * 60},
		"chat":    {},
		"render":  {Start: 9 * 60, End: 17 * 60},
	}
	st := &proto.Status{
		Leases:  []proto.Lease{{Engine: "render", Window: "18:00-22:00"}, {Engine: "chat"}},
		Waiting: []proto.Lease{{Engine: "job", Window: "01:00-05:00"}},
	}
	want := []proto.Schedule{
		{Engine: "job", Window: "01:00-05:00", Open: false, ChangeS: 5 * 3600},
		{Engine: "nightly", Window: "23:00-07:00", Open: false, ChangeS: 3 * 3600},
		{Engine: "render", Window: "18:00-22:00", Open: true, ChangeS: 2 * 3600},
	}
	if got := schedules(configured, st, now); !slices.Equal(got, want) {
		t.Errorf("schedules =\n%+v\nwant\n%+v", got, want)
	}
}
