// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/vfio"
)

// vfioSettle is how long the run waits after the grant for berthd's own
// probe to go quiet (a daemon that knows VFIO pauses it; an older one
// finishes its nvidia-smi in that time), before the opener check decides.
const vfioSettle = 5 * time.Second

// vfioBack is how long the card may take to come back after the script
// exits: the driver rebind takes seconds.
const vfioBack = 60 * time.Second

// vfioPoll is how often the driver state is re-read while the card comes
// back.
const vfioPoll = 2 * time.Second

// vfioRunOpts carries what a test replaces: the reader's roots, the
// nvidia-smi stand-in, the waits, and the signal source.
type vfioRunOpts struct {
	reader vfio.Reader
	smiBin string
	settle time.Duration    // 0 = vfioSettle
	back   time.Duration    // 0 = vfioBack
	poll   time.Duration    // 0 = vfioPoll
	sigs   <-chan os.Signal // nil: the real signals
}

func (o vfioRunOpts) orSettle() time.Duration {
	if o.settle > 0 {
		return o.settle
	}
	return vfioSettle
}

func (o vfioRunOpts) orBack() time.Duration {
	if o.back > 0 {
		return o.back
	}
	return vfioBack
}

func (o vfioRunOpts) orPoll() time.Duration {
	if o.poll > 0 {
		return o.poll
	}
	return vfioPoll
}

// ctxCanceled is closed by the wait goroutine when a signal arrived during
// the wait for the grant.
func ctxCanceled(canceled <-chan struct{}) bool {
	select {
	case <-canceled:
		return true
	default:
		return false
	}
}

// gotSignal says whether a signal is waiting on the channel: during the
// settle it means the operator asked out.
func gotSignal(sigs <-chan os.Signal) bool {
	select {
	case <-sigs:
		return true
	default:
		return false
	}
}

// drainSignals drops any signal the CMD phase buffered: it was meant for
// the script, and the release loops below recheck on the next one.
func drainSignals(sigs <-chan os.Signal) {
	for gotSignal(sigs) {
	}
}

// runVfioRun holds the card for a passthrough and runs the operator's own
// switch script with the terminal attached. The script (sudo, virsh) owns
// the switch and the VM; Berth owns the drain, the proof the device is
// free, and the wait for the card to come back.
func runVfioRun(c *Context) int {
	r := vfio.Reader{}
	if err := r.Supported(); err != nil {
		return c.fail(Systemf("%v", err))
	}
	return vfioRun(c, vfioRunOpts{reader: r})
}

