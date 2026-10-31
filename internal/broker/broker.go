// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package broker owns the lease table for one GPU. It decides who gets the
// card, asks holders to drain, attributes every GPU process to a lease, and
// reports the ones nobody leased. It never kills anything.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/T-Crypt/berth/internal/eventlog"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/swapcost"
)

type Priority int

const (
	Batch Priority = iota
	Interactive
	Foreground
	// Hold outranks every engine: a game, a call, an app the operator
	// pinned. Holds are pinned too, so nothing at any priority drains one.
	Hold
)

func (p Priority) String() string {
	switch p {
	case Batch:
		return "batch"
	case Foreground:
		return "foreground"
	case Hold:
		return "hold"
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
	case "hold":
		return Hold, nil
	}
	return Interactive, fmt.Errorf("unknown priority %q (batch, interactive, foreground, hold)", s)
}

type state int

const (
	waiting state = iota
	active
	draining  // asked to give the berth back
	parking   // the runner is calling the engine's park adapter
	parked    // parked in RAM; only its residual stays on the card
	releasing // connection gone; waiting for its GPU processes to let go
	stuck     // released, but its processes still hold memory past ReleaseGrace
)

func (s state) String() string {
	return [...]string{"waiting", "active", "draining", "parking", "parked", "releasing", "stuck"}[s]
}

// Reporter hears about squatters (GPU processes no lease accounts for)
// and alert-level crossings; it only ever reports, never touches a process.
type Reporter interface {
	Squatter(appeared bool, card string, p gpu.Proc)
	// Alert hears a threshold or danger level crossing (warn/crit) from
	// the alert evaluator; like Squatter it only ever reports.
	Alert(level, key, message, card string)
}

type Config struct {
	// Card is the card's name on its node (gpu0, gpu1), stamped on every
	// event this broker emits. Events and IDs are the node's event stream and
	// lease number counter, shared by every card's broker so a machine has
	// one ordered stream and no two leases share an ID. Each defaults to the
	// broker's own, as for a node with one card.
	Card   string
	Events *eventlog.Log
	IDs    *atomic.Uint64
	// Seq is the node's arrival order, shared like IDs: every card orders
	// its queue by it, so a lease on several cards stands in the same
	// place on each of them. Defaults to the broker's own.
	Seq *atomic.Uint64

	Poll         time.Duration // how often the card is read
	Reserve      uint64        // bytes never handed out
	APUHeadroom  uint64        // minimum host MemAvailable for APU admission; 0 = 2 GiB
	ReleaseGrace time.Duration // how long a released lease's processes may linger before it is reported stuck
	DefaultWait  time.Duration // how long Acquire waits when the request names no limit
	Allow        []string      // process-name substrings that are never squatters
	RunsDir      string        // where berth run records unprotected runs; "" = none
	// CardAddr is the governed card's PCI address in sysfs form when
	// --gpu named it, so a card that is in a VM at start is found by its
	// own address, not by a scan that cannot choose between two.
	CardAddr string
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
	// Turns is the round-robin policy (maximum turn, rotation, preemption);
	// the zero value is off.
	Turns Turns
	// UsageFile keeps the card-hours per engine across restarts; "" = memory only.
	UsageFile string
	// FootprintFile keeps the VRAM footprints measured per engine across
	// restarts; "" = memory only.
	FootprintFile string
	// SwapCost is the book of measured switch times the broker may price
	// starts from, for the start estimate in the wait reasons; nil = no
	// estimate, the reasons carry the place only.
	SwapCost *swapcost.Book
	// Busy reports whether a lease's engine is serving now (a request in
	// flight, queued work, a model loading). Only a busy holder can be in a
	// juggle; an idle one hands the card over at once. nil, or known=false,
	// counts as busy.
	Busy func(key string) (busy, known bool)
	// Comfy are the node's configured ComfyUI engines (run name, base URL).
	// When an unprotected run of one squats the card, a waiter asks it to
	// unload its models through its own /free and waits for the memory,
	// instead of granting a load onto weights that can only die. Empty = off.
	Comfy []ComfyEngine
	// ReattachWindow is how long a starting daemon holds new grants while
	// engines that lost the previous daemon mid-run are still alive and
	// due to re-attach.
	ReattachWindow time.Duration
	// Park admission and eviction, from [park]. ParkFloor is the RAM a
	// park must leave free, the park margin plus the gateway's headroom
	// floor, in bytes; ParkMax caps what all parked engines may hold
	// together; ParkTTL is how long a park lasts; ParkResidualMax is how
	// much card memory a parked engine may keep and still count as
	// parked; ParkPressure is the memory-pressure "some avg10" figure
	// that evicts a parked engine. A zero keeps the default.
	ParkFloor       uint64
	ParkMax         uint64
	ParkTTL         time.Duration
	ParkResidualMax uint64
	ParkPressure    float64
	// MemAvailable reads the kernel's available RAM; nil = the real
	// reading. MemPressure reads the same file's pressure figure; nil =
	// the real reading, 0 when it cannot be read. Tests fake them.
	MemAvailable func() uint64
	MemPressure  func() float64
	// Ledger is the machine's parking RAM, shared by every card's broker;
	// nil = this card's parks only.
	Ledger *park.Ledger
	// ProcTable lists every process on the machine (procs.List on Linux):
	// pid, parent, start time, name. Each tick the broker records every
	// descendant of an attached lease under that lease, so a child that
	// escapes before any GPU poll is still the lease's. nil = no table.
	ProcTable func() []procs.Proc
	// Containers reads a process's runtime container id (procs.Container
	// on Linux); "" = a host process. Tests fake it. A GPU process whose
	// container is one an attached lease names belongs to that lease.
	Containers func(pid int) (string, bool)
	// Scopes reads a process's Berth scope (procs.Scope on Linux): the
	// path of the berth-*.scope cgroup it runs in, "" for a process
	// outside one. Tests fake it. A GPU process in the scope an attached
	// lease recorded belongs to that lease.
	Scopes func(pid int) (string, bool)
	// ScopeMem reads a Berth scope's cgroup memory (procs.ScopeMem on
	// Linux). A parked engine holds its scope's anon plus shmem in RAM,
	// far less than its VRAM footprint when its weights sit in page
	// cache. nil = procs.ScopeMem; tests fake it.
	ScopeMem func(scope string) (procs.Mem, bool)
	// ContainerName resolves a container id to its runtime name (docker,
	// then podman, inspect), or a Proxmox LXC id to its hostname; "" when
	// none answers. Used to name a stowaway's container in the report;
	// never affects scheduling.
	ContainerName func(id string) string
	Version       string
	Reporter      Reporter
	Log           *slog.Logger
}

