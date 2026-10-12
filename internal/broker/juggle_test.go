package broker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

func jugglers(t *testing.T, cfg Config) (*Broker, *card) {
	t.Helper()
	c := &card{total: 24 * GiB}
	cfg.ReleaseGrace, cfg.DefaultWait = time.Hour, 2*time.Second
	cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(cfg, c, procs.Map{})
	b.Tick(context.Background())
	return b, c
}

// notDrained is the opposite of drained: the holder keeps the card.
func notDrained(t *testing.T, l *Lease) {
	t.Helper()
	select {
	case why := <-l.Drains():
		t.Fatalf("holder was drained: %s", why)
	case <-time.After(100 * time.Millisecond):
	}
}

func juggleEvents(b *Broker) []string {
	evs, _, _ := b.Events(0, false)
	var out []string
	for _, e := range evs {
		if e.Kind == proto.EvJuggle {
			out = append(out, e.Detail)
		}
	}
	return out
}

// The flip from the 2026-10-12 switch test: Strata is still loading when
// NInfer is asked for. The order puts llama-swap first, so Strata is drained
// once, and Strata asking again inside the window waits instead of draining
// NInfer back.
func TestJuggleOrderStopsTheFlip(t *testing.T) {
	b, _ := jugglers(t, Config{Order: []string{"llama-swap", "strata"}})
	strata := must(t, acquire(b, Request{Engine: "strata", Priority: Foreground}))

	ninfer := acquire(b, Request{Engine: "NInfer-Qwen3.8-27B", Group: "llama-swap", Priority: Foreground})
	pending(t, ninfer)
	drained(t, strata)
	strata.Release()
	b.Tick(context.Background())
	n := must(t, ninfer)

	again := acquire(b, Request{Engine: "strata", Priority: Foreground})
	pending(t, again)
	notDrained(t, n)
	if c := b.Status().Counters.JugglesStopped; c != 1 {
		t.Fatalf("juggles stopped = %d, want 1", c)
	}
	if evs := juggleEvents(b); len(evs) != 1 || !strings.Contains(evs[0], "llama-swap takes the card from strata") {
		t.Fatalf("juggle events = %q", evs)
	}
	n.Release()
	b.Tick(context.Background())
	must(t, again).Release()
}

// The holder outranks the waiter: it keeps the card and the waiter is told why,
// and it gets the card when the holder lets go.
func TestJuggleHolderKeepsTheCard(t *testing.T) {
	b, _ := jugglers(t, Config{Order: []string{"llama-swap", "strata"}})
	ninfer := must(t, acquire(b, Request{Engine: "NInfer-Qwen3.8-27B", Group: "llama-swap", Priority: Foreground}))
	strata := acquire(b, Request{Engine: "strata", Priority: Foreground})
	pending(t, strata)
	notDrained(t, ninfer)
	st := b.Status()
	if len(st.Waiting) != 1 {
		t.Fatalf("waiting = %+v", st.Waiting)
	}
	evs, _, _ := b.Events(0, false)
	var told string
	for _, e := range evs {
		if e.Kind == proto.EvWaiting && e.Engine == "strata" {
			told = e.Detail
		}
	}
	if !strings.Contains(told, "juggle: llama-swap keeps the card over strata (order: llama-swap > strata)") {
		t.Fatalf("strata was told %q", told)
	}
	ninfer.Release()
	b.Tick(context.Background())
	must(t, strata).Release()
}

// A waiter that lost keeps waiting after the window: the verdict does not
// expire into a flip while the same request is still asking.
func TestJuggleLoserDoesNotDrainAfterTheWindow(t *testing.T) {
	b, _ := jugglers(t, Config{Order: []string{"llama-swap"}, JuggleWindow: 50 * time.Millisecond})
	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Group: "llama-swap", Priority: Foreground}))
	strata := acquire(b, Request{Engine: "strata", Priority: Foreground})
	pending(t, strata)
	time.Sleep(100 * time.Millisecond)
	b.Tick(context.Background())
	notDrained(t, ninfer)
	ninfer.Release()
	b.Tick(context.Background())
	must(t, strata).Release()
}

// Outside the window it is a switch, not a juggle: last switch wins.
func TestSwitchOutsideTheWindowDrains(t *testing.T) {
	b, _ := jugglers(t, Config{Order: []string{"llama-swap"}, JuggleWindow: 50 * time.Millisecond})
	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Group: "llama-swap", Priority: Foreground}))
	time.Sleep(80 * time.Millisecond)
	strata := acquire(b, Request{Engine: "strata", Priority: Foreground})
	pending(t, strata)
	drained(t, ninfer)
	if c := b.Status().Counters.JugglesStopped; c != 0 {
		t.Fatalf("a plain switch counted as a juggle (%d)", c)
	}
	ninfer.Release()
	b.Tick(context.Background())
	must(t, strata).Release()
}

