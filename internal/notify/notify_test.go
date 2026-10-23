// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

func TestForLevels(t *testing.T) {
	stuck := proto.Event{Kind: proto.EvStuck, Engine: "comfyui", Detail: "pid 41 (python) 520 MiB"}
	grant := proto.Event{Kind: proto.EvGranted, Engine: "NInfer-27B", Detail: "waited 2.6s"}

	n, ok := For(stuck, Error)
	if !ok || n != (Note{Kind: proto.EvStuck, Level: Error, Title: "GPU memory not released: comfyui", Body: "pid 41 (python) 520 MiB"}) {
		t.Fatalf("stuck at error = %+v %v", n, ok)
	}
	if n, ok := For(grant, Error); ok {
		t.Fatalf("a grant shows at the error level: %+v", n)
	}
	n, ok = For(grant, Info)
	if !ok || n.Title != "Card switched: NInfer-27B" || n.Body != "waited 2.6s" {
		t.Fatalf("grant at info = %+v %v", n, ok)
	}
	if _, ok := For(proto.Event{Kind: proto.EvRequested, Engine: "x"}, Info); ok {
		t.Fatal("a request shows at the info level")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]Level{"": Error, "error": Error, "INFO": Info} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("debug"); err == nil || err.Error() != `notify level "debug": use error or info` {
		t.Fatalf("ParseLevel(debug) err = %v", err)
	}
}

func TestGateShowsABurstOnce(t *testing.T) {
	g := Gate{Window: 5 * time.Second}
	t0 := time.Date(2026, 10, 23, 13, 0, 0, 0, time.UTC)
	a := Note{Kind: proto.EvSquatter, Title: "GPU memory held without a lease: mpv"}
	b := Note{Kind: proto.EvSquatter, Title: "GPU memory held without a lease: llama-server"}
	got := []bool{
		g.Let(a, t0),
		g.Let(a, t0.Add(2*time.Second)),
		g.Let(b, t0.Add(2*time.Second)),
		g.Let(a, t0.Add(5*time.Second)),
	}
	want := []bool{true, false, true, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Let = %v, want %v", got, want)
		}
	}
}

func TestRecentKeepsTheNewest(t *testing.T) {
	r := Recent{Max: 2}
	for _, title := range []string{"a", "b", "c"} {
		r.Add(Note{Title: title})
	}
	got := r.Notes()
	if len(got) != 2 || got[0].Title != "c" || got[1].Title != "b" {
		t.Fatalf("recent = %+v, want c then b", got)
	}
}
