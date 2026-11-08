// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

// berth unpark and berth park: a person's say over RAM parking. Parking
// stays automatic; unpark drops a parked engine from RAM now (a long render
// that wants the RAM runs it first), and park off stops parking until park
// on.

import (
	"strings"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func runUnpark(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	all := fs.Bool("all", false, "every parked engine")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	if (len(c.Args) == 1) == *all || len(c.Args) > 1 {
		return c.fail(Failf("berth unpark takes one engine name, or --all").
			WithHint("usage: berth unpark ENGINE | berth unpark --all"))
	}
	engine := ""
	if !*all {
		engine = c.Args[0]
	}
	m, e := parkCall(*sock, proto.Msg{Op: proto.OpUnpark, Engine: engine}, proto.OpUnpark)
	if e != nil {
		return c.fail(e)
	}
	return c.Write(parkView{engine: engine, leases: nonNil(m.Leases), unpark: true}, nil)
}

func runPark(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	req := proto.Msg{Op: proto.OpParkMode}
	switch {
	case len(c.Args) == 1 && c.Args[0] == "off":
		off := true
		req.ParkOff = &off
	case len(c.Args) == 1 && c.Args[0] == "on":
		off := false
		req.ParkOff = &off
	case len(c.Args) == 0 || (len(c.Args) == 1 && c.Args[0] == "status"):
	default:
		return c.fail(Failf("berth park takes on, off or status").WithHint("usage: berth park [on|off|status]"))
	}
	m, e := parkCall(*sock, req, proto.OpParkMode)
	if e != nil {
		return c.fail(e)
	}
	v := parkView{leases: nonNil(m.Leases), asked: req.ParkOff != nil}
	if m.ParkOff != nil {
		v.off = *m.ParkOff
	}
	return c.Write(v, nil)
}

// parkCall sends one request and reads its one reply.
func parkCall(sock string, req proto.Msg, want string) (proto.Msg, *Error) {
	conn, raw, err := server.Dial(sock)
	if err != nil {
		return proto.Msg{}, Systemf("berthd unreachable on %s: %v", sock, err)
	}
	defer raw.Close()
	if err := conn.Send(req); err != nil {
		return proto.Msg{}, Systemf("berthd on %s: %v", sock, err)
	}
	m, err := conn.Recv()
	if err != nil {
		return proto.Msg{}, Systemf("no reply from berthd: %v", err)
	}
	if m.Op == proto.OpRefused {
		return proto.Msg{}, Failf("%s", m.Reason)
	}
	if m.Op != want {
		return proto.Msg{}, Systemf("unexpected reply from berthd: op %q (an older berthd? restart it after an update)", m.Op)
	}
	return m, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type parkView struct {
	engine string
	leases []string
	unpark bool // berth unpark, else berth park
	asked  bool // berth park on|off, else status
	off    bool
}

func (v parkView) Data() any {
	if v.unpark {
		return struct {
			Engine  string   `json:"engine,omitempty" yaml:"engine,omitempty"`
			Evicted []string `json:"evicted" yaml:"evicted"`
		}{v.engine, v.leases}
	}
	return struct {
		Parking string   `json:"parking" yaml:"parking"`
		Evicted []string `json:"evicted" yaml:"evicted"`
	}{map[bool]string{true: "off", false: "on"}[v.off], v.leases}
}

func (v parkView) Human(c *Context, ok bool) error {
	if !v.unpark {
		if v.off {
			c.Printf("%s parking is off: a drain stops its engine instead of parking it in RAM\n", c.Style.OK(markOK))
			if !v.asked {
				c.Bullet("berth park on turns it back on")
			}
		} else {
			c.Printf("%s parking is on: a drained engine parks in RAM when it fits\n", c.Style.OK(markOK))
		}
	}
	switch {
	case len(v.leases) > 0:
		c.Printf("evicted from RAM: %s\n", strings.Join(v.leases, ", "))
		c.Bullet("each stops through its own wrapper; its RAM frees as it exits")
	case v.unpark && v.engine != "":
		c.Printf("nothing parked for %s\n", v.engine)
	case v.unpark:
		c.Printf("nothing parked\n")
	}
	return nil
}
