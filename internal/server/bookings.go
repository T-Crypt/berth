// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

// OpBook, OpUnbook and OpRebook: the cards' schedule. berthd owns the file;
// every change goes to the cards' brokers at once and onto the event
// stream.

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/schedule"
)

func bookings(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node, s *schedule.Store) {
	refuse := func(why string) { reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: why}) }
	if s == nil {
		refuse("this berthd keeps no schedule (--schedule \"\")")
		return
	}
	if req.Booking == nil {
		refuse("the request carries no booking")
		return
	}
	now := time.Now()
	b := *req.Booking
	var changed []proto.Booking
	var err error
	switch req.Op {
	case proto.OpBook, proto.OpRebook:
		c, cerr := n.Card(b.Card)
		if cerr != nil {
			refuse(cerr.Error())
			return
		}
		b.Card = c.Name
		if req.Op == proto.OpBook {
			changed, err = s.Book(b, req.Days, req.Earliest, now)
		} else {
			var one proto.Booking
			one, err = s.Rebook(b, now)
			changed = []proto.Booking{one}
		}
	case proto.OpUnbook:
		var one proto.Booking
		one, err = s.Unbook(b.ID, now)
		changed = []proto.Booking{one}
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	n.SetBookings(s.List(now))
	verb := map[string]string{proto.OpBook: "booked", proto.OpUnbook: "unbooked", proto.OpRebook: "rebooked"}[req.Op]
	for _, x := range changed {
		detail := fmt.Sprintf("%s %s %s for %s, %s-%s", verb, x.ID, x.Kind, x.Who, x.Start.Format("Mon Jan 2 15:04"), x.End.Format("15:04"))
		if x.Engine != "" {
			detail += ", " + x.Engine
		}
		if c, err := n.Card(x.Card); err == nil {
			c.B.Note(proto.Event{Kind: proto.EvBooked, Engine: x.Engine, Detail: detail})
		}
		log.Info("schedule", "change", detail)
	}
	reply(conn, log, proto.Msg{Op: req.Op, Bookings: changed})
}
