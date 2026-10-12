// Package broker owns the lease table for one GPU. It decides who gets the
// card, asks holders to drain, attributes every GPU process to a lease, and
// reports the ones nobody leased. It never kills anything.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
)

type Priority int

const (
	Batch Priority = iota
	Interactive
	Foreground
)

func (p Priority) String() string {
	switch p {
	case Batch:
		return "batch"
	case Foreground:
		return "foreground"
	}
	return "interactive"
}

func ParsePriority(s string) (Priority, error) {
	switch strings.ToLower(s) {
	case "", "interactive":
		return Interactive, nil
	case "batch":
		return Batch, nil
	case "foreground":
		return Foreground, nil
	}
	return Interactive, fmt.Errorf("unknown priority %q (batch, interactive, foreground)", s)
}

type state int

const (
	waiting state = iota
	active
	draining  // asked to give the berth back
	releasing // connection gone; waiting for its GPU processes to let go
	stuck     // released, but its processes still hold memory past ReleaseGrace
)

func (s state) String() string {
	return [...]string{"waiting", "active", "draining", "releasing", "stuck"}[s]
}

// Reporter hears about squatters (GPU processes no lease accounts for)
// and alert-level crossings; it only ever reports, never touches a process.
type Reporter interface {
	Squatter(appeared bool, p gpu.Proc)
	// Alert hears a threshold or danger level crossing (warn/crit) from
	// the alert evaluator; like Squatter it only ever reports.
	Alert(level, key, message string)
}

type Config struct {
	Poll         time.Duration // how often the card is read
	Reserve      uint64        // bytes never handed out
	ReleaseGrace time.Duration // how long a released lease's processes may linger before it is reported stuck
	DefaultWait  time.Duration // how long Acquire waits when the request names no limit
	Allow        []string      // process-name substrings that are never squatters
	RunsDir      string        // where berth run records unprotected runs; "" = none
	// CountersFile keeps the collisions board across restarts and reboots:
	// read at start, rewritten after any tick that changed a count. "" = the
	// board starts at zero with every daemon, as before.
	CountersFile string
	// Order ranks engines (lease groups, else names) for juggles, first is
	// best; empty = rank by card-hours over the last 7 days (UsageFile).
	Order []string
	// JuggleWindow: an equal-priority drain of a holder granted less than
	// this ago is a juggle, decided by rank. 0 = DefaultJuggleWindow; < 0 = off.
	JuggleWindow time.Duration
	// UsageFile keeps the card-hours per engine across restarts; "" = memory only.
	UsageFile string
	// Busy reports whether a lease's engine is serving now (a request in
	// flight, queued work, a model loading). Only a busy holder can be in a
	// juggle; an idle one hands the card over at once. nil, or known=false,
	// counts as busy.
	Busy func(key string) (busy, known bool)
	// ReattachWindow is how long a starting daemon holds new grants while
	// engines that lost the previous daemon mid-run are still alive and
	// due to re-attach.
	ReattachWindow time.Duration
	Version        string
	Reporter       Reporter
	Log            *slog.Logger
}

type Request struct {
	Engine   string
	Group    string // the engine family a lease counts for (llama-swap's models); "" = Engine
	Priority Priority
	Needs    uint64 // bytes; 0 = the whole card
	Wait     time.Duration
}

// ident is a process identity. PIDs are reused, so a PID only means the same
// process while its start time matches.
type ident struct {
	pid   int
	start uint64
}

type lease struct {
	id        string
	engine    string
	group     string
	lostTo    string // a juggle this waiter lost: the winner's lease id; it waits, never drains it
	lostWhy   string
	reattach  bool // re-attached after a daemon restart: granted is the re-attach, not a fresh switch
	prio      Priority
	needs     uint64
	state     state
	pid       int
	pidStart  uint64
	gpu       map[ident]gpu.Proc
	contested uint64 // squatter bytes on the card when granted
	peak      uint64 // most memory its processes were measured holding
	arrived   time.Time
	granted   time.Time
	released  time.Time

	grantCh  chan struct{}
	drainCh  chan string
	waitCh   chan string
	lastWait string
}