// An idle holder is not in a juggle: nobody loses a request, so the card goes
// to the newcomer at once, whatever the order says. Chat -> image -> chat.
func TestIdleHolderHandsOver(t *testing.T) {
	busy := func(k string) (bool, bool) { return false, true }
	b, _ := jugglers(t, Config{Order: []string{"llama-swap", "comfyui"}, Busy: busy})
	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Group: "llama-swap", Priority: Foreground}))
	comfy := acquire(b, Request{Engine: "comfyui", Priority: Foreground})
	pending(t, comfy)
	drained(t, ninfer)
	ninfer.Release()
	b.Tick(context.Background())
	must(t, comfy).Release()
}

// A busy holder (a ComfyUI job running) is in a juggle; unknown counts as busy.
func TestBusyHolderJuggles(t *testing.T) {
	for _, busy := range []func(string) (bool, bool){
		func(string) (bool, bool) { return true, true },
		func(string) (bool, bool) { return false, false },
	} {
		b, _ := jugglers(t, Config{Order: []string{"comfyui", "llama-swap"}, Busy: busy})
		comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
		ninfer := acquire(b, Request{Engine: "ninfer", Group: "llama-swap", Priority: Foreground})
		pending(t, ninfer)
		notDrained(t, comfy)
		comfy.Release()
		b.Tick(context.Background())
		must(t, ninfer).Release()
	}
}

// With no order, the engine with more card-hours in the last 7 days wins;
// a tie leaves the holder in place.
func TestJuggleUsageRanking(t *testing.T) {
	b, _ := jugglers(t, Config{})
	today := day(time.Now())
	b.mu.Lock()
	b.usage[today] = map[string]float64{"strata": 3 * 3600, "llama-swap": 1 * 3600}
	b.mu.Unlock()
	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Group: "llama-swap", Priority: Foreground}))
	strata := acquire(b, Request{Engine: "strata", Priority: Foreground})
	pending(t, strata)
	if why := drained(t, ninfer); !strings.Contains(why, "strata") {
		t.Fatalf("drain reason %q", why)
	}
	if evs := juggleEvents(b); len(evs) != 1 || !strings.Contains(evs[0], "usage: strata 3.0 h vs llama-swap 1.0 h") {
		t.Fatalf("juggle events = %q", evs)
	}
	ninfer.Release()
	b.Tick(context.Background())
	must(t, strata).Release()

	tie, _ := jugglers(t, Config{})
	a := must(t, acquire(tie, Request{Engine: "a", Priority: Foreground}))
	w := acquire(tie, Request{Engine: "b", Priority: Foreground})
	pending(t, w)
	notDrained(t, a)
	a.Release()
	tie.Tick(context.Background())
	must(t, w).Release()
}

// Card-hours accrue per group while a lease holds the card and survive a
// restart through the usage file; the ranking in status follows them.
func TestUsageAccruesAndPersists(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.json")
	b, _ := jugglers(t, Config{UsageFile: file, Poll: time.Second})
	l := must(t, acquire(b, Request{Engine: "ninfer", Group: "llama-swap"}))
	b.Tick(context.Background())
	b.mu.Lock()
	b.usageAt = time.Now().Add(-5 * time.Second) // pretend the last tick was 5 s ago
	b.mu.Unlock()
	b.Tick(context.Background())
	l.Release()
	b.persistUsage(time.Now(), true)

	b2, _ := jugglers(t, Config{UsageFile: file})
	b2.mu.Lock()
	secs := b2.usage[day(time.Now())]["llama-swap"]
	b2.mu.Unlock()
	if secs < 4 || secs > 7 {
		t.Fatalf("restored usage = %.1f s, want about 5", secs)
	}
	ps := b2.Status().Priority
	if ps == nil || ps.Source != "usage" || len(ps.Engines) != 1 || ps.Engines[0].Engine != "llama-swap" || ps.Engines[0].Rank != 1 {
		t.Fatalf("priority status = %+v", ps)
	}
}

// A long gap between ticks (a suspended machine) is not credited.
func TestUsageSkipsGaps(t *testing.T) {
	b, _ := jugglers(t, Config{Poll: time.Second})
	must(t, acquire(b, Request{Engine: "x"}))
	b.mu.Lock()
	b.usageAt = time.Now().Add(-time.Hour)
	b.mu.Unlock()
	b.Tick(context.Background())
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.usage[day(time.Now())]["x"]; s != 0 {
		t.Fatalf("an hour-long gap was credited: %.0f s", s)
	}
}
