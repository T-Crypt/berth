// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package engines

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
)

func TestBusy(t *testing.T) {
	now := time.Now()
	w := &Watcher{Engines: []config.Engine{{Name: "llama-swap"}, {Name: "comfyui"}, {Name: "strata"}, {Name: "old"}}}
	w.state = map[string]proto.Engine{
		"llama-swap": {Name: "llama-swap", Up: true, At: now,
			Models: []proto.Model{{Name: "NInfer-Qwen3.8-27B", State: "ready"}},
			Slots:  []proto.Slot{{Model: "NInfer-Qwen3.8-27B", Processing: true}}},
		"comfyui": {Name: "comfyui", Up: true, At: now, Metrics: proto.EngineMetrics{InFlightReported: true}},
		"strata":  {Name: "strata", Up: false, At: now},
		"old":     {Name: "old", Up: true, At: now.Add(-time.Hour)},
	}
	for _, c := range []struct {
		name        string
		busy, known bool
	}{
		{"llama-swap", true, true},
		{"NInfer-Qwen3.8-27B", true, true}, // a model name finds its engine
		{"comfyui", false, true},
		{"strata", false, false}, // down or loading: nobody can say it is idle
		{"old", false, false},    // stale reading
		{"nobody", false, false},
	} {
		if b, k := w.Busy(c.name); b != c.busy || k != c.known {
			t.Errorf("Busy(%q) = %v,%v want %v,%v", c.name, b, k, c.busy, c.known)
		}
	}
	w.state["comfyui"] = proto.Engine{Name: "comfyui", Up: true, At: now, Metrics: proto.EngineMetrics{Waiting: 2}}
	if b, k := w.Busy("comfyui"); !b || !k {
		t.Error("queued ComfyUI work is not busy")
	}
	w.state["llama-swap"] = proto.Engine{Name: "llama-swap", Up: true, At: now, Models: []proto.Model{{Name: "m", State: "starting"}}}
	if b, k := w.Busy("llama-swap"); !b || !k {
		t.Error("a model still loading is not busy")
	}
	var nilW *Watcher
	if b, k := nilW.Busy("x"); b || k {
		t.Error("a nil watcher claimed to know")
	}
}

// An engine whose reading carries no request-in-flight figure cannot be
// called idle: the NInfer build on aphotic answers /metrics with only its
// own ninfer_* counters and serves no /slots, so a request it is serving
// reads exactly like no request. Busy says unknown for it, and idle again
// once a reading names the in-flight count.
func TestBusyIsUnknownWithoutAnInFlightFigure(t *testing.T) {
	ninfer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprintln(w, `ninfer_requests_total{outcome="ok"} 12`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ninfer.Close()
	gauged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprintln(w, "llamacpp:requests_processing 0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer gauged.Close()

	w := &Watcher{Engines: []config.Engine{
		{Name: "old-ninfer", Type: "ninfer", URL: ninfer.URL},
		{Name: "gauged", Type: "ninfer", URL: gauged.URL},
	}}
	w.PollOnce(context.Background())
	for _, c := range []struct {
		name        string
		busy, known bool
	}{
		{"old-ninfer", false, false},
		{"gauged", false, true},
	} {
		if b, k := w.Busy(c.name); b != c.busy || k != c.known {
			t.Errorf("Busy(%q) = %v,%v want %v,%v", c.name, b, k, c.busy, c.known)
		}
	}
}
