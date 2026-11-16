// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

// The scheduler over HTTP, for agents and devices that hold the node's key.
// GET /v1/berth/schedule is the board: what holds a card, the waiters in
// grant order, and the bookings. POST books a run, PATCH /{id} moves one and
// DELETE /{id} cancels one. A priority change and downtime act on everyone
// else's work, so they stay the operator's: berth schedule or a paired
// harbor device.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/schedule"
	"github.com/T-Crypt/berth/internal/server"
)

// bookBody is a booking as a client writes it. Start takes what berth
// schedule --at takes, RFC 3339 included; For is a Go duration, "5h".
type bookBody struct {
	Who      string `json:"who"`
	Engine   string `json:"engine"`
	Priority string `json:"priority,omitempty"`
	Start    string `json:"start"`
	For      string `json:"for"`
	Days     int    `json:"days,omitempty"`
	Earliest bool   `json:"earliest,omitempty"`
	Card     string `json:"card,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (g *Gateway) scheduleRead(w http.ResponseWriter, r *http.Request) {
	st := g.nodeStatus()
	if st == nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; no schedule to show")
		return
	}
	writeJSON(w, http.StatusOK, proto.BoardOf(st, time.Now()))
}

func (g *Gateway) scheduleBook(w http.ResponseWriter, r *http.Request) {
	var in bookBody
	if !readBody(w, r, &in) {
		return
	}
	now := time.Now()
	start, err := schedule.ParseAt(in.Start, now)
	length, lerr := time.ParseDuration(in.For)
	if err != nil || lerr != nil || length <= 0 {
		apiError(w, http.StatusBadRequest, `the body must be JSON like {"who":"pi-pocket","engine":"NAME","start":"23:00","for":"5h"}`)
		return
	}
	b := proto.Booking{Card: in.Card, Kind: proto.BookRun, Who: in.Who, Engine: in.Engine, Priority: in.Priority,
		Start: start, End: start.Add(length), Note: in.Note}
	g.scheduleAsk(w, proto.Msg{Op: proto.OpBook, Booking: &b, Days: in.Days, Earliest: in.Earliest}, http.StatusCreated)
}

// runBooking is the run booking with the path's ID, or false after the
// reply says why not.
func (g *Gateway) runBooking(w http.ResponseWriter, r *http.Request) (proto.Booking, bool) {
	st := g.nodeStatus()
	if st == nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; the schedule is unchanged")
		return proto.Booking{}, false
	}
	id := r.PathValue("id")
	i := slices.IndexFunc(st.Bookings, func(b proto.Booking) bool { return b.ID == id })
	switch {
	case i < 0:
		apiError(w, http.StatusNotFound, fmt.Sprintf("no booking %s on the schedule", id))
		return proto.Booking{}, false
	case st.Bookings[i].Kind != proto.BookRun:
		apiError(w, http.StatusForbidden, fmt.Sprintf("%s is a %s booking: only the operator changes it (berth schedule, or the harbor)", id, st.Bookings[i].Kind))
		return proto.Booking{}, false
	case st.Bookings[i].Claim:
		apiError(w, http.StatusForbidden, fmt.Sprintf("%s claims its hours: only the operator changes it (berth schedule, or the harbor)", id))
		return proto.Booking{}, false
	}
	return st.Bookings[i], true
}

func (g *Gateway) scheduleMove(w http.ResponseWriter, r *http.Request) {
	b, ok := g.runBooking(w, r)
	if !ok {
		return
	}
	var in bookBody
	if !readBody(w, r, &in) {
		return
	}
	length := b.End.Sub(b.Start)
	if in.Start != "" {
		start, err := schedule.ParseAt(in.Start, time.Now())
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		b.Start = start
	}
	if in.For != "" {
		d, err := time.ParseDuration(in.For)
		if err != nil || d <= 0 {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("for %q is not a length like 5h", in.For))
			return
		}
		length = d
	}
	b.End = b.Start.Add(length)
	if in.Priority != "" {
		b.Priority = in.Priority
	}
	if in.Note != "" {
		b.Note = in.Note
	}
	g.scheduleAsk(w, proto.Msg{Op: proto.OpRebook, Booking: &b}, http.StatusOK)
}

func (g *Gateway) scheduleCancel(w http.ResponseWriter, r *http.Request) {
	b, ok := g.runBooking(w, r)
	if !ok {
		return
	}
	g.scheduleAsk(w, proto.Msg{Op: proto.OpUnbook, Booking: &proto.Booking{ID: b.ID}}, http.StatusOK)
}

// scheduleAsk sends one booking change to berthd and answers with the
// bookings it changed, or 409 with berthd's reason.
func (g *Gateway) scheduleAsk(w http.ResponseWriter, msg proto.Msg, ok int) {
	conn, raw, err := server.Dial(g.Socket)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd is not answering; the schedule is unchanged")
		return
	}
	defer raw.Close()
	if err := conn.Send(msg); err != nil {
		apiError(w, http.StatusServiceUnavailable, "berthd: "+err.Error())
		return
	}
	m, err := conn.Recv()
	switch {
	case err != nil:
		apiError(w, http.StatusServiceUnavailable, "no schedule reply from berthd: "+err.Error())
	case m.Op == proto.OpRefused:
		apiError(w, http.StatusConflict, m.Reason)
	case m.Op != msg.Op:
		apiError(w, http.StatusServiceUnavailable, fmt.Sprintf("berthd answered %q: restart it on this build", m.Op))
	default:
		writeJSON(w, ok, map[string][]proto.Booking{"bookings": m.Bookings})
	}
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, v) != nil {
		apiError(w, http.StatusBadRequest, "the body must be JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
