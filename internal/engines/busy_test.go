package engines

import (
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
		"comfyui": {Name: "comfyui", Up: true, At: now},
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
