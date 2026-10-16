package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func runEvents(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	follow := fs.Bool("f", false, "keep following new events")
	fs.BoolVar(follow, "follow", false, "keep following new events")
	since := fs.Uint64("since", 0, "only events after this sequence number")
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
	if err := conn.Send(proto.Msg{Op: proto.OpEvents, Since: *since, Watch: *follow}); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}

	stream := c.Stream()
	for {
		m, err := conn.Recv()
		if err != nil {
			// berthd closed after the backlog, or the follow was cut short.
			return ExitOK
		}
		if m.Op == proto.OpRefused {
			return c.fail(Failf("%s", m.Reason))
		}
		e := m.Event
		if e == nil {
			continue
		}
		if c.Machine() {
			if err := stream.Write(e); err != nil {
				return c.fail(Systemf("%v", err))
			}
			continue
		}
		c.Printf("%s\n", eventLine(c, *e))
	}
}

// eventLine is one event in the human format: the sequence number, the time,
// the kind, then whoever and whatever it names. Verbose labels every field, so
// the line can be read without the column layout.
func eventLine(c *Context, e proto.Event) string {
	var b strings.Builder
	if c.Verbose {
		fmt.Fprintf(&b, "at=%s seq=%d kind=%s", e.At.Local().Format(time.RFC3339), e.Seq, e.Kind)
		if e.Lease != "" {
			fmt.Fprintf(&b, " lease=%s", e.Lease)
		}
		if e.Engine != "" {
			fmt.Fprintf(&b, " engine=%s", e.Engine)
		}
		if e.PID != 0 {
			fmt.Fprintf(&b, " pid=%d", e.PID)
		}
		if e.Bytes != 0 {
			fmt.Fprintf(&b, " bytes=%d", e.Bytes)
		}
		if e.Detail != "" {
			fmt.Fprintf(&b, " detail=%s", e.Detail)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "%6d  %s  %s", e.Seq, c.Style.Dim(e.At.Local().Format("15:04:05.000")),
		eventKind(c, fmt.Sprintf("%-18s", e.Kind)))
	if e.Engine != "" {
		b.WriteString("  " + e.Engine)
	}
	if e.Lease != "" && e.Lease != e.Engine {
		b.WriteString(" (" + e.Lease + ")")
	}
	if e.PID != 0 {
		fmt.Fprintf(&b, "  pid %d", e.PID)
	}
	if e.Bytes != 0 {
		b.WriteString("  " + server.MiB(e.Bytes))
	}
	if e.Detail != "" {
		b.WriteString("  " + e.Detail)
	}
	return b.String()
}

// eventKind paints the kind by what it means to a reader: a failure to look
// at, a warning to know about, a recovery, or the ordinary flow.
func eventKind(c *Context, kind string) string {
	switch kind {
	case proto.EvRefused, proto.EvStuck, proto.EvProbeFailed, proto.EvAlertRaised:
		return c.Style.Err(kind)
	case proto.EvSquatter, proto.EvJuggle:
		return c.Style.Warn(kind)
	case proto.EvGranted, proto.EvFreed, proto.EvReattached, proto.EvAlertCleared, proto.EvProbeRecovered, proto.EvSquatterGone:
		return c.Style.OK(kind)
	}
	return c.Style.Dim(kind)
}
