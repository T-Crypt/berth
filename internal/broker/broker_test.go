// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
)

const GiB = 1 << 30

// card is a fake GPU whose processes the test sets directly. When used is
// set, the driver's used figure is that, not the processes' sum: the
// readings a test gives can disagree with what the driver reports, as
// Windows counters can.
type card struct {
	mu     sync.Mutex
	total  uint64
	used   uint64
	procs  []gpu.Proc
	onRead func() // runs at the start of a read, outside the card lock
}

func (c *card) set(ps ...gpu.Proc) {
	c.mu.Lock()
	c.procs = ps
	c.mu.Unlock()
}

func (c *card) Read(context.Context) (gpu.Snapshot, error) {
	c.mu.Lock()
	hook := c.onRead
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	used := c.used
	if used == 0 {
		for _, p := range c.procs {
			used += p.Used
		}
	}
	free := uint64(0)
	if used <= c.total {
		free = c.total - used
	}
	return gpu.Snapshot{Total: c.total, Used: used, Free: free, Procs: append([]gpu.Proc(nil), c.procs...)}, nil
}

type reports struct {
	mu   sync.Mutex
	seen []string
}

func (r *reports) Squatter(appeared bool, card string, p gpu.Proc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	verb := "gone"
	if appeared {
		verb = "appeared"
	}
	r.seen = append(r.seen, verb+":"+p.Name)
}

func (r *reports) Alert(level, key, message, card string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, "alert:"+level+":"+key)
}

func setup(t *testing.T, tree procs.Tree, allow ...string) (*Broker, *card, *reports) {
	t.Helper()
	c := &card{total: 24 * GiB}
	r := &reports{}
	b := New(Config{
		Reporter:     r,
		Allow:        allow,
		ReleaseGrace: time.Hour,
		DefaultWait:  2 * time.Second,
		JuggleWindow: -1, // these tests are about drains; juggles have their own
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, c, tree)
	b.Tick(context.Background())
	return b, c, r
}

type result struct {
	l   *Lease
	err error
}

func acquire(b *Broker, r Request) chan result {
	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), r, nil)
		ch <- result{l, err}
	}()
	return ch
}

func must(t *testing.T, ch chan result) *Lease {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("acquire: %v", r.err)
		}
		return r.l
	case <-time.After(time.Second):
		t.Fatal("acquire did not return")
	}
	return nil
}

func pending(t *testing.T, ch chan result) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("acquire returned early: lease=%v err=%v", r.l, r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func drained(t *testing.T, l *Lease) string {
	t.Helper()
	select {
	case why := <-l.Drains():
		return why
	case <-time.After(time.Second):
		t.Fatal("holder was not asked to drain")
	}
	return ""
}

// granted waits for an engine's grant, failing with the reason the queue
// last gave, and returns its lease.
func granted(t *testing.T, b *Broker, engine string, ch chan result) *Lease {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("acquire: %v", r.err)
		}
		return r.l
	case <-time.After(time.Second):
	}
	st := b.Status()
	t.Fatalf("%s was never granted: waiting = %+v, last wait reason %q", engine, st.Waiting, lastWaiting(b))
	return nil
}

// grantDetail is the detail an engine's newest grant event carried, and
// fails the test when no grant event names it.
func grantDetail(t *testing.T, b *Broker, engine string) string {
	t.Helper()
	evs, _, _ := b.Events(0, false)
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == proto.EvGranted && evs[i].Engine == engine {
			return evs[i].Detail
		}
	}
	t.Fatalf("no grant event names %s", engine)
	return ""
}

// The v0 exit test in miniature: Strata asks while NInfer holds the card.
// NInfer is drained, and Strata is let in only once the card shows NInfer's
// memory gone, not when NInfer's wrapper merely disconnects.
func TestHandoffWaitsForMeasuredRelease(t *testing.T) {
	tree := procs.Map{101: 100, 201: 200}
	b, c, _ := setup(t, tree)

	ninfer := must(t, acquire(b, Request{Engine: "ninfer", Priority: Interactive}))
	ninfer.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 22 * GiB})
	b.Tick(context.Background())

	strata := acquire(b, Request{Engine: "strata", Priority: Interactive})
	pending(t, strata)
	if why := drained(t, ninfer); !strings.Contains(why, "strata") {
		t.Fatalf("drain reason %q does not name the requester", why)
	}

	ninfer.Release()
	pending(t, strata) // wrapper gone, memory not yet

	c.set()
	b.Tick(context.Background())
	must(t, strata)
}

