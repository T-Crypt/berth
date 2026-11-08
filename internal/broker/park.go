// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Park in RAM. A draining lease whose engine can offload itself asks the
// broker to park (admission over the machine's readings), parks through its
// adapter, and stays a lease whose residual keeps the card counted. A
// waiter builds beside a small residual, evicts a big one, and a resume
// hands the parked engine the card back at its own priority. Nothing here
// is automatic: park = "hook" on the engine's entry is the only way in, and
// an eviction is a drain the runner stops, exactly as today.

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// Park asks the broker to admit this lease's engine into RAM parking: a
// refusal comes back with its why line, and the drain runs as it always
// has.
func (l *Lease) Park() (bool, string) { return l.b.Park(l.l.id) }

// Parked records that this lease's park call is done, so the broker watches
// for the residual to leave the card.
func (l *Lease) Parked() {
	l.b.mu.Lock()
	l.l.parkedCall = l.b.now()
	l.l.parkedRead = l.b.reads.Load()
	l.b.mu.Unlock()
}

// ParkFailed puts a lease whose park call failed back on today's ladder:
// draining, and the runner stops it as any other. The why goes on the
// event stream, so a park that never works (a missing tool, an engine
// that refuses) shows in berth events, the harbor and berth notify
// instead of only in the run's own log.
func (l *Lease) ParkFailed(why string) {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	if l.l.state != parking {
		return
	}
	l.l.state = draining
	detail := "park failed; stopping it instead"
	if why != "" {
		detail = "park failed: " + why + "; stopping it instead"
	}
	l.b.log.Warn("park failed", "lease", l.l.id, "engine", l.l.engine, "why", why)
	l.b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: l.l.id, Engine: l.l.engine, Detail: detail})
}

// Waits delivers the reasons while a resumed lease waits in the queue.
func (l *Lease) Waits() <-chan string { return l.l.waitCh }

// Resumed delivers the runner's wake signal: one token per grant.
func (l *Lease) Resumed() <-chan struct{} { return l.l.resumeCh }

// Awake records that a resumed lease's engine answers its probe again:
// the event carries the seconds the wake took, and from this point the
// gateway may proxy the engine.
func (l *Lease) Awake() {
	l.b.mu.Lock()
	defer l.b.mu.Unlock()
	if l.l.state != active || l.l.granted.IsZero() {
		return
	}
	l.b.emit(proto.Event{Kind: proto.EvResumed, Lease: l.l.id, Engine: l.l.engine,
		Detail: fmt.Sprintf("resumed in %.1f s", time.Since(l.l.granted).Seconds())})
}

// footprintOf is the VRAM a park moves off the card: this run's measured
// peak, or the engine's learned footprint when this run has not peaked
// yet. Without a scope reading it is also the estimate of the RAM a park
// holds. b.mu must be held.
func (b *Broker) footprintOf(l *lease) uint64 {
	if l.peak > 0 {
		return l.peak
	}
	if f := b.footprints[l.key()]; f != nil {
		return f.Last
	}
	return 0
}

// ramOf is the RAM a parking or parked lease holds: its scope's anon plus
// shmem at the last read, else its VRAM footprint where Berth cannot read
// a scope (no cgroup v2, Windows, a container). b.mu must be held.
func (b *Broker) ramOf(l *lease) uint64 {
	if l.ram > 0 {
		return l.ram
	}
	return b.footprintOf(l)
}

// parkCost is the RAM a new park of l is expected to take: what the
// engine's last parked run grew by while it parked (the RAM it held hot
// was already out of MemAvailable), else its VRAM footprint. It is
// learned per engine, not per group: the models of one llama-swap park
// very different amounts. b.mu must be held.
func (b *Broker) parkCost(l *lease) uint64 {
	if f := b.footprints[l.engine]; f != nil && f.ParkedRAM > 0 {
		return f.ParkedRAM - min(f.ParkBase, f.ParkedRAM)
	}
	return b.footprintOf(l)
}

// parkTotals is this card's parking RAM for the machine's ledger: the
// expected cost of every park in flight, what parked engines hold, and the
// RAM its holders and waiters are expected to climb back to. b.mu must be
// held.
func (b *Broker) parkTotals() park.Totals {
	t := park.Totals{Parked: b.parkedRAM()}
	t.ToCome, _, _ = b.ramToCome()
	for _, l := range b.leases {
		if l.state == parking {
			t.Parking += b.parkCost(l)
		}
	}
	return t
}

// parkedRAM is the RAM all parked engines hold: the sum of ramOf. b.mu
// must be held.
func (b *Broker) parkedRAM() uint64 {
	var n uint64
	for _, l := range b.leases {
		if l.state == parked {
			n += b.ramOf(l)
		}
	}
	return n
}

