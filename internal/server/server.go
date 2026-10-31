// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package server puts the broker on a Unix socket. One connection is one
// lease: the handler holds the berth until the client disconnects.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/proto"
)

// Listen opens the socket, refusing if another berthd already answers on it
// and clearing it if it is stale. Only the owner may connect.
func Listen(path string) (net.Listener, error) {
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("berthd already listening on %s", path)
	}
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Windows has no mode bits on a socket; the folder's ACL is the guard.
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// writeDeadline bounds one fan-out write to a socket consumer. It
// mirrors the harbor collector's 5 s roundtrip deadline, the other
// side of the same socket: a reader slower than that is gone. A var
// so tests can shrink it.
var writeDeadline = 5 * time.Second

// sendTimeouts counts fan-out writes that hit writeDeadline;
// droppedConsumers counts consumers dropped for slow reads. Both are
// stamped into Status.Counters before status replies and rendered in
// /metrics; the harbor store keeps no column for them.
var (
	sendTimeouts     atomic.Uint64
	droppedConsumers atomic.Uint64
)

// consumer is one socket client under the slow-reader policy: every
// write carries a deadline, and the first failed write drops that
// consumer only. send reports false after a drop, so the caller stops
// serving it; the rest keep their stream.
type consumer struct {
	conn    *proto.Conn
	log     *slog.Logger
	dropped atomic.Bool
}

func (u *consumer) send(m proto.Msg) bool {
	if u.dropped.Load() {
		return false
	}
	if err := u.conn.SendWithDeadline(m, writeDeadline); err != nil {
		if u.dropped.CompareAndSwap(false, true) {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				sendTimeouts.Add(1)
				droppedConsumers.Add(1)
				u.log.Warn("server: dropping slow consumer", "err", err)
			}
		}
		return false
	}
	return true
}

// reply is a one-shot answer on a connection that reads nothing more:
// a slow reader still gets its deadline, then the handler returns and
// the deferred close drops it.
func reply(conn *proto.Conn, log *slog.Logger, m proto.Msg) {
	(&consumer{conn: conn, log: log}).send(m)
}

// Extras are the node's readings beyond the lease table; any may be nil.
type Extras struct {
	Engines  func() []proto.Engine
	Host     func() *proto.Host
	History  func() proto.History
	Insights func() *proto.Insights
	// Requests is the in-memory request log after since, oldest first.
	Requests func(since time.Time) []proto.Request
	// Alerts is the node's alert state: threshold table + sticky levels.
	// It can be one sample older than the status body it rides in (the
	// sampler's store vs the socket's dial); levels move at most once
	// per sample and the UI repolls more often, so they re-align within
	// one poll.
	Alerts func() *proto.Alerts
	// Pair is the harbor's device pairing; nil turns the OpPair* ops off.
	Pair *pair.Store
	// Finished hears that a run's engine ended on purpose (exit 0, or a
	// stop the run carried out), by the lease's engine name.
	Finished func(engine string)
}

// Serve answers clients on ln.
func Serve(ctx context.Context, ln net.Listener, n *node.Node, x Extras, log *slog.Logger) error {
	if x.Engines == nil {
		x.Engines = func() []proto.Engine { return []proto.Engine{} }
	}
	if x.Host == nil {
		x.Host = func() *proto.Host { return nil }
	}
	if x.Insights == nil {
		x.Insights = func() *proto.Insights { return nil }
	}
	if x.History == nil {
		x.History = func() proto.History { return proto.History{Samples: []proto.Sample{}} }
	}
	if x.Requests == nil {
		x.Requests = func(time.Time) []proto.Request { return []proto.Request{} }
	}
	if x.Alerts == nil {
		x.Alerts = func() *proto.Alerts { return nil }
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(ctx, c, n, x, log)
	}
}

func handle(ctx context.Context, c net.Conn, n *node.Node, x Extras, log *slog.Logger) {
	defer c.Close()
	conn := proto.NewConn(c)
	first, err := conn.Recv()
	if err != nil {
		return
	}
	switch first.Op {
	case proto.OpStatus:
		st := n.Status()
		st.Engines, st.Host, st.Insights = x.Engines(), x.Host(), x.Insights()
		st.Alerts = x.Alerts()
		st.Counters.SendTimeouts = sendTimeouts.Load()
		st.Counters.DroppedConsumers = droppedConsumers.Load()
		reply(conn, log, proto.Msg{Op: proto.OpStatus, Status: &st})
	case proto.OpHistory:
		h := x.History()
		reply(conn, log, proto.Msg{Op: proto.OpHistory, History: &h})
	case proto.OpRequests:
		rs := x.Requests(first.SinceT)
		if rs == nil {
			rs = []proto.Request{}
		}
		reply(conn, log, proto.Msg{Op: proto.OpRequests, Requests: rs})
	case proto.OpAcquire, proto.OpReattach:
		hold(ctx, conn, first, n, x.Finished, log)
	case proto.OpReclaim:
		reclaim(conn, log, first, n)
	case proto.OpStop:
		stop(conn, log, first, n)
	case proto.OpPairCode, proto.OpPairRedeem, proto.OpPairCheck, proto.OpPairList, proto.OpPairRevoke:
		pairing(conn, log, first, x.Pair)
	case proto.OpResume:
		resume(conn, log, first, n)
	case proto.OpSwitch:
		switched(conn, log, first, n)
	case proto.OpEvents:
		events(ctx, c, conn, first, n, log)
	default:
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: fmt.Sprintf("unknown op %q", first.Op)})
	}
}

