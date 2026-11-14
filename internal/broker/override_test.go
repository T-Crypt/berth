// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// queueOrder is the queue as the status reports it, engine names in order.
func queueOrder(b *Broker) []string {
	var out []string
	for _, l := range b.Status().Waiting {
		out = append(out, l.Engine)
	}
	return out
}

func waiterID(t *testing.T, b *Broker, engine string) string {
	t.Helper()
	for _, l := range b.Status().Waiting {
		if l.Engine == engine {
			return l.ID
		}
	}
	t.Fatalf("no waiter %s in %v", engine, queueOrder(b))
	return ""
}

// The operator reorders the queue: a bump puts a waiter first in its class,
// a prio moves it to another class, a drop refuses it. Each is an event, and
// the next grant follows the new order.
func TestQueueOverride(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "holder", Priority: Foreground, Pinned: true}))
	a := acquire(b, Request{Engine: "a", Priority: Interactive})
	time.Sleep(10 * time.Millisecond)
	bb := acquire(b, Request{Engine: "bb", Priority: Interactive})
	time.Sleep(10 * time.Millisecond)
	c := acquire(b, Request{Engine: "c", Priority: Batch})
	time.Sleep(20 * time.Millisecond)
	if got := queueOrder(b); !slices.Equal(got, []string{"a", "bb", "c"}) {
		t.Fatalf("queue = %v, want priority then arrival", got)
	}

	if _, err := b.Queue(waiterID(t, b, "bb"), QueueBump, ""); err != nil {
		t.Fatal(err)
	}
	if got := queueOrder(b); !slices.Equal(got, []string{"bb", "a", "c"}) {
		t.Fatalf("after bump bb: %v", got)
	}
	detail, err := b.Queue(waiterID(t, b, "c"), QueuePrio, "foreground")
	if err != nil || detail != "priority batch -> foreground by the operator" {
		t.Fatalf("prio c: %q, %v", detail, err)
	}
	if got := queueOrder(b); !slices.Equal(got, []string{"c", "bb", "a"}) {
		t.Fatalf("after prio c: %v", got)
	}

	if _, err := b.Queue(waiterID(t, b, "c"), QueuePrio, "hold"); err == nil {
		t.Fatal("prio hold was accepted")
	}
	if _, err := b.Queue("nope-9", QueueBump, ""); !IsNotFound(err) {
		t.Fatalf("unknown id: %v, want not found", err)
	}

	if _, err := b.Queue(waiterID(t, b, "a"), QueueDrop, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-a:
		if !errors.Is(r.err, ErrRefused) || !strings.Contains(r.err.Error(), "dropped from the queue by the operator") {
			t.Fatalf("dropped a: %v", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("dropped waiter still waiting")
	}

	var overrides []string
	events, _, _ := b.EventLog().Events(0, false)
	for _, e := range events {
		if e.Kind == proto.EvQueueOverride {
			overrides = append(overrides, e.Engine+": "+e.Detail)
		}
	}
	want := []string{
		"bb: moved to the front of its class by the operator",
		"c: priority batch -> foreground by the operator",
		"a: dropped by the operator",
	}
	if !slices.Equal(overrides, want) {
		t.Fatalf("override events = %q, want %q", overrides, want)
	}

	holder.Release()
	b.Tick(context.Background())
	l := must(t, c)
	pending(t, bb)
	l.Release()
	must(t, bb)
}
