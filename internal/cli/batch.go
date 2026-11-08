// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

// berth batch: an agent or a script is about to queue many jobs on one
// engine (forty images on ComfyUI) and wants the card for all of them.

import (
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func runBatch(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	grace := fs.Duration("grace", broker.DefaultBatchGrace, "how long the engine must stay idle (no job running or queued) before the batch ends")
	max := fs.Duration("max", broker.DefaultBatchMax, "the longest the batch lasts")
	end := fs.Bool("end", false, "end the engine's batch now")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	if len(c.Args) != 1 {
		return c.fail(Failf("berth batch takes one engine name").
			WithHint("usage: berth batch ENGINE [--grace 30s] [--max 4h] | berth batch ENGINE --end"))
	}
	if *grace <= 0 || *max <= 0 {
		return c.fail(Failf("--grace and --max must be positive"))
	}
	engine := c.Args[0]
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err))
	}
	defer raw.Close()
	msg := proto.Msg{Op: proto.OpBatch, Engine: engine, End: *end}
	if !*end {
		msg.Grace, msg.Max = grace.Seconds(), max.Seconds()
	}
	if err := conn.Send(msg); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	m, err := conn.Recv()
	if err != nil {
		return c.fail(Systemf("no batch reply from berthd: %v", err))
	}
	if m.Op == proto.OpRefused {
		return c.fail(Failf("%s", m.Reason))
	}
	return c.Write(batchView{engine: engine, end: *end, grace: *grace, max: *max}, nil)
}

type batchView struct {
	engine     string
	end        bool
	grace, max time.Duration
}

func (v batchView) Data() any {
	if v.end {
		return struct {
			Engine string `json:"engine"`
			Ended  bool   `json:"ended"`
		}{v.engine, true}
	}
	return struct {
		Engine string  `json:"engine"`
		Grace  float64 `json:"grace_seconds"`
		Max    float64 `json:"max_seconds"`
	}{v.engine, v.grace.Seconds(), v.max.Seconds()}
}

func (v batchView) Human(c *Context, ok bool) error {
	if v.end {
		c.Printf("%s: batch ended; the card goes to the next waiter\n", v.engine)
		return nil
	}
	c.Printf("%s: batch on; it keeps the card until it has been idle %s (at most %s)\n", v.engine, v.grace, v.max)
	c.Bullet("waiting requests for other engines wait for the batch; a hold still takes the card")
	c.Bullet("if %s is waiting for the card, it takes it as a switch would", v.engine)
	c.Bullet("berth batch %s --end hands the card on now", v.engine)
	return nil
}