// B1: a released lease never frees inside Release. An engine that allocates
// and dies inside one poll interval has its lease still marked releasing, so
// a process that turns up on the next card read stays the lease's instead of
// becoming a stowaway, and a waiter is granted only after the driver shows
// the memory gone.
func TestReleaseKeepsAProcessAppearingAfterIt(t *testing.T) {
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 10, 101: 11}}
	b, c, r := setup(t, tree)
	l := must(t, acquire(b, Request{Engine: "ninfer"}))
	l.Attach(100)
	l.Release()

	// The engine got on the card after the last probe read.
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 20 * GiB})
	b.Tick(context.Background())

	st := b.Status()
	if len(st.Leases) != 1 || st.Leases[0].State != "releasing" || st.Leases[0].Used != 20*GiB {
		t.Fatalf("leases = %+v, want one releasing lease holding the process", st.Leases)
	}
	if len(st.Squatters) != 0 || strings.Join(r.seen, ",") != "" {
		t.Fatalf("squatters = %+v, reports = %v", st.Squatters, r.seen)
	}

	c.set()
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("leases = %+v, want none once the card shows the memory gone", st.Leases)
	}
}

// A released lease with a wrapper PID but nothing ever seen on the card
// frees on the next tick, not inside Release: the tick's card read is the
// only evidence that nothing turned up after the release.
func TestReleaseWithAWrapperPIDFreesOnTheNextTick(t *testing.T) {
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 10, 101: 11}}
	b, _, _ := setup(t, tree)
	l := must(t, acquire(b, Request{Engine: "ninfer"}))
	l.Attach(100)
	l.Release()
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].State != "releasing" {
		t.Fatalf("Release dropped the lease at once: %+v", st.Leases)
	}
	b.Tick(context.Background())
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("leases = %+v, want none after the freeing tick", st.Leases)
	}
}

// On a coarse clock (Windows, ~15 ms) a release and the tick that frees
// the lease share one Now() value. The tick's read still counts as
// read-after, so the lease frees on that tick and the waiter is granted.
func TestReleaseFreesWhenTheTickSharesTheReleaseQuantum(t *testing.T) {
	tree := procs.Map{101: 100}
	b, c, _ := setup(t, tree)
	ninfer := must(t, acquire(b, Request{Engine: "ninfer"}))
	ninfer.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 22 * GiB})
	b.Tick(context.Background())

	strata := acquire(b, Request{Engine: "strata"})
	pending(t, strata)
	drained(t, ninfer)
	ninfer.Release()
	c.set()

	// Pin the broker's clock to the release instant, so the next tick's
	// read starts in the release's own quantum.
	released := b.leases[ninfer.ID()].released
	b.nowFn = func() time.Time { return released }
	b.Tick(context.Background())

	if got := b.Status().Counters; got.VRAMHandedOff != 22*GiB {
		t.Fatalf("the same-quantum tick did not free the lease: counters = %+v", got)
	}
	must(t, strata)
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "strata" {
		t.Fatalf("leases = %+v, want only strata", st.Leases)
	}
}

// B1, read order: a release that lands while a tick is already reading the
// card does not free on that tick, whatever the clock says.
// The read began before the release, so it says nothing about a process that
// started after it; the next read does.
func TestReleaseDuringATicksReadWaitsForTheNextRead(t *testing.T) {
	tree := procs.Map{101: 100}
	b, c, _ := setup(t, tree)
	ninfer := must(t, acquire(b, Request{Engine: "ninfer"}))
	ninfer.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 22 * GiB})
	b.Tick(context.Background())

	strata := acquire(b, Request{Engine: "strata"})
	pending(t, strata)
	drained(t, ninfer)
	c.set()
	c.mu.Lock()
	c.onRead = func() { ninfer.Release() } // the release lands mid-read
	c.mu.Unlock()
	b.Tick(context.Background())
	pending(t, strata)

	c.mu.Lock()
	c.onRead = nil
	c.mu.Unlock()
	b.Tick(context.Background())
	must(t, strata)
}

func TestHigherPriorityHolderIsNotDrained(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	game := must(t, acquire(b, Request{Engine: "game", Priority: Foreground}))

	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), Request{Engine: "eval", Priority: Batch, Wait: 100 * time.Millisecond}, nil)
		ch <- result{l, err}
	}()
	r := <-ch
	if !errors.Is(r.err, ErrRefused) || !strings.Contains(r.err.Error(), "game") {
		t.Fatalf("want a refusal naming the holder, got %v", r.err)
	}
	select {
	case why := <-game.Drains():
		t.Fatalf("foreground holder was drained for a batch job: %s", why)
	default:
	}
}

