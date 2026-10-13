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
	"strings"
	"sync/atomic"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
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
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
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
}

// Serve answers clients on ln.
func Serve(ctx context.Context, ln net.Listener, b *broker.Broker, x Extras, log *slog.Logger) error {
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
		go handle(ctx, c, b, x, log)
	}
}

func handle(ctx context.Context, c net.Conn, b *broker.Broker, x Extras, log *slog.Logger) {
	defer c.Close()
	conn := proto.NewConn(c)
	first, err := conn.Recv()
	if err != nil {
		return
	}
	switch first.Op {
	case proto.OpStatus:
		st := b.Status()
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
		hold(ctx, conn, first, b, log)
	case proto.OpEvents:
		events(ctx, c, conn, first, b, log)
	default:
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: fmt.Sprintf("unknown op %q", first.Op)})
	}
}

func hold(ctx context.Context, conn *proto.Conn, req proto.Msg, b *broker.Broker, log *slog.Logger) {
	u := &consumer{conn: conn, log: log}
	prio, err := broker.ParsePriority(req.Priority)
	if err != nil || req.Engine == "" {
		if err == nil {
			err = errors.New("engine name is required")
		}
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}

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
			select {
			case incoming <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	r := broker.Request{
		Engine:   req.Engine,
		Group:    req.Group,
		Priority: prio,
		Needs:    req.Needs,
		Wait:     time.Duration(req.Wait * float64(time.Second)),
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
	granted := proto.Msg{Op: proto.OpGranted, Lease: lease.ID()}
	if c := lease.Contested(); c > 0 {
		granted.Reason = fmt.Sprintf("card contested: processes without a lease hold %d MiB", c/gpu.MiB)
	}
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
		case m := <-incoming:
			if m.Op == proto.OpAttach && m.PID > 0 {
				lease.Attach(m.PID)
				log.Info("attached", "lease", lease.ID(), "pid", m.PID)
			}
		}
	}
}

// events replays the ring after Since, then, with Watch, streams new events
// until the client goes away or falls too far behind.
func events(ctx context.Context, c net.Conn, conn *proto.Conn, req proto.Msg, b *broker.Broker, log *slog.Logger) {
	u := &consumer{conn: conn, log: log}
	backlog, live, stop := b.Events(req.Since, req.Watch)
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
