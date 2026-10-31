// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/eventlog"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// startNode runs a real berthd over one broker per named card, sharing one
// event log as berthd's cards do, and notes one event on the last card.
func startNode(t *testing.T, names ...string) string {
	t.Helper()
	sock, brokers := serveNode(t, names...)
	brokers[len(brokers)-1].Note(proto.Event{Kind: proto.EvAlertRaised, Detail: "too hot"})
	return sock
}

// serveNode runs a real berthd over one broker per named card, the card at
// position i reading as nvidia-smi index i, and returns the brokers in order.
func serveNode(t *testing.T, names ...string) (string, []*broker.Broker) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, ids := eventlog.New(), &atomic.Uint64{}
	var cards []*node.Card
	var brokers []*broker.Broker
	for i, name := range names {
		b := broker.New(broker.Config{
			Card: name, Events: events, IDs: ids,
			Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1,
			Version: "test", Log: log,
		}, indexCard(i), procs.System{})
		b.Tick(ctx)
		go b.Run(ctx)
		cards = append(cards, &node.Card{Name: name, B: b})
		brokers = append(brokers, b)
	}
	n, err := node.New(events, ids, cards...)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ctx, ln, n, server.Extras{}, log)
	return sock, brokers
}

// indexCard is an empty card that nvidia-smi numbers with its value.
type indexCard int

func (i indexCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Index: int(i), Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

// leaseNames is the engines holding a lease on b.
func leaseNames(b *broker.Broker) []string {
	var out []string
	for _, l := range b.Status().Leases {
		out = append(out, l.Engine)
	}
	return out
}

// An event line names its card once berthd runs more than one, as the CARD
// column of berth status does; on one card it stays as it was (#189).
func TestEventLineNamesTheCardOnSeveralCards(t *testing.T) {
	isolate(t)
	for _, tc := range []struct {
		cards []string
		want  string
	}{
		{[]string{"gpu0", "gpu1"}, "alert.raised        gpu1  too hot"},
		{[]string{"gpu0"}, "alert.raised        too hot"},
	} {
		code, out, errOut := runCLI(t, "events", "--socket", startNode(t, tc.cards...))
		if code != ExitOK {
			t.Fatalf("%v: exit = %d (stderr %q)", tc.cards, code, errOut)
		}
		if !strings.HasSuffix(strings.TrimSpace(out), tc.want) {
			t.Errorf("%v: events = %q, want the line to end %q", tc.cards, out, tc.want)
		}
	}
}
