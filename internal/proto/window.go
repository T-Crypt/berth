// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package proto

import (
	"fmt"
	"strings"
	"time"
)

// minutesPerDay is a day's length in the unit a Window keeps its bounds in.
const minutesPerDay = 24 * 60

// Window is the set of hours a lease may run in, written "23:00-07:00".
// Start and End are minutes of the day (0..1439): with Start < End the
// window is [Start, End), and with Start > End it crosses midnight,
// [Start, 24:00) plus [00:00, End). The zero value, whose bounds meet, names
// no window, so nothing there is held to hours. It keeps that spelling in
// TOML, JSON and YAML through the text codec.
type Window struct {
	Start int
	End   int
}

// ParseWindow reads "HH:MM-HH:MM": 24 h, two digits per field. "" is no
// window. A window that starts when it ends is refused, as it would hold no
// hours at all.
func ParseWindow(s string) (Window, error) {
	if s == "" {
		return Window{}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("window %q is not START-END, like \"23:00-07:00\"", s)
	}
	start, err := minuteOfDayField(a)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %w", s, err)
	}
	end, err := minuteOfDayField(b)
	if err != nil {
		return Window{}, fmt.Errorf("window %q: %w", s, err)
	}
	if start == end {
		return Window{}, fmt.Errorf("window %q starts when it ends: give it hours to run in", s)
	}
	return Window{Start: start, End: end}, nil
}

// minuteOfDayField is one "HH:MM" field as a minute of the day.
func minuteOfDayField(f string) (int, error) {
	if len(f) != len("00:00") || f[2] != ':' || !digit(f[0]) || !digit(f[1]) || !digit(f[3]) || !digit(f[4]) {
		return 0, fmt.Errorf("%q is not HH:MM", f)
	}
	h, m := int(f[0]-'0')*10+int(f[1]-'0'), int(f[3]-'0')*10+int(f[4]-'0')
	if h > 23 || m > 59 {
		return 0, fmt.Errorf("%q is not a time of day", f)
	}
	return h*60 + m, nil
}

func digit(b byte) bool { return b >= '0' && b <= '9' }

// Set is whether w names a window at all: the zero value holds nothing.
func (w Window) Set() bool { return w.Start != w.End }

// Contains is whether t's wall clock, read in t's own location, is inside
// w: [Start, End), or both ends of the day when w crosses midnight. A window
// whose bounds meet holds no hours, and so contains every hour: that is the
// zero value, no window.
func (w Window) Contains(t time.Time) bool {
	m := minuteOf(t)
	if w.Start < w.End {
		return m >= w.Start && m < w.End
	}
	return m >= w.Start || m < w.End
}

// Opens is how long a lease outside w waits for it: 0 inside, and otherwise
// until the next Start.
func (w Window) Opens(t time.Time) time.Duration {
	if w.Contains(t) {
		return 0
	}
	// The minute t sits in has already run part of the way, and that part
	// runs before the window opens too.
	left := (w.Start - minuteOf(t) + minutesPerDay) % minutesPerDay
	return time.Duration(left)*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond())*time.Nanosecond
}

// Closes is how long a lease inside w keeps its hours: 0 outside, and with
// no window, which never closes.
func (w Window) Closes(t time.Time) time.Duration {
	if !w.Set() || !w.Contains(t) {
		return 0
	}
	left := (w.End - minuteOf(t) + minutesPerDay) % minutesPerDay
	return time.Duration(left)*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond())*time.Nanosecond
}

// Schedule is one engine's hours as the node's clock reads them now.
type Schedule struct {
	Engine string `json:"engine"`
	Window string `json:"window"`
	Open   bool   `json:"open"`
	// ChangeS is the seconds until the window closes when Open, else until
	// it opens.
	ChangeS float64 `json:"change_s"`
}

// ScheduleAt reads w for engine at t.
func ScheduleAt(engine string, w Window, t time.Time) Schedule {
	s := Schedule{Engine: engine, Window: w.String(), Open: w.Contains(t)}
	if s.Open {
		s.ChangeS = w.Closes(t).Seconds()
	} else {
		s.ChangeS = w.Opens(t).Seconds()
	}
	return s
}

func minuteOf(t time.Time) int { return t.Hour()*60 + t.Minute() }

// String is the window as a config file writes it, "23:00-07:00". The zero
// value, which names no window, is the empty string.
func (w Window) String() string {
	if !w.Set() {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
}

func (w *Window) UnmarshalText(b []byte) error {
	v, err := ParseWindow(string(b))
	*w = v
	return err
}

func (w Window) MarshalText() ([]byte, error) { return []byte(w.String()), nil }
