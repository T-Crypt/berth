// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// The runner's side of parking. One lease's connection is the line the
// park protocol rides: on a drain the runner asks the broker for
// admission, runs the adapter's park, and holds the lease with the
// engine asleep in RAM; a grant with Resume set wakes the engine again
// and waits for its readiness probe. Eviction and resume arrive on the
// same conn, so the line lives as long as the lease does.
package runner

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/httpauth"
	"github.com/T-Crypt/berth/internal/lend"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/proto"
)

// The park protocol's windows. The broker's admission answer is one call
// away; the adapter calls carry the engines' own cost.
const (
	parkReplyWait   = 30 * time.Second // how long the broker has to answer admission
	parkCallTimeout = 60 * time.Second // the adapter moves the weights once
	wakeCallTimeout = 120 * time.Second
	parkCadence     = time.Second // the resume probe's cadence
)

// leaseLine is one lease's park and lend side: the connection the lease
// rides (rebuilt across reattach), the daemon's park replies, whether the
// engine is parked now, the one protocol probe a hook without adapter URLs
// is allowed, and the run's lend ledger: the open tickets its engine gave,
// so a re-attach can tell the new daemon what to stop counting.
type leaseLine struct {
	mu      sync.Mutex
	conn    *proto.Conn
	replies chan proto.Msg
	parked  bool
	// stopped is set once the run itself sets out to end the engine (a
	// drain past the park, a failed wake, a forwarded stop signal), so the
	// exit that follows is on purpose whatever its code.
	stopped bool
	probed  *parkVerdict
	lends   map[string]lend.Ticket
	// the 503 watch's pacing: how long it reads the engine's ledger and how
	// often, defaults from internal/runner/lend.go; fields so a test can
	// shorten the watch before it starts.
	lendEvery  time.Duration
	lendWindow time.Duration
	// endWatch ends a running 503 watch. A new lend calls it before it asks
	// the engine, so the watch never takes back a ticket the run is about to
	// record.
	endWatch context.CancelFunc
}

func newLine(conn *proto.Conn) *leaseLine {
	return &leaseLine{conn: conn, replies: make(chan proto.Msg, 1),
		lendEvery: lendPollEvery, lendWindow: lendPendingWindow}
}

func (l *leaseLine) isParked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.parked
}

func (l *leaseLine) askStop() {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
}

func (l *leaseLine) stopWasAsked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

func (l *leaseLine) setConn(conn *proto.Conn) {
	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()
}

func (l *leaseLine) send(m proto.Msg) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return errors.New("no connection to berthd")
	}
	return l.conn.Send(m)
}

// clear takes a reply an earlier drain left behind, so a new park starts
// on an empty line.
func (l *leaseLine) clear() {
	for {
		select {
		case <-l.replies:
		default:
			return
		}
	}
}

// note takes the park replies the drain waits on; the runner routes them
// as they arrive.
func (l *leaseLine) note(m proto.Msg) {
	select {
	case l.replies <- m:
	default:
	}
}

func (l *leaseLine) waitReply() (*proto.Msg, bool) {
	t := time.NewTimer(parkReplyWait)
	defer t.Stop()
	select {
	case m := <-l.replies:
		return &m, true
	case <-t.C:
		return nil, false
	}
}

// adapterFor is the park adapter for this run: out options, or the
// protocol verdict the first drain recorded. Before a drain has asked,
// a hook without URLs has none yet.
func (l *leaseLine) adapterFor(o Options) (park.Adapter, bool) {
	if o.Park != nil {
		return *o.Park, true
	}
	l.mu.Lock()
	probed := l.probed
	l.mu.Unlock()
	if probed == nil || !probed.ok {
		return park.Adapter{}, false
	}
	return probed.adapter, true
}

// adapter is the park adapter the park step asks through: out options,
// or Berth's own park protocol, probed once the first time a drain would
// ask. A hook whose engine does not speak the protocol answers false
// once, and every later drain runs today's ladder without asking again.
func (l *leaseLine) adapter(o Options, say func(string, ...any)) (park.Adapter, bool) {
	if o.Park != nil {
		return *o.Park, true
	}
	if !o.Hook {
		return park.Adapter{}, false
	}
	l.mu.Lock()
	probed := l.probed
	l.mu.Unlock()
	if probed == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		adapter, ok := park.Protocol(httpauth.WithBearer(ctx, o.APIKey), o.ParkBase)
		cancel()
		probed = &parkVerdict{adapter: adapter, ok: ok}
		l.mu.Lock()
		l.probed = probed
		l.mu.Unlock()
		if !ok {
			say("%s does not speak Berth's park protocol; draining the usual way", o.ParkBase)
		}
	}
	return probed.adapter, probed.ok
}

