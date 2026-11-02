// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
)

// A background engine (a tagger) runs while the card is free, a foreground
// request drains it, it stays out while that request's engine holds the
// card, and it comes back once the card has been free for its idle_after.
func TestBackgroundEngineRunsOnlyWhileTheCardIsFree(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "tagger", Type: "llamacpp", URL: "http://" + addr,
			Start:      []string{"env", "BERTH_GW_ENGINE=" + addr + ",tagger-model", os.Args[0]},
			Background: true, IdleAfter: config.Duration{Duration: 300 * time.Millisecond}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Background(ctx, 50*time.Millisecond)

	holds := func(engine, prio string) func() bool {
		return func() bool {
			ls := b.Status().Leases
			return len(ls) == 1 && ls[0].Engine == engine && ls[0].State == "active" && ls[0].Priority == prio
		}
	}
	until(t, "the tagger to start at batch on a free card", holds("tagger", "batch"))

	chat := make(chan int, 1)
	go func() {
		chat <- runner.Run(runner.Options{Socket: sock, Engine: "chat", Priority: "foreground",
			Grace: 200 * time.Millisecond, Argv: []string{"sleep", "1.5"}, Stderr: io.Discard})
	}()
	until(t, "the chat engine to take the card from the tagger", holds("chat", "foreground"))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		st := b.Status()
		for _, l := range append(st.Leases, st.Waiting...) {
			if l.Engine == "tagger" {
				t.Fatalf("the tagger asked for the card while chat held it: leases %+v, waiting %+v", st.Leases, st.Waiting)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := <-chat; code != 0 {
		t.Fatalf("chat exit %d", code)
	}
	freed := time.Now()
	until(t, "the tagger to come back on the free card", holds("tagger", "batch"))
	if back := time.Since(freed); back < 250*time.Millisecond {
		t.Fatalf("the tagger came back %s after the card freed, before its idle_after", back)
	}
}

// A background engine outside its window is not started at all: it would
// only sit in the queue as a waiter all day. Its neighbour on the same card,
// with no window, starts as it always did.
func TestBackgroundEngineStaysOutSideItsWindow(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "tagger", Type: "llamacpp", URL: "http://" + addr,
				Start:      []string{"env", "BERTH_GW_ENGINE=" + addr + ",tagger-model", os.Args[0]},
				Background: true, IdleAfter: config.Duration{Duration: 200 * time.Millisecond},
				Window: windowFrom(6*60, 60)},
			{Name: "indexer", Type: "llamacpp", URL: "http://" + addr,
				Start:      []string{"env", "BERTH_GW_ENGINE=" + addr + ",indexer-model", os.Args[0]},
				Background: true, IdleAfter: config.Duration{Duration: 200 * time.Millisecond}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Background(ctx, 50*time.Millisecond)

	// The Background loop takes one engine per pass, in file order, so the
	// tagger is declined and the indexer starts.
	until(t, "the windowless indexer to start at batch on a free card", func() bool {
		ls := b.Status().Leases
		return len(ls) == 1 && ls[0].Engine == "indexer" && ls[0].State == "active"
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		st := b.Status()
		for _, l := range append(st.Leases, st.Waiting...) {
			if l.Engine == "tagger" {
				t.Fatalf("the tagger asked for the card outside its window: leases %+v, waiting %+v", st.Leases, st.Waiting)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Give the window its hours: the same engine starts the moment the card has
// been free for its idle_after.
func TestBackgroundEngineStartsInsideItsWindow(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "tagger", Type: "llamacpp", URL: "http://" + addr,
			Start:      []string{"env", "BERTH_GW_ENGINE=" + addr + ",tagger-model", os.Args[0]},
			Background: true, IdleAfter: config.Duration{Duration: 200 * time.Millisecond},
			Window: windowFrom(-6*60, 12*60)}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Background(ctx, 50*time.Millisecond)
	until(t, "the tagger to start inside its window", func() bool {
		ls := b.Status().Leases
		return len(ls) == 1 && ls[0].Engine == "tagger" && ls[0].State == "active"
	})
}

// windowFrom is a window that starts offset minutes from the minute the
// gateway's clock reads and lasts span of them: the loop reads time.Now, so
// a test says where the window sits against now rather than pinning it. Six
// hours of slack either way keeps the verdict stable however long it runs.
func windowFrom(offset, span int) proto.Window {
	n := time.Now()
	m := n.Hour()*60 + n.Minute()
	return proto.Window{Start: (m + offset + minutesDay) % minutesDay, End: (m + offset + span + minutesDay) % minutesDay}
}

const minutesDay = 24 * 60

// The card is free only when nothing holds, uses or waits for it and it
// serves this machine.
func TestCardFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *proto.Status
		want bool
	}{
		{"no reading", nil, false},
		{"empty card", &proto.Status{}, true},
		{"a lease", &proto.Status{Leases: []proto.Lease{{Engine: "chat"}}}, false},
		{"a waiter", &proto.Status{Waiting: []proto.Lease{{Engine: "chat"}}}, false},
		{"a stowaway", &proto.Status{Squatters: []proto.Squatter{{PID: 7}}}, false},
		{"claim rules, none matching", &proto.Status{Claims: []proto.Claim{{Name: "steam-game"}, {Name: "blender"}}}, true},
		{"a tracked app running", &proto.Status{Claims: []proto.Claim{{Name: "steam-game"}, {Name: "blender", Active: true}}}, false},
		{"in a VM", &proto.Status{Passthrough: &proto.Passthrough{State: proto.CardPassthrough}}, false},
		{"probe failing", &proto.Status{ProbeErr: "nvidia-smi: exit 9"}, false},
	} {
		if got := cardFree(tc.st); got != tc.want {
			t.Errorf("%s: cardFree = %v, want %v", tc.name, got, tc.want)
		}
	}
}
