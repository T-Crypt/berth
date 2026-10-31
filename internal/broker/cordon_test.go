// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/procs"
)

// A cordoned card grants nothing new but a hold, keeps its holder, and
// says why in the waiter's reason; uncordoning lets the waiter in.
func TestCordonStopsNewGrantsUntilUncordoned(t *testing.T) {
	b, _ := startTurnBroker(t, procs.Map{})
	holder := must(t, acquire(b, Request{Engine: "llm", Priority: Batch}))
	b.Cordon(true)
	if st := b.Status(); len(st.GPUs) == 0 || !st.GPUs[0].Cordoned {
		t.Fatalf("status does not show the cordon: %+v", st.GPUs)
	}
	holder.Release()

	w := acquireW(b, Request{Engine: "image", Priority: Foreground})
	pending(t, w.ch)
	if r := firstReason(t, w); !strings.Contains(r, "cordoned") {
		t.Errorf("reason = %q, want the cordon named", r)
	}
	hold := must(t, acquire(b, Request{Engine: "game", Priority: Hold}))
	hold.Release()
	pending(t, w.ch)

	b.Cordon(false)
	b.Tick(t.Context())
	must(t, w.ch)
	if st := b.Status(); st.GPUs[0].Cordoned {
		t.Error("status still shows the cordon")
	}
}