// vfioRun is the run flow; a test names its own reader roots, nvidia-smi
// stand-in, waits and signals.
func vfioRun(c *Context, o vfioRunOpts) int {
	say := func(format string, a ...any) { fmt.Fprintf(c.Err, "berth: "+format+"\n", a...) }
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	reason := fs.String("reason", "", "shown on the lease, e.g. galaxy (default: passthrough)")
	wait := fs.Duration("wait", 10*time.Minute, "how long to wait for engines to drain; 0 = berthd's default; negative = no deadline")
	before := fs.String("before", "", "shell command run once the engines are off the card, before the check that nothing holds it (stop your own services here)")
	after := fs.String("after", "", "shell command run once the hold has ended, whenever --before ran (start those services again)")
	gpuKey := fs.String("gpu", "", "card to pass through: its name on the node (gpu1) or its PCI address; default the node's first card")
	if code, done := c.Parse(fs); done {
		return code
	}
	if len(c.Args) == 0 {
		return c.fail(Systemf("the command to run is required").
			WithHint("usage: berth vfio run [--reason NAME] -- CMD [ARGS...]"))
	}
	argv := c.Args
	if *gpuKey != "" {
		if err := gpu.CheckCard(*gpuKey); err != nil {
			return c.fail(Systemf("%v", err))
		}
		if strings.Contains(*gpuKey, ",") {
			return c.fail(Systemf("--gpu %s names several cards; a passthrough moves one card", *gpuKey))
		}
	}

	// The card, by way of berthd's scheduled index and nvidia-smi.
	card, e := vfioCardAt(*sock, "", *gpuKey, o.smiBin)
	if e != nil {
		return c.fail(e)
	}
	if card.Daemon == nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, card.DialErr).
			WithHint("nothing is held: start berthd, or the card is not coordinated at all"))
	}

	// Preflight, before anything is asked of the engines: without IOMMU, or
	// with devices that are not the card's in its group, the passthrough
	// cannot work and the script would fail halfway.
	funcs, e := vfioPreflight(o.reader, card)
	if e != nil {
		return c.fail(e)
	}

	name := "vfio"
	if r := strings.TrimSpace(*reason); r != "" {
		name = "vfio:" + strings.ReplaceAll(r, " ", "-")
	}

	// The signals: the operator's Ctrl-C. During the wait for the grant a
	// signal cancels it; from the grant on, this run's own loops read them.
	var sigs <-chan os.Signal
	if o.sigs != nil {
		sigs = o.sigs
	} else {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(ch)
		sigs = ch
	}

	// The hold: engines drain at their boundaries, none starts, until this
	// process ends. Pinned at hold priority, like berth hold.
	conn, raw, err := server.Dial(*sock)
	if err != nil {
		return c.fail(Systemf("berthd unreachable on %s: %v", *sock, err).
			WithHint("nothing is held: start berthd, or the card is not coordinated at all"))
	}
	defer raw.Close()
	stopped := make(chan struct{})
	canceled := make(chan struct{})
	go func() {
		select {
		case <-stopped:
		case <-sigs:
			close(canceled)
			raw.Close()
		}
	}()
	if err := conn.Send(proto.Msg{Op: proto.OpAcquire, Engine: name, Card: *gpuKey, Group: proto.GroupVFIO, Priority: "hold",
		Wait: wait.Seconds(), Pinned: true}); err != nil {
		return c.fail(Systemf("berthd on %s: %v", *sock, err))
	}
	lease := ""
	for granted := false; !granted; {
		m, err := conn.Recv()
		if err != nil {
			if ctxCanceled(canceled) {
				fmt.Fprintln(c.Err, "berth: vfio hold cancelled before the card was free")
				return 0
			}
			return c.fail(Systemf("berthd went away: %v", err))
		}
		switch m.Op {
		case proto.OpWaiting:
			fmt.Fprintf(c.Err, "berth: waiting: %s\n", m.Reason)
		case proto.OpRefused:
			return c.fail(Failf("vfio hold refused: %s", m.Reason))
		case proto.OpGranted:
			lease = m.Lease
			granted = true
		}
	}
	close(stopped)
	if ctxCanceled(canceled) {
		fmt.Fprintln(c.Err, "berth: vfio hold cancelled before the card was free")
		return 0
	}

	say("card held as %s (%s); engines are off the card", lease, name)
	devices := vfioFunctionList(funcs, card.Addr)

	// --before stops what the operator runs beside the engines (a monitor
	// loop, a desktop shell) while the hold keeps the engines off; --after
	// starts it again once the hold has ended, whatever happened between.
	// The hold is closed first: a deferred --after must not run while the
	// card is still held.
	if *before != "" {
		defer func() {
			raw.Close()
			if *after == "" {
				return
			}
			say("running --after: %s", *after)
			if code, err := vfioExec([]string{"/bin/sh", "-c", *after}, devices); err != nil || code != 0 {
				c.Warnf("--after did not finish cleanly (exit %d, %v)", code, err)
			}
		}()
		say("running --before: %s", *before)
		if code, err := vfioExec([]string{"/bin/sh", "-c", *before}, devices); err != nil || code != 0 {
			return c.fail(Failf("--before did not finish cleanly (exit %d, %v); not holding %s", code, err, card.Addr))
		}
	}

	// berthd's probe opens the card's device every tick. Give it the settle
	// time to go quiet (V1 pauses it for a vfio hold), then decide on one
	// reading: anyone else holding a node keeps the card where it is.
	time.Sleep(o.orSettle())
	if ctxCanceled(canceled) || gotSignal(sigs) {
		fmt.Fprintln(c.Err, "berth: vfio hold cancelled before the card was free")
		return 0
	}
	openers, err := o.reader.Openers(card.Addr)
	if err != nil {
		return c.fail(Systemf("read the openers: %v", err))
	}
	if bad := vfioUnreadable(openers); len(bad) > 0 {
		named := strings.Join(bad[:min(len(bad), 3)], "; ")
		if len(bad) > 3 {
			named += "; ..."
		}
		c.Warnf("%d processes could not be read, so they are not proven clear (%s); if one holds the card, the driver unbind blocks",
			len(bad), named)
	}
	if bad := vfioBadOpeners(openers); len(bad) > 0 {
		raw.Close() // the hold is not earned: the next engine may load
		e := Failf("the card's device nodes are in use; not holding %s", card.Addr)
		for _, b := range bad {
			e = e.Detail("%s", b)
		}
		e = e.WithHint("stop the holders yourself (Berth never stops a process); berth vfio status names them. " +
			"An engine for another card opens this one too unless CUDA_VISIBLE_DEVICES names its own card's UUID")
		return c.fail(e)
	}

	say("running %s with %s", argv[0], devices)
	code, err := vfioExec(argv, devices)
	if err != nil {
		// It never started, so the card never left: nothing to wait for.
		return c.fail(Systemf("start %s: %v", argv[0], err))
	}
	if code != 0 {
		say("%s exited with %d", argv[0], code)
	}

	// The hold ends only when the card is back for the next engine: every
	// function of the card off vfio-pci, and the card answering nvidia-smi.
	drainSignals(sigs)
	answers := func() error { return vfioAnswers(o.smiBin, card.BusID, card.Addr) }
	if back, _ := vfioCardBack(o.reader, funcs, card.Addr, answers); back {
		say("the card is back; hold released")
		return code
	}
	// Poll the driver for the rebind the script's tail does; a signal
	// during the wait goes to the manual wait below, which releases only
	// once the card really is back.
	until := time.Now().Add(o.orBack())