type Request struct {
	Engine   string
	Group    string // the engine family a lease counts for (llama-swap's models); "" = Engine
	Priority Priority
	Needs    uint64 // bytes; 0 = the whole card
	Wait     time.Duration
	// Pinned: never a drain victim. A hold (berth hold, a game claim) or an
	// app that must not be stopped (berth run --pinned) keeps the card until
	// it lets go; waiters are told who pins it.
	Pinned bool
	// Switched: a person asked for this engine (the wake page's Launch,
	// /v1/berth/switch). It is the last switch, so it never loses a
	// juggle, and inside the juggle window a request no person made does
	// not take the card back from it.
	Switched bool
	// Parked: a re-attach of an engine whose weights are parked in RAM. Its
	// lease comes back parked, so a waiter builds beside its residual and a
	// resume wakes it, as before the daemon restarted.
	Parked bool
	// Lend: the engine was started with lend calls (its entry says
	// lend = "hook"), so this lease is asked to give part of the card to a
	// sized waiter that does not fit, instead of being drained for one.
	Lend bool
	// WholeMachine: the engine needs the machine to itself (its entry says
	// whole_machine = true). Nothing parks while it waits, and it is never
	// granted beside a parked engine.
	WholeMachine bool
	// LendID and LentBytes: a re-attach of a holder the last daemon had
	// confirmed a lend for. The lease comes back `lent` with that ticket
	// and those bytes, counting its budget less the lend until the card
	// has room and the engine takes it back.
	LendID    string
	LentBytes uint64
	// MinTurn is a minimum turn: while the lease is younger than it after
	// the grant, no waiter drains it, whatever the waiter's priority,
	// except a hold (a game, a call, the operator's own berth hold). When
	// the turn ends the normal rules apply. A re-attach starts a fresh
	// turn. 0 = off.
	MinTurn time.Duration
	// ParkTTL is how long the lease may stay parked before the broker
	// stops its engine, in place of the node's ParkTTL. 0 = the node's.
	ParkTTL time.Duration
	// Containers are the containers the engine runs in: Acquire takes the
	// names the operator gave (--container), the attach resolves the full
	// IDs, a Compose engine's whole project arriving as one set. A GPU
	// process inside any of them belongs to the lease.
	Containers []string
}

// ident is a process identity. PIDs are reused, so a PID only means the same
// process while its start time matches.
type ident struct {
	pid   int
	start uint64
}

type lease struct {
	// gang is the lease's shares on its other cards, nil for a lease on
	// one card. A share is never granted alone (see AcquireAll).
	gang     *gang
	id       string
	engine   string
	group    string
	lostTo   string // a juggle this waiter lost: the winner's lease id; it waits, never drains it
	lostWhy  string
	reattach bool // re-attached after a daemon restart: granted is the re-attach, not a fresh switch
	pinned   bool // never drained (Request.Pinned)
	switched bool // a person's switch (Request.Switched)
	minTurn  time.Duration
	parkTTL  time.Duration // 0 = the node's
	prio     Priority
	needs    uint64
	state    state
	pid      int
	pidStart uint64
	// goneTicks counts the consecutive ticks that saw the attached pid
	// gone (no process with its start time); the reaper needs two, so a
	// pid lookup hiccup on one tick cannot free a live lease.
	goneTicks uint64
	// containers are the full IDs of the runtime containers the engine
	// runs in (empty for a host process).
	containers []string
	// scope is the berth-*.scope path the attached engine runs in
	// ("" when it runs outside one). A cgroup is inherited on fork, so
	// every process under that path is the lease's, however its process
	// tree is shaped.
	scope              string
	gpu                map[ident]gpu.Proc
	desc               map[ident]struct{} // descendants recorded while the lease ran
	contested          uint64             // squatter bytes on the card when granted
	peak               uint64             // most memory its processes were measured holding
	ram                uint64             // parking or parked: its scope's anon + shmem at the last read; 0 = no reading
	ramLearned         bool               // this park's reading is recorded as the engine's park cost
	ramHot             uint64             // its scope's anon + shmem when the park was admitted, before any of it moved
	releaseBaseline    uint64             // guarded probes: unattributed driver baseline before this lease loaded
	releaseGTTBaseline uint64             // APU shared pool must return independently
	contestedGTT       uint64
	arrived            time.Time
	granted            time.Time
	released           time.Time
	// releasedRead and parkedRead are the card reads started when the lease
	// was released or its park call was done: only a tick whose read is
	// later frees it or marks it parked. Clock-free, so a coarse clock
	// (Windows, ~15 ms) cannot put a read before the release in its quantum.
	releasedRead uint64
	parkedRead   uint64
	// arrivedN, grantedN and parkedN order the lease among the others by
	// the broker's own count, not the clock: on a coarse clock (Windows)
	// two leases granted in one quantum would otherwise tie, and which one
	// is older, and so drained first, would be arbitrary.
	arrivedN, grantedN, parkedN uint64
	// parkedAt is when the lease became parked; parkedCall is when the
	// runner said its park call was done (the parked transition waits for
	// a later card read). resumed marks a waiting lease that came from
	// parked: its grant goes out on resumeCh, not grantCh. beside is the
	// note a beside-parked grant carries, read at the grant.
	parkedAt   time.Time
	parkedCall time.Time
	resumed    bool
	beside     string

	// The lend sub-state (docs/design.md): at most one open lend per holder,
	// and never a park at the same time. lentBytes is what the engine said it
	// gave; it counts against the card no more only once a card read newer
	// than lentRead (b.reads when the engine answered) shows the memory free.
	// lendAsked is when the ask went out and lentAt when the engine answered,
	// for rule 4's grace (lendOverdue); noLend names the
	// waiter a lend failed for, so that waiter drains instead of asking the
	// same holder again; lendRetry is the last return ask, so a retry waits a
	// second instead of calling the engine every tick.
	lend      lendState
	lentBytes uint64
	lentID    string
	lentTo    *lease
	lentRead  uint64
	lendAsked time.Time
	lentAt    time.Time
	noLend    *lease
	lendRetry time.Time
	// lendCap is the run's declaration that its engine has lend calls
	// (Request.Lend); the broker never asks a lease that did not declare it.
	lendCap bool
	// wholeMachine is Request.WholeMachine.
	wholeMachine bool

	grantCh  chan struct{}
	resumeCh chan struct{}
	drainCh  chan string
	waitCh   chan string
	// lendAsk and returnAsk are the asks the broker made and this lease's
	// runner has not picked up yet; lendSig and returnSig wake its connection.
	// One ask of each kind per lease, so a runner that stops reading loses
	// nothing: the next ask replaces the unread one.
	lendAsk   *LendAsk
	returnAsk *ReturnAsk
	lendSig   chan struct{}
	returnSig chan struct{}
	lastWait  string
}

func (l *lease) used() uint64 {
	var n uint64
	for _, p := range l.gpu {
		n += p.Used
	}
	return n
}

func (l *lease) gttUsed() uint64 {
	var n uint64
	for _, p := range l.gpu {
		n += p.GTT
	}
	return n
}

// awaitingGrant is whether l came back from a park and still waits for the
// grant that wakes it: its weights are in RAM, so what it keeps on the card
// is only the residual the driver measures, and every admission rule of a
// parked engine is its rule too.
func (l *lease) awaitingGrant() bool {
	return l.state == waiting && l.resumed
}

// inTurn is whether l is inside its minimum turn: granted, and younger
// than the turn, by the broker's clock (a duration from the grant, not
// an order among leases).
func (l *lease) inTurn(now time.Time) bool {
	return l.minTurn > 0 && now.Sub(l.granted) < l.minTurn
}

// turnWhy is the line a waiter blocked by l's minimum turn is told.
func (l *lease) turnWhy(now time.Time) string {
	return fmt.Sprintf("%s has its minimum turn, %s left", l.engine, (l.minTurn - now.Sub(l.granted)).Round(time.Second))
}

