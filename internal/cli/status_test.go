// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// emptyCard is a machine whose card reports no processes, so the daemon below
// has nothing to report but its own state.
type emptyCard struct{}

func (emptyCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

// startDaemon runs a real berthd on a temp socket: the CLI is then driven
// through the real wire, which is the only way to verify a document that has to
// match what the daemon sends.
func startDaemon(t *testing.T) string {
	t.Helper()
	return startDaemonWith(t, server.Extras{})
}

// startDaemonWith is startDaemon serving x's hooks (the engines berthd
// found, say); its alerts are the daemon's own.
func startDaemonWith(t *testing.T, x server.Extras) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{
		Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1,
		Version: "test", Log: log,
	}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Read the card once before serving: Run's first read happens in its own
	// goroutine, and a status asked for before it shows no GPU at all.
	b.Tick(ctx)
	go b.Run(ctx)
	ev := alerts.New(alerts.FromConfig(config.AlertsConfig{}), 3, nil)
	lastAlerts := &atomic.Pointer[proto.Alerts]{}
	lastAlerts.Store(ev.Snapshot())
	x.Alerts = lastAlerts.Load
	go server.Serve(ctx, ln, node.One(b), x, log)
	return sock
}

func TestStatusHumanAgainstADaemon(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "status", "--socket", sock)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "berthd test on "+sock) {
		t.Errorf("stdout %q lacks the daemon it read", out)
	}
	if !strings.Contains(out, "no leases, nobody waiting, no squatters") {
		t.Errorf("stdout %q lacks the empty state", out)
	}
}

func TestStatusJSONIsTheWireDocument(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, _ := runCLI(t, "status", "--socket", sock, "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var st proto.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if st.API != proto.APIVersion {
		t.Errorf("api = %d, want %d", st.API, proto.APIVersion)
	}
	if len(st.GPUs) != 1 || st.GPUs[0].Scheduled != true {
		t.Errorf("gpus = %+v", st.GPUs)
	}
}

// procCard is a fake card whose processes the test sets, so a status can
// carry a footprint the way a real engine run leaves one.
type procCard struct {
	mu    sync.Mutex
	procs []gpu.Proc
}

func (c *procCard) set(ps ...gpu.Proc) {
	c.mu.Lock()
	c.procs = ps
	c.mu.Unlock()
}

func (c *procCard) Read(context.Context) (gpu.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var used uint64
	for _, p := range c.procs {
		used += p.Used
	}
	return gpu.Snapshot{Total: 24 << 30, Used: used, Free: (24 << 30) - used, At: time.Now(),
		Procs: append([]gpu.Proc(nil), c.procs...)}, nil
}

// startFootprintDaemon is startDaemon with a card the test drives and one
// engine in the status, so the reply has a footprint to carry.
func startFootprintDaemon(t *testing.T) (string, *broker.Broker, *procCard) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &procCard{}
	b := broker.New(broker.Config{
		Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1,
		Version: "test", Log: log,
	}, c, procs.Map{101: 100})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.Tick(ctx)
	go b.Run(ctx)
	engines := func() []proto.Engine {
		return []proto.Engine{{Name: "llama", Type: "llamacpp", URL: "http://127.0.0.1:8080",
			Up: true, State: "up", Models: []proto.Model{{Name: "m", State: "loaded"}}}}
	}
	go server.Serve(ctx, ln, node.One(b), server.Extras{Engines: engines}, log)
	return sock, b, c
}

// The status reply carries the engine's footprint: the human view shows it
// as EST. VRAM, and the JSON names it estimated_vram_bytes.
func TestStatusCarriesTheEngineFootprint(t *testing.T) {
	isolate(t)
	sock, b, c := startFootprintDaemon(t)

	l, err := b.Acquire(context.Background(), broker.Request{Engine: "llama", Priority: broker.Foreground}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "llama-server", Used: 12 << 30})
	b.Tick(context.Background())
	l.Release()
	c.set()
	deadline := time.Now().Add(time.Second)
	for {
		if est, ok := b.Footprint("llama"); ok && est == 12<<30 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the freed run's footprint never folded")
		}
		time.Sleep(5 * time.Millisecond)
	}

	code, out, errOut := runCLI(t, "status", "--socket", sock)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "EST. VRAM") {
		t.Errorf("stdout %q lacks the EST. VRAM column", out)
	}
	if !strings.Contains(out, "12288 MiB") {
		t.Errorf("stdout %q lacks the engine's estimate", out)
	}

	code, out, _ = runCLI(t, "status", "--socket", sock, "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var st proto.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if len(st.Footprints) != 1 || st.Footprints[0].Engine != "llama" || st.Footprints[0].Bytes != 12<<30 || st.Footprints[0].Runs != 1 {
		t.Errorf("footprints = %+v, want llama at 12 GiB over 1 run", st.Footprints)
	}
	if !strings.Contains(out, "estimated_vram_bytes") {
		t.Errorf("json %q lacks the estimated_vram_bytes field", out)
	}
}