func TestSizedLeasesShareTheCard(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	must(t, acquire(b, Request{Engine: "embed", Needs: 2 * GiB}))
	must(t, acquire(b, Request{Engine: "chat", Needs: 18 * GiB}))

	big := acquire(b, Request{Engine: "big", Needs: 10 * GiB, Priority: Batch})
	pending(t, big) // batch may not drain interactive holders, and 10 GiB is not free
}

func TestSizedRequestDrainsOnlyWhatItNeeds(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	small := must(t, acquire(b, Request{Engine: "small", Needs: 4 * GiB}))
	large := must(t, acquire(b, Request{Engine: "large", Needs: 16 * GiB}))

	// 4 GiB free; 6 GiB wanted; draining the older holder (small) suffices.
	ch := acquire(b, Request{Engine: "new", Needs: 6 * GiB})
	pending(t, ch)
	drained(t, small)
	select {
	case why := <-large.Drains():
		t.Fatalf("large was drained needlessly: %s", why)
	default:
	}
	small.Release()
	must(t, ch)
}

// The same on a clock that never moves, as Windows' 15 ms clock looks to
// leases granted together: the older holder is still the one drained, by
// the broker's own order, not by a tie of timestamps.
func TestSizedRequestDrainsTheOlderHolderOnAFrozenClock(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	frozen := time.Now()
	b.nowFn = func() time.Time { return frozen } // before any goroutine reads it
	small := must(t, acquire(b, Request{Engine: "small", Needs: 4 * GiB}))
	large := must(t, acquire(b, Request{Engine: "large", Needs: 16 * GiB}))

	ch := acquire(b, Request{Engine: "new", Needs: 6 * GiB})
	pending(t, ch)
	drained(t, small)
	select {
	case why := <-large.Drains():
		t.Fatalf("large was drained needlessly: %s", why)
	default:
	}
	small.Release()
	must(t, ch)
}

func TestSquatterReportedOnceAndNeverBlocksExclusive(t *testing.T) {
	b, c, r := setup(t, procs.Map{}, "Xorg")
	c.set(gpu.Proc{PID: 9, Name: "node", Used: 2 * GiB}, gpu.Proc{PID: 10, Name: "/usr/lib/Xorg", Used: GiB})
	b.Tick(context.Background())
	b.Tick(context.Background())

	st := b.Status()
	if len(st.Squatters) != 1 || st.Squatters[0].Name != "node" {
		t.Fatalf("squatters = %+v, want only node", st.Squatters)
	}
	must(t, acquire(b, Request{Engine: "ninfer"}))

	c.set()
	b.Tick(context.Background())
	if got := strings.Join(r.seen, ","); got != "appeared:node,gone:node" {
		t.Fatalf("reports = %s", got)
	}
}

// Gap b: a released lease whose processes outlive the grace does not let
// the next waiter in on process exit alone. It turns stuck, the waiter is
// told which process still holds what, and it gets in once the card shows
// the memory gone.
func TestStuckLeaseBlocksUntilMemoryFrees(t *testing.T) {
	tree := procs.Map{101: 100}
	b, c, r := setup(t, tree)
	b.cfg.ReleaseGrace = time.Millisecond

	l := must(t, acquire(b, Request{Engine: "leaky"}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "leaky-engine", Used: 3 * GiB})
	b.Tick(context.Background())
	l.Release()
	time.Sleep(5 * time.Millisecond)
	b.Tick(context.Background())

	reasons := make(chan string, 8)
	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), Request{Engine: "next"}, func(s string) { reasons <- s })
		ch <- result{l, err}
	}()
	pending(t, ch)
	b.Tick(context.Background())
	pending(t, ch)
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].State != "stuck" {
		t.Fatalf("leases = %+v, want one stuck", st.Leases)
	}
	if why := <-reasons; !strings.Contains(why, "stuck") || !strings.Contains(why, "pid 101 (leaky-engine) 3072 MiB") {
		t.Fatalf("wait reason %q does not say what is stuck", why)
	}
	if len(r.seen) != 0 {
		t.Fatalf("a stuck lease's process was reported as a squatter: %v", r.seen)
	}

	c.set()
	b.Tick(context.Background())
	must(t, ch)
}