// measureRAM reads an active, parking or parked lease's scope. An active
// run keeps its peak, which a park beside it must leave room for. A parked
// run records its first reading as its engine's next park cost: the
// reading moves a little every tick, and the footprint file is written
// once per park, not every poll. File pages are left out: the kernel
// counts page cache as available and reclaims it itself. Called with b.mu
// held, from Tick.
func (b *Broker) measureRAM(l *lease) {
	if (l.state != active && l.state != parking && l.state != parked) || l.scope == "" {
		return
	}
	m, ok := b.cfg.ScopeMem(l.scope)
	if !ok {
		return
	}
	l.ram = m.Anon + m.Shmem
	if l.state == active {
		l.ramPeak = max(l.ramPeak, l.ram)
		return
	}
	if l.state != parked || l.ram == 0 || l.ramLearned {
		return
	}
	l.ramLearned = true
	f := b.footprints[l.engine]
	if f == nil {
		f = &engineFootprint{}
		b.footprints[l.engine] = f
	}
	f.ParkedRAM, f.ParkBase = l.ram, l.ramHot
	b.footprintDirty = true
}

// parkedOOMScoreAdj is the oom_score_adj a parked engine's processes get:
// the highest, so when RAM runs out the kernel or earlyoom stops the copy
// that only saves a reload, never the job running on the card.
const parkedOOMScoreAdj = 1000

// setOOMScoreAdj is Config.OOMScoreAdj's default. The package's tests,
// whose process trees name made-up PIDs, swap it for a fake.
var setOOMScoreAdj = procs.SetOOMScoreAdj

// oomFirst raises every live process of a lease that just parked to
// parkedOOMScoreAdj and keeps what each had, for oomRestore. A process
// that will not take it (gone, another user's) keeps its own. b.mu must be
// held.
func (b *Broker) oomFirst(l *lease) {
	l.oomWas = map[ident]int{}
	ids := []ident{{pid: l.pid, start: l.pidStart}}
	for id := range l.desc {
		ids = append(ids, id)
	}
	for _, id := range ids {
		// Without a start time the PID could be anyone's by now.
		if id.pid <= 0 || id.start == 0 || b.tree.StartTime(id.pid) != id.start {
			continue
		}
		if old, err := b.cfg.OOMScoreAdj(id.pid, parkedOOMScoreAdj); err == nil {
			l.oomWas[id] = old
		}
	}
}

// oomRestore gives each raised process of a lease leaving parked its own
// oom_score_adj back. An evicted lease keeps the raise: it is stopping.
// b.mu must be held.
func (b *Broker) oomRestore(l *lease) {
	for id, old := range l.oomWas {
		if b.tree.StartTime(id.pid) == id.start {
			_, _ = b.cfg.OOMScoreAdj(id.pid, old)
		}
	}
	l.oomWas = nil
}

// foldRAMPeak records a freed run's RAM peak under its engine name when it
// is the most any run of that engine held. The most, not the last: a quick
// image after a long video must not shrink what the next video is booked
// at. b.mu must be held.
func (b *Broker) foldRAMPeak(l *lease) {
	f := b.footprints[l.engine]
	if l.ramPeak == 0 || (f != nil && f.RAMPeak >= l.ramPeak) {
		return
	}
	if f == nil {
		f = &engineFootprint{}
		b.footprints[l.engine] = f
	}
	f.RAMPeak = l.ramPeak
	b.footprintDirty = true
}

// ramToCome is the RAM this card's holders and waiters are expected to
// take beyond what they hold now: each one's peak (this run's, or the most
// any run of its engine held) less its last reading. A render between
// stages, or one waiting for the card a park is about to free, holds
// little now and climbs back. top names the largest share, for the
// refusal line. b.mu must be held.
func (b *Broker) ramToCome() (total uint64, top string, topPeak uint64) {
	var topBytes uint64
	holders := slices.DeleteFunc(slices.Collect(maps.Values(b.leases)), func(l *lease) bool { return l.state != active })
	for _, l := range append(holders, b.queue...) {
		peak := l.ramPeak
		if f := b.footprints[l.engine]; f != nil {
			peak = max(peak, f.RAMPeak)
		}
		if peak <= l.ram {
			continue
		}
		total += peak - l.ram
		if peak-l.ram > topBytes {
			topBytes, top, topPeak = peak-l.ram, l.engine, peak
		}
	}
	return total, top, topPeak
}

