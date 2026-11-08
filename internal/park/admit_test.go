// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package park

import (
	"slices"
	"testing"
	"time"
)

func TestAdmit(t *testing.T) {
	const g = gib
	tests := []struct {
		name      string
		b         Budget
		footprint uint64
		want      string // the refusal line; empty when admitted
	}{
		{
			name:      "fits above the floor",
			b:         Budget{Available: 10 * g, Floor: 4 * g, Parked: 1 * g},
			footprint: 2 * g,
		},
		{
			name:      "lands exactly on the floor",
			b:         Budget{Available: 7 * g, Floor: 4 * g},
			footprint: 3 * g,
		},
		{
			name:      "the floor refuses with one decimal each side",
			b:         Budget{Available: 5 * g, Floor: 4 * g},
			footprint: 2 * g,
			want:      "park refused: needs 2.0 GiB, 1.0 GiB available above the floor",
		},
		{
			name:      "nothing available",
			b:         Budget{Available: 0, Floor: 4 * g},
			footprint: 1 * g,
			want:      "park refused: needs 1.0 GiB, 0.0 GiB available above the floor",
		},
		{
			name:      "parked RAM is already out of Available, so it does not count twice",
			b:         Budget{Available: 10 * g, Floor: 4 * g, Parked: 6 * g},
			footprint: 2 * g,
		},
		{
			name:      "parked RAM still counts toward the cap",
			b:         Budget{Available: 10 * g, Floor: 4 * g, Parked: 6 * g, Max: 7 * g},
			footprint: 2 * g,
			want:      "park refused: 8.0 GiB parked would pass the 7.0 GiB cap",
		},
		{
			name:      "no cap when Max is zero",
			b:         Budget{Available: 120 * g, Floor: 1 * g, Parked: 50 * g, Max: 0},
			footprint: 60 * g,
		},
		{
			name:      "lands exactly on the cap",
			b:         Budget{Available: 100 * g, Floor: 1 * g, Parked: 31 * g, Max: 32 * g},
			footprint: 1 * g,
		},
		{
			name:      "the cap refuses",
			b:         Budget{Available: 100 * g, Floor: 1 * g, Parked: 31 * g, Max: 32 * g},
			footprint: 2 * g,
			want:      "park refused: 33.0 GiB parked would pass the 32.0 GiB cap",
		},
		{
			name:      "the floor speaks before the cap",
			b:         Budget{Available: 5 * g, Floor: 4 * g, Max: 1 * g},
			footprint: 2 * g,
			want:      "park refused: needs 2.0 GiB, 1.0 GiB available above the floor",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := Admit(tc.b, tc.footprint)
			if ok != (tc.want == "") || why != tc.want {
				t.Fatalf("Admit = %v, %q; want %v, %q", ok, why, tc.want == "", tc.want)
			}
		})
	}
}

