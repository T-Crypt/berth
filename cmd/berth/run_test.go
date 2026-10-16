package main

import "testing"

// --keep-comfyui-up without --drain-comfyui is a silent no-op downstream
// (KeepComfyUI never fires without DrainComfyUI), so the CLI rejects it
// like the berth.toml validation does.
func TestCheckRunFlagsKeepNeedsDrain(t *testing.T) {
	if err := checkRunFlags(true, ""); err == nil {
		t.Fatal("keep without drain should error")
	}
	if err := checkRunFlags(true, "http://127.0.0.1:8188"); err != nil {
		t.Fatalf("keep with drain should pass, got %v", err)
	}
	if err := checkRunFlags(false, ""); err != nil {
		t.Fatalf("plain run should pass, got %v", err)
	}
}
