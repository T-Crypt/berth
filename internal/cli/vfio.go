// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/vfio"
)

// vfioCard is the card Berth governs: berthd's scheduled index, and its PCI
// address in both forms nvidia-smi and sysfs use.
type vfioCard struct {
	Index   int    // gpu index nvidia-smi answers with
	BusID   string // 00000000:01:00.0, the form nvidia-smi's -i takes
	Addr    string // 0000:01:00.0, the form sysfs uses
	Daemon  *proto.Status
	DialErr error // why Daemon is nil
}

// vfioCardAt resolves the governed card. With an explicit address (status
// takes one as its argument) nvidia-smi is not asked, so the state stays
// readable while the card is bound to vfio-pci and nvidia-smi cannot see it;
// berthd is asked either way. Otherwise the status berthd reports names the scheduled
// index, and nvidia-smi names that card's bus id; when berthd is down or
// has no reading yet, index 0. key is the card a run names (vfio run
// --gpu), "" for berthd's first card; berthd's status is narrowed to it.
func vfioCardAt(sock, explicit, key, smiBin string) (vfioCard, *Error) {
	var card vfioCard
	st, err := vfioStatusOf(sock)
	if err == nil && key != "" {
		if st = node.View(st, key); st == nil {
			return card, Failf("berthd has no card %s", key).
				WithHint("berth status names the cards berthd schedules")
		}
	}
	card.Daemon, card.DialErr = st, err
	if explicit != "" {
		card.Addr = vfio.SysAddr(explicit)
		card.BusID = explicitNvidiaForm(card.Addr)
		return card, nil
	}
	if err == nil {
		card.Index = vfioIndex(st)
	}
	addr, busID, err := vfioBusID(smiBin, card.Index)
	if err != nil {
		return card, Systemf("cannot read the card's PCI address: %v", err).
			WithHint("pass the card's PCI address yourself: nvidia-smi cannot see a card bound to vfio-pci (for example: berth vfio status 0000:01:00.0)")
	}
	card.Addr, card.BusID = addr, busID
	return card, nil
}

// explicitNvidiaForm is an address in nvidia-smi's domain form
// ("0000:01:00.0" -> "00000000:01:00.0").
func explicitNvidiaForm(addr string) string {
	domain, rest, ok := strings.Cut(addr, ":")
	if !ok {
		return addr
	}
	for len(domain) < 8 {
		domain = "0" + domain
	}
	return domain + ":" + rest
}

// vfioStatusOf asks berthd for its view of the node.
func vfioStatusOf(sock string) (*proto.Status, error) {
	conn, raw, err := server.Dial(sock)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		return nil, err
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		return nil, fmt.Errorf("no status from berthd: %w", err)
	}
	return m.Status, nil
}

// vfioIndex is the card berthd leases: the scheduled GPU's index, 0 when
// berthd has no reading yet.
func vfioIndex(st *proto.Status) int {
	for _, g := range st.GPUs {
		if g.Scheduled {
			return g.Index
		}
	}
	return 0
}

// vfioBusID asks nvidia-smi for one card's PCI bus id, in both forms:
// sysfs's, which the reader takes, and nvidia-smi's own, which -i takes.
func vfioBusID(bin string, index int) (addr, busID string, err error) {
	out, err := vfioSMI(bin, "-i", strconv.Itoa(index), "--query-gpu=pci.bus_id", "--format=csv,noheader")
	if err != nil {
		return "", "", err
	}
	busID = strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	return vfio.SysAddr(busID), strings.TrimSpace(busID), nil
}

// vfioAnswers reports whether the card answers nvidia-smi again: its driver
// is bound and talking.
func vfioAnswers(bin, busID, addr string) error {
	out, err := vfioSMI(bin, "-i", busID, "--query-gpu=pci.bus_id", "--format=csv,noheader")
	if err != nil {
		return err
	}
	if got := vfio.SysAddr(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]); got != addr {
		return fmt.Errorf("nvidia-smi answered with %s, not %s", got, addr)
	}
	return nil
}