func TestEvict(t *testing.T) {
	const g = gib
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	m := func(name string, bytes uint64, age time.Duration) Parked {
		return Parked{Name: name, Bytes: bytes, Since: now.Add(-age)}
	}
	ttl := 30 * time.Minute
	tests := []struct {
		name     string
		parked   []Parked
		b        Budget
		pressure float64
		want     []string
	}{
		{
			name:     "RAM above the floor and pressure low: nobody stops",
			parked:   []Parked{m("new", g, time.Minute), m("old", g, 2*time.Minute)},
			b:        Budget{Available: 10 * g, Floor: 4 * g},
			pressure: 0.05,
		},
		{
			name:     "ttl stops the expired, in age order, RAM and pressure fine",
			parked:   []Parked{m("new", g, time.Minute), m("stale", g, 31*time.Minute), m("staler", g, 40*time.Minute)},
			b:        Budget{Available: 10 * g, Floor: 4 * g},
			pressure: 0,
			want:     []string{"staler", "stale"},
		},
		{
			name: "an engine's own ttl beats the node's, both ways",
			parked: []Parked{
				{Name: "short", Bytes: g, Since: now.Add(-6 * time.Minute), TTL: 5 * time.Minute},
				{Name: "long", Bytes: g, Since: now.Add(-40 * time.Minute), TTL: time.Hour},
				m("node", g, 6*time.Minute),
			},
			b:    Budget{Available: 10 * g, Floor: 4 * g},
			want: []string{"short"},
		},
		{
			name:     "below the floor: least recently parked until back at it",
			parked:   []Parked{m("new", 4*g, time.Minute), m("mid", 4*g, 5*time.Minute), m("old", 4*g, 10*time.Minute)},
			b:        Budget{Available: 5 * g, Floor: 10 * g},
			pressure: 0,
			want:     []string{"old", "mid"},
		},
		{
			name:     "the floor is not reachable: everything stops",
			parked:   []Parked{m("a", 4*g, time.Minute), m("b", 4*g, 2*time.Minute)},
			b:        Budget{Available: 1 * g, Floor: 10 * g},
			pressure: 0,
			want:     []string{"b", "a"},
		},
		{
			name:     "RAM a ttl stop already gave back reaches the floor",
			parked:   []Parked{m("new", 4*g, time.Minute), m("stale", 6*g, 31*time.Minute)},
			b:        Budget{Available: 5 * g, Floor: 10 * g},
			pressure: 0,
			want:     []string{"stale"},
		},
		{
			name:     "pressure over the threshold stops one, the least recently parked",
			parked:   []Parked{m("new", g, time.Minute), m("old", g, 2*time.Minute)},
			b:        Budget{Available: 10 * g, Floor: 4 * g},
			pressure: 0.5,
			want:     []string{"old"},
		},
		{
			name:     "pressure adds one on top of the floor pass",
			parked:   []Parked{m("new", g, time.Minute), m("old", 6*g, 10*time.Minute)},
			b:        Budget{Available: 5 * g, Floor: 10 * g},
			pressure: 0.5,
			want:     []string{"old", "new"},
		},
		{
			name: "nothing parked, nothing to stop",
			b:    Budget{Available: 1 * g, Floor: 4 * g},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evict(tc.parked, tc.b, tc.pressure, 0.1, now, ttl)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Evict = %v, want %v", got, tc.want)
			}
		})
	}
}

// The cap on parked RAM is the machine's: what another card has parked
// counts toward it. A machine of one card (a nil ledger) sees only its own.
func TestLedgerCapsTheMachinesParkedRAM(t *testing.T) {
	b := Budget{Available: 32 << 30, Max: 10 << 30}
	var one *Ledger
	if ok, why := one.Admit("gpu1", Totals{}, b, 5<<30); !ok {
		t.Fatalf("one card, nothing parked: refused %q", why)
	}
	l := &Ledger{}
	l.Set("gpu0", Totals{Parked: 6 << 30})
	ok, why := l.Admit("gpu1", Totals{}, b, 5<<30)
	if want := "park refused: 11.0 GiB parked would pass the 10.0 GiB cap"; ok || why != want {
		t.Fatalf("gpu1 beside 6 GiB parked on gpu0 = %v, %q; want false, %q", ok, why, want)
	}
	if got := l.Others("gpu0"); got != (Totals{}) {
		t.Fatalf("a refused park was recorded: gpu0 sees %+v", got)
	}
}

// A render on gpu0 that will climb back to its peak keeps that RAM from a
// park on gpu1, the same as a park in flight there.
func TestLedgerBooksOtherCardsRAMToCome(t *testing.T) {
	l := &Ledger{}
	l.Set("gpu0", Totals{ToCome: 30 << 30})
	b := Budget{Available: 40 << 30, Floor: 2 << 30}
	ok, why := l.Admit("gpu1", Totals{}, b, 18<<30)
	if want := "park refused: needs 18.0 GiB, 8.0 GiB available above the floor"; ok || why != want {
		t.Fatalf("gpu1 beside 30 GiB to come on gpu0 = %v, %q; want false, %q", ok, why, want)
	}
	l.Set("gpu0", Totals{})
	if ok, why := l.Admit("gpu1", Totals{ToCome: 4 << 30}, b, 18<<30); !ok {
		t.Fatalf("gpu1 with room: refused %q", why)
	}
	if got := l.Others("gpu0"); got != (Totals{Parking: 18 << 30, ToCome: 4 << 30}) {
		t.Fatalf("gpu1's admitted totals = %+v, want its parking and its own RAM to come", got)
	}
}