// A keep-up drain leaves the engine running unloaded and records it as an
// unprotected run. The record lets the released lease free on the next
// tick: the lingering process no longer blocks the card, and it is listed
// as a run-backed squatter instead of holding the lease stuck.
func TestKeepUpRunFreesTheReleasedLease(t *testing.T) {
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	b, c, r := setup(t, tree)
	b.cfg.ReleaseGrace = time.Millisecond
	dir := t.TempDir()
	b.cfg.RunsDir = dir

	l := must(t, acquire(b, Request{Engine: "comfyui"}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 512 << 20})
	b.Tick(context.Background())
	if _, err := runs.Write(dir, runs.Record{
		Engine: "comfyui", PID: 100, Start: 1, Started: time.Now(), Reason: runs.ReasonKeepUp,
	}); err != nil {
		t.Fatal(err)
	}
	l.Release()
	b.Tick(context.Background())

	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("leases = %+v, want none after the keep-up release", st.Leases)
	}
	if got := strings.Join(r.seen, ","); !strings.Contains(got, "appeared:python") {
		t.Fatalf("reports = %s, want the lingering process listed as a run", got)
	}
	must(t, acquire(b, Request{Engine: "next"}))
}

// comfyStub is a ComfyUI whose /free counts calls.
func comfyStub(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	frees := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/free" {
			frees.Add(1)
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, frees
}

// An unprotected ComfyUI squatting the card is asked to unload through its
// own /free, and the waiter is held until the memory is actually free:
// granting onto its weights can only produce a load that dies (live on
// 2026-10-16).
func TestComfySquatIsPokedAndWaited(t *testing.T) {
	srv, frees := comfyStub(t)
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	b, c, _ := setup(t, tree)
	b.cfg.RunsDir = t.TempDir()
	b.cfg.Comfy = []ComfyEngine{{Name: "comfyui", URL: srv.URL}}
	if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
		Engine: "comfyui", PID: 100, Start: 1, Started: time.Now(), Reason: runs.ReasonKeepUp,
	}); err != nil {
		t.Fatal(err)
	}

	c.set(gpu.Proc{PID: 101, Name: "python", Used: 17 * GiB})
	b.Tick(context.Background())

	ch := acquire(b, Request{Engine: "ninfer"})
	pending(t, ch)
	b.Tick(context.Background())
	b.Tick(context.Background())
	select {
	case res := <-ch:
		t.Fatalf("granted while ComfyUI squatted: %+v", res)
	default:
	}
	deadline := time.Now().Add(2 * time.Second)
	for frees.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := frees.Load(); got != 1 {
		t.Fatalf("free asks = %d, want exactly 1 (rate limited)", got)
	}

	// The unload lands: the server stays alive, well under the threshold.
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 512 << 20})
	b.Tick(context.Background())
	must(t, ch)
}

// An unprotected run whose engine is not a configured ComfyUI keeps the
// old behavior: nothing to ask, so a whole-card request is granted and the
// contested counter flags the fit.
func TestSquatWithoutComfyStillGrants(t *testing.T) {
	srv, _ := comfyStub(t)
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 1, 101: 1}}
	b, c, _ := setup(t, tree)
	b.cfg.RunsDir = t.TempDir()
	b.cfg.Comfy = []ComfyEngine{{Name: "comfyui", URL: srv.URL}}
	if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
		Engine: "strata", PID: 100, Start: 1, Started: time.Now(), Reason: runs.ReasonKeepUp,
	}); err != nil {
		t.Fatal(err)
	}
	c.set(gpu.Proc{PID: 101, Name: "python", Used: 2 * GiB})
	b.Tick(context.Background())
	must(t, acquire(b, Request{Engine: "ninfer"}))
}