// newLease builds the lease one request becomes: the same shape Acquire
// queues and Reattach grants at once.
func newLease(id string, r Request, now time.Time) *lease {
	return &lease{
		id:           id,
		engine:       r.Engine,
		group:        r.Group,
		prio:         r.Priority,
		needs:        r.Needs,
		pinned:       r.Pinned,
		switched:     r.Switched,
		minTurn:      r.MinTurn,
		parkTTL:      r.ParkTTL,
		lendCap:      r.Lend,
		wholeMachine: r.WholeMachine,
		gpu:          map[ident]gpu.Proc{},
		desc:         map[ident]struct{}{},
		arrived:      now,
		grantCh:      make(chan struct{}),
		resumeCh:     make(chan struct{}, 1),
		drainCh:      make(chan string, 1),
		waitCh:       make(chan string, 8),
		lendSig:      make(chan struct{}, 1),
		returnSig:    make(chan struct{}, 1),
	}
}

type squatter struct {
	id   ident
	proc gpu.Proc
	run  string // the unprotected run it descends from, if any
	// container is the runtime container the process holds memory in
	// (lowercase, full ID), "" for a host process; containerName is the
	// name a lease gave it, "" when no lease named it.
	container     string
	containerName string
	seen          time.Time
}

type Broker struct {
	cfg   Config
	probe gpu.Probe
	tree  procs.Tree
	log   *slog.Logger
	// nowFn is the broker's clock. Tests pin it to replay a coarse clock
	// quantum; with it nil the broker reads the wall clock.
	nowFn func() time.Time
	// reads counts the card reads started, bumped before each probe read.
	reads atomic.Uint64

	mu       sync.Mutex
	snap     gpu.Snapshot
	probeErr error
	// cardAddr is the governed card's PCI function in sysfs form
	// ("0000:01:00.0"), remembered from the last successful snapshot and
	// found by scan when the probe fails and no reading ever came.
	// cardState is "" normal, CardPassthrough or CardDetaching, and
	// cardDriver is the driver bound at the last look.
	cardAddr   string
	cardState  string
	cardDriver string
	// squatClamped is whether the squatter total was last capped at the
	// card's used figure (boundSquat); a change of that state logs once.
	squatClamped bool
	leases       map[string]*lease // granted: active, draining, releasing
	queue        []*lease
	squatters    map[ident]*squatter
	// containerNames caches container id -> runtime name ("" while unknown
	// or resolving); containerAsked marks the ids already sent to resolve.
	containerNames map[string]string
	containerAsked map[string]bool
	// comfyPoked is the last unload ask per ComfyUI engine, rate limiting
	// comfyPoke while a waiter is blocked.
	comfyPoked map[string]time.Time
	runs       []runs.Record
	started    time.Time

	counters proto.Counters
	// saved is the board as last written to cfg.CountersFile; saveMu keeps
	// two writers off the file, saveFailed logs a failing write once.
	saved      proto.Counters
	saveMu     sync.Mutex
	saveFailed bool

	verdicts map[string]verdict // loser engine key -> the juggle it lost
	// lastKey is the engine key of the last grant, and preemptedAt the last
	// time a waiter cut a minimum turn short (Turns).
	lastKey     string
	preemptedAt time.Time
	// tracked is the claim watcher's last scan: a process (and every child)
	// a track claim names. Its card memory is the claim's, never a stowaway.
	tracked    map[int]string
	claimed    map[ident]claimedProc // this tick's card memory under tracked processes
	claims     []proto.Claim
	usage      usage
	usageAt    time.Time // last tick credited to usage
	usageSaved time.Time
	usageDirty bool
	// footprints is the VRAM each engine's leases held at their peak, per
	// engine key (group, else name): an estimate of what a run needs, shown
	// as EST. VRAM, never a reservation. The save state keeps the file's
	// writes out of the tick's lock, like saved does for the board.
	footprints     footprints
	footprintDirty bool
	footSaveMu     sync.Mutex
	footSaveFailed bool
}

type claimedProc struct {
	claim string
	proc  gpu.Proc
}

// SetClaims takes the claim watcher's latest scan: the processes track
// claims name (pid -> claim) and the state of every rule, for Status.
func (b *Broker) SetClaims(tracked map[int]string, states []proto.Claim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tracked, b.claims = tracked, states
}

// trackedBy is the track claim a process belongs to, if any: the process
// itself or an ancestor is one the watcher matched.
func (b *Broker) trackedBy(pid int) string {
	if name, ok := b.tracked[pid]; ok {
		return name
	}
	for root, name := range b.tracked {
		if procs.DescendsFrom(b.tree, pid, root) {
			return name
		}
	}
	return ""
}

func (b *Broker) claimedBytes() uint64 {
	var n uint64
	for _, c := range b.claimed {
		n += c.proc.Used
	}
	return n
}

// EventRing is how many events berthd keeps in memory.
const EventRing = eventlog.Ring

// emit records an event and fans it out. Called with b.mu held. A watcher
// too slow to keep up is cut off rather than allowed to stall scheduling;
// it reconnects with Since and loses nothing still in the ring.
func (b *Broker) emit(e proto.Event) {
	e.At, e.Card = b.now(), b.cfg.Card
	b.cfg.Events.Emit(e)
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
	return b.cfg.Events.Events(since, watch)
}

// order is the next number in the node's arrival sequence (Config.Seq).
func (b *Broker) order() uint64 {
	return b.cfg.Seq.Add(1)
}

// now is the broker's clock. It reads the pinned b.nowFn in tests, and the
// wall clock otherwise, so a test can replay a coarse clock quantum.
func (b *Broker) now() time.Time {
	if b.nowFn != nil {
		return b.nowFn()
	}
	return time.Now()
}

func New(cfg Config, probe gpu.Probe, tree procs.Tree) *Broker {
	if cfg.Events == nil {
		cfg.Events = eventlog.New()
	}
	if cfg.IDs == nil {
		cfg.IDs = &atomic.Uint64{}
	}
	if cfg.Seq == nil {
		cfg.Seq = &atomic.Uint64{}
	}
	if cfg.APUHeadroom == 0 {
		cfg.APUHeadroom = 2 * gpu.GiB
	}
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
	if cfg.ProcTable == nil {
		cfg.ProcTable = procs.List
	}
	if cfg.MemAvailable == nil {
		cfg.MemAvailable = func() uint64 { return host.MemAvailable("") }
	}
	if cfg.ScopeMem == nil {
		cfg.ScopeMem = procs.ScopeMem
	}
	if cfg.MemPressure == nil {
		cfg.MemPressure = func() float64 {
			p, _ := host.MemPressure("")
			return p
		}
	}
	if cfg.ParkFloor == 0 {
		// The park margin's default (4 GiB) plus the gateway headroom
		// floor's default (2 GiB); the daemon wiring passes the real
		// figures when they differ.
		cfg.ParkFloor = 6 << 30
	}
	if cfg.ParkTTL == 0 {
		cfg.ParkTTL = 30 * time.Minute
	}
	if cfg.ParkResidualMax == 0 {
		cfg.ParkResidualMax = 2 << 30
	}
	if cfg.ParkPressure == 0 {
		cfg.ParkPressure = 10.0
	}
	if cfg.Containers == nil {
		cfg.Containers = procs.Container
	}
	if cfg.Scopes == nil {
		cfg.Scopes = procs.Scope
	}
	if cfg.ContainerName == nil {
		cfg.ContainerName = containerNameCLI
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
	foots := footprints{}
	if cfg.FootprintFile != "" {
		if f, err := loadFootprints(cfg.FootprintFile); err == nil {
			foots = f
		} else if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("engine footprints not restored; the estimates start empty", "file", cfg.FootprintFile, "err", err)
		}
	}
	return &Broker{
		cfg: cfg, probe: probe, tree: tree, log: log, runs: rs,
		cardAddr:       cfg.CardAddr,
		leases:         map[string]*lease{},
		squatters:      map[ident]*squatter{},
		containerNames: map[string]string{},
		containerAsked: map[string]bool{},
		comfyPoked:     map[string]time.Time{},
		counters:       counters,
		saved:          counters,
		verdicts:       map[string]verdict{},
		usage:          use,
		footprints:     foots,
		started:        time.Now(),
	}
}

