// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

// berth cordon and berth uncordon: a person stops new grants on a card,
// for maintenance or before a VM takes it, while the other cards serve.

import (
	"strings"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func runCordon(c *Context) int   { return cordonCmd(c, proto.OpCordon) }
func runUncordon(c *Context) int { return cordonCmd(c, proto.OpUncordon) }

func cordonCmd(c *Context, op string) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	if len(c.Args) != 1 {
		return c.fail(Failf("berth %s takes one card", op).WithHint("usage: berth %s CARD (berth status names the cards)", op))
	}
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err))
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: op, Card: c.Args[0]}); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	m, err := conn.Recv()
	if err != nil {
		return c.fail(Systemf("no %s reply from berthd: %v", op, err))
	}
	if m.Op == proto.OpRefused {
		return c.fail(Failf("%s", m.Reason))
	}
	return c.Write(cordonView{op: op, cards: m.Cards}, nil)
}

type cordonView struct {
	op    string
	cards []string
}

func (v cordonView) Data() any {
	return struct {
		Cordoned bool     `json:"cordoned"`
		Cards    []string `json:"cards"`
	}{v.op == proto.OpCordon, v.cards}
}

func (v cordonView) Human(c *Context, ok bool) error {
	names := strings.Join(v.cards, ", ")
	if v.op == proto.OpUncordon {
		c.Printf("%s uncordoned: new work gets the card again\n", names)
		return nil
	}
	c.Printf("%s cordoned: no new grants but holds\n", names)
	c.Bullet("what holds it keeps running; berth stop ENGINE drains it")
	c.Bullet("the cordon lasts until berth uncordon or a berthd restart")
	return nil
}