poll:
	for {
		select {
		case <-time.After(o.orPoll()):
		case <-sigs:
			break poll
		}
		if back, _ := vfioCardBack(o.reader, funcs, card.Addr, answers); back {
			say("the card is back; hold released")
			return code
		}
		if time.Now().After(until) {
			break
		}
	}
	// Not back on its own: say which function and what to do about it, and
	// keep the hold. Ctrl-C rechecks; it releases only when the card is
	// back, so the next engine never loads onto a card that is out.
	vfioWaitForTheCard(o.reader, funcs, card.Addr, answers, sigs, say)
	return code
}

// vfioWaitForTheCard says what is not back and what to do about it, then
// waits for a signal; on each one it rechecks and releases only when the
// card is back.
func vfioWaitForTheCard(r vfio.Reader, funcs []vfio.Function, addr string, answers func() error,
	sigs <-chan os.Signal, say func(string, ...any)) {
	for {
		back, why := vfioCardBack(r, funcs, addr, answers)
		if back {
			say("the card is back; hold released")
			return
		}
		say("%s; the hold stays", why)
		<-sigs
	}
}

// vfioPreflight refuses a passthrough that cannot work, before anything is
// asked of the engines. It returns the card's own functions, which the run
// passes to the script and watches until each is back.
func vfioPreflight(r vfio.Reader, card vfioCard) ([]vfio.Function, *Error) {
	on, err := r.IOMMUOn()
	if err != nil {
		return nil, Systemf("%v", err)
	}
	if !on {
		return nil, Failf("IOMMU is off; the card cannot pass through").
			WithHint("turn VT-d (or AMD-Vi) on in the firmware and add intel_iommu=on iommu=pt (or its AMD form) to the kernel command line")
	}
	group, err := r.Group(card.Addr)
	if err != nil {
		return nil, Systemf("%v", err)
	}
	var funcs []vfio.Function
	var others []string
	for _, f := range group {
		if vfio.CardFunction(f.Addr, card.Addr) {
			funcs = append(funcs, f)
			continue
		}
		others = append(others, fmt.Sprintf("%s (%s)", f.Addr, orDash(f.Driver)))
	}
	if len(others) > 0 {
		return nil, Failf("the IOMMU group of %s holds %s, and the whole group goes with the card", card.Addr, strings.Join(others, ", ")).
			WithHint("pass the other device through too, or move the card to its own group (ACS)")
	}
	return funcs, nil
}

