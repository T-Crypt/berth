package cli

import (
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
