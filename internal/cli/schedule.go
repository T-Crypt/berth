// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/schedule"
	"github.com/T-Crypt/berth/internal/server"
)

type scheduleView struct {
	b     proto.ScheduleBoard
	cards int
}

func (v scheduleView) Data() any { return v.b }

func (v scheduleView) Human(c *Context, ok bool) error {
	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	card := func(s string) string { return claimCardCol(v.cards, s) }
	fmt.Fprint(tw, "RUNNING\t"+card("CARD")+"ENGINE\tPRIORITY\tSTATE\tFOR\tBOOKING\n")
	if len(v.b.Running) == 0 {
		fmt.Fprint(tw, "-\t"+card("")+"nothing holds a card\t\t\t\t\n")
	}
	for _, l := range v.b.Running {
		fmt.Fprintf(tw, "%s\t%s%s\t%s\t%s\t%s\t%s\n", l.ID, card(l.Card), l.Holder(), l.Priority, l.State, age(l.Age), dash(l.Booking))
	}
	fmt.Fprint(tw, "\nQUEUED\t"+card("CARD")+"ENGINE\tPRIORITY\tWAITING\tBOOKING\t\n")
	if len(v.b.Queued) == 0 {
		fmt.Fprint(tw, "-\t"+card("")+"nobody is waiting\t\t\t\t\n")
	}
	for i, l := range v.b.Queued {
		fmt.Fprintf(tw, "%d. %s\t%s%s\t%s\t%s\t%s\t\n", i+1, l.ID, card(l.Card), l.Holder(), l.Priority, age(l.Age), dash(l.Booking))
	}
	fmt.Fprint(tw, "\nBOOKED\t"+card("CARD")+"KIND\tWHO\tENGINE\tPRIORITY\tWHEN\tNOTE\n")
	if len(v.b.Bookings) == 0 {
		fmt.Fprint(tw, "-\t"+card("")+"nothing booked; berth schedule book --help\t\t\t\t\t\n")
	}
	for _, x := range v.b.Bookings {
		when := x.Start.Format("Mon Jan 2 15:04") + "-" + x.End.Format("15:04")
		if x.End.YearDay() != x.Start.YearDay() {
			when = x.Start.Format("Mon Jan 2 15:04") + "-" + x.End.Format("Mon 15:04")
		}
		if x.Holds(v.b.Now) {
			when += " (now)"
		}
		fmt.Fprintf(tw, "%s\t%s%s\t%s\t%s\t%s\t%s\t%s\n", x.ID, card(x.Card), x.Kind, x.Who, dash(x.Engine), dash(x.Priority), when, x.Note)
	}
	return tw.Flush()
}

type bookingsView struct {
	op string
	bs []proto.Booking
}

func (v bookingsView) Data() any { return v.bs }

func (v bookingsView) Human(c *Context, ok bool) error {
	verb := map[string]string{proto.OpBook: "booked", proto.OpUnbook: "cancelled", proto.OpRebook: "moved"}[v.op]
	for _, x := range v.bs {
		c.Printf("%s %s %s: %s for %s, %s-%s\n", c.Style.OK(markOK), verb, x.ID, x.Kind, x.Who,
			x.Start.Format("Mon Jan 2 15:04"), x.End.Format("Mon 15:04"))
	}
	return nil
}

