// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestScopeAllowed(t *testing.T) {
	for _, tc := range []struct {
		notify string
		want   bool
	}{
		{"", true},
		{"/run/user/1000/systemd/notify", false},
		{"@/org/freedesktop/systemd1/notify/123", false},
	} {
		env := func(k string) string {
			if k == "NOTIFY_SOCKET" {
				return tc.notify
			}
			return ""
		}
		if got := scopeAllowed(env); got != tc.want {
			t.Errorf("NOTIFY_SOCKET=%q: scopeAllowed = %v, want %v", tc.notify, got, tc.want)
		}
	}
}

// An operator who set slice for its limits gets an engine that can swap
// when the scope is dropped, so a slice is never dropped quietly: the
// line names the engine, the slice and the reason, at Warn. A plain run
// with no slice keeps the Debug line it always had.
func TestScopeDropWarnsTheSlice(t *testing.T) {
	argv := []string{"engine", "--flag"}
	for _, tc := range []struct {
		name    string
		allowed bool
		ready   bool
		slice   string
		want    string // level to expect; "" = no line at all
	}{
		{"notify service, slice", false, true, "strata.slice", "WARN"},
		{"no user manager, slice", true, false, "strata.slice", "WARN"},
		{"notify service, no slice", false, true, "", "DEBUG"},
		{"no user manager, no slice", true, false, "", "DEBUG"},
		{"scope taken", true, true, "strata.slice", ""},
	} {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		got := pickScopeArgv(tc.allowed, tc.ready, "fixture", tc.slice, argv, log)
		if strings.Join(got, " ") != strings.Join(argv, " ") {
			t.Errorf("%s: the engine's argv changed: %q", tc.name, got)
		}
		if tc.want == "" {
			if buf.Len() > 0 {
				t.Errorf("%s: a placed scope logged: %s", tc.name, buf.String())
			}
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("%s: no log line: %v (%q)", tc.name, err, buf.String())
		}
		if rec["level"] != tc.want {
			t.Errorf("%s: level = %v, want %s (%q)", tc.name, rec["level"], tc.want, buf.String())
		}
		if rec["engine"] != "fixture" {
			t.Errorf("%s: the line lost the engine: %q", tc.name, buf.String())
		}
		if tc.slice == "" {
			continue
		}
		if rec["slice"] != tc.slice {
			t.Errorf("%s: the line lost the slice: %q", tc.name, buf.String())
		}
		if msg, _ := rec["msg"].(string); !strings.Contains(msg, "slice is dropped") {
			t.Errorf("%s: the line does not name the drop: %q", tc.name, msg)
		}
	}
}
