// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package schedule

import (
	"fmt"
	"strings"
	"time"
)

// ParseAt reads a start time on the node's clock, as a person types it:
// "now"; "22:00", the next time the clock reads it; "tue 22:00", the next
// Tuesday at 22:00 (today if still ahead); "2026-11-14 22:00" or
// "2026-11-14T22:00"; or RFC 3339.
func ParseAt(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	loc := now.Location()
	if s == "now" {
		return now, nil
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(s)); err == nil {
		return t.In(loc), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02t15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	day, clock, hasDay := strings.Cut(s, " ")
	if !hasDay {
		clock = day
	}
	hm, err := time.ParseInLocation("15:04", clock, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("start %q: use now, 22:00, tue 22:00 or 2026-11-14 22:00", s)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if !hasDay {
		if t.Before(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	wd, ok := weekday(day)
	if !ok {
		return time.Time{}, fmt.Errorf("start %q: %q is not a day (mon, tue, ...)", s, day)
	}
	t = t.AddDate(0, 0, (int(wd)-int(t.Weekday())+7)%7)
	if t.Before(now) {
		t = t.AddDate(0, 0, 7)
	}
	return t, nil
}

func weekday(s string) (time.Weekday, bool) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if name := strings.ToLower(d.String()); len(s) >= 3 && strings.HasPrefix(name, s) {
			return d, true
		}
	}
	return 0, false
}