func (l *lease) used() uint64 {
	var n uint64
	for _, p := range l.gpu {
		n += p.Used
	}
	return n
}

type squatter struct {
	id   ident
	proc gpu.Proc
	run  string // the unprotected run it descends from, if any
	seen time.Time
}

type Broker struct {
	cfg   Config
	probe gpu.Probe
	tree  procs.Tree
	log   *slog.Logger

	mu        sync.Mutex
	snap      gpu.Snapshot
	probeErr  error
	leases    map[string]*lease // granted: active, draining, releasing
	queue     []*lease
	squatters map[ident]*squatter
	runs      []runs.Record
	nextID    int
	started   time.Time

	events   []proto.Event // ring, oldest first
	seq      uint64
	subs     map[chan proto.Event]struct{}
	counters proto.Counters
	// saved is the board as last written to cfg.CountersFile; saveMu keeps
	// two writers off the file, saveFailed logs a failing write once.
	saved      proto.Counters
	saveMu     sync.Mutex
	saveFailed bool

	verdicts   map[string]verdict // loser engine key -> the juggle it lost
	usage      usage
	usageAt    time.Time // last tick credited to usage
	usageSaved time.Time
	usageDirty bool
}

// EventRing is how many events berthd keeps in memory.
const EventRing = 1000

// emit records an event and fans it out. Called with b.mu held. A watcher
// too slow to keep up is cut off rather than allowed to stall scheduling;
// it reconnects with Since and loses nothing still in the ring.
func (b *Broker) emit(e proto.Event) {
	b.seq++
	e.Seq, e.At = b.seq, time.Now()
	if len(b.events) == EventRing {
		copy(b.events, b.events[1:])
		b.events = b.events[:EventRing-1]
	}
	b.events = append(b.events, e)
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(b.subs, ch)
		}
	}
}

// Note records an event raised outside the tick cycle (the alert layer)
// and fans it out like any other. The caller must not hold b.mu.
func (b *Broker) Note(e proto.Event) {
	b.mu.Lock()
	b.emit(e)
	b.mu.Unlock()
}