// Run reads the card every Poll until ctx ends.
func (b *Broker) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.Poll)
	defer t.Stop()
	defer b.persist() // counts made after the last tick
	defer b.persistFootprints()
	defer func() { b.persistUsage(b.now(), true) }()
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
	defer b.persistFootprints()
	defer func() { b.persistUsage(b.now(), false) }()
	read := b.reads.Add(1)
	b.mu.Lock()
	held := b.vfioHold()
	b.mu.Unlock()
	if p, ok := b.probe.(gpu.Pauser); ok {
		// A probe that keeps nvidia-smi running would hold the card open
		// through the driver switch, so it stops for the whole hold.
		p.Pause(held != nil)
	}
	var snap gpu.Snapshot
	var err error
	if held == nil {
		// While a VFIO hold is granted the operator is switching the card,
		// and the probe opens the device; it does not run at all.
		snap, err = b.probe.Read(ctx)
		if err == nil {
			for _, p := range snap.Procs {
				if start := b.tree.StartTime(p.PID); p.Start != 0 && start != 0 && start != p.Start {
					err = fmt.Errorf("%w: pid %d changed identity after GPU scan", gpu.ErrUnsafeRead, p.PID)
					break
				}
			}
		}
	}
	var rs []runs.Record
	if b.cfg.RunsDir != "" {
		rs = runs.List(b.cfg.RunsDir, b.tree.StartTime)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() { b.cfg.Ledger.Set(b.cfg.Card, b.parkTotals()) }() // runs before the unlock
	now := b.now()
	b.runs = rs
	b.accountUsage(now)
	b.pruneVerdicts(now)
	if held != nil {
		// The switch is berthd's to watch, not to touch: read the driver
		// from sysfs, grant nothing, keep the hold HOT.
		b.holdTick()
		return
	}
	if err == nil && snap.Guarded && (snap.At.IsZero() || snap.MaxAge <= 0 || now.Sub(snap.At) > snap.MaxAge) {
		err = fmt.Errorf("%w: GPU sample is stale", gpu.ErrUnsafeRead)
	}
	if err != nil {
		if d := b.vfioBound(); d != "" {
			// The card is in a VM, switched by hand: no probe-failed
			// warning, the card state says it instead.
			b.setCard(proto.CardPassthrough, d)
			b.schedule(now)
			return
		}
		if b.probeErr == nil {
			b.log.Warn("gpu probe failed", "err", err, "admission_blocked", b.snap.Guarded || errors.Is(err, gpu.ErrUnsafeRead))
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
	if a := gpu.SysfsAddr(gpu.BusID(snap)); a != "" {
		b.cardAddr = a
	}
	if b.cardState != "" {
		// The probe answers again, so the card is back; the driver it reads
		// now is the one the returned event names.
		b.setCard("", gpu.DriverFor(b.cardAddr))
	}
	b.snap = snap
	b.recordDescendants()
	b.refreshScopes()

	present := map[ident]bool{}
	claimed := map[ident]claimedProc{}
	b.retainUnobserved(snap.Unobserved, present, claimed)
	for _, p := range snap.Procs {
		id := b.identify(p.PID)
		if p.Start != 0 {
			id = ident{pid: p.PID, start: p.Start}
		}
		present[id] = true
		scope := b.scopeOf(p.PID)
		if l := b.owner(id, p, scope); l != nil {
			if l.state != active && l.state != draining && b.runFor(p.PID) != nil {
				// A released lease keeps blocking while its processes hold
				// memory, unless a run record says they were left behind on
				// purpose (a keep-up drain): those processes fall through to
				// the unprotected-run listing and no longer hold the card.
				delete(l.gpu, id)
			} else {
				l.gpu[id] = p
				if scope != "" && l.scope == scope {
					// In the lease's scope but never recorded: a child that
					// escaped the tree before any tick. Record it now, so
					// reclaim and the stuck report name it exactly like a
					// recorded descendant.
					l.desc[id] = struct{}{}
				}
				l.peak = max(l.peak, l.used())
				if s, ok := b.squatters[id]; ok {
					b.reportGone(s)
				}
				continue
			}
		}
		if b.allowed(p.Name) {
			continue
		}
		if name := b.trackedBy(p.PID); name != "" {
			claimed[id] = claimedProc{claim: name, proc: p}
			if _, ok := b.squatters[id]; ok {
				// Seen before the watcher's first scan named it: it was never a stowaway.
				delete(b.squatters, id)
				b.log.Info("squatter is claimed", "pid", p.PID, "name", p.Name, "claim", name)
			}
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
		if cid := b.containerOf(p.PID); cid != "" {
			s.container = cid
			id := id
			b.containerLabel(cid, func(name string) {
				if s, ok := b.squatters[id]; ok && s.container == cid {
					s.containerName = name
				}
			})
		}
		b.squatters[id] = s
		detail := p.Name
		if s.container != "" {
			detail += fmt.Sprintf(" in container %s", short(s.container))
			if s.containerName != "" {
				detail += " (" + s.containerName + ")"
			}
		}
		b.log.Warn("squatter: GPU memory held without a lease", "pid", p.PID, "name", p.Name, "mib", p.Used/gpu.MiB, "gtt_mib", p.GTT/gpu.MiB, "unprotected_run", s.run, "container", short(s.container))
		b.counters.SquattersCaught++
		b.emit(proto.Event{Kind: proto.EvSquatter, PID: p.PID, Bytes: p.Used, Engine: s.run, Detail: detail})
		if b.cfg.Reporter != nil {
			b.cfg.Reporter.Squatter(true, b.cfg.Card, p)
		}
	}
	for id, s := range b.squatters {
		if !present[id] {
			b.reportGone(s)
		}
	}
	for key, l := range b.leases {
		for id, p := range l.gpu {
			// A releasing lease lets go of the WSL VM's row: the VM outlives
			// the containers, so the row would hold the lease forever, and as
			// a stowaway it still counts against the card.
			if !present[id] || ((l.state == releasing || l.state == stuck) && isWSLVM(p.Name)) {
				delete(l.gpu, id)
			}
		}
		if l.state == releasing || l.state == stuck {
			if len(l.gpu) == 0 && read > l.releasedRead && b.driverReleased(l) {
				// Only a tick whose card read started after the release frees
				// the lease: an earlier read knows nothing about a process that
				// appeared after it. Read order, not the clock, decides.
				b.log.Info("lease freed", "lease", key, "engine", l.engine)
				b.counters.VRAMHandedOff += l.peak
				b.foldFootprint(l, now)
				b.emit(proto.Event{Kind: proto.EvFreed, Lease: key, Engine: l.engine, Bytes: l.peak})
				b.lendFreed(l) // a holder lending to this lease takes its memory back now
				delete(b.leases, key)
			} else if l.state == releasing && now.Sub(l.released) > b.cfg.ReleaseGrace {
				// Never grant on process exit alone: the lease keeps blocking
				// until the card shows its memory gone, and waiters are told why.
				l.state = stuck
				b.log.Warn("lease stuck: memory release not confirmed", "lease", key, "engine", l.engine, "holding", b.guardedDetail(l))
				b.counters.StuckCaught++
				b.emit(proto.Event{Kind: proto.EvStuck, Lease: key, Engine: l.engine, Bytes: l.used(), Detail: b.guardedDetail(l)})
			}
			continue
		}
		// The remaining granted leases (active, draining, parking, parked)
		// get the reaper's pass: a run whose attached process is gone gives
		// its lease back without the wrapper's disconnect. One the reaper
		// released is on its way out, so it skips the park tick.
		b.reapAttached(key, l, now)
		switch l.state {
		case parking:
			b.parkingTick(key, l, now, read)
		case parked:
			b.parkedTick(key, l, now)
		}
		// A lending holder stays active, so its lend is watched here: the
		// card read that confirms the memory free, and the grace after which
		// the waiter falls back to park or drain.
		b.lendTick(key, l, now, read)
		b.measureParkedRAM(l)
	}
	b.claimed = claimed
	b.evictParked(now)
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
		b.cfg.Reporter.Squatter(false, b.cfg.Card, s.proc)
	}
}

// owner finds the lease a GPU process belongs to: one it was already
// attributed to, else a recorded descendant of an attached lease, else a
// scope the lease recorded, else a lease that names its container, else the
// WSL VM row of the one container lease on the card, else the lease whose
// wrapped engine it descends from. The engine's PID only counts while its
// start time still matches, so a reused PID never inherits a lease. An empty
// scope never matches an empty one.
func (b *Broker) owner(id ident, p gpu.Proc, scope string) *lease {
	container := b.containerOf(p.PID)
	for _, l := range b.leases {
		if _, ok := l.gpu[id]; ok {
			return l
		}
	}
	for _, l := range b.leases {
		if _, ok := l.desc[id]; ok {
			return l
		}
	}
	if l := b.scopeLease(scope); l != nil {
		return l
	}
	if container != "" {
		for _, l := range b.leases {
			if l.ownsNew() && l.hasContainer(container) {
				return l
			}
		}
	}
	if l := b.wslVMLease(p.Name); l != nil {
		return l
	}
	for _, l := range b.leases {
		if l.ownsNew() && l.pid > 0 &&
			b.tree.StartTime(l.pid) == l.pidStart && procs.DescendsFrom(b.tree, id.pid, l.pid) {
			return l
		}
	}
	return nil
}

// ownsNew is whether a GPU process the lease has not recorded yet can be
// its own: a lease on the card, parked ones included, whose engine keeps
// running (a re-attach starts with no processes recorded).
func (l *lease) ownsNew() bool {
	switch l.state {
	case active, draining, parking, parked, releasing:
		return true
	}
	return false
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
		part := fmt.Sprintf("pid %d (%s) %d MiB", p.PID, p.Name, p.Used/gpu.MiB)
		if p.GTT > 0 {
			part += fmt.Sprintf(" + %d MiB GTT", p.GTT/gpu.MiB)
		}
		parts = append(parts, part)
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
	if b.cardState != "" {
		// The card does not serve the node, so nothing new is granted, and
		// every waiter reads the same reason. The VFIO hold itself keeps
		// the card.
		b.notifyWait(b.queue, b.cardWhy())
		return
	}
	if why := b.observationWait(now); why != "" {
		b.notifyWait(b.queue, why)
		return
	}
	b.sortQueue()
	for len(b.queue) > 0 {
		w := b.queue[0]
		// A fresh daemon does not yet know what engines that ran under the
		// last one hold; granting now could put a second engine on a card
		// that is already full. The Tick after the window lifts this.
		if n := b.reattaching(now); n > 0 {
			b.notifyWait(b.queue, fmt.Sprintf("berthd restarted; waiting up to %s for %d running engine(s) to re-attach",
				(b.cfg.ReattachWindow-now.Sub(b.started)).Round(time.Second), n))
			return
		}
		ok, reason, victims := b.evaluate(w, now)
		if ok && b.snap.APU {
			if why := b.apuAdmissionWait(); why != "" {
				ok, reason = false, why
			}
		}
		if ok && w.gang != nil {
			// A share of a lease on several cards fits here. It takes
			// nothing until every card fits; its gang grants them all
			// at once. Until then it keeps its place at the head.
			b.setWait(w, fmt.Sprintf("fits on %s; waiting for its other cards", b.cfg.Card))
			b.notifyWait(b.queue[1:], fmt.Sprintf("next in line: %s waits for its other cards", w.engine))
			w.gang.poke()
			return
		}
		if ok {
			b.grant(w, now)
			continue
		}
		for _, v := range victims {
			if w.prio != Hold && v.inTurn(now) {
				b.notePreemption(now)
			}
			v.state = draining
			// Its memory goes with the drain: an open lend is dropped, and
			// never asked back, since the borrower still holds it (rule 2).
			b.clearLend(v, "it is draining")
			msg := fmt.Sprintf("%s (%s) needs the card", w.engine, w.prio)
			select {
			case v.drainCh <- msg:
			default:
			}
			b.log.Info("draining", "lease", v.id, "engine", v.engine, "for", w.engine)
			b.counters.Drains++
			b.emit(proto.Event{Kind: proto.EvDraining, Lease: v.id, Engine: v.engine, Detail: msg})
		}
		b.notifyWait(b.queue, reason)
		return
	}
}

// grant gives the card to w, a waiter at the head of the queue. b.mu must be
// held.
func (b *Broker) grant(w *lease, now time.Time) {
	b.queue = slices.DeleteFunc(b.queue, func(q *lease) bool { return q == w })
	w.state = active
	w.granted = now
	w.grantedN = b.order()
	b.lastKey = w.key()
	w.releaseBaseline = b.snap.Baseline()
	w.releaseGTTBaseline = b.snap.GTTBaseline()
	w.contested = b.squatBytes()
	w.contestedGTT = b.squatGTTBytes()
	b.leases[w.id] = w
	beside := w.beside
	w.beside = ""
	if w.resumed {
		w.resumed = false
		// A resume is a one-shot signal per grant, and a lease can
		// cycle park and resume many times: never close the
		// channel, and never double-send.
		select {
		case w.resumeCh <- struct{}{}:
		default:
		}
	} else {
		close(w.grantCh)
	}
	if beside != "" {
		b.log.Info("granted beside parked", "lease", w.id, "engine", w.engine, "beside", beside)
	}
	if w.contested > 0 || w.contestedGTT > 0 {
		b.log.Warn("granted on a contested card", "lease", w.id, "squatters_mib", w.contested/gpu.MiB, "squatters_gtt_mib", w.contestedGTT/gpu.MiB)
		b.counters.ContestedGrants++
	}
	if w.lastWait != "" {
		b.counters.CollisionsAvoided++
	}
	detail := fmt.Sprintf("waited %s", now.Sub(w.arrived).Round(time.Millisecond))
	if beside != "" {
		detail += "; " + beside
	}
	b.emit(proto.Event{Kind: proto.EvGranted, Lease: w.id, Engine: w.engine, Bytes: w.contested, Detail: detail})
	b.log.Info("granted", "lease", w.id, "engine", w.engine, "priority", w.prio, "needs_mib", w.needs/gpu.MiB, "waited", now.Sub(w.arrived).Round(time.Millisecond))
}

// sortQueue orders the waiters: higher priority first, then by arrival on
// the node. b.mu must be held.
func (b *Broker) sortQueue() {
	sort.SliceStable(b.queue, func(i, j int) bool {
		if b.queue[i].prio != b.queue[j].prio {
			return b.queue[i].prio > b.queue[j].prio
		}
		if len(b.cfg.Turns.Cycle) > 0 {
			if ri, rj := b.cycleRank(b.queue[i].key()), b.cycleRank(b.queue[j].key()); ri != rj {
				return ri < rj
			}
		}
		return b.queue[i].arrivedN < b.queue[j].arrivedN
	})
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

// budget is what a holder counts against the card: the card's ceiling if it
// asked for the whole card (the baseline and reserve were never its to give),
// else whichever is larger of what it asked for and what it
// holds, less the memory a confirmed lend gave to a borrower.
func (b *Broker) budget(l *lease) uint64 {
	if l.state == parking || l.state == parked || l.awaitingGrant() {
		// A parked engine cannot load anything more; what it counts for
		// is the residual it still measures on the card, never promised
		// free until the driver shows it gone. A resumed lease that is
		// still waiting for its grant is that same engine one step later:
		// its weights have not come back onto the card yet.
		return l.used()
	}
	var held uint64
	if l.needs == 0 {
		held = b.ceiling()
	} else {
		held = max(l.needs, l.used())
	}
	if l.lend == lent {
		// A confirmed lend is memory the engine handed to a borrower and
		// cannot use until it takes it back (docs/design.md rule 2), so it
		// stops counting here. What the driver still measures under this
		// holder always counts: an estimate never frees more than a reading.
		return max(l.used(), sat(held, l.lentBytes))
	}
	return held
}

// ceiling is what the card can ever hand out to leases: its total minus the
// reserve floor and the memory no lease can give up (driver baseline,
// squatters, track claims).
func (b *Broker) ceiling() uint64 {
	return sat(b.snap.Total, b.cfg.Reserve+b.snap.Baseline()+b.squatBytes()+b.claimedBytes())
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
		return hs[i].grantedN < hs[j].grantedN
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
		pin := ""
		if l.pinned {
			pin = ", pinned"
		}
		names[i] = fmt.Sprintf("%s [%s%s, %s]", l.engine, l.prio, pin, l.state)
	}
	return strings.Join(names, ", ")
}

// squatBytes is the squatter total admission counts: the sum of the
// per-process readings, capped by what the card can really hand squatters
// (boundSquat). The per-squatter entries in status and events keep the raw
// readings; only the total that drives decisions is bounded.
func (b *Broker) squatBytes() uint64 {
	var n uint64
	for _, s := range b.squatters {
		n += s.proc.Used
	}
	return b.boundSquat(n)
}

// boundSquat caps the squatter sum at the card's used figure. Per-process
// readings can overcount: on Windows the counter reports a process's
// committed reservation, which the driver can list far above what the card
// actually uses, so the SUM can exceed the card's used memory (issue #103)
// and block every sized admission forever. Used memory minus the baseline
// and the bytes the card's processes under leases and claims really hold
// is all any squatter can hold, so the sum is capped there. A zero used
// figure is unknown: the raw sum stands, as before.
func (b *Broker) boundSquat(sum uint64) uint64 {
	if b.snap.Used == 0 {
		return sum
	}
	bound := sat(b.snap.Used, b.snap.Baseline()+b.leaseBytes()+b.claimedBytes())
	if sum <= bound {
		b.squatClamped = false
		return sum
	}
	if !b.squatClamped {
		b.squatClamped = true
		b.log.Info("squatter readings exceed the card's used memory; counting the card's figure",
			"sum_mib", sum/gpu.MiB, "bound_mib", bound/gpu.MiB)
	}
	return bound
}

// leaseBytes is the card memory the processes under leases really hold:
// each lease's per-process readings, not its needs budget, so the cap keeps
// real lease memory out of a squatter's possible share of the card.
func (b *Broker) leaseBytes() uint64 {
	var n uint64
	for _, l := range b.leases {
		n += l.used()
	}
	return n
}

// cardBudget is one schedule/evaluate pass's view of the card: what the
// card holds in total, what is already spoken for, and what is left for
// the waiter's needs. evaluate branches on this, not on scattered uint64s.
type cardBudget struct {
	total    uint64
	reserve  uint64
	baseline uint64
	squat    uint64
	claimed  uint64 // card memory under track claims: real, never drained, never a stowaway
	held     uint64
}

// ceiling is what the card can ever hand out: the total minus everything
// no lease can give up (the reserve floor, driver baseline, squatters).
// avail is what is free for a new grant right now.
func (c cardBudget) ceiling() uint64 { return sat(c.total, c.reserve+c.baseline+c.squat+c.claimed) }
func (c cardBudget) avail() uint64   { return sat(c.ceiling(), c.held) }

// evaluate decides whether w fits now. If not, it returns why and which
// holders to drain; no victims means draining would not help, so w waits.
func (b *Broker) evaluate(w *lease, now time.Time) (bool, string, []*lease) {
	// A resumed lease is still in b.leases while it waits: its own
	// residual is the copy it comes back to, never a holder it waits on.
	hs := slices.DeleteFunc(b.holders(), func(h *lease) bool { return h == w })
	budget := cardBudget{
		total:    b.snap.Total,
		reserve:  b.cfg.Reserve,
		baseline: b.snap.Baseline(),
		squat:    b.squatBytes(),
		claimed:  b.claimedBytes(),
	}
	for _, h := range hs {
		budget.held += b.budget(h)
	}
	if len(hs) == 0 {
		// Nobody holds a lease, so draining helps nothing and there
		// are no victims: a sized request that does not fit the
		// unleased remainder waits head-of-queue (nobody jumps a
		// blocked waiter) until the driver shows the memory free.
		// A configured ComfyUI squatting the card is the one squatter
		// that can hand memory back on request: ask it to unload its
		// models (its own /free, applied at its job boundary) and wait
		// for the memory, instead of granting a load onto weights that
		// can only die.
		comfyBytes, comfy := b.comfySquat()
		if w.needs > 0 && budget.avail() < w.needs {
			reason := fmt.Sprintf("needs %d MiB; at most %d MiB can be freed",
				w.needs/gpu.MiB, budget.avail()/gpu.MiB)
			if budget.squat > 0 {
				reason += fmt.Sprintf("; squatters hold %d MiB", budget.squat/gpu.MiB)
			}
			if budget.claimed > 0 {
				reason += fmt.Sprintf("; claims hold %d MiB", budget.claimed/gpu.MiB)
			}
			if comfyBytes > 0 {
				b.comfyPoke(now, comfy)
				reason += "; asked ComfyUI to free its models"
			}
			return false, reason, nil
		}
		if comfyBytes > 0 && w.needs == 0 {
			b.comfyPoke(now, comfy)
			return false, fmt.Sprintf("waiting for ComfyUI to free its models (%d MiB squatted)",
				comfyBytes/gpu.MiB), nil
		}
		if b.snap.Guarded && w.needs > 0 && sat(b.snap.Free, b.cfg.Reserve) < w.needs {
			return false, "waiting for driver-confirmed free VRAM", nil
		}
		return true, "", nil
	}
	exclusive := w.needs == 0 || b.snap.Total == 0

	var blockers, pending, drainable, kept, warm []*lease
	var keptWhy string
	for _, h := range hs {
		switch {
		case h.state == parked || h.awaitingGrant():
			warm = append(warm, h)
		case h.state != active:
			// Draining, releasing, stuck, and parking: a parking engine is
			// still moving its weights off the card, so the waiter waits
			// for it to become parked (or fall back to a drain); evicting
			// it mid-park would throw the park away.
			pending = append(pending, h)
		case h.pinned || (h.prio > w.prio && !b.turnExpired(h, now)):
			// (a holder past its maximum turn no longer blocks a lower priority)
			blockers = append(blockers, h)
		case w.prio != Hold && h.inTurn(now):
			// A minimum turn: the holder keeps the card until its turn
			// ends, whatever the waiter's priority; only a hold (a game,
			// a call, the operator's own berth hold) takes it out before,
			// or a higher-priority waiter when preemption is on and its
			// gap has passed.
			ok, why := b.preempts(h, w, now)
			if ok {
				drainable = append(drainable, h)
				continue
			}
			kept = append(kept, h)
			keptWhy = h.turnWhy(now)
			if why != "" {
				keptWhy += "; " + why
			}
			continue
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
			return false, "held by " + describe(blockers), nil
		}
		if len(kept) > 0 {
			return false, keptWhy, nil
		}
		if len(drainable) > 0 {
			return false, "draining " + describe(drainable), drainable
		}
		if len(pending) > 0 {
			return false, "waiting for " + describe(pending) + " to free memory", nil
		}
		// A parked engine's residual is small enough to build a load
		// beside: when the parked leases together keep no more than
		// ParkResidualMax on the card, the whole card is granted beside
		// them, and the grant event names what it sits next to. A
		// whole-machine waiter evicts them all instead.
		var residual uint64
		for _, p := range warm {
			residual += p.used()
		}
		if len(warm) > 0 && residual <= b.cfg.ParkResidualMax && !w.wholeMachine {
			w.beside = besideLine(warm, residual)
			return true, "", nil
		}
		// Otherwise the parked leases give way first, least recently
		// parked first, and the waiter is told to wait while they stop.
		if len(warm) > 0 {
			var victims []*lease
			var evicted uint64
			for _, p := range sortParked(warm) {
				if residual-evicted <= b.cfg.ParkResidualMax && !w.wholeMachine {
					break
				}
				victims = append(victims, p)
				evicted += p.used()
			}
			keepWarm := subtract(warm, victims)
			reason := "evicting parked " + describe(victims)
			if len(keepWarm) > 0 {
				reason += "; keeping parked " + describe(keepWarm)
			}
			return false, reason, victims
		}
		// (unreachable: the buckets above partition the holders, so one
		// of them is non-empty whenever a holder exists)
		return false, "waiting for " + describe(pending) + " to free memory", nil
	}

	squat := budget.squat
	avail := budget.avail()
	if avail >= w.needs {
		if b.snap.Guarded && sat(b.snap.Free, b.cfg.Reserve) < w.needs {
			return false, "waiting for driver-confirmed free VRAM", nil
		}
		return true, "", nil
	}
	var leaving uint64
	for _, h := range pending {
		leaving += b.budget(h)
	}
	if avail+leaving >= w.needs {
		return false, "waiting for " + describe(pending) + " to free memory", nil
	}
	// Making room step 3 (docs/design.md): before a park or a drain, a holder
	// that can give the shortfall keeps serving beside the waiter. Only the
	// holders a drain would be allowed to take are asked, so priorities,
	// holds, turns and juggles still decide whether this holder gives room at
	// all; the lend only decides how.
	if why, wait := b.lendRoom(w, budget, drainable, now); wait {
		return false, why, nil
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
		reason += "; held by " + describe(blockers)
	}
	if len(kept) > 0 {
		reason += "; " + keptWhy
	}
	if squat > 0 {
		reason += fmt.Sprintf("; squatters hold %d MiB", squat/gpu.MiB)
	}
	if budget.claimed > 0 {
		reason += fmt.Sprintf("; claims hold %d MiB", budget.claimed/gpu.MiB)
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

// ContestedGTT is unleased APU shared-RAM usage at grant, separate from VRAM.
func (l *Lease) ContestedGTT() uint64 {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	return l.l.contestedGTT
}

// Attach records the engine holding the berth, by PID plus start time.
// Its Berth scope is read on the next tick, not here: systemd-run moves
// itself into the scope only after a round trip to the user manager, so
// at attach the engine still sits in its wrapper's cgroup.
func (l *Lease) Attach(pid int) {
	start := l.b.tree.StartTime(pid)
	l.b.mu.Lock()
	l.l.pid, l.l.pidStart = pid, start
	l.l.scope = ""
	l.b.mu.Unlock()
}

// reapAttached is the second, connection-independent signal that a run has
// ended: the wrapper's disconnect is the first, and it is the one that never
// arrives when the run's whole process tree is killed out from under it.
// The broker asks the process table itself. The attached process must have
// been seen gone on two consecutive ticks, so a pid lookup hiccup on one
// tick cannot free a live lease; a PID the kernel reused (same number, new
// start time) counts as gone. A lease that names a container is left alone
// (its processes belong to the container, not the PID), and so is one the
// card still counts a process under or that has a live descendant: its
// memory is not shown gone. The release then runs exactly as the wrapper's
// would. Nothing here signals a process. Called with b.mu held, from Tick.
func (b *Broker) reapAttached(key string, l *lease, now time.Time) {
	if l.pid == 0 || l.pidStart == 0 {
		l.goneTicks = 0
		return
	}
	if b.tree.StartTime(l.pid) == l.pidStart {
		l.goneTicks = 0
		return
	}
	l.goneTicks++
	if l.goneTicks < 2 || len(l.containers) != 0 || len(l.gpu) != 0 || len(l.desc) != 0 {
		return
	}
	b.log.Warn("attached process is gone; releasing the lease", "lease", key, "engine", l.engine, "pid", l.pid)
	b.emit(proto.Event{Kind: proto.EvOrphaned, Lease: key, Engine: l.engine, PID: l.pid, Detail: "attached process is gone"})
	l.goneTicks = 0
	b.beginRelease(l, now, true)
}

// beginRelease takes a granted lease off the grant path: releasing waits for
// a tick whose card read happened after now to show none of its processes.
// A lease that never recorded anything (no attached wrapper, nothing on the
// card, no container named) has nothing that could turn up later, and goes
// at once. announce keeps abandon quiet, as it has always been.
func (b *Broker) beginRelease(x *lease, now time.Time, announce bool) {
	// A resumed lease waits in the queue while it is still a lease; once
	// released it must never be granted.
	b.queue = slices.DeleteFunc(b.queue, func(q *lease) bool { return q == x })
	// A released holder's memory goes with it: an open lend is dropped, and
	// the engine is asked to take the memory back when it has a ticket.
	b.clearLend(x, "it released")
	x.state = releasing
	x.released = now
	x.releasedRead = b.reads.Load()
	nothing := x.pid == 0 && len(x.gpu) == 0 && len(x.containers) == 0 && len(x.desc) == 0
	if !announce {
		if nothing {
			delete(b.leases, x.id)
		}
		return
	}
	b.log.Info("released", "lease", x.id, "engine", x.engine, "held", now.Sub(x.granted).Round(time.Second))
	b.emit(proto.Event{Kind: proto.EvReleased, Lease: x.id, Engine: x.engine, Bytes: x.used()})
	if nothing {
		b.counters.VRAMHandedOff += x.peak
		b.emit(proto.Event{Kind: proto.EvFreed, Lease: x.id, Engine: x.engine, Bytes: x.peak})
		b.lendFreed(x)
		delete(b.leases, x.id)
	}
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
	b.beginRelease(x, b.now(), true)
	b.schedule(b.now())
}

// Reattach leases the card again to an engine that started under berth run
// and outlived its daemon. It is already on the card, so it is granted at
// once rather than queued; the identity check (PID plus start time) is what
// keeps a stranger from claiming the lease. A run names what its engine was
// in the middle of: parked weights, or an open lend with its ticket and
// bytes (Request.LendID), and the lease takes that state up.
func (b *Broker) Reattach(r Request, pid int, start uint64) (*Lease, error) {
	return b.reattach(fmt.Sprintf("%s-%d", r.Engine, b.cfg.IDs.Add(1)), r, pid, start)
}

// reattach is Reattach under a lease ID the caller picked.
func (b *Broker) reattach(id string, r Request, pid int, start uint64) (*Lease, error) {
	if pid <= 0 || start == 0 || b.tree.StartTime(pid) != start {
		return nil, fmt.Errorf("%w: pid %d is not the process that started as %s", ErrRefused, pid, r.Engine)
	}
	scope, _ := b.cfg.Scopes(pid)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snap.APU && r.Needs > 0 {
		return nil, fmt.Errorf("%w: APU requires an exclusive lease; omit needs-mib", ErrRefused)
	}
	for _, l := range b.leases {
		if l.pid == pid && l.pidStart == start {
			return nil, fmt.Errorf("%w: pid %d already holds %s", ErrRefused, pid, l.id)
		}
	}
	now := b.now()
	l := newLease(id, r, now)
	l.arrivedN = b.order()
	l.containers = containerSet(r.Containers)
	l.state, l.reattach = active, true
	l.pid, l.pidStart = pid, start
	l.scope = scope
	l.granted = now
	l.grantedN = b.order()
	l.releaseBaseline = b.snap.Baseline()
	l.releaseGTTBaseline = b.snap.GTTBaseline()
	detail := l.prio.String()
	if r.Parked {
		l.state = parked
		l.parkedAt = now
		l.parkedN = b.order()
		detail += ", parked"
	}
	if r.LendID != "" {
		// The lend was confirmed under the old daemon; this one takes up
		// the same state with no borrower, so the take-back waits on the
		// card having room (lendTick, case lent).
		l.lend, l.lentID, l.lentBytes = lent, r.LendID, r.LentBytes
		detail += ", lent"
	}
	close(l.grantCh)
	b.leases[l.id] = l
	b.log.Info("reattached", "lease", l.id, "engine", l.engine, "priority", l.prio, "pid", pid, "parked", r.Parked, "lending", l.lend.String())
	b.emit(proto.Event{Kind: proto.EvReattached, Lease: l.id, Engine: l.engine, PID: pid, Detail: detail})
	b.schedule(now)
	return &Lease{b: b, l: l}, nil
}

// ErrRefused wraps every reason Acquire gives up.
var ErrRefused = errors.New("refused")

// notFound is an answer that this card has no such engine or lease, as
// opposed to one it has but cannot act on. A node with several cards asks
// each in turn and takes the first that is not notFound (IsNotFound).
type notFound string

func (e notFound) Error() string { return string(e) }

// EventLog is the event stream this broker writes to, and IDs the counter its
// lease numbers come from: the node's when it was built with them.
func (b *Broker) EventLog() *eventlog.Log { return b.cfg.Events }

// IDs is the counter this broker's lease numbers come from.
func (b *Broker) IDs() *atomic.Uint64 { return b.cfg.IDs }

// IsNotFound is whether err says the card has no such engine or lease.
func IsNotFound(err error) bool {
	var nf notFound
	return errors.As(err, &nf)
}

// Acquire queues for a berth and blocks until it is granted, ctx ends, or
// the wait runs out. onWait hears each new reason for waiting.
func (b *Broker) Acquire(ctx context.Context, r Request, onWait func(string)) (*Lease, error) {
	b.mu.Lock()
	if err := b.refuse(r); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	l := newLease(fmt.Sprintf("%s-%d", r.Engine, b.cfg.IDs.Add(1)), r, b.now())
	l.arrivedN = b.order()
	b.queue = append(b.queue, l)
	b.log.Info("acquire", "lease", l.id, "engine", l.engine, "priority", l.prio, "needs_mib", l.needs/gpu.MiB)
	b.emit(proto.Event{Kind: proto.EvRequested, Lease: l.id, Engine: l.engine, Bytes: l.needs, Detail: l.prio.String()})
	b.schedule(b.now())
	b.mu.Unlock()

	wait := r.Wait
	if wait == 0 {
		wait = b.cfg.DefaultWait
	}
	// A negative wait has no deadline: the waiter stays queued until it
	// gets the card, its context is cancelled, or the daemon exits.
	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
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
		case <-timeout:
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

// refuse is why r can never be granted on this card, nil when it can be.
// b.mu must be held.
func (b *Broker) refuse(r Request) error {
	why := ""
	switch limit := sat(b.snap.Total, b.cfg.Reserve); {
	case b.snap.APU && r.Needs > 0:
		why = "APU requires an exclusive lease; omit needs-mib (VRAM and GTT are separate pools)"
	case r.Needs > 0 && b.snap.Total > 0 && r.Needs > limit:
		// A request bigger than the card can never fit, and would block
		// the queue behind it until it timed out.
		why = fmt.Sprintf("%s needs %d MiB; the card can never give more than %d MiB", r.Engine, r.Needs/gpu.MiB, limit/gpu.MiB)
	default:
		return nil
	}
	b.counters.Refused++
	b.emit(proto.Event{Kind: proto.EvRefused, Engine: r.Engine, Bytes: r.Needs, Detail: why})
	return fmt.Errorf("%w: %s", ErrRefused, why)
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
	// A waiter that leaves the queue never gets the lend, so the holder that
	// was asked (or has already given the memory) takes it back.
	b.dropLend(l, "it gave up waiting")
	if l.state != waiting {
		b.beginRelease(l, b.now(), false)
	}
	b.schedule(b.now())
	if l.lastWait != "" {
		why += ": " + l.lastWait
	}
	b.log.Warn("refused", "lease", l.id, "engine", l.engine, "reason", why)
	b.counters.Refused++
	b.emit(proto.Event{Kind: proto.EvRefused, Lease: l.id, Engine: l.engine, Detail: why})
	return fmt.Errorf("%w: %s", ErrRefused, why)
}
