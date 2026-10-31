// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// runHold keeps the card for the operator: every engine on it is drained,
// none starts, until the hold ends (Ctrl-C, --for, or the shell closing).
// The hold is a pinned lease at hold priority, so nothing drains it, and it
// lives exactly as long as this process: a crashed hold frees the card.
func runHold(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	reason := fs.String("reason", "", "shown on the lease, e.g. game or call (default: hold)")
	dur := fs.Duration("for", 0, "end the hold after this long; 0 = until Ctrl-C")
	needs := fs.Uint64("needs-mib", 0, "hold only this much memory instead of the whole card")
	card := fs.String("gpu", "", "card to hold: its name on the node (gpu1) or its PCI address, or a comma list of them for one hold on several cards; default the node's first card")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for engines to drain; 0 = berthd's default; negative = no deadline")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	if *card != "" {
		if err := gpu.CheckCard(*card); err != nil {
			return c.fail(Systemf("%v", err))
		}
	}
	name := "hold"
	if r := strings.TrimSpace(*reason); r != "" {
		name = "hold:" + strings.ReplaceAll(r, " ", "-")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err).
			WithHint("nothing is held: start berthd, or the card is not coordinated at all"))
	}
	defer raw.Close()
	go func() { <-ctx.Done(); raw.Close() }()
	if err := conn.Send(proto.Msg{Op: proto.OpAcquire, Engine: name, Card: *card, Group: "hold", Priority: "hold",
		Needs: *needs * gpu.MiB, Wait: wait.Seconds(), Pinned: true}); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	for {
		m, err := conn.Recv()
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(c.Err, "berth: hold cancelled before the card was free")
				return 0
			}
			return c.fail(Systemf("berthd went away: %v", err))
		}
		switch m.Op {
		case proto.OpWaiting:
			fmt.Fprintf(c.Err, "berth: waiting: %s\n", m.Reason)
			continue
		case proto.OpRefused:
			return c.fail(Failf("hold refused: %s", m.Reason))
		case proto.OpGranted:
		default:
			continue
		}
		until := "until Ctrl-C"
		var timer <-chan time.Time
		if *dur > 0 {
			until = "for " + dur.String()
			timer = time.After(*dur)
		}
		held := "card"
		if *card != "" {
			held = *card
		}
		fmt.Fprintf(c.Err, "berth: %s held as %s (%s); engines wait until it ends\n", held, m.Lease, until)
		if m.Reason != "" {
			fmt.Fprintf(c.Err, "berth: note: %s\n", m.Reason)
		}
		select {
		case <-ctx.Done():
		case <-timer:
		}
		fmt.Fprintln(c.Err, "berth: hold released")
		return 0
	}
}

// claimsView is the claim rules and what each matches right now.
type claimsView struct {
	claims []proto.Claim
	cards  int // cards berthd schedules
}

func (v claimsView) Data() any { return v.claims }

func (v claimsView) Human(c *Context, ok bool) error {
	if len(v.claims) == 0 {
		c.Printf("%s No claim rules: the built-in pack is off and berth.toml has no [[claim]]\n", c.Style.OK(markOK))
		return nil
	}
	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CLAIM\tACTION\tSTATE\tLEASE\t"+claimCardCol(v.cards, "CARD")+"HOLDS\tPROCESSES")
	for _, cl := range v.claims {
		state := c.Style.Dim("idle")
		if cl.Active {
			state = c.Style.OK("ACTIVE")
		}
		name := cl.Name
		if cl.Builtin {
			name += c.Style.Dim(" (built-in)")
		}
		used := "-"
		if cl.Used > 0 || cl.GTTUsed > 0 {
			used = memoryHeld(cl.Used, cl.GTTUsed)
		}
		lease := cl.Lease
		if lease == "" {
			lease = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s%s\t%s\n", name, cl.Action, state, lease, claimCardCol(v.cards, cardNames(cl)), used, cl.Procs)
	}
	return tw.Flush()
}

func runClaims(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err))
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		return c.fail(Systemf("no status from berthd: %v", err))
	}
	cl := m.Status.Claims
	if cl == nil {
		cl = []proto.Claim{}
	}
	return c.Write(claimsView{claims: cl, cards: scheduledCards(m.Status)}, nil)
}