func hold(ctx context.Context, conn *proto.Conn, req proto.Msg, n *node.Node, finished func(string), log *slog.Logger) {
	u := &consumer{conn: conn, log: log}
	prio, err := broker.ParsePriority(req.Priority)
	if err != nil || req.Engine == "" {
		if err == nil {
			err = errors.New("engine name is required")
		}
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}
	cards, err := n.Pick(req.Card)
	if err != nil {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}
	card := cards[0]
	b := card.B

	// The client's later messages (attach) and its disconnect both arrive
	// through this reader. A disconnect cancels the wait or ends the lease.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	incoming := make(chan proto.Msg)
	go func() {
		defer cancel()
		for {
			m, err := conn.Recv()
			if err != nil {
				return
			}
			if m.Op == proto.OpFinished {
				if finished != nil {
					finished(req.Engine)
				}
				continue
			}
			select {
			case incoming <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	r := broker.Request{
		Engine:       req.Engine,
		Group:        req.Group,
		Priority:     prio,
		Needs:        req.Needs,
		Wait:         time.Duration(req.Wait * float64(time.Second)),
		Pinned:       req.Pinned,
		Switched:     req.Switched,
		Parked:       req.Parked,
		Lend:         req.Lend,
		WholeMachine: req.WholeMachine,
		LendID:       req.LendID,
		LentBytes:    req.Lent,
		MinTurn:      time.Duration(req.MinTurn * float64(time.Second)),
		ParkTTL:      time.Duration(req.ParkTTL * float64(time.Second)),
		Containers:   req.ContainerIDs(),
	}
	if len(cards) > 1 {
		holdAll(ctx, u, req, r, cards, incoming, cancel, log)
		return
	}
	var lease *broker.Lease
	if req.Op == proto.OpReattach {
		lease, err = b.Reattach(r, req.PID, req.PIDStart)
	} else {
		lease, err = b.Acquire(ctx, r, func(reason string) {
			// A slow reader must not wedge the wait for everyone
			// behind it: drop this consumer and abandon its queue
			// seat, the same as a disconnect.
			if !u.send(proto.Msg{Op: proto.OpWaiting, Reason: reason}) {
				cancel()
			}
		})
	}
	if err != nil {
		u.send(proto.Msg{Op: proto.OpRefused, Reason: strings.TrimPrefix(err.Error(), broker.ErrRefused.Error()+": ")})
		return
	}
	defer lease.Release()
	granted := proto.Msg{Op: proto.OpGranted, Lease: lease.ID(), Card: card.Name, CardUUID: card.UUID, Reason: contested(lease)}
	if !u.send(granted) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case why := <-lease.Drains():
			if !u.send(proto.Msg{Op: proto.OpDrain, Reason: why}) {
				return
			}
		case why := <-lease.Waits():
			u.send(proto.Msg{Op: proto.OpWaiting, Reason: why})
		case <-lease.Resumed():
			if !u.send(proto.Msg{Op: proto.OpGranted, Lease: lease.ID(), Resume: true}) {
				return
			}
		case <-lease.Lends():
			if ask := lease.TakeLend(); ask != nil {
				if !u.send(proto.Msg{Op: proto.OpLend, Needs: ask.MiB << 20, Free: ask.FreeMiB, GPU: ask.GPU}) {
					return
				}
			}
		case <-lease.Returns():
			if ask := lease.TakeReturn(); ask != nil {
				if !u.send(proto.Msg{Op: proto.OpReturn, LendID: ask.ID}) {
					return
				}
			}
		case m := <-incoming:
			switch {
			case m.Op == proto.OpPark:
				if ok, why := lease.Park(); ok {
					u.send(proto.Msg{Op: proto.OpParkOK, Lease: lease.ID()})
				} else {
					u.send(proto.Msg{Op: proto.OpRefused, Reason: why})
				}
			case m.Op == proto.OpParked:
				lease.Parked()
			case m.Op == proto.OpParkFailed:
				lease.ParkFailed(m.Reason)
			case m.Op == proto.OpAwake:
				lease.Awake()
			case m.Op == proto.OpLent:
				lease.Lent(m.LendID, m.Needs)
			case m.Op == proto.OpLendFailed:
				lease.LendFailed(m.Reason)
			case m.Op == proto.OpReturned:
				lease.Returned()
			case m.Op == proto.OpReturnFailed:
				lease.ReturnFailed(m.Reason, m.Retry)
			case m.Op == proto.OpAttach && m.PID > 0:
				lease.Attach(m.PID)
				log.Info("attached", "lease", lease.ID(), "pid", m.PID)
			}
			if ids := m.ContainerIDs(); len(ids) > 0 {
				lease.AttachContainers(ids)
				log.Info("attached containers", "lease", lease.ID(), "containers", ids)
			}
		}
	}
}

// contested is the warning a grant carries when processes without a lease
// held memory on the card, "" when none did.
func contested(lease *broker.Lease) string {
	why := ""
	if c := lease.Contested(); c > 0 {
		why = fmt.Sprintf("card contested: processes without a lease hold %d MiB", c/gpu.MiB)
	}
	if shared := lease.ContestedGTT(); shared > 0 {
		if why == "" {
			why = "card contested: processes without a lease hold shared RAM"
		}
		why += fmt.Sprintf(" (%d MiB GTT)", shared/gpu.MiB)
	}
	return why
}

// holdAll holds one lease on several cards, granted on all of them at once.
// Each card schedules and drains its own share, and a drain on any card is a
// drain of the engine, which runs on all of them. The connection's end frees
// every card. A share cannot park or lend yet: a park is refused, so the run
// drains the usual way, and no card asks the lease to lend.
func holdAll(ctx context.Context, u *consumer, req proto.Msg, r broker.Request, cards []*node.Card, incoming <-chan proto.Msg, cancel context.CancelFunc, log *slog.Logger) {
	bs := make([]*broker.Broker, len(cards))
	names := make([]string, len(cards))
	uuids := make([]string, len(cards))
	for i, c := range cards {
		bs[i], names[i], uuids[i] = c.B, c.Name, c.UUID
	}
	r.Lend = false
	var leases []*broker.Lease
	var err error
	if req.Op == proto.OpReattach {
		leases, err = broker.ReattachAll(bs, r, req.PID, req.PIDStart)
	} else {
		leases, err = broker.AcquireAll(ctx, bs, r, func(reason string) {
			if !u.send(proto.Msg{Op: proto.OpWaiting, Reason: reason}) {
				cancel()
			}
		})
	}
	if err != nil {
		u.send(proto.Msg{Op: proto.OpRefused, Reason: strings.TrimPrefix(err.Error(), broker.ErrRefused.Error()+": ")})
		return
	}
	defer func() {
		for _, l := range leases {
			l.Release()
		}
	}()
	granted := proto.Msg{Op: proto.OpGranted, Lease: leases[0].ID(), Card: strings.Join(names, ",")}
	// CUDA_VISIBLE_DEVICES takes every card or none: a card with no UUID
	// leaves the engine to pick its cards itself.
	if !slices.Contains(uuids, "") {
		granted.CardUUID = strings.Join(uuids, ",")
	}
	var warns []string
	for i, l := range leases {
		if why := contested(l); why != "" {
			warns = append(warns, names[i]+": "+why)
		}
	}
	granted.Reason = strings.Join(warns, "; ")
	if !u.send(granted) {
		return
	}
	drains := make(chan string)
	for i, l := range leases {
		go func() {
			for {
				select {
				case why := <-l.Drains():
					select {
					case drains <- names[i] + ": " + why:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case why := <-drains:
			if !u.send(proto.Msg{Op: proto.OpDrain, Reason: why}) {
				return
			}
		case m := <-incoming:
			switch {
			case m.Op == proto.OpPark:
				u.send(proto.Msg{Op: proto.OpRefused, Reason: "a lease on several cards cannot park"})
			case m.Op == proto.OpAttach && m.PID > 0:
				for _, l := range leases {
					l.Attach(m.PID)
				}
				log.Info("attached", "lease", leases[0].ID(), "cards", granted.Card, "pid", m.PID)
			}
			if ids := m.ContainerIDs(); len(ids) > 0 {
				for _, l := range leases {
					l.AttachContainers(ids)
				}
				log.Info("attached containers", "lease", leases[0].ID(), "containers", ids)
			}
		}
	}
}

// events replays the ring after Since, then, with Watch, streams new events
// until the client goes away or falls too far behind.
func events(ctx context.Context, c net.Conn, conn *proto.Conn, req proto.Msg, n *node.Node, log *slog.Logger) {
	u := &consumer{conn: conn, log: log}
	backlog, live, stop := n.Events.Events(req.Since, req.Watch)
	defer stop()
	for i := range backlog {
		if !u.send(proto.Msg{Op: proto.OpEvent, Event: &backlog[i]}) {
			return
		}
	}
	if live == nil {
		return
	}
	// The client sends nothing more; a read returning means it hung up.
	gone := make(chan struct{})
	go func() {
		var buf [1]byte
		c.Read(buf[:])
		close(gone)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gone:
			return
		case e, ok := <-live:
			if !ok {
				reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "fell behind; reconnect with since"})
				return
			}
			if !u.send(proto.Msg{Op: proto.OpEvent, Event: &e}) {
				return
			}
		}
	}
}

// Dial connects a client.
func Dial(path string) (*proto.Conn, net.Conn, error) {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, nil, err
	}
	return proto.NewConn(c), c, nil
}

// MiB formats bytes for humans.
func MiB(n uint64) string { return fmt.Sprintf("%d MiB", n/gpu.MiB) }
