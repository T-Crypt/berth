// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package proto

import (
	"testing"
	"time"
)

// zone is a location of its own: a window reads the wall clock where the
// node is, so the test must not depend on where it runs.
var zone = time.FixedZone("TEST", -4*3600)

func TestWindowParses(t *testing.T) {
	for _, tc := range []struct{ in, want, wantErr string }{
		{in: "23:00-07:00", want: "23:00-07:00"},
		{in: "09:30-17:00", want: "09:30-17:00"},
		{in: "00:00-23:59", want: "00:00-23:59"},
		{in: "", want: ""},
		{in: "7:00-9:00", wantErr: `window "7:00-9:00": "7:00" is not HH:MM`},
		{in: "1O:00-08:00", wantErr: `window "1O:00-08:00": "1O:00" is not HH:MM`},
		{in: "25:00-01:00", wantErr: `window "25:00-01:00": "25:00" is not a time of day`},
		{in: "23:00-24:00", wantErr: `window "23:00-24:00": "24:00" is not a time of day`},
		{in: "23:60-07:00", wantErr: `window "23:60-07:00": "23:60" is not a time of day`},
		{in: "10:00-10:00", wantErr: `window "10:00-10:00" starts when it ends: give it hours to run in`},
		{in: "10:00", wantErr: `window "10:00" is not START-END, like "23:00-07:00"`},
		{in: "10:00 - 11:00", wantErr: `window "10:00 - 11:00": "10:00 " is not HH:MM`},
	} {
		w, err := ParseWindow(tc.in)
		switch {
		case tc.wantErr != "":
			if err == nil {
				t.Errorf("ParseWindow(%q) = %s, want the error %q", tc.in, w, tc.wantErr)
				continue
			}
			if err.Error() != tc.wantErr {
				t.Errorf("ParseWindow(%q) error = %q, want %q", tc.in, err, tc.wantErr)
			}
		case err != nil:
			t.Errorf("ParseWindow(%q): %v", tc.in, err)
		case w.String() != tc.want:
			t.Errorf("ParseWindow(%q) = %q, want %q", tc.in, w.String(), tc.want)
		}
	}
}

// A window holds [Start, End): its own start is inside, its own end is
// outside. One that crosses midnight holds both ends of the day. Opens
// counts to the next Start, across midnight when that is the way to it, and
// an unset window opens now.
func TestWindowContainsAndOpens(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 2, h, m, 0, 0, zone) }
	night := Window{Start: 23 * 60, End: 7 * 60}
	work := Window{Start: 9*60 + 30, End: 17 * 60}
	for _, tc := range []struct {
		name  string
		w     Window
		at    time.Time
		in    bool
		opens time.Duration
	}{
		{"night at its start", night, at(23, 0), true, 0},
		{"night at 23:59", night, at(23, 59), true, 0},
		{"night past midnight", night, at(0, 0), true, 0},
		{"night a minute before its end", night, at(6, 59), true, 0},
		{"night at its end", night, at(7, 0), false, 16 * time.Hour},
		{"night a minute before its start", night, at(22, 59), false, time.Minute},
		{"night half a minute before its start", night, at(22, 59).Add(30 * time.Second), false, 30 * time.Second},
		{"work at its start", work, at(9, 30), true, 0},
		{"work a minute before its end", work, at(16, 59), true, 0},
		{"work at its end", work, at(17, 0), false, 16*time.Hour + 30*time.Minute},
		{"work the evening after", work, at(18, 0), false, 15*time.Hour + 30*time.Minute},
		{"work at 23:59, next start across midnight", work, at(23, 59), false, 9*time.Hour + 31*time.Minute},
		{"no window at midnight", Window{}, at(0, 0), true, 0},
		{"no window at noon", Window{}, at(12, 0), true, 0},
	} {
		if got := tc.w.Contains(tc.at); got != tc.in {
			t.Errorf("%s: Contains = %v, want %v", tc.name, got, tc.in)
		}
		if got := tc.w.Opens(tc.at); got != tc.opens {
			t.Errorf("%s: Opens = %v, want %v", tc.name, got, tc.opens)
		}
	}
}

func TestWindowCloses(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 2, h, m, 0, 0, zone) }
	night := Window{Start: 23 * 60, End: 7 * 60}
	work := Window{Start: 9*60 + 30, End: 17 * 60}
	for _, tc := range []struct {
		name   string
		w      Window
		at     time.Time
		closes time.Duration
	}{
		{"night at its start", night, at(23, 0), 8 * time.Hour},
		{"night past midnight", night, at(0, 30), 6*time.Hour + 30*time.Minute},
		{"night half a minute before its end", night, at(6, 59).Add(30 * time.Second), 30 * time.Second},
		{"night at its end", night, at(7, 0), 0},
		{"work at noon", work, at(12, 0), 5 * time.Hour},
		{"work the evening after", work, at(18, 0), 0},
		{"no window", Window{}, at(12, 0), 0},
	} {
		if got := tc.w.Closes(tc.at); got != tc.closes {
			t.Errorf("%s: Closes = %v, want %v", tc.name, got, tc.closes)
		}
	}
}

func TestScheduleAt(t *testing.T) {
	night := Window{Start: 23 * 60, End: 7 * 60}
	open := ScheduleAt("agent", night, time.Date(2026, 11, 2, 1, 0, 0, 0, zone))
	if want := (Schedule{Engine: "agent", Window: "23:00-07:00", Open: true, ChangeS: 6 * 3600}); open != want {
		t.Errorf("inside: %+v, want %+v", open, want)
	}
	shut := ScheduleAt("agent", night, time.Date(2026, 11, 2, 20, 0, 0, 0, zone))
	if want := (Schedule{Engine: "agent", Window: "23:00-07:00", Open: false, ChangeS: 3 * 3600}); shut != want {
		t.Errorf("outside: %+v, want %+v", shut, want)
	}
}