// The LEASE table carries a TURN column: the remaining turn of a lease that
// has one, "-" for a lease that does not.
func TestStatusShowsTheTurnColumn(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
	st := &proto.Status{
		API: proto.APIVersion, Version: "test",
		Leases: []proto.Lease{
			{ID: "comfyui-1", Engine: "comfyui", Priority: "interactive", State: "active", Age: 180, TurnS: 270},
			{ID: "llama-2", Engine: "llama", Priority: "batch", State: "active", Age: 300},
		},
	}
	if err := (statusView{st: st}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "TURN") {
		t.Errorf("stdout %q lacks the TURN column", s)
	}
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.Contains(line, "comfyui-1") && !strings.Contains(line, "4m30s"):
			t.Errorf("the lease with a turn does not show it: %q", line)
		case strings.Contains(line, "llama-2") && !strings.Contains(line, "  -  "):
			t.Errorf("the lease without a turn does not show '-': %q", line)
		}
	}
}

// The scheduled card's gpu line names the VM instead of memory figures
// while the card is passed through, and the others keep their memory.
func TestStatusShowsTheCardPassthrough(t *testing.T) {
	run := func(p *proto.Passthrough) string {
		t.Helper()
		var out, errOut bytes.Buffer
		c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
		st := &proto.Status{
			API: proto.APIVersion, Version: "test",
			GPUs: []proto.GPU{
				{Index: 0, Name: "NVIDIA GeForce RTX 4090", Scheduled: true, Total: 24 << 30},
				{Index: 1, Name: "GT 710"},
			},
			Passthrough: p,
		}
		if err := (statusView{st: st}).Human(c, true); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	s := run(&proto.Passthrough{State: proto.CardPassthrough, Driver: "vfio-pci"})
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "gpu0") {
			continue
		}
		if !strings.Contains(line, "passed through to a VM (vfio-pci)") || strings.Contains(line, " used of ") {
			t.Errorf("the scheduled card's line = %q, want the VM and no memory figures", line)
		}
	}
	if !strings.Contains(s, "inventory only") {
		t.Errorf("stdout %q lacks the other card's memory line", s)
	}
	s = run(&proto.Passthrough{State: proto.CardDetaching, Driver: "nvidia"})
	if !strings.Contains(s, "being passed through to a VM (driver nvidia)") {
		t.Errorf("stdout %q lacks the switching state", s)
	}
}

func TestStatusYAML(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, _ := runCLI(t, "status", "--socket", sock, "--format", "yaml")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var st proto.Status
	if err := yaml.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("yaml %q: %v", out, err)
	}
	if st.API != proto.APIVersion {
		t.Errorf("api = %d, want %d", st.API, proto.APIVersion)
	}
}

// berthd unreachable is a system failure: exit 2, the reason on stderr, and
// nothing on stdout, in every format.
func TestStatusUnreachable(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.sock")
	for _, args := range [][]string{
		{"status", "--socket", missing},
		{"status", "--socket", missing, "--format", "json"},
		{"-q", "status", "--socket", missing},
	} {
		code, out, errOut := runCLI(t, args...)
		if code != ExitSystem {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitSystem)
		}
		if out != "" {
			t.Errorf("%v: stdout = %q, want empty", args, out)
		}
		if !strings.Contains(errOut, "[ERROR] berthd unreachable on "+missing) {
			t.Errorf("%v: stderr = %q", args, errOut)
		}
	}
}

// Without -f the backlog is printed and the command exits, so this terminates.
func TestEventsAgainstADaemon(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "events", "--socket", sock)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "  ") {
			t.Errorf("line %q is not an event line", line)
		}
	}
}

// --format json is one event per line, which is what a follower pipes into jq.
func TestEventsJSONIsLineDelimited(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "events", "--socket", sock, "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var e proto.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %q is not one event: %v", line, err)
		}
	}
}

func TestEventsBadSince(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, _, errOut := runCLI(t, "events", "--socket", sock, "--since", "abc")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "invalid value") {
		t.Errorf("stderr = %q", errOut)
	}
}