// Gap a: a PID that a lease's engine once had, reused by an unrelated
// process, is a squatter, not the lease's.
func TestPIDReuseIsNotOwnership(t *testing.T) {
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 10, 101: 11}}
	b, c, r := setup(t, tree)
	l := must(t, acquire(b, Request{Engine: "ninfer"}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 20 * GiB})
	b.Tick(context.Background())
	if st := b.Status(); st.Leases[0].Used != 20*GiB {
		t.Fatalf("engine not attributed: %+v", st.Leases[0])
	}

	// 101 exits; the kernel hands the PID to something else entirely.
	tree.Parents[101] = 1
	tree.Starts[101] = 99
	c.set(gpu.Proc{PID: 101, Name: "someone-else", Used: 2 * GiB})
	b.Tick(context.Background())
	st := b.Status()
	if st.Leases[0].Used != 0 || len(st.Squatters) != 1 || st.Squatters[0].Name != "someone-else" {
		t.Fatalf("reused PID inherited the lease: leases=%+v squatters=%+v", st.Leases, st.Squatters)
	}
	if strings.Join(r.seen, ",") != "appeared:someone-else" {
		t.Fatalf("reports = %v", r.seen)
	}

	// The engine itself restarting under the old root PID is no longer the lease either.
	tree.Starts[100] = 50
	tree.Parents[102] = 100
	tree.Starts[102] = 51
	c.set(gpu.Proc{PID: 102, Name: "impostor", Used: GiB})
	b.Tick(context.Background())
	if b.Status().Leases[0].Used != 0 {
		t.Fatal("a reused root PID still attributes children")
	}
}

// Gap c: report-only means a whole-card grant on a contested card still
// happens, but never silently.
func TestContestedGrantIsFlagged(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	c.set(gpu.Proc{PID: 9, Name: "orphan", Used: 2 * GiB})
	b.Tick(context.Background())
	l := must(t, acquire(b, Request{Engine: "ninfer"}))
	if l.Contested() != 2*GiB {
		t.Fatalf("contested = %d", l.Contested())
	}
	if st := b.Status(); st.Leases[0].Contested != 2*GiB {
		t.Fatalf("status lease = %+v", st.Leases[0])
	}
	l.Release()
	c.set()
	b.Tick(context.Background())
	if l2 := must(t, acquire(b, Request{Engine: "clean"})); l2.Contested() != 0 {
		t.Fatalf("clean card flagged contested: %d", l2.Contested())
	}
}

// Item 2: a sized grant on a card already full of squatters must wait,
// not grant. Nobody holds a lease, so there is nothing to drain and no
// victims: the waiter blocks head-of-queue until the memory shows free.
func TestEmptyHoldersBigSquatWaits(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	c.set(gpu.Proc{PID: 9, Name: "orphan", Used: 22 * GiB})
	b.Tick(context.Background())

	reasons := make(chan string, 8)
	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), Request{Engine: "big", Needs: 4 * GiB}, func(s string) { reasons <- s })
		ch <- result{l, err}
	}()
	pending(t, ch)
	select {
	case why := <-reasons:
		if !strings.Contains(why, "squatters hold 22528 MiB") {
			t.Fatalf("wait reason %q does not name the squatters", why)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter heard no reason")
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("leases = %+v, want none: a contested card must not grant", st.Leases)
	}
}

// Item 2: a sized grant that still fits despite squatters grants, and the
// contested counter records the residual fit.
func TestEmptyHoldersSmallSquatStillGrants(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	c.set(gpu.Proc{PID: 9, Name: "orphan", Used: 2 * GiB})
	b.Tick(context.Background())
	l := must(t, acquire(b, Request{Engine: "small", Needs: 4 * GiB}))
	if l.Contested() != 2*GiB {
		t.Fatalf("contested = %d, want 2 GiB", l.Contested())
	}
	if b.counters.ContestedGrants != 1 {
		t.Fatalf("ContestedGrants = %d, want 1", b.counters.ContestedGrants)
	}
}

// #103: on Windows the per-process cap bounds one process, but the SUM of
// the readings can still exceed what the driver reports as used: the
// counter counts committed reservations, which the card does not hold.
// The squatter total is bounded by what the card can really hand
// squatters, so a small probe still fits the free space the driver shows
// instead of waiting behind a phantom 27 GB.
func TestSquatterSumIsBoundedByTheCardsUsedMemory(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	c.total = 16303 * gpu.MiB
	c.used = 15600 * gpu.MiB
	c.set(
		gpu.Proc{PID: 9, Name: "llama-server", Used: 14230 * gpu.MiB},
		gpu.Proc{PID: 10, Name: "dwm.exe", Used: 11138 * gpu.MiB},
	)
	b.Tick(context.Background())

	// The readings sum to 25368 MiB on a card that uses 15600, so the
	// total counts the card's figure: 100 MiB fits the 703 MiB the card
	// really has free, and the contested figure stays bounded too.
	l := must(t, acquire(b, Request{Engine: "probe", Needs: 100 * gpu.MiB}))
	if l.Contested() != 15600*gpu.MiB {
		t.Fatalf("contested = %d MiB, want the card's used figure", l.Contested()/gpu.MiB)
	}
	l.Release()
	b.Tick(context.Background())

	// The per-squatter entries keep the raw readings; only the total
	// driving decisions is bounded.
	if st := b.Status(); len(st.Squatters) != 2 ||
		st.Squatters[0].Name != "llama-server" || st.Squatters[0].Used != 14230*gpu.MiB ||
		st.Squatters[1].Name != "dwm.exe" || st.Squatters[1].Used != 11138*gpu.MiB {
		t.Fatalf("status squatters = %+v, want the raw readings kept", st.Squatters)
	}
}

