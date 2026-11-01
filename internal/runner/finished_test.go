// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/server"
)

// A batch engine that exits 0 stopped answering on purpose: the engines
// tab shows it down, not latched (issue 265). So does one Berth stopped,
// through berth stop or a stop signal from its supervisor, whatever code
// the signal leaves it. One that exits non-zero on its own keeps the
// latch, so a crash still reads as one.
func TestACleanExitDoesNotLatchTheEngine(t *testing.T) {
	forever := []string{"sh", "-c", "exec sleep 30"}
	for _, c := range []struct {
		name string
		argv []string
		stop func(t *testing.T, b *broker.Broker)
		want string
	}{
		{"exit 0", []string{"sh", "-c", "exit 0"}, nil, "down"},
		{"exit 1", []string{"sh", "-c", "exit 1"}, nil, "latched"},
		{"berth stop", forever, func(t *testing.T, b *broker.Broker) {
			if _, err := b.Stop("deduper"); err != nil {
				t.Fatal(err)
			}
		}, "down"},
		{"supervisor stop", forever, func(t *testing.T, _ *broker.Broker) {
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}, "down"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var up atomic.Bool
			up.Store(true)
			eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !up.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(w, "ok")
			}))
			defer eng.Close()
			w := &engines.Watcher{Engines: []config.Engine{{Name: "deduper", Type: "custom", URL: eng.URL}}}

			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second,
				JuggleWindow: -1, Log: log}, &parkCard{total: 24 << 30}, procs.System{})
			sock := filepath.Join(t.TempDir(), "berth.sock")
			ln, err := server.Listen(sock)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go b.Run(ctx)
			go func() { _ = server.Serve(ctx, ln, node.One(b), server.Extras{Finished: w.Finished}, log) }()

			w.Poll(ctx)
			if s := w.Snapshot()[0].State; s != "up" {
				t.Fatalf("before the run: state %q, want up", s)
			}
			ran := make(chan int, 1)
			go func() {
				ran <- Run(Options{Socket: sock, Engine: "deduper", Priority: "batch", Grace: time.Second,
					Argv: c.argv, Stderr: io.Discard})
			}()
			if c.stop != nil {
				waitFor(t, "the lease granted", func() bool { return len(b.Status().Leases) == 1 })
				c.stop(t, b)
			}
			select {
			case <-ran:
			case <-time.After(10 * time.Second):
				t.Fatal("the run never ended")
			}
			up.Store(false)
			waitFor(t, "the lease released", func() bool { return len(b.Status().Leases) == 0 })
			w.Poll(ctx)
			w.Poll(ctx)
			if s := w.Snapshot()[0].State; s != c.want {
				t.Errorf("after the run: state %q, want %q", s, c.want)
			}
		})
	}
}