// vfioBadOpeners is the openers that keep the card where it is: every
// process that is not berthd's own probe and that the reader could read.
func vfioBadOpeners(openers []vfio.Opener) []string {
	var bad []string
	for _, o := range openers {
		switch {
		case o.Unreadable:
		case o.Probe:
			// berthd stops its probe for a VFIO hold; one still open after
			// the settle time is a berthd that does not, and it would block
			// the unbind like any other holder.
			bad = append(bad, fmt.Sprintf("pid %d%s holds %s: berthd's own probe, still running; upgrade berthd so it stops the probe for a VFIO hold",
				o.PID, vfioNamed(o.Name), strings.Join(o.Nodes, ", ")))
		default:
			bad = append(bad, fmt.Sprintf("pid %d%s holds %s", o.PID, vfioNamed(o.Name), strings.Join(o.Nodes, ", ")))
		}
	}
	return bad
}

// vfioUnreadable is the processes whose fd directories could not be read.
// They are reported, never refused: an unreadable list is no proof of a
// holder, and the unbind the script does blocks where there is one.
func vfioUnreadable(openers []vfio.Opener) []string {
	var bad []string
	for _, o := range openers {
		if o.Unreadable {
			bad = append(bad, fmt.Sprintf("pid %d%s: /proc unreadable", o.PID, vfioNamed(o.Name)))
		}
	}
	return bad
}

func vfioNamed(name string) string {
	if name == "" {
		return ""
	}
	return " " + name
}

// vfioCardBack reports whether the card is back for the next engine: every
// function of the card on a host driver (anything but vfio-pci, and not
// unbound), and the card answering nvidia-smi. why names the first thing
// that is not, and what to do about it.
func vfioCardBack(r vfio.Reader, funcs []vfio.Function, addr string, answers func() error) (bool, string) {
	for _, f := range funcs {
		d, err := r.Driver(f.Addr)
		if err != nil {
			return false, fmt.Sprintf("%s: %v", f.Addr, err)
		}
		if d == "vfio-pci" {
			return false, fmt.Sprintf("%s is still bound to vfio-pci; stop the VM and bind the function back to its host driver", f.Addr)
		}
		if d == "" {
			return false, fmt.Sprintf("%s is bound to no driver; bind it back to its host driver", f.Addr)
		}
	}
	if err := answers(); err != nil {
		return false, fmt.Sprintf("%s is off vfio-pci but does not answer nvidia-smi (%v); the driver is still coming up", addr, err)
	}
	return true, ""
}

// vfioFunctionList is the card's functions, space-separated, for
// BERTH_VFIO_DEVICES. The card's own function first, so a single-function
// card reads one plain address.
func vfioFunctionList(funcs []vfio.Function, card string) string {
	out := []string{card}
	for _, f := range funcs {
		if f.Addr != card {
			out = append(out, f.Addr)
		}
	}
	return strings.Join(out, " ")
}

// vfioExec runs one of the operator's commands with the terminal attached,
// so a sudo prompt and a VM console reach it, and with the card's PCI
// functions in BERTH_VFIO_DEVICES, so it hardcodes no address. It stays in
// this process group, unlike an engine under berth run: a child in its own
// group that reads the terminal is stopped by SIGTTIN, and sudo would hang
// at its password prompt. A Ctrl-C reaches it from the terminal directly,
// so nothing is forwarded. err is set only when the command did not start;
// a command that ran reports its exit code, whatever it is.
func vfioExec(argv []string, devices string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "BERTH_VFIO_DEVICES="+devices)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return runner.ExitCode(cmd, cmd.Wait()), nil
}