// Events returns the events after since, and, if watch, a channel of every
// later one plus a func to stop watching. The channel closes if the watcher
// falls more than 256 events behind.
func (b *Broker) Events(since uint64, watch bool) ([]proto.Event, <-chan proto.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []proto.Event
	for _, e := range b.events {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	if !watch {
		return out, nil, func() {}
	}
	ch := make(chan proto.Event, 256)
	b.subs[ch] = struct{}{}
	return out, ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

func New(cfg Config, probe gpu.Probe, tree procs.Tree) *Broker {
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.ReleaseGrace <= 0 {
		cfg.ReleaseGrace = 30 * time.Second
	}
	if cfg.DefaultWait <= 0 {
		cfg.DefaultWait = 120 * time.Second
	}
	if cfg.ReattachWindow <= 0 {
		cfg.ReattachWindow = 15 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	// Read the run records now, not on the first Tick: a request that
	// lands before then must already see engines due to re-attach.
	var rs []runs.Record
	if cfg.RunsDir != "" {
		rs = runs.List(cfg.RunsDir, tree.StartTime)
	}
	counters := proto.Counters{Since: time.Now()}
	if cfg.CountersFile != "" {
		switch c, err := loadCounters(cfg.CountersFile); {
		case err == nil:
			counters = c
			log.Info("collisions board restored", "file", cfg.CountersFile, "since", c.Since.Format(time.RFC3339), "collisions_avoided", c.CollisionsAvoided)
		case !errors.Is(err, fs.ErrNotExist):
			log.Warn("collisions board not restored; starting at zero", "file", cfg.CountersFile, "err", err)
		}
	}
	use := usage{}
	if cfg.UsageFile != "" {
		if u, err := loadUsage(cfg.UsageFile); err == nil {
			use = u
		} else if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("engine usage not restored; the default ranking starts empty", "file", cfg.UsageFile, "err", err)
		}
	}
	return &Broker{
		cfg: cfg, probe: probe, tree: tree, log: log, runs: rs,
		leases:    map[string]*lease{},
		squatters: map[ident]*squatter{},
		subs:      map[chan proto.Event]struct{}{},
		counters:  counters,
		saved:     counters,
		verdicts:  map[string]verdict{},
		usage:     use,
		started:   time.Now(),
	}
}

func loadCounters(path string) (proto.Counters, error) {
	var c proto.Counters
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Since.IsZero() {
		return c, errors.New("no since time in the file")
	}
	return c, nil
}

// persist writes the board when a count changed since the last write. The
// write is a temp file renamed over the old one, so a crash mid-write leaves
// the previous board, never a torn one. It runs outside b.mu.
func (b *Broker) persist() {
	if b.cfg.CountersFile == "" {
		return
	}
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	c := b.counters
	b.mu.Unlock()
	if c == b.saved {
		return
	}
	data, err := json.Marshal(c)
	if err == nil {
		err = writeFileAtomic(b.cfg.CountersFile, data)
	}
	if err != nil {
		if !b.saveFailed {
			b.log.Warn("collisions board not saved; it will not survive a restart", "file", b.cfg.CountersFile, "err", err)
			b.saveFailed = true
		}
		return
	}
	b.saveFailed = false
	b.saved = c
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op after the rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Run reads the card every Poll until ctx ends.
func (b *Broker) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.Poll)
	defer t.Stop()
	defer b.persist() // counts made after the last tick
	defer func() { b.persistUsage(time.Now(), true) }()
	for {
		b.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick reads the card once, attributes every process, and reschedules.
func (b *Broker) Tick(ctx context.Context) {
	defer b.persist() // after the unlock below; also saves counts made between ticks
	defer func() { b.persistUsage(time.Now(), false) }()
	snap, err := b.probe.Read(ctx)
	var rs []runs.Record
	if b.cfg.RunsDir != "" {
		rs = runs.List(b.cfg.RunsDir, b.tree.StartTime)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.runs = rs
	b.accountUsage(now)
	b.pruneVerdicts(now)
	if err != nil {
		if b.probeErr == nil {
			b.log.Warn("gpu probe failed; scheduling on the lease table alone", "err", err)
			b.emit(proto.Event{Kind: proto.EvProbeFailed, Detail: err.Error()})
		}
		b.probeErr = err
		b.schedule(now)
		return
	}
	if b.probeErr != nil {
		b.log.Info("gpu probe recovered")
		b.emit(proto.Event{Kind: proto.EvProbeRecovered})
	}
	b.probeErr = nil
	b.snap = snap

	present := map[ident]bool{}
	for _, p := range snap.Procs {
		id := b.identify(p.PID)
		present[id] = true
		if l := b.owner(id); l != nil {
			l.gpu[id] = p
			l.peak = max(l.peak, l.used())
			if s, ok := b.squatters[id]; ok {
				b.reportGone(s)
			}
			continue
		}
		if b.allowed(p.Name) {
			continue
		}
		if s, ok := b.squatters[id]; ok {
			s.proc = p
			continue
		}
		run := b.runFor(p.PID)
		if run != nil && b.dueToReattach(*run, now) {
			// Its berth run is about to re-attach it; it is not a squatter.
			continue
		}
		s := &squatter{id: id, proc: p, seen: now}
		if run != nil {
			s.run = run.Engine
		}
		b.squatters[id] = s
		b.log.Warn("squatter: GPU memory held without a lease", "pid", p.PID, "name", p.Name, "mib", p.Used/gpu.MiB, "unprotected_run", s.run)
		b.counters.SquattersCaught++
		b.emit(proto.Event{Kind: proto.EvSquatter, PID: p.PID, Bytes: p.Used, Engine: s.run, Detail: p.Name})
		if b.cfg.Reporter != nil {
			b.cfg.Reporter.Squatter(true, p)
		}
	}
	for id, s := range b.squatters {
		if !present[id] {
			b.reportGone(s)
		}
	}
	for key, l := range b.leases {
		for id := range l.gpu {
			if !present[id] {
				delete(l.gpu, id)
			}
		}
		if l.state != releasing && l.state != stuck {
			continue
		}
		if len(l.gpu) == 0 {
			b.log.Info("lease freed", "lease", key, "engine", l.engine)
			b.counters.VRAMHandedOff += l.peak
			b.emit(proto.Event{Kind: proto.EvFreed, Lease: key, Engine: l.engine, Bytes: l.peak})
			delete(b.leases, key)
		} else if l.state == releasing && now.Sub(l.released) > b.cfg.ReleaseGrace {
			// Never grant on process exit alone: the lease keeps blocking
			// until the card shows its memory gone, and waiters are told why.
			l.state = stuck
			b.log.Warn("lease stuck: released but its processes still hold memory", "lease", key, "engine", l.engine, "holding", holding(l))
			b.counters.StuckCaught++
			b.emit(proto.Event{Kind: proto.EvStuck, Lease: key, Engine: l.engine, Bytes: l.used(), Detail: holding(l)})
		}
	}
	b.schedule(now)
}

// identify gives a GPU process its identity. A process that has exited
// while the driver still lists its memory has no /proc entry and no start
// time; it is still the process already recorded under that PID, and its
// memory still counts until the driver lets go.
func (b *Broker) identify(pid int) ident {
	if st := b.tree.StartTime(pid); st != 0 {
		return ident{pid, st}
	}
	for _, l := range b.leases {
		for id := range l.gpu {
			if id.pid == pid {
				return id
			}
		}
	}
	for id := range b.squatters {
		if id.pid == pid {
			return id
		}
	}
	return ident{pid, 0}
}

func (b *Broker) reportGone(s *squatter) {
	delete(b.squatters, s.id)
	b.log.Info("squatter gone", "pid", s.proc.PID, "name", s.proc.Name)
	b.emit(proto.Event{Kind: proto.EvSquatterGone, PID: s.proc.PID, Bytes: s.proc.Used, Detail: s.proc.Name})
	if b.cfg.Reporter != nil {
		b.cfg.Reporter.Squatter(false, s.proc)
	}
}

// owner finds the lease a GPU process belongs to: one it was already
// attributed to, else the lease whose wrapped engine it descends from. The
// engine's PID only counts while its start time still matches, so a reused
// PID never inherits a lease.
func (b *Broker) owner(id ident) *lease {
	for _, l := range b.leases {
		if _, ok := l.gpu[id]; ok {
			return l
		}
	}
	for _, l := range b.leases {
		if (l.state == active || l.state == draining) && l.pid > 0 &&
			b.tree.StartTime(l.pid) == l.pidStart && procs.DescendsFrom(b.tree, id.pid, l.pid) {
			return l
		}
	}
	return nil
}

// runFor finds the unprotected run a process descends from, if any.
func (b *Broker) runFor(pid int) *runs.Record {
	for i, r := range b.runs {
		if procs.DescendsFrom(b.tree, pid, r.PID) {
			return &b.runs[i]
		}
	}
	return nil
}

// dueToReattach is true for a run that lost the previous daemon mid-run,
// while this daemon is young enough that its berth run is still dialling
// back, and it has not re-attached yet.
func (b *Broker) dueToReattach(r runs.Record, now time.Time) bool {
	if r.Reason != runs.ReasonLost || now.Sub(b.started) >= b.cfg.ReattachWindow {
		return false
	}
	for _, l := range b.leases {
		if l.pid == r.PID && l.pidStart == r.Start {
			return false
		}
	}
	return true
}

// reattaching counts the runs dueToReattach.
func (b *Broker) reattaching(now time.Time) int {
	n := 0
	for _, r := range b.runs {
		if b.dueToReattach(r, now) {
			n++
		}
	}
	return n
}

func holding(l *lease) string {
	parts := make([]string, 0, len(l.gpu))
	for _, p := range l.gpu {
		parts = append(parts, fmt.Sprintf("pid %d (%s) %d MiB", p.PID, p.Name, p.Used/gpu.MiB))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (b *Broker) allowed(name string) bool {
	for _, a := range b.cfg.Allow {
		if a != "" && strings.Contains(name, a) {
			return true
		}
	}
	return false
}

// schedule grants whatever fits, in priority then arrival order, and asks
// holders to drain for the first waiter that does not fit. Nobody behind a
// blocked waiter jumps it, so a big request is not starved by small ones.
func (b *Broker) schedule(now time.Time) {
	sort.SliceStable(b.queue, func(i, j int) bool {
		if b.queue[i].prio != b.queue[j].prio {
			return b.queue[i].prio > b.queue[j].prio
		}
		return b.queue[i].arrived.Before(b.queue[j].arrived)
	})
	for len(b.queue) > 0 {
		w := b.queue[0]
		// A fresh daemon does not yet know what engines that ran under the
		// last one hold; granting now could put a second engine on a card
		// that is already full. The Tick after the window lifts this.
		if n := b.reattaching(now); n > 0 {
			b.setWait(w, fmt.Sprintf("berthd restarted; waiting up to %s for %d running engine(s) to re-attach",
				(b.cfg.ReattachWindow-now.Sub(b.started)).Round(time.Second), n))
			return
		}
		ok, reason, victims := b.evaluate(w, now)
		if ok {
			b.queue = b.queue[1:]
			w.state = active
			w.granted = now
			w.contested = b.squatBytes()
			b.leases[w.id] = w
			close(w.grantCh)
			if w.contested > 0 {
				b.log.Warn("granted on a contested card", "lease", w.id, "squatters_mib", w.contested/gpu.MiB)
				b.counters.ContestedGrants++
			}
			if w.lastWait != "" {
				b.counters.CollisionsAvoided++
			}
			b.emit(proto.Event{Kind: proto.EvGranted, Lease: w.id, Engine: w.engine, Bytes: w.contested,
				Detail: fmt.Sprintf("waited %s", now.Sub(w.arrived).Round(time.Millisecond))})
			b.log.Info("granted", "lease", w.id, "engine", w.engine, "priority", w.prio, "needs_mib", w.needs/gpu.MiB, "waited", now.Sub(w.arrived).Round(time.Millisecond))
			continue
		}
		for _, v := range victims {
			v.state = draining
			msg := fmt.Sprintf("%s (%s) needs the card", w.engine, w.prio)
			select {
			case v.drainCh <- msg:
			default:
			}
			b.log.Info("draining", "lease", v.id, "engine", v.engine, "for", w.engine)
			b.counters.Drains++
			b.emit(proto.Event{Kind: proto.EvDraining, Lease: v.id, Engine: v.engine, Detail: msg})
		}
		b.setWait(w, reason)
		return
	}
}

func (b *Broker) setWait(w *lease, reason string) {
	if reason == w.lastWait {
		return
	}
	w.lastWait = reason
	b.emit(proto.Event{Kind: proto.EvWaiting, Lease: w.id, Engine: w.engine, Detail: reason})
	select {
	case w.waitCh <- reason:
	default:
	}
}

// budget is what a holder counts against the card: the whole card if it
// asked for it, else whichever is larger of what it asked for and what it holds.
func (b *Broker) budget(l *lease) uint64 {
	if l.needs == 0 {
		return b.snap.Total
	}
	return max(l.needs, l.used())
}

func (b *Broker) holders() []*lease {
	hs := make([]*lease, 0, len(b.leases))
	for _, l := range b.leases {
		hs = append(hs, l)
	}
	sort.Slice(hs, func(i, j int) bool {
		if hs[i].prio != hs[j].prio {
			return hs[i].prio < hs[j].prio
		}
		return hs[i].granted.Before(hs[j].granted)
	})
	return hs
}

func describe(ls []*lease) string {
	names := make([]string, len(ls))
	for i, l := range ls {
		if l.state == stuck {
			names[i] = fmt.Sprintf("%s [%s, stuck: %s still held after release]", l.engine, l.prio, holding(l))
			continue
		}
		names[i] = fmt.Sprintf("%s [%s, %s]", l.engine, l.prio, l.state)
	}
	return strings.Join(names, ", ")
}

func (b *Broker) squatBytes() uint64 {
	var n uint64
	for _, s := range b.squatters {
		n += s.proc.Used
	}
	return n
}

// evaluate decides whether w fits now. If not, it returns why and which
// holders to drain; no victims means draining would not help, so w waits.
func (b *Broker) evaluate(w *lease, now time.Time) (bool, string, []*lease) {
	hs := b.holders()
	if len(hs) == 0 {
		return true, "", nil
	}
	exclusive := w.needs == 0 || b.snap.Total == 0

	var blockers, pending, drainable, kept []*lease
	var keptWhy string
	for _, h := range hs {
		switch {
		case h.state != active:
			pending = append(pending, h)
		case h.prio > w.prio:
			blockers = append(blockers, h)
		case h.prio == w.prio:
			if keep, why := b.keeps(h, w, now); keep {
				kept, keptWhy = append(kept, h), why
				continue
			}
			drainable = append(drainable, h)
		default:
			drainable = append(drainable, h)
		}
	}

	if exclusive {
		if len(blockers) > 0 {
			return false, "held at higher priority by " + describe(blockers), nil
		}
		if len(kept) > 0 {
			return false, keptWhy, nil
		}
		if len(drainable) > 0 {
			return false, "draining " + describe(drainable), drainable
		}
		return false, "waiting for " + describe(pending) + " to free memory", nil
	}

	squat := b.squatBytes()
	var held uint64
	for _, h := range hs {
		held += b.budget(h)
	}
	ceiling := sat(b.snap.Total, b.cfg.Reserve+b.snap.Baseline()+squat)
	avail := sat(ceiling, held)
	if avail >= w.needs {
		return true, "", nil
	}
	var leaving uint64
	for _, h := range pending {
		leaving += b.budget(h)
	}
	if avail+leaving >= w.needs {
		return false, "waiting for " + describe(pending) + " to free memory", nil
	}
	var victims []*lease
	freed := avail + leaving
	for _, h := range drainable {
		victims = append(victims, h)
		freed += b.budget(h)
		if freed >= w.needs {
			return false, "draining " + describe(victims), victims
		}
	}
	reason := fmt.Sprintf("needs %d MiB; at most %d MiB can be freed", w.needs/gpu.MiB, freed/gpu.MiB)
	if len(blockers) > 0 {
		reason += "; held at higher priority by " + describe(blockers)
	}
	if len(kept) > 0 {
		reason += "; " + keptWhy
	}
	if squat > 0 {
		reason += fmt.Sprintf("; squatters hold %d MiB", squat/gpu.MiB)
	}
	return false, reason, nil
}

func sat(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

// Lease is a granted berth.
type Lease struct {
	b *Broker
	l *lease
}

func (l *Lease) ID() string { return l.l.id }

// Drains delivers the reason when the broker wants the berth back.
func (l *Lease) Drains() <-chan string { return l.l.drainCh }

// Contested is how many bytes squatters held on the card at grant time.
// Report-only means the grant still happens; it must not happen silently.
func (l *Lease) Contested() uint64 {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	return l.l.contested
}

// Attach records the engine holding the berth, by PID plus start time.
func (l *Lease) Attach(pid int) {
	start := l.b.tree.StartTime(pid)
	l.b.mu.Lock()
	l.l.pid, l.l.pidStart = pid, start
	l.b.mu.Unlock()
}

// Release gives the berth back. Its memory stays counted until the card
// shows its processes gone, or ReleaseGrace passes.
func (l *Lease) Release() {
	b := l.b
	b.mu.Lock()
	defer b.mu.Unlock()
	x, ok := b.leases[l.l.id]
	if !ok || x.state == releasing || x.state == stuck {
		return
	}
	x.state = releasing
	x.released = time.Now()
	b.log.Info("released", "lease", x.id, "engine", x.engine, "held", x.released.Sub(x.granted).Round(time.Second))
	b.emit(proto.Event{Kind: proto.EvReleased, Lease: x.id, Engine: x.engine, Bytes: x.used()})
	if len(x.gpu) == 0 {
		b.counters.VRAMHandedOff += x.peak
		b.emit(proto.Event{Kind: proto.EvFreed, Lease: x.id, Engine: x.engine, Bytes: x.peak})
		delete(b.leases, x.id)
	}
	b.schedule(time.Now())
}

// Reattach leases the card again to an engine that started under berth run
// and outlived its daemon. It is already on the card, so it is granted at
// once rather than queued; the identity check (PID plus start time) is what
// keeps a stranger from claiming the lease.
func (b *Broker) Reattach(r Request, pid int, start uint64) (*Lease, error) {
	if pid <= 0 || start == 0 || b.tree.StartTime(pid) != start {
		return nil, fmt.Errorf("%w: pid %d is not the process that started as %s", ErrRefused, pid, r.Engine)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.leases {
		if l.pid == pid && l.pidStart == start {
			return nil, fmt.Errorf("%w: pid %d already holds %s", ErrRefused, pid, l.id)
		}
	}
	b.nextID++
	now := time.Now()
	l := &lease{
		id:       fmt.Sprintf("%s-%d", r.Engine, b.nextID),
		engine:   r.Engine,
		group:    r.Group,
		prio:     r.Priority,
		needs:    r.Needs,
		state:    active,
		reattach: true,
		pid:      pid,
		pidStart: start,
		gpu:      map[ident]gpu.Proc{},
		arrived:  now,
		granted:  now,
		grantCh:  make(chan struct{}),
		drainCh:  make(chan string, 1),
		waitCh:   make(chan string, 8),
	}
	close(l.grantCh)
	b.leases[l.id] = l
	b.log.Info("reattached", "lease", l.id, "engine", l.engine, "priority", l.prio, "pid", pid)
	b.emit(proto.Event{Kind: proto.EvReattached, Lease: l.id, Engine: l.engine, PID: pid, Detail: l.prio.String()})
	b.schedule(now)
	return &Lease{b: b, l: l}, nil
}

// ErrRefused wraps every reason Acquire gives up.
var ErrRefused = errors.New("refused")

// Acquire queues for a berth and blocks until it is granted, ctx ends, or
// the wait runs out. onWait hears each new reason for waiting.
func (b *Broker) Acquire(ctx context.Context, r Request, onWait func(string)) (*Lease, error) {
	b.mu.Lock()
	// A request bigger than the card can never fit, and would block the
	// queue behind it until it timed out.
	if limit := sat(b.snap.Total, b.cfg.Reserve); r.Needs > 0 && b.snap.Total > 0 && r.Needs > limit {
		why := fmt.Sprintf("%s needs %d MiB; the card can never give more than %d MiB", r.Engine, r.Needs/gpu.MiB, limit/gpu.MiB)
		b.counters.Refused++
		b.emit(proto.Event{Kind: proto.EvRefused, Engine: r.Engine, Bytes: r.Needs, Detail: why})
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrRefused, why)
	}
	b.nextID++
	l := &lease{
		id:      fmt.Sprintf("%s-%d", r.Engine, b.nextID),
		engine:  r.Engine,
		group:   r.Group,
		prio:    r.Priority,
		needs:   r.Needs,
		gpu:     map[ident]gpu.Proc{},
		arrived: time.Now(),
		grantCh: make(chan struct{}),
		drainCh: make(chan string, 1),
		waitCh:  make(chan string, 8),
	}
	b.queue = append(b.queue, l)
	b.log.Info("acquire", "lease", l.id, "engine", l.engine, "priority", l.prio, "needs_mib", l.needs/gpu.MiB)
	b.emit(proto.Event{Kind: proto.EvRequested, Lease: l.id, Engine: l.engine, Bytes: l.needs, Detail: l.prio.String()})
	b.schedule(time.Now())
	b.mu.Unlock()

	wait := r.Wait
	if wait <= 0 {
		wait = b.cfg.DefaultWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-l.grantCh:
			return &Lease{b: b, l: l}, nil
		case reason := <-l.waitCh:
			if onWait != nil {
				onWait(reason)
			}
		case <-ctx.Done():
			return nil, b.abandon(l, ctx.Err().Error())
		case <-timer.C:
			// A grant may have landed in the same instant, possibly after a
			// holder was already drained for it; keep it rather than waste the drain.
			select {
			case <-l.grantCh:
				return &Lease{b: b, l: l}, nil
			default:
			}
			return nil, b.abandon(l, fmt.Sprintf("gave up after %s", wait))
		}
	}
}

// abandon takes a waiter out of the queue, unless it was granted in the
// meantime, in which case the grant is handed straight back.
func (b *Broker) abandon(l *lease, why string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, q := range b.queue {
		if q == l {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			break
		}
	}
	if l.state != waiting {
		l.state = releasing
		l.released = time.Now()
		delete(b.leases, l.id)
	}
	b.schedule(time.Now())
	if l.lastWait != "" {
		why += ": " + l.lastWait
	}
	b.log.Warn("refused", "lease", l.id, "engine", l.engine, "reason", why)
	b.counters.Refused++
	b.emit(proto.Event{Kind: proto.EvRefused, Lease: l.id, Engine: l.engine, Detail: why})
	return fmt.Errorf("%w: %s", ErrRefused, why)
}

// Status is a copy of the broker's view: the shape /v1/status serves.
func (b *Broker) Status() proto.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	st := proto.Status{
		API:         proto.APIVersion,
		Version:     b.cfg.Version,
		GPUs:        []proto.GPU{},
		Leases:      []proto.Lease{},
		Waiting:     []proto.Lease{},
		Squatters:   []proto.Squatter{},
		Unprotected: []proto.Run{},
		Counters:    b.counters,
		LastEvent:   b.seq,
		Priority:    b.priorityStatus(now),
	}
	if b.probeErr != nil {
		st.ProbeErr = b.probeErr.Error()
	}
	devs := b.snap.Devices
	if len(devs) == 0 && b.snap.Total > 0 {
		devs = []gpu.Device{{Index: b.snap.Index, Name: b.snap.Name, Total: b.snap.Total, Used: b.snap.Used, Free: b.snap.Free,
			Util: -1, Temp: -1, Power: -1, PowerLimit: -1, Fan: -1, ClockSM: -1, ClockMem: -1, PCIeGen: -1, PCIeWidth: -1}}
	}
	for _, d := range devs {
		g := proto.GPU{Index: d.Index, Name: d.Name, Total: d.Total, Used: d.Used, Free: d.Free, Age: now.Sub(b.snap.At).Seconds(),
			Util: d.Util, Temp: d.Temp, Power: d.Power, PowerLimit: d.PowerLimit, Fan: d.Fan,
			ClockSM: d.ClockSM, ClockMem: d.ClockMem, PCIeGen: d.PCIeGen, PCIeWidth: d.PCIeWidth}
		if d.Index == b.snap.Index {
			g.Scheduled, g.Baseline = true, b.snap.Baseline()
		}
		st.GPUs = append(st.GPUs, g)
	}
	view := func(l *lease, since time.Time) proto.Lease {
		pids := make([]int, 0, len(l.gpu))
		for id := range l.gpu {
			pids = append(pids, id.pid)
		}
		sort.Ints(pids)
		return proto.Lease{
			ID: l.id, Engine: l.engine, Priority: l.prio.String(), State: l.state.String(),
			Needs: l.needs, PID: l.pid, PIDStart: l.pidStart, GPUPIDs: pids, Used: l.used(),
			Contested: l.contested, Age: now.Sub(since).Seconds(),
		}
	}
	for _, l := range b.holders() {
		st.Leases = append(st.Leases, view(l, l.granted))
	}
	for _, l := range b.queue {
		st.Waiting = append(st.Waiting, view(l, l.arrived))
	}
	for _, s := range b.squatters {
		st.Squatters = append(st.Squatters, proto.Squatter{
			PID: s.proc.PID, PIDStart: s.id.start, Name: s.proc.Name, Used: s.proc.Used,
			Run: s.run, Age: now.Sub(s.seen).Seconds(),
		})
	}
	sort.Slice(st.Squatters, func(i, j int) bool { return st.Squatters[i].PID < st.Squatters[j].PID })
	for _, r := range b.runs {
		st.Unprotected = append(st.Unprotected, proto.Run{Engine: r.Engine, PID: r.PID, Reason: r.Reason, Age: now.Sub(r.Started).Seconds()})
	}
	return st
}