func runSchedule(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	kind := fs.String("kind", "run", "book: run (the engine's turn), priority (change its class) or down (downtime)")
	who := fs.String("who", "", "book: who the booking is for: a device, an agent, a person")
	engine := fs.String("engine", "", "book: the engine or group the booking is for (not for down)")
	prio := fs.String("priority", "", "book: batch, interactive or foreground (run default foreground; priority needs one)")
	atFlag := fs.String("at", "", "book, move: the start: now, 22:00, tue 22:00, 2026-11-14 22:00")
	forFlag := fs.Duration("for", 0, "book, move: how long, like 5h or 90m")
	days := fs.Int("days", 1, "book: the same hours on this many days in a row")
	earliest := fs.Bool("earliest", false, "book: take the first free hours at or after --at")
	card := fs.String("card", "", "book: the card (default the first)")
	note := fs.String("note", "", "book, move: a note the board shows")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	usage := "usage: berth schedule | berth schedule book --who W --at T --for D [--engine E] [--kind run|priority|down] | berth schedule move ID --at T [--for D] | berth schedule cancel ID"
	now := time.Now()
	var msg proto.Msg
	switch args := c.Args; {
	case len(args) == 0:
		msg = proto.Msg{Op: proto.OpStatus}
	case len(args) == 1 && args[0] == "book":
		start, err := schedule.ParseAt(*atFlag, now)
		if err != nil || *forFlag <= 0 {
			return c.fail(Failf("berth schedule book needs --at and --for").WithHint("%s", usage))
		}
		b := proto.Booking{Card: *card, Kind: proto.BookingKind(*kind), Who: *who, Engine: *engine, Priority: *prio,
			Start: start, End: start.Add(*forFlag), Note: *note}
		if err := b.Check(now); err != nil {
			return c.fail(Failf("%v", err).WithHint("%s", usage))
		}
		msg = proto.Msg{Op: proto.OpBook, Booking: &b, Days: *days, Earliest: *earliest}
	case len(args) == 2 && args[0] == "move":
		msg = proto.Msg{Op: proto.OpRebook}
	case len(args) == 2 && args[0] == "cancel":
		msg = proto.Msg{Op: proto.OpUnbook, Booking: &proto.Booking{ID: args[1]}}
	default:
		return c.fail(Failf("berth schedule: unexpected arguments %q", args).WithHint("%s", usage))
	}

	st, code := scheduleStatus(c, *sock)
	if st == nil {
		return code
	}
	if msg.Op == proto.OpStatus {
		return c.Write(scheduleView{b: proto.BoardOf(st, now), cards: scheduledCards(st)}, nil)
	}
	if msg.Op == proto.OpRebook {
		i := slices.IndexFunc(st.Bookings, func(x proto.Booking) bool { return x.ID == c.Args[1] })
		if i < 0 {
			return c.fail(Failf("no booking %s on the schedule", c.Args[1]).WithHint("berth schedule lists them"))
		}
		b := st.Bookings[i]
		length := b.End.Sub(b.Start)
		if *atFlag != "" {
			start, err := schedule.ParseAt(*atFlag, now)
			if err != nil {
				return c.fail(Failf("%v", err))
			}
			b.Start = start
		}
		if *forFlag > 0 {
			length = *forFlag
		}
		b.End = b.Start.Add(length)
		if *prio != "" {
			b.Priority = *prio
		}
		if *note != "" {
			b.Note = *note
		}
		msg.Booking = &b
	}
	m, code := askBerthd(c, *sock, msg)
	if m == nil {
		return code
	}
	if m.Op == proto.OpRefused {
		return c.fail(Failf("%s", m.Reason).WithHint("berth schedule shows the board"))
	}
	if m.Op != msg.Op {
		return c.fail(Systemf("berthd answered %q: it may be older than berth schedule", m.Op).WithHint("restart berthd on this build"))
	}
	return c.Write(bookingsView{op: msg.Op, bs: m.Bookings}, nil)
}

func scheduleStatus(c *Context, sock string) (*proto.Status, int) {
	m, code := askBerthd(c, sock, proto.Msg{Op: proto.OpStatus})
	if m == nil {
		return nil, code
	}
	if m.Status == nil {
		return nil, c.fail(Systemf("no status from berthd: empty reply"))
	}
	return m.Status, ExitOK
}

// askBerthd sends one message and reads one reply; nil means it failed and
// the int is the exit code.
func askBerthd(c *Context, sock string, msg proto.Msg) (*proto.Msg, int) {
	conn, raw, err := server.Dial(sock)
	if err != nil {
		return nil, c.fail(Systemf("berthd unreachable on %s: %v", sock, err))
	}
	defer raw.Close()
	if err := conn.Send(msg); err != nil {
		return nil, c.fail(Systemf("berthd on %s: %v", sock, err))
	}
	m, err := conn.Recv()
	if err != nil {
		return nil, c.fail(Systemf("no reply from berthd: %v", err))
	}
	return &m, ExitOK
}
