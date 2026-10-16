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

func (v statusView) Human(c *Context, ok bool) error {
	st := v.st
	c.Printf("%s\n", c.Style.Dim(fmt.Sprintf("berthd %s on %s (api %d)", st.Version, v.sock, st.API)))

	if len(st.GPUs) == 0 {
		c.Printf("gpu     no reading yet\n")
	}
	for _, g := range st.GPUs {
		role := "inventory only"
		if g.Scheduled {
			role = "leased by berth, baseline " + server.MiB(g.Baseline)
		}
		c.Printf("gpu%d    %s: %s used of %s, %s free (%s, read %s ago)\n", g.Index, g.Name,
			server.MiB(g.Used), server.MiB(g.Total), server.MiB(g.Free), role, age(g.Age))
	}
	if st.ProbeErr != "" {
		c.Printf("probe   %s\n", c.Style.Err("FAILING: "+st.ProbeErr))
	}
	if c.Verbose {
		hostLine(c, st.Host)
		alertsLine(c, st.Alerts)
	}

	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', tabwriter.StripEscape)
	lease := func(title string, ls []proto.Lease) {
		if len(ls) == 0 {
			return
		}
		header := "\n" + title + "\tENGINE\tPRIORITY\tSTATE\tNEEDS\tHOLDS\tPIDS"
		if c.Verbose {
			header += "\tPID"
		}
		fmt.Fprintln(tw, header+"\tAGE")
		for _, l := range ls {
			needs := "card"
			if l.Needs > 0 {
				needs = server.MiB(l.Needs)
			}
			state := leaseState(c, l)
			row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%v", l.ID, l.Engine, l.Priority, state, needs, server.MiB(l.Used), l.GPUPIDs)
			if c.Verbose {
				row += fmt.Sprintf("\t%d", l.PID)
			}
			fmt.Fprintf(tw, "%s\t%s\n", row, age(l.Age))
		}
	}
	lease("LEASE", st.Leases)
	lease("WAITING", st.Waiting)
	if len(st.Squatters) > 0 {
		fmt.Fprint(tw, "\nSQUATTER\tPID\tHOLDS\tSEEN\tUNPROTECTED RUN\n")
		for _, s := range st.Squatters {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", s.Name, s.PID, server.MiB(s.Used), age(s.Age), s.Run)
		}
	}
	if len(st.Unprotected) > 0 {
		fmt.Fprint(tw, "\nUNPROTECTED\tPID\tSINCE\tWHY\n")
		for _, r := range st.Unprotected {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", r.Engine, r.PID, age(r.Age), r.Reason)
		}
	}
	if len(st.Engines) > 0 {
		fmt.Fprint(tw, "\nENGINE\tTYPE\tUP\tMODELS\tDECODE\tPREFILL\tOUT TOKENS\tQUEUE\n")
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f t/s\t%.0f t/s\t%.0f\t%.0f\n", e.Name, e.Type, esc(up), strings.Join(names, ","),
				e.DecodeTPS, e.PrefillTPS, e.Metrics.OutputTokens, e.Metrics.Processing+e.Metrics.Waiting)
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

// leaseState paints the one word a reader looks for, and keeps the contested
// flag beside it.
func leaseState(c *Context, l proto.Lease) string {
	word := l.State
	switch l.State {
	case "active":
		word = c.Style.OK(word)
	case "draining":
		word = c.Style.Warn(word)
	case "stuck", "aground":
		word = c.Style.Err(word)
	}
	if l.Contested > 0 {
		word += " (contested " + server.MiB(l.Contested) + ")"
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
	c.Printf("%s %s\n", c.Style.Dim("host"),
		fmt.Sprintf("CPU %.0f%% of %d cores, RAM %s of %s, swap %s of %s, load %.2f %.2f %.2f",
			h.CPU, h.Cores, server.MiB(h.RAMUsed), server.MiB(h.RAMTotal),
			server.MiB(h.SwapUsed), server.MiB(h.SwapTotal), h.Load[0], h.Load[1], h.Load[2]))
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

// age is a duration since the reading, rounded to the second.
func age(s float64) string {
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}
