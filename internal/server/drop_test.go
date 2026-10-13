package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// A slow reader trips the deadline while a healthy consumer on a plain
// writer sends byte-identical output to Send.
func TestConsumerDropsSlowKeepsHealthy(t *testing.T) {
	old := writeDeadline
	writeDeadline = 50 * time.Millisecond
	defer func() { writeDeadline = old }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	slowR, slowW := net.Pipe()
	defer slowR.Close()
	defer slowW.Close()
	slow := &consumer{conn: proto.NewConn(slowW), log: log}

	var healthyBuf bytes.Buffer
	healthy := &consumer{conn: proto.NewConn(nopRW{&healthyBuf}), log: log}

	t0, d0 := sendTimeouts.Load(), droppedConsumers.Load()
	if slow.send(proto.Msg{Op: proto.OpEvent}) {
		t.Fatal("slow reader was never dropped: send succeeded")
	}
	if got := sendTimeouts.Load() - t0; got != 1 {
		t.Fatalf("send timeouts rose by %d, want 1", got)
	}
	if got := droppedConsumers.Load() - d0; got != 1 {
		t.Fatalf("dropped consumers rose by %d, want 1", got)
	}
	// A second send after the drop stays dropped without double counting.
	if slow.send(proto.Msg{Op: proto.OpEvent}) {
		t.Fatal("dropped consumer sent again")
	}
	if got := droppedConsumers.Load() - d0; got != 1 {
		t.Fatalf("dropped consumers rose by %d, want exactly 1", got)
	}

	m := proto.Msg{Op: proto.OpStatus}
	if !healthy.send(m) {
		t.Fatal("healthy consumer was dropped")
	}
	var plain bytes.Buffer
	if err := proto.NewConn(nopRW{&plain}).Send(m); err != nil {
		t.Fatal(err)
	}
	if healthyBuf.String() != plain.String() {
		t.Fatalf("healthy send differs from Send: %q vs %q", healthyBuf.String(), plain.String())
	}
}

// One stuck events watcher is dropped while a healthy watcher keeps its
// stream and the daemon keeps answering: the wedge from the plan, through
// the real socket.
func TestSlowEventsConsumerDroppedHealthyKept(t *testing.T) {
	old := writeDeadline
	writeDeadline = 100 * time.Millisecond
	defer func() { writeDeadline = old }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	sock := filepath.Join(t.TempDir(), "berth.sock")
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, Log: log}, dropCard{}, procs.System{})
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	go Serve(ctx, ln, b, Extras{}, log)
	t0, d0 := sendTimeouts.Load(), droppedConsumers.Load()

	// Build no backlog: both watchers subscribe first, then a paced
	// flood overfills the slow socket's kernel buffers. Pacing keeps
	// the server ahead of the live channel (no fell-behind race), so
	// the only way out is the write deadline.
	// The slow consumer never reads after its watch request.
	slowRaw, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer slowRaw.Close()
	slow := proto.NewConn(slowRaw)
	if err := slow.Send(proto.Msg{Op: proto.OpEvents, Watch: true}); err != nil {
		t.Fatal(err)
	}

	healthy, healthyRaw, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer healthyRaw.Close()
	if err := healthy.Send(proto.Msg{Op: proto.OpEvents, Watch: true}); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 4096)
	go func() {
		for {
			m, err := healthy.Recv()
			if err != nil || m.Event == nil {
				return
			}
			got <- m.Event.Kind
		}
	}()

	// ~600 leases make ~1800 events (~330 KB), past the 208 KB
	// socket buffers; 1 ms pacing keeps the live channel drained.
	prio, _ := broker.ParsePriority("batch")
	for i := 0; i < 600; i++ {
		lease, err := b.Acquire(ctx, broker.Request{Engine: "slowtest", Priority: prio}, nil)
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(8 * time.Second)
	for sendTimeouts.Load() == t0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sendTimeouts.Load() == t0 {
		t.Fatal("slow consumer was never dropped: no send timeout")
	}
	if droppedConsumers.Load() <= d0 {
		t.Fatal("slow consumer was never dropped: counter did not move")
	}

	// The server closed the slow connection: drain its unread backlog
	// until the close arrives.
	_ = slowRaw.SetReadDeadline(time.Now().Add(5 * time.Second))
	var one [4096]byte
	for {
		n, err := slowRaw.Read(one[:])
		_ = n
		if err == nil {
			continue
		}
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			t.Fatal("slow connection still open after the drop")
		}
		break
	}

	// The healthy watcher kept its stream through the same flood.
	seen := map[string]bool{}
	for time.Now().Before(deadline) && (!seen[proto.EvRequested] || !seen[proto.EvGranted]) {
		select {
		case k := <-got:
			seen[k] = true
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !seen[proto.EvRequested] || !seen[proto.EvGranted] {
		t.Fatalf("healthy watcher lost its stream: %v", seen)
	}

	// The daemon still answers new clients, with the drop counters stamped.
	c2, r2, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if err := c2.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		t.Fatal(err)
	}
	m, err := c2.Recv()
	if err != nil || m.Status == nil {
		t.Fatalf("status after drop: %v %+v", err, m)
	}
	if m.Status.Counters.DroppedConsumers == 0 || m.Status.Counters.SendTimeouts == 0 {
		t.Fatalf("status lacks the drop counters: %+v", m.Status.Counters)
	}
}

type dropCard struct{}

func (dropCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

type nopRW struct{ *bytes.Buffer }

func (n nopRW) Read(p []byte) (int, error) { return 0, os.ErrClosed }