// #103: the bound shrinks the squatter total, it does not erase it: a
// request that does not fit the card's real free space still waits, told
// the bounded figure.
func TestSquatterSumBoundedBigRequestStillWaits(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	c.total = 16303 * gpu.MiB
	c.used = 15600 * gpu.MiB
	c.set(
		gpu.Proc{PID: 9, Name: "llama-server", Used: 14230 * gpu.MiB},
		gpu.Proc{PID: 10, Name: "dwm.exe", Used: 11138 * gpu.MiB},
	)
	b.Tick(context.Background())

	reasons := make(chan string, 8)
	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), Request{Engine: "big", Needs: 2000 * gpu.MiB}, func(s string) { reasons <- s })
		ch <- result{l, err}
	}()
	pending(t, ch)
	select {
	case why := <-reasons:
		if !strings.Contains(why, "at most 703 MiB can be freed") || !strings.Contains(why, "squatters hold 15600 MiB") {
			t.Fatalf("wait reason %q", why)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter heard no reason")
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("leases = %+v, want none: 2000 MiB does not fit", st.Leases)
	}
}

// Linux readings are exact: when the sum is under the card's used figure
// the bound is a no-op and admission sees the sum, as before.
func TestSquatterSumUnderTheUsedFigureIsUnchanged(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	c.total = 16303 * gpu.MiB
	c.used = 9000 * gpu.MiB
	c.set(
		gpu.Proc{PID: 9, Name: "llama-server", Used: 2000 * gpu.MiB},
		gpu.Proc{PID: 10, Name: "dwm.exe", Used: 3000 * gpu.MiB},
	)
	b.Tick(context.Background())

	// The 5000 MiB sum is under the card's 9000 MiB used figure, so the
	// bound (used minus the 4000 MiB baseline) equals the sum and nothing
	// is clamped: the waiter hears the raw sum and the same fit as before.
	reasons := make(chan string, 8)
	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), Request{Engine: "big", Needs: 10000 * gpu.MiB}, func(s string) { reasons <- s })
		ch <- result{l, err}
	}()
	pending(t, ch)
	select {
	case why := <-reasons:
		if !strings.Contains(why, "at most 7303 MiB can be freed") || !strings.Contains(why, "squatters hold 5000 MiB") {
			t.Fatalf("wait reason %q", why)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter heard no reason")
	}
}

// #103: the cap logs at most one line per change of the clamped state, not
// once per tick.
func TestSquatterCapLogsOncePerStateChange(t *testing.T) {
	b, c, _ := setup(t, procs.Map{})
	var buf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&buf, nil))
	c.total = 16303 * gpu.MiB
	c.used = 15600 * gpu.MiB
	c.set(
		gpu.Proc{PID: 9, Name: "llama-server", Used: 14230 * gpu.MiB},
		gpu.Proc{PID: 10, Name: "dwm.exe", Used: 11138 * gpu.MiB},
	)
	// The first tick reads the card and sees the squatters; the queued
	// waiter makes every later schedule recompute the bounded sum. Its
	// first wait reason only goes out once that schedule ran.
	b.Tick(context.Background())
	reasons := make(chan string, 8)
	why := make(chan struct{})
	ch := make(chan result, 1)
	go func() {
		l, err := b.Acquire(context.Background(), Request{Engine: "big", Needs: 2000 * gpu.MiB}, func(s string) {
			reasons <- s
			select {
			case why <- struct{}{}:
			default:
			}
		})
		ch <- result{l, err}
	}()
	select {
	case <-why:
	case <-time.After(time.Second):
		t.Fatal("waiter never heard a reason")
	}
	for i := 0; i < 3; i++ {
		b.Tick(context.Background())
	}
	const line = "squatter readings exceed the card's used memory"
	if n := strings.Count(buf.String(), line); n != 1 {
		t.Fatalf("cap logged %d times over clamped ticks, want 1:\n%s", n, buf.String())
	}

	// The driver shows the card using what the sum says: the state flips
	// back to the raw sum, and one further tick logs nothing.
	c.used = 25368 * gpu.MiB
	b.Tick(context.Background())
	b.Tick(context.Background())
	if n := strings.Count(buf.String(), line); n != 1 {
		t.Fatalf("cap logged %d times after unclamping, want still 1:\n%s", n, buf.String())
	}

	// And a new clamp is one more line: one per change of state.
	c.used = 15600 * gpu.MiB
	b.Tick(context.Background())
	if n := strings.Count(buf.String(), line); n != 2 {
		t.Fatalf("cap logged %d times, want 2 after re-clamping:\n%s", n, buf.String())
	}
}

