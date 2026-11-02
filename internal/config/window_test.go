// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/T-Crypt/berth/internal/proto"
)

// window on an engine entry: the string reads into the window berthd leases
// by, an entry with none is the common case, and a bad one is a load error
// naming the key.
func TestEngineWindowKey(t *testing.T) {
	const entry = "[[engine]]\nname = \"comfyui\"\ntype = \"comfyui\"\nurl = \"http://127.0.0.1:8188\"\n"
	c, err := Load(write(t, entry+"window = \"23:00-07:00\"\n"))
	if err != nil {
		t.Fatalf("a valid window was refused: %v", err)
	}
	if got := c.Engine[0].Window; !got.Set() || got.String() != "23:00-07:00" {
		t.Errorf("window = %s, want 23:00-07:00", got)
	}
	c, err = Load(write(t, entry))
	if err != nil {
		t.Fatalf("an entry with no window was refused: %v", err)
	}
	if c.Engine[0].Window.Set() {
		t.Errorf("window = %s, want none", c.Engine[0].Window)
	}
	for _, bad := range []struct{ in, wantErr string }{
		{`window = "7:00-9:00"`, `window "7:00-9:00": "7:00" is not HH:MM`},
		{`window = "10:00-10:00"`, `window "10:00-10:00" starts when it ends`},
	} {
		if _, err := Load(write(t, entry+bad.in+"\n")); err == nil || !strings.Contains(err.Error(), bad.wantErr) {
			t.Errorf("%s: error lacks %q: %v", bad.in, bad.wantErr, err)
		}
	}
}

// The window keeps its "HH:MM-HH:MM" spelling in every encoding the config
// has (TOML reads it through the same text codec), and an entry with no
// window reads back as no window.
func TestWindowKeepsItsSpelling(t *testing.T) {
	for _, enc := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
		want      string
	}{
		{"json", json.Marshal, json.Unmarshal, `"window":"23:00-07:00"`},
		{"yaml", yaml.Marshal, yaml.Unmarshal, "window: 23:00-07:00"},
	} {
		b, err := enc.marshal(Engine{Name: "comfyui", Window: proto.Window{Start: 23 * 60, End: 7 * 60}})
		if err != nil {
			t.Fatalf("%s: %v", enc.name, err)
		}
		if !strings.Contains(string(b), enc.want) {
			t.Errorf("%s: %s lacks %s", enc.name, b, enc.want)
		}
		var back Engine
		if err := enc.unmarshal(b, &back); err != nil {
			t.Fatalf("%s back: %v", enc.name, err)
		}
		if got := back.Window.String(); got != "23:00-07:00" {
			t.Errorf("%s: window reads back %q, want 23:00-07:00", enc.name, got)
		}
		b, err = enc.marshal(Engine{Name: "comfyui"})
		if err != nil {
			t.Fatal(err)
		}
		var none Engine
		if err := enc.unmarshal(b, &none); err != nil {
			t.Fatalf("%s: an engine with no window: %v (%s)", enc.name, err, b)
		}
		if none.Window.Set() {
			t.Errorf("%s: window = %s, want none", enc.name, none.Window)
		}
	}
}