// A berthd that started with its card in a VM has no reading, so its index
// is unknown and gpu0 may be another card: status names the address.
func TestStatusNamesAnUnreadPassthroughCardByAddress(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
	st := &proto.Status{API: proto.APIVersion, Version: "test",
		Passthrough: &proto.Passthrough{State: proto.CardPassthrough, Driver: "vfio-pci", Addr: "0000:02:00.0"}}
	if err := (statusView{st: st}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "gpu     0000:02:00.0: passed through to a VM (vfio-pci)") || strings.Contains(s, "gpu0") {
		t.Fatalf("stdout = %q, want the card named by its address", s)
	}
}

// With more than one card on the node, the lease and stowaway tables name
// each row's card. With one card they stay as they were.
func TestStatusNamesTheCardWithSeveral(t *testing.T) {
	render := func(gpus []proto.GPU) string {
		var out, errOut bytes.Buffer
		c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
		st := &proto.Status{
			API: proto.APIVersion, Version: "test", GPUs: gpus,
			Leases:    []proto.Lease{{ID: "maya-3", Card: "gpu1", Engine: "maya", Priority: "interactive", State: "active", Age: 60}},
			Waiting:   []proto.Lease{{ID: "voice-4", Card: "gpu1", Engine: "voice", Priority: "foreground", State: "waiting", Position: 1}},
			Squatters: []proto.Squatter{{Card: "gpu0", Name: "python3", PID: 77, Used: 1 << 30}},
		}
		if err := (statusView{st: st}).Human(c, true); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	two := render([]proto.GPU{{Index: 0, Card: "gpu0", Scheduled: true}, {Index: 1, Card: "gpu1", Scheduled: true}})
	heads := map[string]bool{}
	for _, line := range strings.Split(two, "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			heads[f[0]+" "+f[1]] = true
		}
	}
	for _, want := range []string{"LEASE CARD", "WAITING CARD", "SQUATTER CARD"} {
		if !heads[want] {
			t.Errorf("two cards: no %q header in\n%s", want, two)
		}
	}
	for _, line := range strings.Split(two, "\n") {
		for id, card := range map[string]string{"maya-3": "gpu1", "voice-4": "gpu1", "python3": "gpu0"} {
			if strings.HasPrefix(line, id) && !strings.Contains(line, card) {
				t.Errorf("row %q does not name %s", line, card)
			}
		}
	}
	if one := render([]proto.GPU{{Index: 0, Card: "gpu0", Scheduled: true}}); strings.Contains(one, "CARD") {
		t.Errorf("one card grew a CARD column:\n%s", one)
	}
}

// The claims table labels the memory column HOLDS like the lease table, and
// adds a CARD column of the cards a claim sits on only with several cards.
func TestClaimsTablesSayHoldsAndNameCards(t *testing.T) {
	claims := []proto.Claim{{Name: "blender", Action: "track", Active: true, Used: 2 << 30, Cards: []string{"gpu0", "gpu1"}, Procs: "blender"}}
	two := []proto.GPU{{Index: 0, Card: "gpu0", Scheduled: true}, {Index: 1, Card: "gpu1", Scheduled: true}}
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
	st := &proto.Status{API: proto.APIVersion, Version: "test", GPUs: two, Claims: claims}
	if err := (statusView{st: st}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "CLAIM    ACTION  LEASE  CARD       HOLDS") || !strings.Contains(s, "gpu0,gpu1") {
		t.Errorf("status claims table:\n%s", s)
	}
	out.Reset()
	if err := (claimsView{claims: claims, cards: 2}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "LEASE  CARD       HOLDS") || strings.Contains(s, "CARD  PROCESSES") {
		t.Errorf("berth claims table:\n%s", s)
	}
	out.Reset()
	if err := (claimsView{claims: claims, cards: 1}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "HOLDS") || strings.Contains(s, "CARD") {
		t.Errorf("one card claims table:\n%s", s)
	}
}

// berth status lists every engine held to hours and where the node's clock
// sits in them, so a window shows before its engine runs.
func TestStatusShowsTheSchedules(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &Context{Out: &out, Err: &errOut, Format: FormatHuman, Style: newStyle(&out), errStyle: newStyle(&errOut)}
	st := &proto.Status{
		API: proto.APIVersion, Version: "test",
		Schedules: []proto.Schedule{
			{Engine: "nightly", Window: "23:00-07:00", Open: false, ChangeS: 3 * 3600},
			{Engine: "render", Window: "18:00-22:00", Open: true, ChangeS: 2*3600 + 15*60},
		},
	}
	if err := (statusView{st: st}).Human(c, true); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"SCHEDULE", "HOURS", "nightly", "23:00-07:00", "closed, opens in 3h", "render", "open, closes in 2h15m"} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout lacks %q:\n%s", want, s)
		}
	}
}