// Gap d: an engine started without a lease is listed, and its GPU
// processes say which unprotected run they belong to.
func TestUnprotectedRunsAreListed(t *testing.T) {
	dir := t.TempDir()
	tree := procs.Fake{Parents: map[int]int{301: 300}, Starts: map[int]uint64{300: 7, 301: 8}}
	b, c, _ := setup(t, tree)
	b.cfg.RunsDir = dir
	// Past the re-attach window: inside it the run is due to re-attach,
	// not a squatter.
	b.cfg.ReattachWindow = time.Nanosecond
	if _, err := runs.Write(dir, runs.Record{Engine: "strata", PID: 300, Start: 7, Started: time.Now(), Reason: runs.ReasonStart}); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Write(dir, runs.Record{Engine: "ghost", PID: 400, Start: 3}); err != nil {
		t.Fatal(err)
	}
	c.set(gpu.Proc{PID: 301, Name: "strata-engine", Used: 20 * GiB})
	b.Tick(context.Background())

	st := b.Status()
	if len(st.Unprotected) != 1 || st.Unprotected[0].Engine != "strata" {
		t.Fatalf("unprotected = %+v (the dead run must be dropped)", st.Unprotected)
	}
	if len(st.Squatters) != 1 || st.Squatters[0].Run != "strata" {
		t.Fatalf("squatters = %+v", st.Squatters)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(left) != 1 {
		t.Fatalf("records left = %v, want only the live one", left)
	}
}

func TestOversizedRequestRefusedAtOnce(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	start := time.Now()
	_, err := b.Acquire(context.Background(), Request{Engine: "huge", Needs: 128 * GiB}, nil)
	if !errors.Is(err, ErrRefused) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestQueueOrderIsPriorityThenArrival(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "holder", Priority: Foreground}))

	batch := acquire(b, Request{Engine: "batch", Priority: Batch})
	time.Sleep(10 * time.Millisecond)
	inter := acquire(b, Request{Engine: "inter", Priority: Interactive})
	time.Sleep(10 * time.Millisecond)

	holder.Release()
	l := must(t, inter)
	pending(t, batch)
	l.Release()
	must(t, batch)
}

// An engine that has exited while the driver still lists its memory keeps
// its lease releasing: the lease is not freed until the driver lets go.
func TestExitedProcessStillListedKeepsItsLease(t *testing.T) {
	tree := procs.Fake{Parents: map[int]int{101: 100}, Starts: map[int]uint64{100: 10, 101: 11}}
	b, c, _ := setup(t, tree)
	l := must(t, acquire(b, Request{Engine: "ninfer"}))
	l.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 20 * GiB})
	b.Tick(context.Background())

	// Gone from /proc, still on the card.
	delete(tree.Parents, 101)
	delete(tree.Starts, 101)
	l.Release()
	next := acquire(b, Request{Engine: "strata"})
	b.Tick(context.Background())
	b.Tick(context.Background())
	pending(t, next)
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Used != 20*GiB || len(st.Squatters) != 0 {
		t.Fatalf("leases=%+v squatters=%+v", st.Leases, st.Squatters)
	}

	c.set()
	b.Tick(context.Background())
	must(t, next)
}

