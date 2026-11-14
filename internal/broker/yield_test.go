// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
)

// A share of a lease on several cards cannot park, so a drain that skipped
// the job boundary would stop the engine mid-job. The yield is refused with
// the reason, and its run never hears it.
func TestYieldRefusesALeaseOnSeveralCards(t *testing.T) {
	bs := node(t, "gpu0", "gpu1")
	got := <-acquireAll(bs, Request{Engine: "comfyui", Priority: Foreground})
	if got.err != nil {
		t.Fatal(got.err)
	}
	for _, l := range got.ls {
		l.Attach(100)
	}
	for _, b := range bs {
		_, err := b.Yield("comfyui", time.Minute)
		if err == nil || IsNotFound(err) || !strings.Contains(err.Error(), "several cards") {
			t.Fatalf("yield on %s = %v, want a refusal that names the cards", b.cfg.Card, err)
		}
	}
	for _, l := range got.ls {
		select {
		case d := <-l.Yields():
			t.Fatalf("the run heard a yield of %s it cannot act on", d)
		default:
		}
	}
}

// A lease on one card hears its yield, and the newest yield replaces one
// the run has not read yet.
func TestYieldReachesItsRunNewestFirst(t *testing.T) {
	b, _, _ := setup(t, procs.Fake{Starts: map[int]uint64{100: 7}})
	l := must(t, acquire(b, Request{Engine: "comfyui", Priority: Foreground}))
	l.Attach(100)
	if _, err := b.Yield("comfyui", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Yield("comfyui", 0); err != nil {
		t.Fatal(err)
	}
	if d := <-l.Yields(); d != 0 {
		t.Fatalf("the run heard %s, want the newer end", d)
	}
}
