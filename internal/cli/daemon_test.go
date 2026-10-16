package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
)

// broker.comfySquat fires only for engines this list names; a dropped entry
// silently turns every squat into a contested grant.
func TestComfyEngines(t *testing.T) {
	cfg := config.Config{Engine: []config.Engine{
		{Name: "llama-swap", Type: "llama-swap", URL: "http://127.0.0.1:9090"},
		{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:8188"},
		{Name: "ollama", Type: "ollama", URL: "http://127.0.0.1:11434"},
	}}
	got := comfyEngines(cfg)
	want := []broker.ComfyEngine{{Name: "comfyui", URL: "http://127.0.0.1:8188"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("comfyEngines = %+v, want %+v", got, want)
	}
	if g := comfyEngines(config.Config{}); len(g) != 0 {
		t.Errorf("empty config = %+v, want none", g)
	}
}

// The arming line is emitted for the empty case too: a silently empty list is
// the #19 regression (every squat becomes a contested grant), and only this
// line can say so in the daemon log.
func TestLogComfyArmed(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	logComfyArmed(log, nil)
	if !strings.Contains(buf.String(), "not armed") {
		t.Fatalf("empty list log = %q, want a not-armed line", buf.String())
	}

	buf.Reset()
	logComfyArmed(log, []broker.ComfyEngine{{Name: "comfyui", URL: "http://127.0.0.1:8188"}})
	out := buf.String()
	if !strings.Contains(out, "armed") || !strings.Contains(out, "comfyui") || !strings.Contains(out, "http://127.0.0.1:8188") {
		t.Fatalf("armed log = %q, want the engine name and url", out)
	}
}
