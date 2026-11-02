// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// A run that names a card holds its lease there, and the engine sees only
// that card through CUDA_VISIBLE_DEVICES, unless its environment already
// picks one. A run that names no card goes to the first card and its
// environment is left alone, so an engine split across cards keeps them.
func TestRunOnANamedCard(t *testing.T) {
	sock, n := twoCardDaemon(t)
	run := func(engine, gpu string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "env")
		code := Run(Options{Socket: sock, Engine: engine, Priority: "interactive", Card: gpu, Grace: time.Second,
			Argv:   []string{"sh", "-c", `printf %s "${CUDA_VISIBLE_DEVICES-unset}" > ` + out},
			Stderr: io.Discard})
		if code != 0 {
			t.Fatalf("%s: exit %d", engine, code)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := run("maya", "gpu1"); got != "GPU-gpu1-uuid" {
		t.Errorf("named gpu1: CUDA_VISIBLE_DEVICES = %q, want GPU-gpu1-uuid", got)
	}
	if got := run("plain", ""); got != "unset" {
		t.Errorf("no card named: CUDA_VISIBLE_DEVICES = %q, want it unset", got)
	}
	t.Setenv("CUDA_VISIBLE_DEVICES", "3")
	if got := run("pinned", "gpu1"); got != "3" {
		t.Errorf("environment set: CUDA_VISIBLE_DEVICES = %q, want the operator's 3", got)
	}

	granted := map[string]string{}
	evs, _, _ := n.Events.Events(0, false)
	for _, e := range evs {
		if e.Kind == proto.EvGranted {
			granted[e.Engine] = e.Card
		}
	}
	if granted["maya"] != "gpu1" || granted["plain"] != "gpu0" || granted["pinned"] != "gpu1" {
		t.Errorf("granted on = %v, want maya and pinned on gpu1, plain on gpu0", granted)
	}
}

// twoCardDaemon is a berthd over gpu0 and gpu1, 24 GiB each, whose UUIDs
// are GPU-<name>-uuid.
func twoCardDaemon(t *testing.T) (string, *node.Node) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	events, ids, seq, ledger := node.Shared()
	card := func(name string) *node.Card {
		b := broker.New(broker.Config{Card: name, Events: events, IDs: ids, Seq: seq, Ledger: ledger,
			Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1, Log: log},
			&parkCard{total: 24 << 30}, procs.System{})
		return &node.Card{Name: name, UUID: "GPU-" + name + "-uuid", B: b}
	}
	n, err := node.New(events, ids, card("gpu0"), card("gpu1"))
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "berth.sock")
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, c := range n.Cards() {
		go c.B.Run(ctx)
	}
	go func() { _ = server.Serve(ctx, ln, n, server.Extras{}, log) }()
	return sock, n
}

// A run that names several cards holds one lease on all of them, and the
// engine sees those cards, in the order named, through CUDA_VISIBLE_DEVICES.
func TestRunOnSeveralCards(t *testing.T) {
	sock, n := twoCardDaemon(t)
	out := filepath.Join(t.TempDir(), "env")
	code := Run(Options{Socket: sock, Engine: "maya", Priority: "interactive", Card: "gpu1,gpu0", Grace: time.Second,
		Argv:   []string{"sh", "-c", `printf %s "${CUDA_VISIBLE_DEVICES-unset}" > ` + out},
		Stderr: io.Discard})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "GPU-gpu1-uuid,GPU-gpu0-uuid" {
		t.Errorf("CUDA_VISIBLE_DEVICES = %q, want GPU-gpu1-uuid,GPU-gpu0-uuid", env)
	}
	granted := map[string]string{}
	evs, _, _ := n.Events.Events(0, false)
	for _, e := range evs {
		if e.Kind == proto.EvGranted && e.Engine == "maya" {
			granted[e.Card] = e.Lease
		}
	}
	if len(granted) != 2 || granted["gpu0"] == "" || granted["gpu0"] != granted["gpu1"] {
		t.Errorf("maya granted %v, want one lease on gpu0 and gpu1", granted)
	}
}

// A drain on one card of a lease on several stops the engine, and its end
// frees every card.
func TestDrainOnOneCardStopsALeaseOnSeveral(t *testing.T) {
	sock, n := twoCardDaemon(t)
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "maya", Priority: "interactive", Card: "gpu0,gpu1", Grace: time.Second,
			Argv: []string{"sleep", "30"}, Stderr: io.Discard})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(n.Status().Leases) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("maya never held both cards: %v", n.Status().Leases)
		}
		time.Sleep(20 * time.Millisecond)
	}
	gpu1, err := n.Card("gpu1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	voice, err := gpu1.B.Acquire(ctx, broker.Request{Engine: "voice", Priority: broker.Foreground}, nil)
	if err != nil {
		t.Fatalf("voice on gpu1: %v", err)
	}
	defer voice.Release()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("maya kept running after gpu1 drained it")
	}
	for time.Now().Before(deadline.Add(5 * time.Second)) {
		left := false
		for _, l := range n.Status().Leases {
			left = left || l.Engine == "maya"
		}
		if !left {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("maya still holds a card after it stopped: %v", n.Status().Leases)
}

// A run re-attaches to the card it was granted on, so a restarted berthd
// puts a gpu1 engine back on gpu1, not on the first card. Its hours ride
// along, so a daemon restart does not hand back a window.
func TestReattachNamesTheCard(t *testing.T) {
	m := reattachMsg(Options{Engine: "maya", Card: "gpu1", Window: proto.Window{Start: 23 * 60, End: 7 * 60}},
		101, 7, &containerRef{}, false, newLine(nil))
	if m.Op != proto.OpReattach || m.Card != "gpu1" || m.Window != "23:00-07:00" {
		t.Fatalf("reattach msg = op %s card %q window %q, want reattach on gpu1 with its window", m.Op, m.Card, m.Window)
	}
}
