// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package notify

import "testing"

// #186: the tooltip read "Berth: Berth refused a request", and after
// explorer restarted it went back to plain "Berth" while the recent events
// were still there.
func TestTrayTip(t *testing.T) {
	for _, tc := range []struct {
		notes []Note
		want  string
	}{
		{nil, "Berth"},
		{[]Note{{Title: "Berth refused a request"}}, "Berth refused a request"},
		{[]Note{{Title: "gpu1: GPU passed to a VM"}, {Title: "older"}}, "Berth: gpu1: GPU passed to a VM"},
	} {
		if got := tip(tc.notes); got != tc.want {
			t.Errorf("tip(%v) = %q, want %q", tc.notes, got, tc.want)
		}
	}
}