// Park admits a draining lease's engine into RAM parking: park.Admit runs
// on the machine's readings, because only the broker sees every parked
// engine. The returned why line is the runner's refusal, and the drain runs
// as it always has.
func (b *Broker) Park(id string) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.leases[id]
	if !ok || l.state != draining {
		return false, fmt.Sprintf("lease %s is not draining", id)
	}
	if b.parkOff {
		why := "park refused: parking is switched off (berth park on turns it back on)"
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Detail: why})
		return false, why
	}
	if b.snap.APU {
		why := "park refused: APU VRAM/GTT offload accounting is not yet supported; using normal drain"
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Detail: why})
		return false, why
	}
	for _, w := range b.queue {
		if w.wholeMachine {
			why := "park refused: " + w.engine + " needs the whole machine"
			b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Detail: why})
			return false, why
		}
	}
	if b.footprintOf(l) == 0 {
		// Nothing was ever measured: there is nothing to hold in RAM,
		// and nothing worth parking.
		why := "park refused: this run never held card memory"
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Detail: why})
		return false, why
	}
	// A park still in flight has not landed in MemAvailable yet: two
	// parks at once, on this card or another, must not both claim the
	// same free RAM. The ledger adds the other cards.
	// The RAM a holder or waiter climbs back to is booked the same way.
	mine := b.parkTotals()
	_, top, topPeak := b.ramToCome()
	avail := b.cfg.MemAvailable()
	avail -= min(avail, mine.Parking+mine.ToCome)
	cost := b.parkCost(l)
	ok, why := b.cfg.Ledger.Admit(b.cfg.Card, mine, park.Budget{
		Available: avail,
		Floor:     b.cfg.ParkFloor,
		Parked:    mine.Parked,
		Max:       b.cfg.ParkMax,
	}, cost)
	if !ok && mine.ToCome > 0 {
		why += fmt.Sprintf("; %s more is kept for %s, which has held %s", sizeHuman(mine.ToCome), top, sizeHuman(topPeak))
	}
	if !ok {
		b.emit(proto.Event{Kind: proto.EvParkRefused, Lease: id, Engine: l.engine, Bytes: cost, Detail: why})
		return false, why
	}
	l.state = parking
	// The grace runs from this park's own call. A lease that parked
	// before still carries that call's stamps, and a park that takes
	// longer than one tick would be given up on at once.
	l.parkedCall, l.parkedRead = time.Time{}, 0
	l.ram, l.ramLearned, l.ramHot = 0, false, 0
	if l.scope != "" {
		if m, ok := b.cfg.ScopeMem(l.scope); ok {
			l.ramHot = m.Anon + m.Shmem
		}
	}
	return true, ""
}

// parkingTick moves a parking lease to parked once a card read at or
// after the park call shows the residual, and gives up on the park after
// the grace: the runner is told to stop the engine the usual way. Called
// with b.mu held, from Tick.
func (b *Broker) parkingTick(key string, l *lease, now time.Time, read uint64) {
	if l.parkedCall.IsZero() || read <= l.parkedRead {
		return
	}
	if l.used() <= b.cfg.ParkResidualMax && b.driverReleased(l) {
		l.state = parked
		l.parkedAt = now
		l.parkedN = b.order()
		b.oomFirst(l)
		foot := b.footprintOf(l)
		b.log.Info("parked", "lease", key, "engine", l.engine, "footprint", sizeHuman(foot), "residual", sizeHuman(l.used()))
		b.emit(proto.Event{Kind: proto.EvParked, Lease: key, Engine: l.engine, Bytes: foot,
			Detail: fmt.Sprintf("residual %s", sizeHuman(l.used()))})
		return
	}
	if now.Sub(l.parkedCall) > b.cfg.ReleaseGrace {
		// The park did not free the card: stop the engine as any drain.
		l.state = draining
		msg := "park did not free the card"
		select {
		case l.drainCh <- msg:
		default:
		}
		b.log.Warn("park did not free the card", "lease", key, "engine", l.engine, "holding", holding(l))
		b.emit(proto.Event{Kind: proto.EvDraining, Lease: key, Engine: l.engine, Detail: msg})
	}
}

// parkedTick catches a parked engine that loaded back onto the card with
// no resume: a client sent it work directly. It holds the card again, so
// its lease is active again and its runner hears the wake as a resume;
// the next drain parks it again, where an eviction would stop it as an
// oversized residual. Called with b.mu held, from Tick.
func (b *Broker) parkedTick(key string, l *lease, now time.Time) {
	if l.used() <= b.cfg.ParkResidualMax {
		return
	}
	l.state = active
	l.granted = now
	l.grantedN = b.order()
	b.oomRestore(l)
	select {
	case l.resumeCh <- struct{}{}:
	default:
	}
	b.log.Warn("parked engine loaded back onto the card", "lease", key, "engine", l.engine, "holding", sizeHuman(l.used()))
	b.emit(proto.Event{Kind: proto.EvUnparked, Lease: key, Engine: l.engine, Bytes: l.used(),
		Detail: fmt.Sprintf("%s loaded %s back onto the card while parked, without a resume (a client used it directly)",
			l.engine, sizeHuman(l.used()))})
}

