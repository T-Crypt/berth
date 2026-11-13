// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// queueView is the waiters in the order berthd will grant them.
type queueView struct {
	waiting []proto.Lease
	cards   int
}

func (v queueView) Data() any { return v.waiting }

func (v queueView) Human(c *Context, ok bool) error {
	if len(v.waiting) == 0 {
		c.Printf("%s nobody is waiting\n", c.Style.OK(markOK))
		return nil
	}
	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprint(tw, "POS\tID\tENGINE\tPRIORITY\t"+claimCardCol(v.cards, "CARD")+"WAITING\n")
	for i, l := range v.waiting {
		prio := l.Priority
		if l.Training {
			prio += ",training"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s%s\n", i+1, l.ID, l.Holder(), prio, claimCardCol(v.cards, l.Card), age(l.Age))
	}
	return tw.Flush()
}

// queueChange is the reply to bump, prio and drop.
type queueChange struct {
	Lease  string `json:"lease" yaml:"lease"`
	Change string `json:"change" yaml:"change"`
}

type queueChangeView struct{ c queueChange }

func (v queueChangeView) Data() any { return v.c }

func (v queueChangeView) Human(c *Context, ok bool) error {
	c.Printf("%s %s: %s\n", c.Style.OK(markOK), v.c.Lease, v.c.Change)
	return nil
}

func runQueue(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	usage := "usage: berth queue | berth queue bump ID | berth queue prio ID batch|interactive|foreground | berth queue drop ID"
	msg := proto.Msg{Op: proto.OpQueue}
	switch args := c.Args; {
	case len(args) == 0:
		msg = proto.Msg{Op: proto.OpStatus}
	case len(args) == 2 && (args[0] == "bump" || args[0] == "drop"):
		msg.Action, msg.Lease = args[0], args[1]
	case len(args) == 3 && args[0] == "prio":
		msg.Action, msg.Lease, msg.Priority = args[0], args[1], args[2]
	default:
		return c.fail(Failf("berth queue: unexpected arguments %q", args).WithHint("%s", usage))
	}

	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err))
	}
	defer raw.Close()
	if err := conn.Send(msg); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	m, err := conn.Recv()
	if err != nil {
		return c.fail(Systemf("no reply from berthd: %v", err))
	}
	switch {
	case m.Op == proto.OpRefused:
		return c.fail(Failf("%s", m.Reason).WithHint("berth queue lists the waiters and their IDs"))
	case msg.Op == proto.OpStatus:
		if m.Status == nil {
			return c.fail(Systemf("no status from berthd: empty reply"))
		}
		w := m.Status.Waiting
		if w == nil {
			w = []proto.Lease{}
		}
		return c.Write(queueView{waiting: w, cards: scheduledCards(m.Status)}, nil)
	case m.Op != proto.OpQueue:
		return c.fail(Systemf("berthd answered %q: it may be older than berth queue", m.Op).
			WithHint("restart berthd on this build"))
	}
	return c.Write(queueChangeView{queueChange{Lease: m.Lease, Change: m.Reason}}, nil)
}
