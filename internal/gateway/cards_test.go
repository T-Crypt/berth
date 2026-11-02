// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/eventlog"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/server"
)

// twoCards is a berthd with two cards, gpu0 at 0000:01:00.0 and gpu1 at
// 0000:09:00.0, sharing one event log and lease counter as berthd's do.
func twoCards(t *testing.T) (string, *node.Node) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, ids := eventlog.New(), &atomic.Uint64{}
	var cards []*node.Card
	for _, c := range []struct{ name, addr string }{{"gpu0", "0000:01:00.0"}, {"gpu1", "0000:09:00.0"}} {
		b := broker.New(broker.Config{Card: c.name, Events: events, IDs: ids,
			Poll: 20 * time.Millisecond, DefaultWait: 10 * time.Second, Log: log}, emptyCard{}, procs.System{})
		go b.Run(ctx)
		cards = append(cards, &node.Card{Name: c.name, Addr: c.addr, B: b})
	}
	n, err := node.New(events, ids, cards...)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(ctx, ln, n, server.Extras{}, log) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, c := range cards {
			if l, err := c.B.Acquire(ctx, broker.Request{Engine: "test-cleanup", Priority: broker.Hold, Pinned: true, Wait: 10 * time.Second}, nil); err == nil {
				l.Release()
			}
		}
	})
	return sock, n
}

// A background engine on gpu1, named by its PCI address, starts while a
// hold has gpu0, and the board puts each holder under its own card.
func TestBackgroundEngineStartsOnItsCardWhileAnotherIsHeld(t *testing.T) {
	sock, n := twoCards(t)
	hold, err := n.Cards()[0].B.Acquire(context.Background(), broker.Request{Engine: "game", Priority: broker.Hold, Pinned: true, Wait: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	addr := freeAddr(t)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "tagger", Type: "llamacpp", URL: "http://" + addr, GPU: "0000:09:00.0",
			Start:      []string{"env", "BERTH_GW_ENGINE=" + addr + ",tagger-model", os.Args[0]},
			Background: true, IdleAfter: config.Duration{Duration: 100 * time.Millisecond}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Background(ctx, 50*time.Millisecond)

	until(t, "the tagger to start on gpu1 beside the hold on gpu0", func() bool {
		ls := n.Cards()[1].B.Status().Leases
		return len(ls) == 1 && ls[0].Engine == "tagger" && ls[0].State == "active"
	})
	holders := map[string][]string{}
	st := n.Status()
	for _, c := range boardOf("box", &st, nil, nil, nil).Cards {
		for _, h := range c.Holders {
			holders[c.Card] = append(holders[c.Card], h.Name)
		}
	}
	if len(holders) != 2 || len(holders["gpu0"]) != 1 || holders["gpu0"][0] != "game" ||
		len(holders["gpu1"]) != 1 || holders["gpu1"][0] != "tagger" {
		t.Errorf("board holders = %v, want game on gpu0 and tagger on gpu1", holders)
	}
}