// evictParked asks the eviction table which parked engines to stop this
// tick: past their ttl, for the RAM floor, or under memory pressure. Each
// named lease goes draining and its runner hears OpDrain with the why.
// Called with b.mu held, from Tick.
func (b *Broker) evictParked(now time.Time) {
	var entries []park.Parked
	for _, l := range b.leases {
		if l.state == parked {
			entries = append(entries, park.Parked{Name: l.id, Bytes: b.ramOf(l), Since: l.parkedAt, TTL: l.parkTTL})
		}
	}
	if len(entries) == 0 {
		return
	}
	avail := b.cfg.MemAvailable()
	names := park.Evict(entries, park.Budget{
		Available: avail,
		Floor:     b.cfg.ParkFloor,
		Parked:    b.parkedRAM() + b.cfg.Ledger.Others(b.cfg.Card).Parked,
		Max:       b.cfg.ParkMax,
	}, b.cfg.MemPressure(), b.cfg.ParkPressure, now, b.cfg.ParkTTL)
	for _, name := range names {
		l, ok := b.leases[name]
		if !ok || l.state != parked {
			continue
		}
		why := "the memory pressure is high"
		switch {
		case now.Sub(l.parkedAt) > cmp.Or(l.parkTTL, b.cfg.ParkTTL):
			why = fmt.Sprintf("parked %s, past the ttl", now.Sub(l.parkedAt).Round(time.Second))
		case avail < b.cfg.ParkFloor:
			why = "available RAM is under the floor"
		}
		b.evict(l, why)
	}
}

// evict stops a parked engine rather than resuming it: its runner hears
// OpDrain and stops it as any drain, with no park to keep. why names the
// rule that says so: the RAM floor, the ttl, memory pressure, a window that
// closed. Called with b.mu held.
func (b *Broker) evict(l *lease, why string) {
	l.state = draining
	select {
	case l.drainCh <- "evicted: " + why:
	default:
	}
	b.log.Warn("evicting a parked engine", "lease", l.id, "engine", l.engine, "why", why)
	b.emit(proto.Event{Kind: proto.EvParkEvicted, Lease: l.id, Engine: l.engine, Bytes: b.ramOf(l), Detail: why})
}

// Resume puts the engine's parked lease back in the queue at its own
// priority, keeping the runner's connection. The runner hears
// OpGranted{Resume: true} when the grant lands. switched marks a person's
// switch, as Request.Switched does.
func (b *Broker) Resume(engine string, switched bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.leases {
		if l.state != parked || (l.engine != engine && l.key() != engine) {
			continue
		}
		l.state = waiting
		l.resumed = true
		l.switched = switched
		b.oomRestore(l)
		l.lostTo, l.lostWhy = "", ""
		l.arrived = b.now()
		l.arrivedN = b.order()
		// The waits from before the park are stale; the runner hears
		// only what happens from here.
		for {
			select {
			case <-l.waitCh:
				continue
			default:
			}
			break
		}
		b.queue = append(b.queue, l)
		b.schedule(b.now())
		return nil
	}
	return notFound(engine + " is not parked")
}

// besideLine is the note a grant beside parked engines carries.
func besideLine(parked []*lease, residual uint64) string {
	names := make([]string, 0, len(parked))
	for _, p := range parked {
		names = append(names, p.engine)
	}
	sort.Strings(names)
	return fmt.Sprintf("beside parked %s (%d MiB)", strings.Join(names, ", "), residual/(1<<20))
}

// sortParked orders parked leases least recently parked first.
func sortParked(ls []*lease) []*lease {
	out := append([]*lease(nil), ls...)
	sort.Slice(out, func(i, j int) bool { return out[i].parkedN < out[j].parkedN })
	return out
}

// subtract is as - bs, by identity.
func subtract(as, bs []*lease) []*lease {
	drop := map[*lease]bool{}
	for _, b := range bs {
		drop[b] = true
	}
	var out []*lease
	for _, a := range as {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}

// Switch makes the engine's queued lease a person's switch, as
// Request.Switched does at acquire: it wins a juggle it already lost.
func (b *Broker) Switch(engine string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.queue {
		if l.engine != engine && l.key() != engine {
			continue
		}
		l.switched = true
		l.lostTo, l.lostWhy = "", ""
		b.schedule(b.now())
		return nil
	}
	return notFound(engine + " has no queued lease")
}