// vfioSMI runs nvidia-smi once and returns its stdout. The binary is a
// parameter, so tests point it at a stand-in.
func vfioSMI(bin string, args ...string) (string, error) {
	if bin == "" {
		bin = "nvidia-smi"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// vfioStatusDoc is what `berth vfio status --format json` prints: the
// reading, and what berthd reports about the card when it answers.
type vfioStatusDoc struct {
	Addr     string          `json:"addr"`
	IOMMU    bool            `json:"iommu"`
	Group    []vfio.Function `json:"group"`
	Blockers []string        `json:"blockers,omitempty"` // group members that are not the card's
	Passed   bool            `json:"passed_through"`     // the card is bound to vfio-pci
	Openers  []vfio.Opener   `json:"openers"`
	Berthd   *vfioBerthdDoc  `json:"berthd,omitempty"`
}

type vfioBerthdDoc struct {
	Reachable bool        `json:"reachable"`
	ProbeErr  string      `json:"probe_error,omitempty"`
	GPU       *vfioGPUDoc `json:"gpu,omitempty"`
	Hold      string      `json:"hold,omitempty"` // the granted vfio hold, lease and engine
	// Passthrough is berthd's card state while the card is in a VM or
	// switching ("passthrough", "detaching"); its memory figures are then the
	// last reading before the hold, not the card now.
	Passthrough string `json:"passthrough,omitempty"`
}

type vfioGPUDoc struct {
	Index int     `json:"index"`
	Name  string  `json:"name"`
	Used  uint64  `json:"used"`
	Total uint64  `json:"total"`
	Age   float64 `json:"age_seconds"`
}

type vfioStatusView struct{ doc vfioStatusDoc }

func (v vfioStatusView) Data() any { return v.doc }

func (v vfioStatusView) Human(c *Context, ok bool) error {
	d := v.doc
	state := fmt.Sprintf("on %s", d.Addr)
	if d.Passed {
		state = fmt.Sprintf("on %s: passed through (vfio-pci)", d.Addr)
	} else {
		for _, f := range d.Group {
			if f.Addr == d.Addr && f.Driver != "" {
				state = fmt.Sprintf("on %s: bound to %s", d.Addr, f.Driver)
			}
		}
	}
	c.Printf("vfio    %s\n", state)
	c.Printf("iommu   %s\n", onOff(d.IOMMU))
	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, " \tFUNCTION\tDRIVER\t")
	for _, f := range d.Group {
		role := "not the card's; blocks the passthrough"
		if vfio.CardFunction(f.Addr, d.Addr) {
			role = "the card"
		}
		fmt.Fprintf(tw, " \t%s\t%s\t%s\n", f.Addr, orDash(f.Driver), c.Style.Dim(role))
	}
	tw.Flush()
	if len(d.Openers) == 0 {
		c.Printf("openers none\n")
	} else {
		confirmed, unreadable := 0, 0
		for _, o := range d.Openers {
			if o.Unreadable {
				unreadable++
			} else {
				confirmed++
			}
		}
		c.Printf("openers %d\n", confirmed)
		for _, o := range d.Openers {
			if o.Unreadable {
				continue
			}
			who := strconv.Itoa(o.PID)
			if o.Name != "" {
				who += " " + o.Name
			}
			if o.Probe {
				c.Sub("pid %s holds %s (berthd's own probe)", who, strings.Join(o.Nodes, ", "))
				continue
			}
			c.Sub("pid %s holds %s", who, strings.Join(o.Nodes, ", "))
		}
		if unreadable > 0 {
			names := make([]string, 0, 2)
			for _, o := range d.Openers {
				if !o.Unreadable {
					continue
				}
				who := "pid " + strconv.Itoa(o.PID)
				if o.Name != "" {
					who += " " + o.Name
				}
				if len(names) < 2 {
					names = append(names, who)
				}
			}
			c.Sub("%s and %d more: %s (their fd directories could not be read; they are not proven clear)",
				strings.Join(names, ", "), unreadable-len(names), c.Style.Warn("/proc unreadable"))
		}
	}
	if d.Berthd == nil {
		return nil
	}
	if !d.Berthd.Reachable {
		c.Printf("berthd  %s\n", c.Style.Dim("unreachable; what it reports is not shown"))
		return nil
	}
	if g := d.Berthd.GPU; g != nil {
		if d.Berthd.Passthrough != "" {
			c.Printf("berthd  gpu%d %s: %s (last memory reading %.0f s ago)\n",
				g.Index, g.Name, cardLine(&proto.Passthrough{State: d.Berthd.Passthrough, Driver: vfioDriverOf(d)}), g.Age)
		} else {
			c.Printf("berthd  gpu%d %s: %s used of %s (read %.0f s ago)\n",
				g.Index, g.Name, server.MiB(g.Used), server.MiB(g.Total), g.Age)
		}
	}
	if d.Berthd.ProbeErr != "" {
		c.Printf("probe   %s\n", c.Style.Err(d.Berthd.ProbeErr))
	}
	if d.Berthd.Hold != "" {
		c.Printf("hold    %s granted\n", d.Berthd.Hold)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// runVfioStatus prints the card's VFIO state. Read only: it changes nothing.
func runVfioStatus(c *Context) int {
	r := vfio.Reader{}
	if err := r.Supported(); err != nil {
		return c.fail(Systemf("%v", err))
	}
	return vfioStatus(c, r, "")
}

// vfioStatus is the status flow; a test names its own reader roots and
// nvidia-smi stand-in.
func vfioStatus(c *Context, r vfio.Reader, smiBin string) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if len(c.Args) > 1 {
		return c.fail(Systemf("unexpected argument %q", c.Args[1]).
			WithHint("the optional argument is the card's PCI address, as in: berth vfio status 0000:01:00.0"))
	}
	explicit := ""
	if len(c.Args) == 1 {
		explicit = c.Args[0]
	}
	card, e := vfioCardAt(*sock, explicit, "", smiBin)
	if e != nil {
		return c.fail(e)
	}
	on, err := r.IOMMUOn()
	if err != nil {
		return c.fail(Systemf("%v", err))
	}
	var group []vfio.Function
	if on {
		// Without IOMMU the card has no group to read; the report says the
		// state that blocks the passthrough and stays readable.
		group, err = r.Group(card.Addr)
		if err != nil {
			return c.fail(Systemf("%v", err))
		}
	}
	if group == nil {
		group = []vfio.Function{}
	}
	openers, err := r.Openers(card.Addr)
	if err != nil {
		return c.fail(Systemf("%v", err))
	}
	if openers == nil {
		openers = []vfio.Opener{}
	}
	doc := vfioStatusDoc{Addr: card.Addr, IOMMU: on, Group: group, Openers: openers}
	for _, f := range group {
		if !vfio.CardFunction(f.Addr, card.Addr) {
			doc.Blockers = append(doc.Blockers, f.Addr)
		}
		if f.Addr == card.Addr && f.Driver == "vfio-pci" {
			doc.Passed = true
		}
	}
	doc.Berthd = vfioBerthdView(card)
	return c.Write(vfioStatusView{doc: doc}, nil)
}

// vfioBerthdView is the card state berthd reports: the scheduled card, its
// probe, and the vfio hold it holds. berthd being down is a report line,
// not a failure: the reading above is the command's work.
func vfioBerthdView(card vfioCard) *vfioBerthdDoc {
	if card.Daemon == nil {
		return &vfioBerthdDoc{}
	}
	b := &vfioBerthdDoc{Reachable: true, ProbeErr: card.Daemon.ProbeErr}
	if p := card.Daemon.Passthrough; p != nil {
		b.Passthrough = p.State
	}
	for _, g := range card.Daemon.GPUs {
		if g.Index == card.Index {
			b.GPU = &vfioGPUDoc{Index: g.Index, Name: g.Name, Used: g.Used, Total: g.Total, Age: g.Age}
		}
	}
	for _, l := range card.Daemon.Leases {
		if l.Group == proto.GroupVFIO {
			b.Hold = l.Engine + " " + l.ID
		}
	}
	return b
}

// vfioDriverOf is the driver bound to the card's own function in the reading.
func vfioDriverOf(d vfioStatusDoc) string {
	for _, f := range d.Group {
		if f.Addr == d.Addr {
			return f.Driver
		}
	}
	return ""
}
