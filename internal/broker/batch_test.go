// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// comfyQueue is the ComfyUI queue as the engine watcher reads it.
type comfyQueue struct{ busy atomic.Bool }

func (q *comfyQueue) read(k string) (bool, bool) {
	if k == "comfyui" {
		return q.busy.Load(), true
	}
	return true, true // the chat engine is mid-turn
}

func batchBroker(t *testing.T) (*Broker, *minClock, *comfyQueue) {
	t.Helper()
	q := &comfyQueue{}
	b, _ := jugglers(t, Config{Order: []string{"llama-swap", "comfyui"}, Busy: q.read})
	clk := newMinClock(turnBase)
	b.nowFn = clk.now
	return b, clk, q
}

func batchEvents(b *Broker) []string {
	evs, _, _ := b.Events(0, false)
	var out []string
	for _, e := range evs {
		if e.Kind == proto.EvBatchStarted || e.Kind == proto.EvBatchEnded {
			out = append(out, e.Kind+" "+e.Detail)
		}
	}
	return out
}

// The 2026-11-08 batch on aphotic: the agent driving it runs on NInfer, and
// every agent turn drained ComfyUI between images. In a batch the chat
// request waits for the queue instead.
func TestBatchKeepsTheCardFromAnEqualPriorityChatTurn(t *testing.T) {
	b, _, q := batchBroker(t)
	q.busy.Store(true)
	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
	b.Batch("comfyui", 0, 0)

	w := acquireW(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground})
	pending(t, w.ch)
	notDrained(t, comfy)
	if got, want := firstReason(t, w), "next in line: comfyui runs a batch; it keeps the card until it has been idle 30s"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// A higher priority waits for the batch too, as for a minimum turn.
func TestBatchKeepsTheCardFromAHigherPriority(t *testing.T) {
	b, _, q := batchBroker(t)
	q.busy.Store(true)
	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Interactive}))
	b.Batch("comfyui", 0, 0)

	w := acquire(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground})
	pending(t, w)
	notDrained(t, comfy)
}

// The other half of the trap: ComfyUI restarted and waited 10 minutes behind
// a llama-swap holder that won the juggle on the engine order. A batch's
// waiting engine takes the card the way a person's switch does.
func TestBatchWaiterTakesTheCardFromAJuggleWinner(t *testing.T) {
	b, _, _ := batchBroker(t)
	ninfer := must(t, acquire(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground}))
	b.Batch("comfyui", 0, 0)

	comfy := acquire(b, Request{Engine: "comfyui", Priority: Foreground})
	pending(t, comfy)
	if why := drained(t, ninfer); !strings.Contains(why, "comfyui") {
		t.Fatalf("drain reason %q does not name comfyui", why)
	}
	ninfer.Release()
	b.Tick(context.Background())
	must(t, comfy)
}

// Without a batch the same order keeps the card on llama-swap, as before.
func TestNoBatchTheJuggleWinnerKeepsTheCard(t *testing.T) {
	b, _, _ := batchBroker(t)
	ninfer := must(t, acquire(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground}))

	comfy := acquire(b, Request{Engine: "comfyui", Priority: Foreground})
	pending(t, comfy)
	notDrained(t, ninfer)
}

// A script submits one job at a time: the queue reads empty between jobs.
// A gap shorter than the grace keeps the batch; a longer one ends it, and
// the chat turn that waited drains ComfyUI at once.
func TestBatchEndsAfterTheGraceOfAnIdleQueue(t *testing.T) {
	b, clk, q := batchBroker(t)
	q.busy.Store(true)
	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
	b.Batch("comfyui", 20*time.Second, 0)
	w := acquire(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground})
	pending(t, w)

	q.busy.Store(false)
	b.Tick(context.Background())
	clk.set(turnBase.Add(10 * time.Second))
	b.Tick(context.Background())
	notDrained(t, comfy)

	q.busy.Store(true) // the next job arrives inside the grace
	b.Tick(context.Background())
	q.busy.Store(false)
	clk.set(turnBase.Add(25 * time.Second))
	b.Tick(context.Background())
	notDrained(t, comfy)

	clk.set(turnBase.Add(46 * time.Second))
	b.Tick(context.Background())
	if why := drained(t, comfy); !strings.Contains(why, "NInfer-27B") {
		t.Fatalf("drain reason = %q", why)
	}
	got := batchEvents(b)
	want := []string{
		"batch.started comfyui keeps the card until it has been idle 20s (at most 4h0m0s)",
		"batch.ended comfyui: idle for 20s",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A hold (a game, a call) still takes the card out of a batch.
func TestHoldTakesTheCardOutOfABatch(t *testing.T) {
	b, _, q := batchBroker(t)
	q.busy.Store(true)
	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
	b.Batch("comfyui", 0, 0)

	h := acquire(b, Request{Engine: "game", Priority: Hold, Pinned: true})
	pending(t, h)
	drained(t, comfy)
}

// A batch nobody ends stops at its cap, busy or not.
func TestBatchEndsAtItsCap(t *testing.T) {
	b, clk, q := batchBroker(t)
	q.busy.Store(true)
	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
	b.Batch("comfyui", 0, time.Hour)
	w := acquire(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground})
	pending(t, w)

	clk.set(turnBase.Add(time.Hour))
	b.Tick(context.Background())
	drained(t, comfy)
}

// berth batch --end hands the card on at once.
func TestEndBatchByHand(t *testing.T) {
	b, _, q := batchBroker(t)
	q.busy.Store(true)
	comfy := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
	b.Batch("comfyui", 0, 0)
	w := acquire(b, Request{Engine: "NInfer-27B", Group: "llama-swap", Priority: Foreground})
	pending(t, w)
	notDrained(t, comfy)

	if !b.EndBatch("comfyui") {
		t.Fatal("EndBatch found no batch")
	}
	drained(t, comfy)
	if b.EndBatch("comfyui") {
		t.Error("a second EndBatch found the batch again")
	}
}
