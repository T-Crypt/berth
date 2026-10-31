// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// Status is a copy of the broker's view: the shape /v1/status serves.

import (
	"sort"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/proto"
)

// clockStep is the smallest age a coarse clock can measure. Windows
// advances its interrupt time every ~15.6 ms, so two reads inside one
// quantum differ by 0 even though the later read is still the later one
// in program order.
const clockStep = 16 * time.Millisecond

// parkedAge is how long a lease has been parked. A status read inside
// the park's own quantum measures 0; a zero would say the lease is not
// parked at all, so the floor is the clock's own step.
func parkedAge(l *lease, now time.Time) float64 {
	if age := now.Sub(l.parkedAt); age > 0 {
		return age.Seconds()
	}
	return clockStep.Seconds()
}

// Status is a copy of the broker's view: the shape /v1/status serves.
func (b *Broker) Status() proto.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	st := proto.Status{
		API:         proto.APIVersion,
		Version:     b.cfg.Version,
		GPUs:        []proto.GPU{},
		Leases:      []proto.Lease{},
		Waiting:     []proto.Lease{},
		Squatters:   []proto.Squatter{},
		Unprotected: []proto.Run{},
		Counters:    b.counters,
		LastEvent:   b.cfg.Events.Last(),
		Priority:    b.priorityStatus(now),
		Footprints:  b.footprintStatus(),
		ParkedBytes: b.parkedRAM(),
	}
	if b.probeErr != nil {
		st.ProbeErr = b.probeErr.Error()
	}
	if b.cardState != "" {
		st.Passthrough = &proto.Passthrough{State: b.cardState, Driver: b.cardDriver, Addr: b.cardAddr}
	}
	devs := b.snap.Devices
	if len(devs) == 0 && b.snap.Total > 0 {
		devs = []gpu.Device{{Index: b.snap.Index, Name: b.snap.Name, Total: b.snap.Total, Used: b.snap.Used, Free: b.snap.Free,
			APU: b.snap.APU, GTTTotal: b.snap.GTTTotal, GTTUsed: b.snap.GTTUsed,
			Util: -1, Temp: -1, Power: -1, PowerLimit: -1, Fan: -1, ClockSM: -1, ClockMem: -1, PCIeGen: -1, PCIeWidth: -1}}
	}
	for _, d := range devs {
		g := proto.GPU{Index: d.Index, Name: d.Name, Total: d.Total, Used: d.Used, Free: d.Free, Age: now.Sub(b.snap.At).Seconds(),
			APU: d.APU, GTTTotal: d.GTTTotal, GTTUsed: d.GTTUsed,
			Util: d.Util, Temp: d.Temp, Power: d.Power, PowerLimit: d.PowerLimit, Fan: d.Fan,
			ClockSM: d.ClockSM, ClockMem: d.ClockMem, PCIeGen: d.PCIeGen, PCIeWidth: d.PCIeWidth}
		if d.Index == b.snap.Index {
			g.Scheduled, g.Baseline, g.Cordoned = true, b.snap.Baseline(), b.cordoned
			g.UnobservedPIDs = append([]int(nil), b.snap.Unobserved...)
			g.GTTBaseline = b.snap.GTTBaseline()
		}
		st.GPUs = append(st.GPUs, g)
	}
	view := func(l *lease, since time.Time) proto.Lease {
		pids := make([]int, 0, len(l.gpu))
		for id := range l.gpu {
			pids = append(pids, id.pid)
		}
		sort.Ints(pids)
		pl := proto.Lease{
			ID: l.id, Engine: l.engine, Group: l.group, Priority: l.prio.String(), State: l.state.String(),
			Needs: l.needs, PID: l.pid, PIDStart: l.pidStart,
			Containers: l.containers,
			GPUPIDs:    pids, Used: l.used(),
			GTTUsed: l.gttUsed(), GTTContested: l.contestedGTT,
			Contested: l.contested, Age: now.Sub(since).Seconds(), Pinned: l.pinned,
		}
		if l.state == parked {
			pl.ParkedSince = parkedAge(l, now)
		}
		if l.minTurn > 0 {
			if rem := l.minTurn - now.Sub(l.granted); rem > 0 {
				pl.TurnS = rem.Seconds()
			}
		}
		return pl
	}
	for _, l := range b.holders() {
		st.Leases = append(st.Leases, view(l, l.granted))
	}
	for i, l := range b.queue {
		pl := view(l, l.arrived)
		pl.Position = i + 1
		pl.EstimateS = b.startCost(b.queue[:i+1]).seconds()
		st.Waiting = append(st.Waiting, pl)
	}
	for _, s := range b.squatters {
		st.Squatters = append(st.Squatters, proto.Squatter{
			PID: s.proc.PID, PIDStart: s.id.start, Name: s.proc.Name, Used: s.proc.Used,
			GTTUsed: s.proc.GTT,
			Run:     s.run, Container: short(s.container), ContainerName: s.containerName,
			Age: now.Sub(s.seen).Seconds(),
		})
	}
	sort.Slice(st.Squatters, func(i, j int) bool { return st.Squatters[i].PID < st.Squatters[j].PID })
	for _, c := range b.claims {
		c.PIDs = append([]int(nil), c.PIDs...)
		for _, cp := range b.claimed {
			if cp.claim == c.Name {
				c.Used += cp.proc.Used
				c.GTTUsed += cp.proc.GTT
			}
		}
		st.Claims = append(st.Claims, c)
	}
	for _, r := range b.runs {
		st.Unprotected = append(st.Unprotected, proto.Run{Engine: r.Engine, PID: r.PID, Reason: r.Reason, Age: now.Sub(r.Started).Seconds()})
	}
	return st
}
