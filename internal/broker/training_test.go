// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runs"
)

// refusedNow is the refusal a request gets at once, well inside its own wait.
func refusedNow(t *testing.T, what string, ch chan result) string {
	t.Helper()
	select {
	case r := <-ch:
		if !errors.Is(r.err, ErrRefused) {
			t.Fatalf("%s: got lease=%v err=%v, want a refusal", what, r.l, r.err)
		}
		return r.err.Error()
	case <-time.After(time.Second):
		t.Fatalf("%s was not refused at once", what)
	}
	return ""
}

// While a training holds the card, every other request is refused at once,
// a hold included, so nothing waits on the card or drains the run. Another
// training queues and gets the card when the first one ends.
func TestTrainingRefusesEverythingButATraining(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	train := must(t, acquire(b, Request{Engine: "train-a", Training: true}))

	why := refusedNow(t, "a foreground request", acquire(b, Request{Engine: "chat", Priority: Foreground, Wait: time.Hour}))
	if !strings.Contains(why, "locked for training by train-a") {
		t.Errorf("refusal %q does not name the training", why)
	}
	refusedNow(t, "a hold", acquire(b, Request{Engine: "hold:rogue", Priority: Hold, Pinned: true, Wait: time.Hour}))
	refusedNow(t, "a pinned run", acquire(b, Request{Engine: "smoke", Priority: Foreground, Pinned: true, Wait: time.Hour}))

	next := acquire(b, Request{Engine: "train-b", Training: true})
	pending(t, next)
	notDrained(t, train)

	train.Release()
	granted(t, b, "train-b", next)
}

// A training that arrives refuses the waiters already queued for the card,
// so a request does not sit out its wait behind a run that can take hours.
// The holder it replaces is drained as a hold drains it.
func TestTrainingLocksOutTheWaitersAlreadyQueued(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "dirk", Priority: Foreground}))
	chat := acquire(b, Request{Engine: "chat", Priority: Interactive, Wait: time.Hour})
	pending(t, chat)

	train := acquire(b, Request{Engine: "train-a", Training: true})
	refusedNow(t, "the queued chat", chat)
	drained(t, holder)
	holder.Release()
	granted(t, b, "train-a", train)
}

// A training never drains a pinned holder (a game, the operator's hold): it
// waits for it, and the card is locked for everything else meanwhile.
func TestTrainingWaitsForAPinnedHolderAndLocksTheCard(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	game := must(t, acquire(b, Request{Engine: "game", Priority: Hold, Pinned: true}))
	train := acquire(b, Request{Engine: "train-a", Training: true})
	pending(t, train)
	notDrained(t, game)

	refusedNow(t, "a request while the training waits", acquire(b, Request{Engine: "chat", Priority: Foreground, Wait: time.Hour}))
	game.Release()
	granted(t, b, "train-a", train)
}

// A training has no wait deadline, whatever the request or the node says.
func TestTrainingWaitsWithNoDeadline(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	for _, r := range []Request{{Training: true}, {Training: true, Wait: time.Second}} {
		if got := b.waitFor(r); got >= 0 {
			t.Errorf("waitFor(%+v) = %v, want no deadline", r, got)
		}
	}
}

// The status names a training lease as one, at hold priority and pinned.
func TestStatusCarriesTheTraining(t *testing.T) {
	b, _, _ := setup(t, procs.Map{})
	must(t, acquire(b, Request{Engine: "train-a", Training: true}))
	ls := b.Status().Leases
	if len(ls) != 1 || !ls[0].Training || !ls[0].Pinned || ls[0].Priority != "hold" {
		t.Fatalf("leases = %+v, want train-a as a pinned hold training", ls)
	}
}

// On a node with several cards the lock covers the cards the training
// holds: a request there is refused at once, and another card keeps serving.
func TestTrainingOnSeveralCardsLocksOnlyItsCards(t *testing.T) {
	bs := node(t, "gpu0", "gpu1", "gpu2")
	r := <-acquireAll(bs[:2], Request{Engine: "train-a", Training: true})
	if r.err != nil || len(r.ls) != 2 {
		t.Fatalf("grant = %+v", r)
	}

	refusedNow(t, "chat on gpu1", acquire(bs[1], Request{Engine: "chat", Priority: Foreground, Wait: time.Hour}))
	gang := acquireAll(bs[1:], Request{Engine: "maya", Priority: Foreground, Wait: time.Hour})
	select {
	case got := <-gang:
		if !errors.Is(got.err, ErrRefused) {
			t.Fatalf("maya on gpu1+gpu2: %+v, want a refusal", got)
		}
	case <-time.After(time.Second):
		t.Fatal("maya on gpu1+gpu2 was not refused at once")
	}
	must(t, acquire(bs[2], Request{Engine: "voice", Priority: Foreground}))
	for _, l := range r.ls {
		notDrained(t, l)
	}
}

// A training on several cards that arrives refuses a gang already queued on
// one of them.
func TestTrainingLocksOutAQueuedGang(t *testing.T) {
	bs := node(t, "gpu0", "gpu1")
	holder := must(t, acquire(bs[0], Request{Engine: "game", Priority: Hold, Pinned: true}))
	gang := acquireAll(bs, Request{Engine: "maya", Priority: Foreground, Wait: time.Hour})
	time.Sleep(50 * time.Millisecond)

	train := acquireAll(bs, Request{Engine: "train-a", Training: true})
	select {
	case got := <-gang:
		if !errors.Is(got.err, ErrRefused) {
			t.Fatalf("maya: %+v, want a refusal", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the queued gang was not refused when the training arrived")
	}
	holder.Release()
	tickAll(bs)
	select {
	case got := <-train:
		if got.err != nil || len(got.ls) != 2 {
			t.Fatalf("training grant = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the training was not granted once the game ended")
	}
}

// trainingLost is a node whose training (pid 200) lost the last daemon: its
// record is on disk, and this daemon's re-attach window is long past.
func trainingLost(t *testing.T) *Broker {
	t.Helper()
	b, _, _ := setup(t, procs.Fake{Starts: map[int]uint64{200: 9}})
	b.cfg.RunsDir = t.TempDir()
	b.cfg.ReattachWindow = time.Nanosecond
	if _, err := runs.Write(b.cfg.RunsDir, runs.Record{
		Engine: "train-a", PID: 200, Start: 9, Started: time.Now(), Reason: runs.ReasonLost, Training: true,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	b.Tick(context.Background())
	return b
}

// A training that has not re-attached yet still locks the card after a
// daemon restart, with no time limit: other requests are refused at once.
func TestTrainingLocksTheCardUntilItReattaches(t *testing.T) {
	b := trainingLost(t)
	why := refusedNow(t, "a request before the training re-attaches", acquire(b, Request{Engine: "chat", Priority: Foreground, Wait: time.Hour}))
	if !strings.Contains(why, "locked for training by train-a") {
		t.Errorf("refusal %q does not name the training", why)
	}

	l, err := b.Reattach(Request{Engine: "train-a", Training: true}, 200, 9)
	if err != nil {
		t.Fatalf("the training's re-attach: %v", err)
	}
	refusedNow(t, "a request after the training re-attached", acquire(b, Request{Engine: "chat", Priority: Foreground, Wait: time.Hour}))
	l.Release()
}

// The lock ends with the training: once its process is gone, the record
// goes and the card serves again.
func TestTrainingRecordLocksOnlyWhileItsProcessLives(t *testing.T) {
	b := trainingLost(t)
	b.tree = procs.Fake{Starts: map[int]uint64{}}
	b.Tick(context.Background())
	must(t, acquire(b, Request{Engine: "chat", Priority: Foreground}))
}