// parkVerdict remembers the one probe a hook without URLs is allowed.
type parkVerdict struct {
	adapter park.Adapter
	ok      bool
}

// park takes the engine off the card on a drain: the broker's admission,
// the adapter's call, the reservation on record. A refusal or a failed
// call says so and leaves the lease on the stop ladder.
func (l *leaseLine) park(o Options, say func(string, ...any)) bool {
	l.clear()
	adapter, ok := l.adapter(o, say)
	if !ok {
		return false
	}
	if err := l.send(proto.Msg{Op: proto.OpPark}); err != nil {
		say("park request unreachable (%v); draining the usual way", err)
		return false
	}
	reply, ok := l.waitReply()
	if !ok {
		say("berthd never answered the park request; draining the usual way")
		_ = l.send(proto.Msg{Op: proto.OpParkFailed, Reason: "berthd never answered the park request"})
		return false
	}
	if reply.Op != proto.OpParkOK {
		say("park refused: %s", reply.Reason)
		return false
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), parkCallTimeout)
	err := adapter.Park(httpauth.WithBearer(ctx, o.APIKey), o.ParkBase)
	cancel()
	if err != nil {
		say("park call failed (%v); draining the usual way", err)
		_ = l.send(proto.Msg{Op: proto.OpParkFailed, Reason: err.Error()})
		return false
	}
	// Parked is believed before the reservation rides out: the broker can
	// grant the wake back at once, and the wake's routing must find the
	// parked flag up.
	l.mu.Lock()
	l.parked = true
	l.mu.Unlock()
	if err := l.send(proto.Msg{Op: proto.OpParked}); err != nil {
		l.mu.Lock()
		l.parked = false
		l.mu.Unlock()
		say("parked, but could not reach berthd (%v); draining the usual way", err)
		return false
	}
	say("parked %s in RAM in %s", o.Engine, time.Since(start).Round(100*time.Millisecond))
	return true
}

// resume drives the wake-up the runner hears as a grant with Resume set:
// the adapter's call, the readiness probe, the engine serving again. A
// wake that never answers stops the engine, which hands the card back:
// a wedged engine must not keep the lease.
func (l *leaseLine) resume(o Options, cmd *exec.Cmd, done <-chan struct{}, say func(string, ...any)) {
	if !l.isParked() {
		return
	}
	l.mu.Lock()
	l.parked = false
	l.mu.Unlock()
	start := time.Now()
	adapter, hasWake := l.adapterFor(o)
	if hasWake && adapter.Resume != nil {
		say("resuming %s", o.Engine)
		ctx, cancel := context.WithTimeout(context.Background(), wakeCallTimeout)
		err := adapter.Resume(httpauth.WithBearer(ctx, o.APIKey), o.ParkBase)
		cancel()
		if err != nil {
			say("resuming %s failed (%v); stopping it", o.Engine, err)
			l.stopNow(o, cmd, done, say)
			return
		}
	}
	if !o.parkReady() {
		say("%s never answered after the wake; stopping it", o.Engine)
		l.stopNow(o, cmd, done, say)
		return
	}
	if o.front != nil {
		o.front.Open()
	}
	say("resumed %s in %s", o.Engine, time.Since(start).Round(100*time.Millisecond))
	if err := l.send(proto.Msg{Op: proto.OpAwake, Lease: o.Engine}); err != nil {
		say("resumed, but could not tell berthd (%v)", err)
	}
}

// parkReady waits for the engine's readiness path (the model list for
// LLMs, the device stats for comfy) after the wake, until the window's
// end.
func (o Options) parkReady() bool {
	deadline := time.Now().Add(wakeCallTimeout)
	url := strings.TrimRight(o.ParkBase, "/") + o.ParkProbe
	for {
		if askReady(url, o.APIKey) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(parkCadence)
	}
}

func askReady(url, key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// stopNow runs the stop ladder's tail: terminate, then kill after Grace.
// The engine exits, the wait returns, and the lease is released with it.
func (l *leaseLine) stopNow(o Options, cmd *exec.Cmd, done <-chan struct{}, say func(string, ...any)) {
	l.askStop()
	terminate(cmd)
	select {
	case <-done:
		return
	case <-time.After(o.Grace):
	}
	say("%s ignored terminate for %s; killing", o.Engine, o.Grace)
	kill(cmd)
}
