// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// statusView is one status reply: the card, who holds it, who is waiting, what
// holds memory without a lease, and the collisions board.
type statusView struct {
	st   *proto.Status
	sock string
}

func (v statusView) Data() any { return v.st }

func memoryHeld(vram, gtt uint64) string {
	if gtt > 0 {
		return server.MiB(vram) + " VRAM + " + server.MiB(gtt) + " GTT"
	}
	return server.MiB(vram)
}

func (v statusView) Human(c *Context, ok bool) error {
	st := v.st
	c.Printf("%s\n", c.Style.Dim(fmt.Sprintf("berthd %s on %s (api %d)", st.Version, v.sock, st.API)))

	if len(st.GPUs) == 0 {
		if p := st.Passthrough; p != nil && p.Addr != "" {
			c.Printf("gpu     %s: %s\n", p.Addr, cardLine(p))
		} else if p != nil {
			c.Printf("gpu0    %s\n", cardLine(p))
		} else {
			c.Printf("gpu     no reading yet\n")
		}
	}
	for _, g := range st.GPUs {
		if p := cardVM(g, st); g.Scheduled && p != nil {
			c.Printf("gpu%d    %s: %s, memory read %s ago\n", g.Index, g.Name, cardLine(p), age(g.Age))
			continue
		}
		role := "inventory only"
		if g.Scheduled {
			role = "leased by berth, baseline " + server.MiB(g.Baseline)
			if g.Cordoned {
				role = c.Style.Err("CORDONED") + ", holds only; " + role
			}
		}
		c.Printf("gpu%d    %s: %s used of %s, %s free (%s, read %s ago)\n", g.Index, g.Name,
			server.MiB(g.Used), server.MiB(g.Total), server.MiB(g.Free), role, age(g.Age))
		if g.APU {
			c.Printf("        reserved UMA VRAM above; GTT %s used of %s driver capacity (shared host RAM, not extra VRAM); exclusive leases only\n", server.MiB(g.GTTUsed), server.MiB(g.GTTTotal))
		}
		if len(g.UnobservedPIDs) > 0 {
			c.Printf("        process visibility limited for PIDs %v; unknown memory remains in baseline\n", g.UnobservedPIDs)
		}
	}
	if st.ProbeErr != "" {
		c.Printf("probe   %s\n", c.Style.Err("FAILING: "+st.ProbeErr))
	}
	if st.ParkedBytes > 0 {
		c.Printf("warm    parked engines hold %s of RAM\n", server.MiB(st.ParkedBytes))
	}
	if c.Verbose {
		hostLine(c, st.Host)
		alertsLine(c, st.Alerts)
	}

	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', tabwriter.StripEscape)
	// Each row names its card once the node has more than one.
	cards := scheduledCards(st)
	cardCol := func(head, card string) string {
		if cards < 2 {
			return head
		}
		return head + "\t" + card
	}
	lease := func(title string, ls []proto.Lease, waiting bool) {
		if len(ls) == 0 {
			return
		}
		header := "\n" + cardCol(title, "CARD") + "\tENGINE\tPRIORITY\tSTATE\tNEEDS\tHOLDS\tPIDS"
		if c.Verbose {
			header += "\tPID"
		}
		if waiting {
			header += "\tPOS\tSTART"
		} else {
			header += "\tTURN"
		}
		fmt.Fprintln(tw, header+"\tAGE")
		for _, l := range ls {
			needs := "card"
			if l.Needs > 0 {
				needs = server.MiB(l.Needs)
			}
			state := leaseState(c, l)
			prio := l.Priority
			if l.Pinned {
				prio += ",pinned"
			}
			row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%v", cardCol(l.ID, l.Card), l.Engine, prio, state, needs, memoryHeld(l.Used, l.GTTUsed), l.GPUPIDs)
			if c.Verbose {
				row += fmt.Sprintf("\t%d", l.PID)
			}
			if waiting {
				row += fmt.Sprintf("\t%d", l.Position)
				if l.EstimateS > 0 {
					row += fmt.Sprintf("\t~%.0f s", l.EstimateS)
				} else {
					row += "\t-"
				}
			} else {
				turn := "-"
				if l.TurnS > 0 {
					turn = age(l.TurnS)
				}
				row += "\t" + turn
			}
			fmt.Fprintf(tw, "%s\t%s\n", row, age(l.Age))
		}
	}
	lease("LEASE", st.Leases, false)
	lease("WAITING", st.Waiting, true)
	if len(st.Squatters) > 0 {
		fmt.Fprint(tw, "\n"+cardCol("SQUATTER", "CARD")+"\tPID\tHOLDS\tSEEN\tUNPROTECTED RUN\n")
		for _, s := range st.Squatters {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", cardCol(s.Name, s.Card), s.PID, memoryHeld(s.Used, s.GTTUsed), age(s.Age), s.Run)
		}
	}
	var active []proto.Claim
	for _, cl := range st.Claims {
		if cl.Active {
			active = append(active, cl)
		}
	}
	if len(active) > 0 {
		fmt.Fprint(tw, "\nCLAIM\tACTION\tLEASE\t"+claimCardCol(cards, "CARD")+"HOLDS\tPROCESSES\n")
		for _, cl := range active {
			lease, used := cl.Lease, "-"
			if lease == "" {
				lease = "-"
			}
			if cl.Used > 0 || cl.GTTUsed > 0 {
				used = memoryHeld(cl.Used, cl.GTTUsed)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s%s\t%s\n", cl.Name, cl.Action, lease, claimCardCol(cards, cardNames(cl)), used, cl.Procs)
		}
	}
	if len(st.Unprotected) > 0 {
		fmt.Fprint(tw, "\nUNPROTECTED\tPID\tSINCE\tWHY\n")
		for _, r := range st.Unprotected {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", r.Engine, r.PID, age(r.Age), r.Reason)
		}
	}
	if len(st.Engines) > 0 {
		// EST. VRAM is the last run's measured peak per engine, an estimate
		// of what a run needs; it is never a reservation.
		est := map[string]uint64{}
		for _, f := range st.Footprints {
			est[f.Engine] = f.Bytes
		}
		fmt.Fprint(tw, "\nENGINE\tTYPE\tUP\tMODELS\tDECODE\tPREFILL\tOUT TOKENS\tQUEUE\tEST. VRAM\n")
		for _, e := range st.Engines {
			// One word in the cell: an error message here would stretch the
			// column. Verbose names the reason under the tables.
			up := c.Style.OK("yes")
			if !e.Up {
				up = c.Style.Err("NO")
				if e.State == "latched" {
					up = c.Style.Err("LATCHED")
				}
			}
			var names []string
			for _, m := range e.Models {
				names = append(names, m.Name)
			}
			estimate := "-"
			if b, ok := est[e.Name]; ok {
				estimate = server.MiB(b)
			}
			// a rate with no metrics behind it is unknown, not a measured idle
			decode, prefill := "-", "-"
			if e.MetricsReported {
				decode, prefill = fmt.Sprintf("%.0f t/s", e.DecodeTPS), fmt.Sprintf("%.0f t/s", e.PrefillTPS)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.0f\t%.0f\t%s\n", e.Name, e.Type, esc(up), strings.Join(names, ","),
				decode, prefill, e.Metrics.OutputTokens, e.Metrics.Processing+e.Metrics.Waiting, estimate)
		}
	}
	tw.Flush()

	if c.Verbose {
		for _, e := range st.Engines {
			detail := fmt.Sprintf("%s %s", e.Name, e.URL)
			if !e.Up && e.Error != "" {
				detail += ", " + e.Error
			}
			if e.Window > 0 {
				detail += fmt.Sprintf(", window %d", e.Window)
			}
			if len(e.Slots) > 0 {
				busy := 0
				for _, s := range e.Slots {
					if s.Processing {
						busy++
					}
				}
				detail += fmt.Sprintf(", %d slots (%d processing)", len(e.Slots), busy)
			}
			c.Sub("%s", detail)
		}
	}

	board(c, st.Counters)
	if p := st.Priority; p != nil && len(p.Engines) > 0 {
		parts := make([]string, 0, len(p.Engines))
		for _, e := range p.Engines {
			parts = append(parts, fmt.Sprintf("%d %s (%.1f h/7d)", e.Rank, e.Engine, e.Hours7d))
		}
		win := "off"
		if p.JuggleWindow > 0 {
			win = fmt.Sprintf("%gs", p.JuggleWindow)
		}
		c.Printf("juggles ranked by %s, window %s: %s\n", p.Source, win, strings.Join(parts, ", "))
	}
	if len(st.Leases)+len(st.Waiting)+len(st.Squatters)+len(st.Unprotected) == 0 {
		c.Printf("no leases, nobody waiting, no squatters\n")
	}
	return nil
}

// cardLine is what the scheduled card's gpu line shows instead of memory
// figures while the card is in a VM or switching to or from one.
// cardVM is g's VM state. A berthd older than per-card passthrough sets
// only the status-wide field, which then means its one card.
func cardVM(g proto.GPU, st *proto.Status) *proto.Passthrough {
	if g.Passthrough != nil {
		return g.Passthrough
	}
	return st.Passthrough
}

func cardLine(p *proto.Passthrough) string {
	if p.State == proto.CardDetaching {
		if p.Driver == "" {
			return "being passed through to a VM"
		}
		return "being passed through to a VM (driver " + p.Driver + ")"
	}
	return "passed through to a VM (" + p.Driver + ")"
}

// leaseState paints the one word a reader looks for, and keeps the contested
// flag beside it.
func leaseState(c *Context, l proto.Lease) string {
	word := l.State
	switch l.State {
	case "active":
		word = c.Style.OK("HOT")
	case "parked":
		word = c.Style.OK("WARM")
	case "parking":
		word = c.Style.Warn(word)
	case "draining":
		word = c.Style.Warn(word)
	case "stuck", "aground":
		word = c.Style.Err(word)
	}
	if l.Contested > 0 {
		word += " (contested " + server.MiB(l.Contested) + ")"
	}
	if l.ParkedSince > 0 {
		word += " (" + age(l.ParkedSince) + ")"
	}
	return esc(word)
}

// esc hides a painted cell from tabwriter's width count, so a coloured table
// still lines up. The literal byte is tabwriter.Escape: writing it as a rune
// would encode it as two bytes and tabwriter would not find it.
func esc(s string) string { return "\xff" + s + "\xff" }

// board is the collisions board: one line by default, one bullet per count in
// verbose, where the numbers can be read one at a time.
func board(c *Context, k proto.Counters) {
	if !c.Verbose {
		c.Printf("\nboard   %d collisions avoided, %s handed off, %d drains, %d stuck caught, %d squatters caught, %d contested grants, %d refused, %d juggles stopped (since %s)\n",
			k.CollisionsAvoided, server.MiB(k.VRAMHandedOff), k.Drains, k.StuckCaught, k.SquattersCaught, k.ContestedGrants, k.Refused, k.JugglesStopped, k.Since.Local().Format("Jan 2 15:04"))
		return
	}
	c.Printf("\nBOARD (since %s)\n", k.Since.Local().Format("Jan 2 15:04"))
	c.Bullet("%d collisions avoided", k.CollisionsAvoided)
	c.Bullet("%s handed off", server.MiB(k.VRAMHandedOff))
	c.Bullet("%d drains, %d stuck caught, %d squatters caught", k.Drains, k.StuckCaught, k.SquattersCaught)
	c.Bullet("%d contested grants, %d refused, %d juggles stopped", k.ContestedGrants, k.Refused, k.JugglesStopped)
	c.Bullet("%d socket consumers dropped, %d writes timed out", k.DroppedConsumers, k.SendTimeouts)
}

func hostLine(c *Context, h *proto.Host) {
	if h == nil {
		return
	}
	if !h.Supported {
		c.Printf("%s  no host telemetry on this platform\n", c.Style.Dim("host"))
		return
	}
	load := "load n/a"
	if h.Load != nil {
		load = fmt.Sprintf("load %.2f %.2f %.2f", h.Load[0], h.Load[1], h.Load[2])
	}
	c.Printf("%s %s\n", c.Style.Dim("host"),
		fmt.Sprintf("CPU %.0f%% of %d cores, RAM %s of %s, swap %s of %s, %s",
			h.CPU, h.Cores, server.MiB(h.RAMUsed), server.MiB(h.RAMTotal),
			server.MiB(h.SwapUsed), server.MiB(h.SwapTotal), load))
}

// alertsLine is the alert state the node holds: the node is the source of
// truth, so this prints its levels rather than deriving any.
func alertsLine(c *Context, a *proto.Alerts) {
	if a == nil {
		return
	}
	if len(a.Levels) == 0 {
		c.Printf("%s none held\n", c.Style.Dim("alerts"))
		return
	}
	keys := make([]string, 0, len(a.Levels))
	for k := range a.Levels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch a.Levels[k] {
		case "crit":
			parts = append(parts, c.Style.Err(k+" crit"))
		case "warn":
			parts = append(parts, c.Style.Warn(k+" warn"))
		default:
			parts = append(parts, k+" "+a.Levels[k])
		}
	}
	c.Printf("%s %s\n", c.Style.Dim("alerts"), strings.Join(parts, ", "))
}

func runStatus(c *Context) int {
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
	if err != nil {
		return c.fail(Systemf("no status from berthd: %v", err))
	}
	if m.Status == nil {
		return c.fail(Systemf("no status from berthd: empty reply"))
	}
	return c.Write(statusView{st: m.Status, sock: *sock}, nil)
}

// claimCardCol is a claims table cell for the CARD column, nothing with
// one card.
func claimCardCol(cards int, cell string) string {
	if cards < 2 {
		return ""
	}
	return cell + "\t"
}

// cardNames is where a claim is active or holds memory.
func cardNames(cl proto.Claim) string {
	if len(cl.Cards) == 0 {
		return "-"
	}
	return strings.Join(cl.Cards, ",")
}

// scheduledCards is how many cards berthd schedules. A line names its card
// only when there is more than one.
func scheduledCards(st *proto.Status) int {
	n := 0
	for _, g := range st.GPUs {
		if g.Scheduled {
			n++
		}
	}
	return n
}

// age is a duration since the reading, rounded to the second.
func age(s float64) string {
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}