// The collisions board counts the handoff from the exit test: one request
// waited instead of loading on a held card, one drain, and the holder's
// peak memory handed off once the card showed it free.
func TestHandoffFillsTheBoardAndTheLog(t *testing.T) {
	tree := procs.Map{101: 100}
	b, c, _ := setup(t, tree)
	ninfer := must(t, acquire(b, Request{Engine: "ninfer"}))
	ninfer.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 22 * GiB})
	b.Tick(context.Background())

	strata := acquire(b, Request{Engine: "strata"})
	pending(t, strata)
	drained(t, ninfer)
	ninfer.Release()
	c.set()
	b.Tick(context.Background())
	must(t, strata)

	got := b.Status().Counters
	if got.CollisionsAvoided != 1 || got.Drains != 1 || got.VRAMHandedOff != 22*GiB {
		t.Fatalf("counters = %+v", got)
	}
	evs, _, _ := b.Events(0, false)
	var kinds []string
	for i, e := range evs {
		if e.Seq != uint64(i+1) {
			t.Fatalf("seq gap at %d: %d", i, e.Seq)
		}
		kinds = append(kinds, e.Kind)
	}
	want := "lease.requested,lease.granted,lease.requested,lease.draining,lease.waiting,lease.released,lease.waiting,lease.freed,lease.granted"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("events =\n %s\nwant\n %s", strings.Join(kinds, ","), want)
	}
	if after, _, _ := b.Events(evs[5].Seq, false); len(after) != 3 {
		t.Fatalf("since filter returned %d", len(after))
	}
}

func TestEventRingAndSlowWatcher(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	_, live, stop := b.Events(0, true)
	defer stop()
	b.mu.Lock()
	for i := 0; i < EventRing+300; i++ {
		b.emit(proto.Event{Kind: "test"})
	}
	b.mu.Unlock()
	evs, _, _ := b.Events(0, false)
	if len(evs) != EventRing || evs[0].Seq != 301 || evs[len(evs)-1].Seq != EventRing+300 {
		t.Fatalf("ring holds %d, first %d, last %d", len(evs), evs[0].Seq, evs[len(evs)-1].Seq)
	}
	n := 0
	for range live {
		n++
	}
	if n != 256 {
		t.Fatalf("slow watcher got %d before being cut off, want 256", n)
	}
}

// The board survives a restart: a second broker on the same counters file
// starts from the first one's counts and its since time, and keeps counting.
func TestBoardSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "counters.json")
	tree := procs.Map{101: 100}
	c := &card{total: 24 * GiB}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{Reporter: &reports{}, ReleaseGrace: time.Hour, DefaultWait: 2 * time.Second, CountersFile: file, JuggleWindow: -1, Log: quiet}

	b := New(cfg, c, tree)
	b.Tick(context.Background())
	ninfer := must(t, acquire(b, Request{Engine: "ninfer"}))
	ninfer.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "ninfer-serve", Used: 22 * GiB})
	b.Tick(context.Background())
	strata := acquire(b, Request{Engine: "strata"})
	pending(t, strata)
	drained(t, ninfer)
	ninfer.Release()
	c.set()
	b.Tick(context.Background())
	must(t, strata).Release()
	b.Tick(context.Background())
	first := b.Status().Counters
	if first.CollisionsAvoided != 1 || first.Drains != 1 {
		t.Fatalf("first daemon counters = %+v", first)
	}
	st, err := os.Stat(file)
	if err != nil {
		t.Fatalf("counters file: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		// NTFS carries no Unix mode bits, so on Windows the file's
		// existence is the whole check.
		t.Fatalf("counters file mode %v", st.Mode())
	}

	c.set()
	b2 := New(cfg, c, tree)
	got := b2.Status().Counters
	if got.CollisionsAvoided != 1 || got.Drains != 1 || got.VRAMHandedOff != first.VRAMHandedOff || !got.Since.Equal(first.Since) {
		t.Fatalf("restored counters = %+v, want %+v", got, first)
	}
	b2.Tick(context.Background())
	if _, err := b2.Acquire(context.Background(), Request{Engine: "x", Needs: 48 * GiB, Wait: time.Millisecond}, nil); err == nil {
		t.Fatal("a request larger than the card was granted")
	}
	b2.Tick(context.Background())
	if b3 := New(cfg, c, tree); b3.Status().Counters.Refused != got.Refused+1 {
		t.Fatalf("a count made after the restore was not saved: %+v", b3.Status().Counters)
	}
}

// A board file that does not parse is reported and the board starts at zero;
// the daemon still starts.
func TestBadBoardFileStartsAtZero(t *testing.T) {
	file := filepath.Join(t.TempDir(), "counters.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := New(Config{CountersFile: file, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &card{total: 24 * GiB}, procs.Map{})
	if c := b.Status().Counters; c.CollisionsAvoided != 0 || c.Since.IsZero() {
		t.Fatalf("counters = %+v", c)
	}
}
