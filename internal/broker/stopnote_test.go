// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

// OnStopOrdered tells the stopped engine's card why it is down, the
// moment a drain is ordered: an engine says "turned down for" its name,
// a hold says its name, an operator stop says so. A vfio handoff tells
// no story, and a lease that is already draining keeps the story the
// first stop told.

import (
	"context"
	"sync"
	"testing"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
)

type stopNote struct{ engine, note string }

type noteSink struct {
	mu    sync.Mutex
	notes []stopNote
}

func (s *noteSink) wire(b *Broker) {
	b.cfg.OnStopOrdered = func(engine, note string) {
		s.mu.Lock()
		s.notes = append(s.notes, stopNote{engine, note})
		s.mu.Unlock()
	}
}

func (s *noteSink) get() []stopNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stopNote{}, s.notes...)
}

// drainStory sets holder a on the card, lets waiter drain it, and returns
// the story the drain told.
func drainStory(t *testing.T, waiter Request, want []stopNote) {
	t.Helper()
	b, c, _ := setup(t, procs.Map{101: 100})
	sink := &noteSink{}
	sink.wire(b)
	a := must(t, acquire(b, Request{Engine: "a"}))
	a.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "hog", Used: 8 * GiB})
	b.Tick(context.Background())
	ch := acquire(b, waiter)
	pending(t, ch)
	drained(t, a)
	got := sink.get()
	if len(got) != len(want) {
		t.Fatalf("waiter %q told %d stories %+v, want %d: %+v", waiter.Engine, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waiter %q story %d = %+v, want %+v", waiter.Engine, i, got[i], want[i])
		}
	}
}

func TestOnStopOrderedTellsTheDrainStory(t *testing.T) {
	t.Run("an engine says turned down for its name", func(t *testing.T) {
		drainStory(t, Request{Engine: "ollama", Priority: Foreground},
			[]stopNote{{"a", "Turned down for ollama"}})
	})
	t.Run("a named hold says its name", func(t *testing.T) {
		drainStory(t, Request{Engine: "hold:game", Priority: Hold},
			[]stopNote{{"a", "Stopped for game"}})
	})
	t.Run("a bare hold says a hold", func(t *testing.T) {
		drainStory(t, Request{Engine: "hold", Priority: Hold},
			[]stopNote{{"a", "Stopped for a hold"}})
	})
	t.Run("a claim says its rule name", func(t *testing.T) {
		drainStory(t, Request{Engine: "games", Priority: Hold},
			[]stopNote{{"a", "Stopped for games"}})
	})
	t.Run("a vfio handoff tells no story", func(t *testing.T) {
		drainStory(t, Request{Engine: "vfio:vm", Priority: Hold}, nil)
	})
}

func TestStopSaysOnCommandOnce(t *testing.T) {
	b, c, _ := setup(t, procs.Map{101: 100, 202: 200})
	sink := &noteSink{}
	sink.wire(b)
	a := must(t, acquire(b, Request{Engine: "a"}))
	a.Attach(100)
	c.set(gpu.Proc{PID: 101, Name: "hog", Used: 8 * GiB})
	b.Tick(context.Background())
	// A plain waiter's drain tells its name.
	breq := acquire(b, Request{Engine: "b", Priority: Foreground})
	pending(t, breq)
	drained(t, a)
	a.Release()
	if got := sink.get(); len(got) != 1 || got[0] != (stopNote{"a", "Turned down for b"}) {
		t.Fatalf("a plain drain told %v, want one note: a, turned down for b", got)
	}
	c.set()
	b.Tick(context.Background())
	blease := must(t, breq)
	blease.Attach(200)
	c.set(gpu.Proc{PID: 202, Name: "b-hog", Used: 4 * GiB})
	b.Tick(context.Background())
	// An operator stop tells "Stopped on command", once.
	if _, err := b.Stop("b"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := sink.get(); len(got) != 2 || got[1] != (stopNote{"b", "Stopped on command"}) {
		t.Fatalf("operator stop story = %+v, want a second note: b, stopped on command", got)
	}
	// The lease is draining: a second stop keeps the first story.
	if _, err := b.Stop("b"); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if got := sink.get(); len(got) != 2 {
		t.Fatalf("a second stop told another story: %+v", got)
	}
}
