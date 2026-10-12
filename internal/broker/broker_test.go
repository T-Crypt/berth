package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
)

const GiB = 1 << 30

// card is a fake GPU whose processes the test sets directly.
type card struct {
	mu    sync.Mutex
	total uint64
	procs []gpu.Proc
}

func (c *card) set(ps ...gpu.Proc) {
	c.mu.Lock()
	c.procs = ps
	c.mu.Unlock()
}

func (c *card) Read(context.Context) (gpu.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var used uint64
	for _, p := range c.procs {
		used += p.Used
	}
	return gpu.Snapshot{Total: c.total, Used: used, Free: c.total - used, Procs: append([]gpu.Proc(nil), c.procs...)}, nil
}

type reports struct {
	mu   sync.Mutex
	seen []string
}

func (r *reports) Squatter(appeared bool, p gpu.Proc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	verb := "gone"
	if appeared {
		verb = "appeared"
	}
	r.seen = append(r.seen, verb+":"+p.Name)
}

func (r *reports) Alert(level, key, message string) {
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

// Gap d: an engine started without a lease is listed, and its GPU
// processes say which unprotected run they belong to.
func TestUnprotectedRunsAreListed(t *testing.T) {
	dir := t.TempDir()
	tree := procs.Fake{Parents: map[int]int{301: 300}, Starts: map[int]uint64{300: 7, 301: 8}}
	b, c, _ := setup(t, tree)
	b.cfg.RunsDir = dir
	if _, err := runs.Write(dir, runs.Record{Engine: "strata", PID: 300, Start: 7, Started: time.Now(), Reason: "berthd unreachable at start"}); err != nil {
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
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("counters file: %v %v", st, err)
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
