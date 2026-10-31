// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cli

// berth vfio against a fake sysfs, a fake /proc and a live daemon: the
// reading, the refusals with their holds released, the happy path with a
// script that flips the fake drivers and back, and the hold kept while the
// card is still out.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/vfio"
)

const (
	vfioAddr      = "0000:01:00.0"
	vfioNvidiaBus = "00000000:01:00.0"
)

// vfioTree is the fake sysfs and /proc the tests build.
type vfioTree struct {
	t     *testing.T
	sysfs string
	proc  string
}

func newVfioTree(t *testing.T) *vfioTree {
	t.Helper()
	return &vfioTree{t: t, sysfs: t.TempDir(), proc: t.TempDir()}
}

// function makes one PCI function with a driver ("" = unbound).
func (tr *vfioTree) function(addr, driver string) {
	tr.t.Helper()
	d := filepath.Join(tr.sysfs, "bus", "pci", "devices", addr)
	if err := os.MkdirAll(d, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if driver != "" {
		if err := os.Symlink(filepath.Join(tr.sysfs, "bus", "pci", "drivers", driver), filepath.Join(d, "driver")); err != nil {
			tr.t.Fatal(err)
		}
	}
}

// group is the card's IOMMU group: every function made so far is linked
// into it, the way sysfs lists a group's members.
func (tr *vfioTree) group() {
	tr.t.Helper()
	group := filepath.Join(tr.sysfs, "kernel", "iommu_groups", "12")
	if err := os.MkdirAll(filepath.Join(group, "devices"), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(tr.sysfs, "bus", "pci", "devices"))
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, e := range entries {
		d := filepath.Join(tr.sysfs, "bus", "pci", "devices", e.Name())
		if err := os.Symlink(group, filepath.Join(d, "iommu_group")); err != nil {
			tr.t.Fatal(err)
		}
		if err := os.Symlink(d, filepath.Join(group, "devices", e.Name())); err != nil {
			tr.t.Fatal(err)
		}
	}
}

// opener records a process holding device nodes, the way /proc does.
func (tr *vfioTree) opener(pid int, comm string, parent int, nodes ...string) {
	tr.t.Helper()
	p := filepath.Join(tr.proc, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(p, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "stat"), []byte(fmt.Sprintf("%d (%s) S %d 0 0 0 0", pid, comm, parent)), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	fds := filepath.Join(p, "fd")
	if err := os.MkdirAll(fds, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	for i, n := range nodes {
		if err := os.Symlink(n, filepath.Join(fds, fmt.Sprintf("%d", i))); err != nil {
			tr.t.Fatal(err)
		}
	}
}

// unreadable closes one process's fd directory to everyone.
func (tr *vfioTree) unreadable(pid int) {
	tr.t.Helper()
	if err := os.Chmod(filepath.Join(tr.proc, fmt.Sprintf("%d", pid), "fd"), 0o000); err != nil {
		tr.t.Fatal(err)
	}
}

// smi writes the nvidia-smi stand-in: it prints the bus id when the card's
// driver is back on nvidia and fails otherwise, which is what the initial
// address query and the answers check each need.
func (tr *vfioTree) smi() string {
	tr.t.Helper()
	bin := filepath.Join(tr.t.TempDir(), "nvidia-smi")
	script := fmt.Sprintf(`#!/bin/sh
d=$(basename "$(readlink "%s/bus/pci/devices/%s/driver" 2>/dev/null)" 2>/dev/null)
if [ "$d" = nvidia ]; then
  echo %s
  exit 0
fi
echo "nvidia-smi: no device" >&2
exit 1
`, tr.sysfs, vfioAddr, vfioNvidiaBus)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	return bin
}

// script writes a command the run executes.
func (tr *vfioTree) script(name, body string) string {
	tr.t.Helper()
	bin := filepath.Join(tr.t.TempDir(), name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	return bin
}

// flip is the shell for one driver change on every function of the card.
func (tr *vfioTree) flip(to string) string {
	return fmt.Sprintf(`for f in $BERTH_VFIO_DEVICES; do
  rm "%s/bus/pci/devices/$f/driver"
  ln -s "%s/bus/pci/drivers/%s" "%s/bus/pci/devices/$f/driver"
done
`, tr.sysfs, tr.sysfs, to, tr.sysfs)
}

// bind puts a function back on its driver by hand, as the operator's script
// or hand does between two checks.
func (tr *vfioTree) bind(addr, driver string) {
	tr.t.Helper()
	p := filepath.Join(tr.sysfs, "bus", "pci", "devices", addr, "driver")
	os.Remove(p)
	if err := os.Symlink(filepath.Join(tr.sysfs, "bus", "pci", "drivers", driver), p); err != nil {
		tr.t.Fatal(err)
	}
}

// runVfioStatusIn runs the status flow against the given roots.
func runVfioStatusIn(t *testing.T, tr *vfioTree, smiBin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Version: "test", Format: FormatHuman,
		Style: newStyle(&out), errStyle: newStyle(&errOut),
		Cmd: lookup(lookup(commands, "vfio").Sub, "status"), Args: args}
	code := vfioStatus(c, vfio.Reader{Sysfs: tr.sysfs, Proc: tr.proc}, smiBin)
	return code, out.String(), errOut.String()
}

// runVfioRunIn runs the flow against the given roots and waits.
func runVfioRunIn(t *testing.T, tr *vfioTree, smiBin string, o vfioRunOpts, args ...string) (int, string, string) {
	t.Helper()
	o.reader = vfio.Reader{Sysfs: tr.sysfs, Proc: tr.proc}
	o.smiBin = smiBin
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Version: "test", Format: FormatHuman,
		Style: newStyle(&out), errStyle: newStyle(&errOut),
		Cmd: lookup(lookup(commands, "vfio").Sub, "run"), Args: args}
	code := vfioRun(c, o)
	return code, out.String(), errOut.String()
}

// startVfioDaemon is berthd on a temp socket with an empty card: the hold
// grants at once and the lease table is observable.
func startVfioDaemon(t *testing.T) (string, *broker.Broker) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &procCard{}
	b := broker.New(broker.Config{
		Poll: 10 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1,
		Version: "test", Log: log,
	}, c, procs.Map{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.Tick(ctx)
	go b.Run(ctx)
	go server.Serve(ctx, ln, node.One(b), server.Extras{}, log)
	return sock, b
}

// none is true when the daemon holds no lease.
func none(b *broker.Broker) func() bool {
	return func() bool { return len(b.Status().Leases) == 0 }
}

// TestVfioStatusReadsTheFakeTree: the reading, no daemon needed, exit 0.
func TestVfioStatusReadsTheFakeTree(t *testing.T) {
	isolate(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	code, out, errOut := runVfioStatusIn(t, tr, tr.smi(), "--socket", "/nonexistent/berth.sock")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "bound to nvidia") || !strings.Contains(out, "iommu   on") {
		t.Errorf("the reading is not what the tree says: %q", out)
	}
	if !strings.Contains(out, "unreachable") {
		t.Errorf("the report does not say berthd is unreachable: %q", out)
	}
}

// TestVfioStatusJSONIsTheDocument covers the machine view of the reading.
func TestVfioStatusJSONIsTheDocument(t *testing.T) {
	isolate(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.function("0000:01:00.1", "snd_hda_intel")
	tr.function("0000:06:00.0", "xhci_hcd")
	tr.group()
	tr.opener(100, "app", 1, "/dev/nvidia0")
	tr.opener(101, "nvidia-smi", 99, "/dev/nvidiactl")
	tr.opener(99, "berth", 1)
	code, out, errOut := runVfioStatusIn(t, tr, tr.smi(), "--socket", "/nonexistent/berth.sock", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	var doc struct {
		Addr     string   `json:"addr"`
		IOMMU    bool     `json:"iommu"`
		Blockers []string `json:"blockers"`
		Passed   bool     `json:"passed_through"`
		Openers  []struct {
			PID   int      `json:"pid"`
			Name  string   `json:"name"`
			Nodes []string `json:"nodes"`
			Probe bool     `json:"probe"`
		} `json:"openers"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if doc.Addr != vfioAddr || !doc.IOMMU || doc.Passed {
		t.Errorf("doc = %+v", doc)
	}
	if len(doc.Blockers) != 1 || doc.Blockers[0] != "0000:06:00.0" {
		t.Errorf("blockers = %v", doc.Blockers)
	}
	if len(doc.Openers) != 2 || doc.Openers[0].PID != 100 || doc.Openers[0].Name != "app" ||
		len(doc.Openers[0].Nodes) != 1 || doc.Openers[0].Nodes[0] != "/dev/nvidia0" {
		t.Errorf("openers = %+v", doc.Openers)
	}
	if !doc.Openers[1].Probe {
		t.Errorf("the nvidia-smi opener is not named as the probe: %+v", doc.Openers[1])
	}
}

// TestVfioStatusReportsPassthroughWithAnAddress: nvidia-smi sees nothing
// once the card is on vfio-pci, and an explicit address still reads.
func TestVfioStatusReportsPassthroughWithAnAddress(t *testing.T) {
	isolate(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "vfio-pci")
	tr.group()
	code, out, errOut := runVfioStatusIn(t, tr, tr.smi(), "--socket", "/nonexistent/berth.sock", vfioAddr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "passed through") {
		t.Errorf("the report does not say the card is passed through: %q", out)
	}
}

// TestVfioStatusWithAnAddressStillAsksBerthd: an address skips nvidia-smi,
// which cannot see a card on vfio-pci, but berthd still answers and its view
// is shown; it is not reported unreachable.
func TestVfioStatusWithAnAddressStillAsksBerthd(t *testing.T) {
	isolate(t)
	sock, _ := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "vfio-pci")
	tr.group()
	code, out, errOut := runVfioStatusIn(t, tr, tr.smi(), "--socket", sock, vfioAddr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if strings.Contains(out, "unreachable") {
		t.Errorf("berthd answers but the report says unreachable: %q", out)
	}
}

// TestVfioRunRefusesIOMMUOff: exit 1 with the hint, and nothing is acquired.
func TestVfioRunRefusesIOMMUOff(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	smi := tr.smi()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond},
		"--socket", sock, "--", "/bin/true")
	if code != ExitFail {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "IOMMU is off") || !strings.Contains(errOut, "intel_iommu=on") {
		t.Errorf("the refusal does not say why and how: %q", errOut)
	}
	if !none(b)() {
		t.Error("the refusal acquired a hold")
	}
}

// TestVfioRunRefusesAnExtraGroupMember: the whole IOMMU group goes with the
// card, so a stranger in it refuses the run with its name and driver.
func TestVfioRunRefusesAnExtraGroupMember(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.function("0000:06:00.0", "xhci_hcd")
	tr.group()
	smi := tr.smi()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond},
		"--socket", sock, "--", "/bin/true")
	if code != ExitFail {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "0000:06:00.0") || !strings.Contains(errOut, "xhci_hcd") {
		t.Errorf("the refusal does not name the stranger: %q", errOut)
	}
	if !none(b)() {
		t.Error("the refusal acquired a hold")
	}
}

// TestVfioRunRefusesOpenersAndReleasesTheHold: a holder outside Berth keeps
// the card where it is, the report names it, and the hold is not kept.
func TestVfioRunRefusesOpenersAndReleasesTheHold(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	tr.opener(100, "qemu", 1, "/dev/nvidia0")
	smi := tr.smi()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond},
		"--socket", sock, "--", "/bin/true")
	if code != ExitFail {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "pid 100 qemu holds /dev/nvidia0") {
		t.Errorf("the refusal does not name the holder: %q", errOut)
	}
	waitUntil(t, none(b))
}

// TestVfioRunRefusesALingeringProbe: berthd stops its probe for a VFIO hold;
// an nvidia-smi of berthd's still open after the settle time is a berthd
// that does not, and it would block the unbind like any other holder.
func TestVfioRunRefusesALingeringProbe(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	tr.opener(99, "berth", 1)
	tr.opener(102, "nvidia-smi", 99, "/dev/nvidiactl")
	smi := tr.smi()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond},
		"--socket", sock, "--", "/bin/true")
	if code != ExitFail {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "berthd's own probe, still running") {
		t.Errorf("the refusal does not name berthd's probe: %q", errOut)
	}
	waitUntil(t, none(b))
}

// TestVfioRunWarnsOnAnUnreadableProc: a process whose fds cannot be read is
// listed as unreadable, never skipped; it is a warning, not a refusal,
// because an unreadable list is no proof of a holder.
func TestVfioRunWarnsOnAnUnreadableProc(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	tr.opener(100, "locked", 1)
	tr.unreadable(100)
	smi := tr.smi()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond},
		"--socket", sock, "--", "/bin/true")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "pid 100") || !strings.Contains(errOut, "unreadable") {
		t.Errorf("the warning does not name the unreadable process: %q", errOut)
	}
	waitUntil(t, none(b))
}

// TestVfioRunHappyPath: the hold is granted, the script runs and its exit
// code passes through once the card is back on its host driver.
func TestVfioRunHappyPath(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	smi := tr.smi()
	cmd := tr.script("switch", tr.flip("vfio-pci")+tr.flip("nvidia")+"exit 7\n")
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond},
		"--socket", sock, "--", cmd)
	if code != 7 {
		t.Fatalf("exit = %d, want the script's 7 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "the card is back; hold released") {
		t.Errorf("the report does not say the card came back: %q", errOut)
	}
	waitUntil(t, none(b))
}

// TestVfioRunSetsTheDevicesEnv: the script sees the card's PCI functions
// without hardcoding them, and the hold covers the whole card.
func TestVfioRunSetsTheDevicesEnv(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.function("0000:01:00.1", "snd_hda_intel")
	tr.group()
	smi := tr.smi()
	seen := filepath.Join(tr.t.TempDir(), "seen")
	cmd := tr.script("switch", fmt.Sprintf("echo \"$BERTH_VFIO_DEVICES\" > %s\n%s\nexit 0\n", seen, tr.flip("nvidia")))
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond},
		"--socket", sock, "--", cmd)
	if code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut)
	}
	wrote, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(wrote)); got != vfioAddr+" 0000:01:00.1" {
		t.Errorf("BERTH_VFIO_DEVICES = %q, want %q", got, vfioAddr+" 0000:01:00.1")
	}
	waitUntil(t, none(b))
}

// TestVfioRunKeepsTheHoldWhileTheCardIsOut: the script exits with the card
// still on vfio-pci, the message names the function, and the hold releases
// only once the card is really back.
func TestVfioRunKeepsTheHoldWhileTheCardIsOut(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	smi := tr.smi()
	cmd := tr.script("switch", tr.flip("vfio-pci")+"exit 3\n")

	sigs := make(chan os.Signal, 4)
	var mu sync.Mutex
	var errOut bytes.Buffer
	code := make(chan int, 1)
	go func() {
		o := vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond,
			back: 100 * time.Millisecond, sigs: sigs}
		o.reader = vfio.Reader{Sysfs: tr.sysfs, Proc: tr.proc}
		o.smiBin = smi
		c := &Context{Out: io.Discard, Err: errWriter{&mu, &errOut}, Version: "test", Format: FormatHuman,
			Style: newStyle(io.Discard), errStyle: newStyle(io.Discard),
			Cmd:  lookup(lookup(commands, "vfio").Sub, "run"),
			Args: []string{"--socket", sock, "--", cmd}}
		code <- vfioRun(c, o)
	}()
	waitFor(t, &mu, &errOut, func(said string) bool {
		return strings.Contains(said, vfioAddr+" is still bound to vfio-pci")
	})
	// Ctrl-C while the card is out: it must not release the hold.
	sigs <- os.Interrupt
	time.Sleep(50 * time.Millisecond)
	if leases := len(b.Status().Leases); leases != 1 {
		t.Fatalf("Ctrl-C released the hold while the card was out (%d leases)", leases)
	}
	// The card comes back (the script's tail, or the operator by hand): the
	// next check releases.
	tr.bind(vfioAddr, "nvidia")
	sigs <- os.Interrupt
	if got := <-code; got != 3 {
		t.Fatalf("exit = %d, want the script's 3", got)
	}
	waitUntil(t, none(b))
}

// waitFor polls the stderr buffer until its text satisfies f.
func waitFor(t *testing.T, mu *sync.Mutex, b *bytes.Buffer, f func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		said := b.String()
		mu.Unlock()
		if f(said) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the message never came: %q", said)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// errWriter is a stderr that stays safe when the flow and the test read it
// from different goroutines.
type errWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w errWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// TestVfioRunBeforeStopsAHolderAndAfterStartsItAgain: --before runs once the
// engines are off the card, so it can stop a service that holds the card;
// the check then passes, the script runs, and --after runs once the hold
// has ended.
func TestVfioRunBeforeStopsAHolderAndAfterStartsItAgain(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	tr.opener(100, "monitor", 1, "/dev/nvidia0")
	smi := tr.smi()
	marks := t.TempDir()
	before := "rm -rf " + filepath.Join(tr.proc, "100") + " && touch " + filepath.Join(marks, "before")
	after := "touch " + filepath.Join(marks, "after")
	cmd := tr.script("switch", tr.flip("vfio-pci")+tr.flip("nvidia"))
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond},
		"--socket", sock, "--before", before, "--after", after, "--", cmd)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, m := range []string{"before", "after"} {
		if _, err := os.Stat(filepath.Join(marks, m)); err != nil {
			t.Errorf("--%s did not run: %v", m, err)
		}
	}
	waitUntil(t, none(b))
}

// TestVfioRunAfterRunsWhenTheCheckRefuses: --before ran and stopped something,
// the check still found a holder, so the run is refused; --after puts back
// what --before stopped.
func TestVfioRunAfterRunsWhenTheCheckRefuses(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	tr.opener(100, "qemu", 1, "/dev/nvidia0")
	smi := tr.smi()
	marks := t.TempDir()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond},
		"--socket", sock, "--before", "true", "--after", "touch "+filepath.Join(marks, "after"), "--", "/bin/true")
	if code != ExitFail || !strings.Contains(errOut, "pid 100 qemu holds /dev/nvidia0") {
		t.Fatalf("exit = %d, want the opener refusal (stderr %q)", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(marks, "after")); err != nil {
		t.Errorf("--after did not run after the refusal: %v", err)
	}
	waitUntil(t, none(b))
}

// TestVfioRunBeforeFailureReleases: a --before that fails ends the run with
// the card still on the host, and the hold is released.
func TestVfioRunBeforeFailureReleases(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	smi := tr.smi()
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond},
		"--socket", sock, "--before", "exit 4", "--", "/bin/true")
	if code != ExitFail || !strings.Contains(errOut, "--before did not finish cleanly (exit 4") {
		t.Fatalf("exit = %d, want the --before refusal (stderr %q)", code, errOut)
	}
	waitUntil(t, none(b))
}

// TestVfioRunScriptExit127StillWaitsForTheCard: a script that ran and exited
// 127 (a typo inside it) may still have moved the card; the run keeps the
// hold until the card is back, as for any other exit code.
func TestVfioRunScriptExit127StillWaitsForTheCard(t *testing.T) {
	isolate(t)
	sock, b := startVfioDaemon(t)
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	smi := tr.smi()
	cmd := tr.script("switch", tr.flip("vfio-pci")+tr.flip("nvidia")+"exit 127\n")
	code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond},
		"--socket", sock, "--", cmd)
	if code != 127 || !strings.Contains(errOut, "the card is back; hold released") {
		t.Fatalf("exit = %d, want 127 after the card-back check (stderr %q)", code, errOut)
	}
	waitUntil(t, none(b))
}

// TestVfioStatusNamesTheVMNotAStaleReading: while berthd reports the card in a
// VM, its memory figures are the last reading before the hold; the report
// says where the card is instead of printing them as current.
func TestVfioStatusNamesTheVMNotAStaleReading(t *testing.T) {
	var out bytes.Buffer
	c := &Context{Out: &out, Err: io.Discard, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(io.Discard)}
	doc := vfioStatusDoc{Addr: vfioAddr, IOMMU: true, Passed: true,
		Group: []vfio.Function{{Addr: vfioAddr, Driver: "vfio-pci"}},
		Berthd: &vfioBerthdDoc{Reachable: true, Passthrough: "passthrough",
			GPU: &vfioGPUDoc{Index: 0, Name: "RTX 4090", Used: 4 << 20, Total: 24 << 30, Age: 1400}}}
	if err := (vfioStatusView{doc}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "passed through to a VM (vfio-pci) (last memory reading 1400 s ago)") || strings.Contains(got, "used of") {
		t.Errorf("berthd's line = %q, want the VM state and no current-looking memory figure", got)
	}
}

// TestVfioRunTakesTheCardItNames: on a node of two cards, --gpu gpu1 holds
// gpu1 alone and passes through the card at nvidia-smi index 1. Index 0 is
// another card with no functions in the tree, so a run that resolved the
// first card would fail its preflight.
func TestVfioRunTakesTheCardItNames(t *testing.T) {
	isolate(t)
	sock, brokers := serveNode(t, "gpu0", "gpu1")
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	smi := tr.script("nvidia-smi", "case \" $* \" in *\" -i 0 \"*) echo 00000000:02:00.0; exit 0;; esac\nexec "+tr.smi()+" \"$@\"\n")
	held, resume := filepath.Join(t.TempDir(), "held"), filepath.Join(t.TempDir(), "resume")
	cmd := tr.script("switch", fmt.Sprintf("touch %s\nwhile [ ! -e %s ]; do sleep 0.01; done\n%s%sexit 0\n",
		held, resume, tr.flip("vfio-pci"), tr.flip("nvidia")))
	done := make(chan string, 1)
	go func() {
		code, _, errOut := runVfioRunIn(t, tr, smi, vfioRunOpts{settle: time.Millisecond, poll: 5 * time.Millisecond},
			"--socket", sock, "--gpu", "gpu1", "--", cmd)
		done <- fmt.Sprintf("exit %d: %s", code, errOut)
	}()
	waitUntil(t, func() bool {
		select {
		case out := <-done:
			t.Fatalf("the run ended before its script held the card: %s", out)
		default:
		}
		_, err := os.Stat(held)
		return err == nil
	})
	if got := leaseNames(brokers[1]); len(got) != 1 || got[0] != "vfio" {
		t.Errorf("gpu1 leases = %v, want the vfio hold", got)
	}
	if got := leaseNames(brokers[0]); len(got) != 0 {
		t.Errorf("gpu0 leases = %v, want none", got)
	}
	if err := os.WriteFile(resume, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := <-done; !strings.HasPrefix(out, "exit 0:") || !strings.Contains(out, "the card is back; hold released") {
		t.Errorf("run = %q, want exit 0 with the card back", out)
	}
	waitUntil(t, none(brokers[1]))
}

// TestVfioRunRefusesSeveralCardsOrOneTheNodeLacks: a passthrough moves one
// card berthd schedules; anything else is refused before a hold is asked.
func TestVfioRunRefusesSeveralCardsOrOneTheNodeLacks(t *testing.T) {
	isolate(t)
	sock, brokers := serveNode(t, "gpu0", "gpu1")
	tr := newVfioTree(t)
	tr.function(vfioAddr, "nvidia")
	tr.group()
	for _, tc := range []struct{ gpu, want string }{
		{"gpu0,gpu1", "a passthrough moves one card"},
		{"gpu7", "berthd has no card gpu7"},
	} {
		code, _, errOut := runVfioRunIn(t, tr, tr.smi(), vfioRunOpts{settle: time.Millisecond},
			"--socket", sock, "--gpu", tc.gpu, "--", "true")
		if code == ExitOK || !strings.Contains(errOut, tc.want) {
			t.Errorf("--gpu %s: exit = %d, stderr %q; want %q", tc.gpu, code, errOut, tc.want)
		}
	}
	for i, b := range brokers {
		if n := leaseNames(b); len(n) > 0 {
			t.Errorf("gpu%d holds %v after a refused run", i, n)
		}
	}
}
