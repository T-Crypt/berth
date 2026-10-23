// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package notify turns berthd's events into desktop notifications: which
// events a person hears about at each level, and a gate that shows a burst
// of the same event once.
package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// Level is how much a person hears. Error, the default, is the events
// that need a look: a refused request, a lease whose memory never came
// back, a stowaway. Info adds every switch, drain, park and resume.
type Level int

const (
	Error Level = iota
	Info
)

// ParseLevel reads a [notify] level or --level value.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "", "error":
		return Error, nil
	case "info":
		return Info, nil
	}
	return Error, fmt.Errorf("notify level %q: use error or info", s)
}

// Note is one desktop notification.
type Note struct {
	Kind  string
	Level Level
	Title string
	Body  string
}

// rule is what one event kind becomes: the level it shows at and its
// title, which names the engine where the event has one.
type rule struct {
	level Level
	title string
}

var rules = map[string]rule{
	proto.EvRefused:         {Error, "Berth refused a request"},
	proto.EvStuck:           {Error, "GPU memory not released"},
	proto.EvOrphaned:        {Error, "Engine exited under its lease"},
	proto.EvSquatter:        {Error, "GPU memory held without a lease"},
	proto.EvParkRefused:     {Error, "Park refused, engine stopped"},
	proto.EvUnparked:        {Error, "Parked engine loaded by a direct client"},
	proto.EvProbeFailed:     {Error, "Engine stopped answering"},
	proto.EvAlertRaised:     {Error, "GPU alert"},
	proto.EvGranted:         {Info, "Card switched"},
	proto.EvDraining:        {Info, "Draining"},
	proto.EvParked:          {Info, "Parked in RAM"},
	proto.EvResumed:         {Info, "Resumed"},
	proto.EvParkEvicted:     {Info, "Parked engine stopped"},
	proto.EvJuggle:          {Info, "Juggle"},
	proto.EvCardPassthrough: {Info, "GPU passed to a VM"},
	proto.EvCardReturned:    {Info, "GPU back from the VM"},
}

// For is the notification e becomes at level, or false when a person at
// that level does not hear about it.
func For(e proto.Event, level Level) (Note, bool) {
	r, ok := rules[e.Kind]
	if !ok || r.level > level {
		return Note{}, false
	}
	title := r.title
	if e.Engine != "" {
		title += ": " + e.Engine
	}
	return Note{Kind: e.Kind, Level: r.level, Title: title, Body: e.Detail}, true
}

// Gate shows a burst of one event once: a note whose kind and title were
// shown less than Window ago is dropped.
type Gate struct {
	Window time.Duration
	last   map[string]time.Time
}

// Let reports whether n is shown at now, and records it if so.
func (g *Gate) Let(n Note, now time.Time) bool {
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	key := n.Kind + "\x00" + n.Title
	if t, ok := g.last[key]; ok && now.Sub(t) < g.Window {
		return false
	}
	g.last[key] = now
	return true
}

// Recent is the last few notes shown, newest first: the tray's menu.
type Recent struct {
	Max   int
	notes []Note
}

// Add puts n first and drops the oldest past Max.
func (r *Recent) Add(n Note) {
	r.notes = append([]Note{n}, r.notes...)
	if len(r.notes) > r.Max {
		r.notes = r.notes[:r.Max]
	}
}

// Notes are the notes, newest first.
func (r *Recent) Notes() []Note { return r.notes }
