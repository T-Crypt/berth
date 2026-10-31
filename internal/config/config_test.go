// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "berth.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tomlList renders paths as a TOML array of strings.
func tomlList(paths ...string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = strconv.Quote(p)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func TestMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || len(c.Engine) != 0 {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

func TestSpeculumShapeLoads(t *testing.T) {
	c, err := Load(write(t, `
[metrics]
listen = "127.0.0.1:9797"
[engines]
every = "3s"
[[engine]]
name = "llama-swap"
type = "llama-swap"
url  = "http://127.0.0.1:9090"
[[engine]]
name = "mine"
type = "custom"
url  = "http://127.0.0.1:7000"
[engine.map]
output_tokens = "my_generated_total"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Engine) != 2 || c.Engines.Every.Seconds() != 3 || c.Engine[1].Map["output_tokens"] != "my_generated_total" {
		t.Fatalf("c = %+v", c)
	}
}

func TestEveryProblemReportedAtOnce(t *testing.T) {
	_, err := Load(write(t, `
[metrics]
listen = "0.0.0.0:9797"
[[engine]]
name = "a"
type = "vllm-ish"
url  = "ftp://x"
[[engine]]
name = "a"
type = "custom"
url  = "http://127.0.0.1:1"
`))
	if err == nil {
		t.Fatal("invalid config accepted")
	}
	for _, want := range []string{"not a loopback", "type \"vllm-ish\"", "url \"ftp://x\"", "duplicate name", "a custom engine needs at least one of"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	if _, err := Load(write(t, "[metrics]\nlisten = \"127.0.0.1:1\"\nlistne = \"x\"\n")); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("err = %v", err)
	}
}

func TestAlertsTableValidation(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr []string // substrings the error must contain; empty = must load
	}{
		{
			name: "speculum values, corrected semantics",
			toml: `
[alerts]
clear_polls = 5
[alerts.queue]
warn = 4
crit = 10
[alerts.ttft]
warn = 1000
crit = 2500
[alerts.tpot]
warn = 50
crit = 120
[alerts.p95]
warn = 5000
crit = 15000
[alerts.cache]
warn = 50
crit = 25
[alerts.reingest]
warn = 50
crit = 75
[alerts.vram]
warn = 0.85
crit = 0.95
[alerts.temp]
warn = 80
crit = 90
[alerts.power]
warn = 0.85
crit = 0.95
[alerts.kv]
warn = 0.75
crit = 0.90
`,
		},
		{
			name: "no alerts section at all",
			toml: "",
		},
		{
			name:    "crit below warn on ttft",
			toml:    "[alerts.ttft]\nwarn = 1000\ncrit = 900\n",
			wantErr: []string{"alerts.ttft: crit 900 must be >= warn 1000"},
		},
		{
			name:    "vram warn above 1",
			toml:    "[alerts.vram]\nwarn = 1.5\n",
			wantErr: []string{"alerts.vram: warn 1.5 must be in (0, 1]"},
		},
		{
			name:    "cache crit above warn",
			toml:    "[alerts.cache]\nwarn = 50\ncrit = 60\n",
			wantErr: []string{"alerts.cache: crit 60 must be < warn 50"},
		},
		{
			name:    "negative cache threshold",
			toml:    "[alerts.cache]\nwarn = -50\n",
			wantErr: []string{"alerts.cache: warn -50 must be > 0"},
		},
		{
			name:    "clear_polls zero",
			toml:    "[alerts]\nclear_polls = 0\n",
			wantErr: []string{"alerts.clear_polls must be >= 1"},
		},
		{
			name:    "unknown alert key rejected by the undecoded rule",
			toml:    "[alerts.bogus]\nwarn = 1\n",
			wantErr: []string{"unknown keys"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(write(t, tc.toml))
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid table accepted")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q:\n%v", want, err)
				}
			}
			_ = c
		})
	}
}

func TestAlertsTableDecoded(t *testing.T) {
	c, err := Load(write(t, `
[alerts]
clear_polls = 7
[alerts.queue]
warn = 4
crit = 10
[alerts.ttft]
warn = 1000
crit = 2500
[alerts.cache]
warn = 50
crit = 25
[alerts.kv]
warn = 0.75
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Alerts.ClearPolls == nil || *c.Alerts.ClearPolls != 7 {
		t.Errorf("ClearPolls = %v, want 7", c.Alerts.ClearPolls)
	}
	if c.Alerts.Queue.Warn != 4 || c.Alerts.Queue.Crit != 10 {
		t.Errorf("Queue = %+v", c.Alerts.Queue)
	}
	if c.Alerts.TTFT.Warn != 1000 || c.Alerts.TTFT.Crit != 2500 {
		t.Errorf("TTFT = %+v", c.Alerts.TTFT)
	}
	if c.Alerts.Cache.Warn != 50 || c.Alerts.Cache.Crit != 25 {
		t.Errorf("Cache = %+v", c.Alerts.Cache)
	}
	if c.Alerts.KV.Warn != 0.75 || c.Alerts.KV.Crit != 0 {
		t.Errorf("KV = %+v", c.Alerts.KV)
	}
}

func TestGatewayKeysValidated(t *testing.T) {
	_, err := Load(write(t, `
[[engine]]
name   = "strata"
type   = "llamacpp"
url    = "http://127.0.0.1:8080"
start  = ["strata-serve"]
serves = ["router-strata"]
[[engine]]
name     = "other"
type     = "llamacpp"
url      = "http://127.0.0.1:8081"
start    = ["other-serve"]
serves   = ["router-strata"]
priority = "urgent"
[[engine]]
name  = "nomodels"
type  = "llamacpp"
url   = "http://127.0.0.1:8082"
start = ["x"]
`))
	if err == nil {
		t.Fatal("invalid gateway keys accepted")
	}
	for _, want := range []string{`serves "router-strata", which "strata" already serves`, `priority "urgent"`, "start needs serves"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// The listen keys: [harbor] listen takes any well-formed host:port (a LAN
// address is the explicit opt-in), [gateway] listen is loopback only, both
// before any command's flag is considered.
func TestListenKeysValidated(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr []string // substrings the error must contain; empty = must load
	}{
		{
			name: "harbor loopback",
			toml: "[harbor]\nlisten = \"127.0.0.1:9798\"\n",
		},
		{
			name: "harbor loopback v6",
			toml: "[harbor]\nlisten = \"[::1]:9798\"\n",
		},
		{
			name: "harbor LAN address is the explicit opt-in",
			toml: "[harbor]\nlisten = \"0.0.0.0:50980\"\n",
		},
		{
			name:    "harbor address needs a host",
			toml:    "[harbor]\nlisten = \":9798\"\n",
			wantErr: []string{`harbor.listen: ":9798" is not a host:port address`},
		},
		{
			name:    "harbor address needs a port",
			toml:    "[harbor]\nlisten = \"127.0.0.1\"\n",
			wantErr: []string{"harbor.listen", "host:port"},
		},
		{
			name: "gateway loopback",
			toml: "[gateway]\nlisten = \"localhost:9799\"\n",
		},
		{
			name:    "gateway on the LAN is refused",
			toml:    "[gateway]\nlisten = \"0.0.0.0:9799\"\n",
			wantErr: []string{"gateway.listen", "not a loopback"},
		},
		{
			name:    "gateway address needs a port",
			toml:    "[gateway]\nlisten = \"127.0.0.1\"\n",
			wantErr: []string{`gateway.listen: address 127.0.0.1: missing port in address`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.toml))
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid listen accepted")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q:\n%v", want, err)
				}
			}
		})
	}
}

// The keys land in the config they name, for the commands to read.
func TestListenKeysDecoded(t *testing.T) {
	c, err := Load(write(t, "[harbor]\nlisten = \"0.0.0.0:50980\"\n\n[gateway]\nlisten = \"127.0.0.1:9799\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Harbor.Listen != "0.0.0.0:50980" || c.Gateway.Listen != "127.0.0.1:9799" {
		t.Errorf("harbor/gateway listen = %q / %q", c.Harbor.Listen, c.Gateway.Listen)
	}
}

func TestComfyUIKeysValidated(t *testing.T) {
	// A ComfyUI engine may carry a start command with no serves: the
	// gateway starts it for /prompt, not for a model name.
	c, err := Load(write(t, `
[[engine]]
name          = "comfyui"
type          = "comfyui"
url           = "http://127.0.0.1:8188"
start         = ["comfyui"]
drain_comfyui = "http://127.0.0.1:8188"
keep_comfyui_up = true
`))
	if err != nil {
		t.Fatalf("valid comfyui engine refused: %v", err)
	}
	if !c.Engine[0].KeepComfyUI || c.Engine[0].DrainComfyUI == "" {
		t.Fatalf("comfyui drain keys not loaded: %+v", c.Engine[0])
	}
	for _, bad := range []struct {
		name string
		body string
		want string
	}{
		{"serves", "serves = [\"pic\"]\n", "not through serves"},
		{"keep without drain", "keep_comfyui_up = true\n", "needs drain_comfyui"},
	} {
		_, err := Load(write(t, "[[engine]]\nname = \"c\"\ntype = \"comfyui\"\nurl = \"http://127.0.0.1:8188\"\n"+bad.body))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: error lacks %q: %v", bad.name, bad.want, err)
		}
	}
}

func TestPriorityValidated(t *testing.T) {
	c, err := Load(write(t, "[priority]\norder = [\"llama-swap\", \"strata\", \"comfyui\"]\njuggle_window = \"20s\"\n"))
	if err != nil || len(c.Priority.Order) != 3 || c.Priority.JuggleWindow.Seconds() != 20 {
		t.Fatalf("c=%+v err=%v", c.Priority, err)
	}
	_, err = Load(write(t, "[priority]\norder = [\"a\", \"\", \"a\"]\n"))
	for _, want := range []string{"priority.order[1] is empty", `priority.order lists "a" twice`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestGUIValidated(t *testing.T) {
	c, err := Load(write(t, `
[[engine]]
name  = "comfyui"
type  = "comfyui"
url   = "http://127.0.0.1:8188"
start = ["comfyui"]
gui   = "https://box.example.ts.net:8188/"
`))
	if err != nil {
		t.Fatalf("valid gui refused: %v", err)
	}
	if c.Engine[0].GUI != "https://box.example.ts.net:8188/" {
		t.Fatalf("gui not loaded: %+v", c.Engine[0])
	}
	for _, bad := range []struct {
		name string
		body string
		want string
	}{
		{"no start", "gui = \"http://127.0.0.1:8188/\"\n", "gui needs start"},
		{"not a url", "start = [\"c\"]\ngui = \"javascript:alert(1)\"\n", "must be an http(s) URL"},
	} {
		_, err := Load(write(t, "[[engine]]\nname = \"c\"\ntype = \"comfyui\"\nurl = \"http://127.0.0.1:8188\"\n"+bad.body))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: error lacks %q: %v", bad.name, bad.want, err)
		}
	}
}

func TestLibraryValidated(t *testing.T) {
	c, err := Load(write(t, `
[library]
free_floor = 0.25
[[library.root]]
path   = "/mnt/windows/ComfyUI/models"
shelf  = "image"
shared = "windows"
[[library.root]]
path = "/data/models"
`))
	if err != nil {
		t.Fatalf("valid library section rejected: %v", err)
	}
	if c.LibraryFloor() != 0.25 {
		t.Errorf("floor = %g, want the configured 0.25", c.LibraryFloor())
	}
	if len(c.Library.Root) != 2 || c.Library.Root[0].Shelf != "image" || c.Library.Root[0].Shared != "windows" {
		t.Errorf("roots = %+v", c.Library.Root)
	}
	_, err = Load(write(t, "[library]\nfree_floor = 1.5\n"))
	if err == nil || !strings.Contains(err.Error(), "free_floor") {
		t.Errorf("floor over 1: %v", err)
	}
	_, err = Load(write(t, "[library]\nfree_floor = -0.1\n"))
	if err == nil || !strings.Contains(err.Error(), "free_floor") {
		t.Errorf("negative floor: %v", err)
	}
	_, err = Load(write(t, "[[library.root]]\npath = \"/a\"\nshelf = \"video\"\n"))
	if err == nil || !strings.Contains(err.Error(), "shelf") {
		t.Errorf("bad shelf: %v", err)
	}
	_, err = Load(write(t, "[[library.root]]\n[[library.root]]\npath = \"/a\"\n"))
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("missing path: %v", err)
	}
	_, err = Load(write(t, "[[library.root]]\npath = \"/a\"\n[[library.root]]\npath = \"/a\"\n"))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate path: %v", err)
	}
}

func TestParkTableValidated(t *testing.T) {
	c, err := Load(write(t, `[park]
floor_gib = 8
max_gib   = 48
ttl       = "45m"
`))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.ParkFloorGiB(); f != 8 {
		t.Errorf("floor = %g, want 8", f)
	}
	if c.Park.MaxGiB != 48 {
		t.Errorf("max = %g, want 48", c.Park.MaxGiB)
	}
	if d := c.ParkTTL(); d != 45*time.Minute {
		t.Errorf("ttl = %v, want 45m", d)
	}
	// A resolved empty config carries the defaults.
	c, err = Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.ParkFloorGiB(); f != DefaultParkFloorGiB {
		t.Errorf("default floor = %g, want %g", f, DefaultParkFloorGiB)
	}
	if d := c.ParkTTL(); d != DefaultParkTTL {
		t.Errorf("default ttl = %v, want %v", d, DefaultParkTTL)
	}
	if c.Park.MaxGiB != 0 {
		t.Errorf("default max = %g, want 0 (no cap)", c.Park.MaxGiB)
	}
	for _, bad := range []struct {
		body string
		want string
	}{
		{`[park]
floor_gib = -1
`, "park.floor_gib"},
		{`[park]
max_gib = -2
`, "park.max_gib"},
		{`[park]
ttl = "-5s"
`, "park.ttl"},
	} {
		_, err := Load(write(t, bad.body))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("error lacks %q: %v", bad.want, err)
		}
	}
}

func TestEngineSliceKeyValidated(t *testing.T) {
	c, err := Load(write(t, `
[[engine]]
name  = "strata"
type  = "llamacpp"
url   = "http://127.0.0.1:8080"
slice = "strata.slice"
[[engine]]
name = "plain"
type = "llamacpp"
url  = "http://127.0.0.1:8081"
`))
	if err != nil {
		t.Fatalf("valid slice keys refused: %v", err)
	}
	if c.Engine[0].Slice != "strata.slice" || c.Engine[1].Slice != "" {
		t.Fatalf("slice keys not loaded: %+v", c.Engine)
	}
	for _, bad := range []string{`slice = "strata"`, `slice = "a/b.slice"`, `slice = ".slice"`} {
		_, err := Load(write(t, `
[[engine]]
name = "s"
type = "llamacpp"
url  = "http://127.0.0.1:8080"
`+bad+"\n"))
		if err == nil || !strings.Contains(err.Error(), "slice") {
			t.Errorf("%s: got %v, want a slice error", bad, err)
		}
	}
}

func TestEngineParkKeysValidated(t *testing.T) {
	c, err := Load(write(t, `
[[engine]]
name = "comfyui"
type = "comfyui"
url  = "http://127.0.0.1:8188"
park = "hook"
[[engine]]
name       = "vllm"
type       = "vllm"
url        = "http://127.0.0.1:8000"
park       = "hook"
park_url   = "http://127.0.0.1:8000/sleep?level=1"
resume_url = "http://127.0.0.1:8000/wake_up"
[[engine]]
name       = "mine"
type       = "custom"
url        = "http://127.0.0.1:9000"
health     = "/health"
park       = "hook"
park_url   = "http://127.0.0.1:9000/offload"
resume_url = "http://127.0.0.1:9000/reload"
[[engine]]
name = "idle"
type = "llamacpp"
url  = "http://127.0.0.1:8080"
[[engine]]
name = "proto"
type = "custom"
url  = "http://127.0.0.1:9100"
health = "/health"
park   = "hook"
`))
	if err != nil {
		t.Fatalf("valid park keys refused: %v", err)
	}
	if c.Engine[0].Park != "hook" || c.Engine[1].ParkURL == "" || c.Engine[2].ResumeURL == "" || c.Engine[3].Park != "" {
		t.Fatalf("park keys not loaded: %+v", c.Engine)
	}
	if c.Engine[4].Park != "hook" || c.Engine[4].ParkURL != "" || c.Engine[4].ResumeURL != "" {
		t.Fatalf("a hook with no URLs is valid: %+v", c.Engine[4])
	}
	for _, bad := range []struct {
		body string
		want string
	}{
		{`park = "ram"
`, `park "ram" is not off or hook`},
		{`park = "hook"
park_url = "http://127.0.0.1:9000/offload"
`, "needs both park_url and resume_url"},
		{`park = "hook"
park_url = "ftp://x"
resume_url = "http://127.0.0.1:9000/reload"
`, `park_url "ftp://x" must be an absolute http(s) URL`},
		{`park = "hook"
park_url = "127.0.0.1:9000/offload"
resume_url = "http://127.0.0.1:9000/reload"
`, `park_url "127.0.0.1:9000/offload" must be an absolute http(s) URL`},
	} {
		_, err := Load(write(t, "[[engine]]\nname = \"mine\"\ntype = \"custom\"\nurl = \"http://127.0.0.1:9000\"\nhealth = \"/health\"\n"+bad.body))
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("error lacks %q: %v", bad.want, err)
		}
	}
	// A kind with a built-in adapter needs no URLs; without them a
	// non-hook engine simply never parks.
	if _, err := Load(write(t, "[[engine]]\nname = \"v\"\ntype = \"vllm\"\nurl = \"http://127.0.0.1:8000\"\npark = \"hook\"\n")); err != nil {
		t.Errorf("built-in adapter refused without URLs: %v", err)
	}
}

func TestParkURLsSetButParkOffWarn(t *testing.T) {
	c, warns, err := Check(write(t, `
[[engine]]
name = "mine"
type = "llamacpp"
url  = "http://127.0.0.1:8080"
park_url   = "http://127.0.0.1:8080/offload"
resume_url = "http://127.0.0.1:8080/reload"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine[0].Park != "" {
		t.Fatalf("park = %q, want the empty default", c.Engine[0].Park)
	}
	got := map[string]bool{}
	for _, w := range warns {
		got[w.Key] = true
	}
	for _, key := range []string{"engine[0].park_url", "engine[0].resume_url"} {
		if !got[key] {
			t.Errorf("no warning for %s: %+v", key, warns)
		}
	}
}

func TestHookWithoutURLsTriesTheProtocol(t *testing.T) {
	// A kind outside the built-in table with park = "hook" and no URLs is
	// valid: the engine may speak the Berth park protocol, and the
	// warning says what Berth will try.
	const want = "proto: park = hook with no park_url/resume_url; Berth uses the Berth park protocol if the engine answers GET /berth/state, otherwise this engine drains as today"
	_, warns, err := Check(write(t, `
[[engine]]
name = "proto"
type = "custom"
url  = "http://127.0.0.1:9100"
health = "/health"
park   = "hook"
`))
	if err != nil {
		t.Fatalf("a hook with no URLs is valid: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("one warning, got %+v", warns)
	}
	if warns[0].Key != "engine[0].park" || warns[0].Message != want {
		t.Errorf("warning = %+v, want key engine[0].park and %q", warns[0], want)
	}

	// A kind in the table falls back to its adapter: no protocol warning.
	_, warns, err = Check(write(t, `
[[engine]]
name = "vllm"
type = "vllm"
url  = "http://127.0.0.1:8000"
park = "hook"
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warns {
		if strings.Contains(w.Message, "park protocol") {
			t.Errorf("a table kind gets no protocol warning: %+v", w)
		}
	}

	// A hook with both URLs is plain hook wiring: no protocol warning.
	_, warns, err = Check(write(t, `
[[engine]]
name = "mine"
type = "custom"
url  = "http://127.0.0.1:9000"
health = "/health"
park       = "hook"
park_url   = "http://127.0.0.1:9000/offload"
resume_url = "http://127.0.0.1:9000/reload"
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warns {
		if strings.Contains(w.Message, "park protocol") {
			t.Errorf("a hook with both URLs gets no protocol warning: %+v", w)
		}
	}
}

func TestEnginePrefetchParsedAndValidated(t *testing.T) {
	// Paths the platform itself calls absolute: one temp tree, spelled its way.
	abs := filepath.Join(t.TempDir(), "models")
	c, err := Load(write(t, "[[engine]]\nname = \"strata\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nstart = [\"/opt/strata/serve\"]\nserves = [\"router-strata\"]\nprefetch = "+tomlList(abs, abs+".gguf")+"\n"))
	if err != nil {
		t.Fatalf("a valid prefetch list was refused: %v", err)
	}
	if got := c.Engine[0].Prefetch; len(got) != 2 || got[0] != abs || got[1] != abs+".gguf" {
		t.Fatalf("prefetch = %v, want %q and %q", got, abs, abs+".gguf")
	}
	// An engine that names none asks for nothing.
	c, err = Load(write(t, "[[engine]]\nname = \"x\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Engine[0].Prefetch) != 0 {
		t.Fatalf("prefetch = %v, want none", c.Engine[0].Prefetch)
	}

	for _, tc := range []struct {
		prefetch string
		wantErr  string
	}{
		{`["models/a.gguf"]`, `engine[0] "x": prefetch "models/a.gguf" must be an absolute path`},
		{`["models/a.gguf", "../other"]`, `engine[0] "x": prefetch "../other" must be an absolute path`},
		{`[""]`, `engine[0] "x": prefetch "" must be an absolute path`},
		{`["."]`, `engine[0] "x": prefetch "." must be an absolute path`},
	} {
		_, err := Load(write(t, "[[engine]]\nname = \"x\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nprefetch = "+tc.prefetch+"\n"))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("prefetch %s: error lacks %q: %v", tc.prefetch, tc.wantErr, err)
		}
	}
	// The platform's own absolute shape is valid, alone or beside another.
	for _, list := range [][]string{{abs}, {abs + ".gguf", abs}} {
		if _, err := Load(write(t, "[[engine]]\nname = \"x\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nprefetch = "+tomlList(list...)+"\n")); err != nil {
			t.Errorf("prefetch %v refused: %v", list, err)
		}
	}
}

func TestLibraryFloorDefaults(t *testing.T) {
	var c Config
	if c.LibraryFloor() != DefaultFreeFloor {
		t.Errorf("empty config floor = %g, want the default %g", c.LibraryFloor(), DefaultFreeFloor)
	}
	off := 0.0
	c.Library.FreeFloor = &off
	if c.LibraryFloor() != 0 {
		t.Errorf("explicit 0 = %g, want the guard off", c.LibraryFloor())
	}
}

// kv_snapshots is an absolute directory the engine's --slot-save-path points
// at, with an open budget that takes the default; a relative path is refused
// and the size parses as given.
func TestEngineKVSnapshotsParsedAndValidated(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "kv")
	c, err := Load(write(t, "[[engine]]\nname = \"llama-server\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nkv_snapshots = '"+abs+"'\nkv_snapshots_mib = 4096\n"))
	if err != nil {
		t.Fatalf("a valid kv_snapshots was refused: %v", err)
	}
	if c.Engine[0].KVSnapshots != abs {
		t.Errorf("kv_snapshots = %q, want %q", c.Engine[0].KVSnapshots, abs)
	}
	if c.Engine[0].KVSnapshotsMiB != 4096 {
		t.Errorf("kv_snapshots_mib = %d, want 4096", c.Engine[0].KVSnapshotsMiB)
	}
	c, err = Load(write(t, "[[engine]]\nname = \"x\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engine[0].KVSnapshots != "" || c.Engine[0].KVSnapshotsMiB != 0 {
		t.Errorf("an engine that names no snapshot directory carries %q and %d",
			c.Engine[0].KVSnapshots, c.Engine[0].KVSnapshotsMiB)
	}
	if DefaultKVSnapshotsMiB != 16384 {
		t.Errorf("the open budget default is %d, want 16384", DefaultKVSnapshotsMiB)
	}

	for _, tc := range []struct {
		dir     string
		wantErr string
	}{
		{"kv", `engine[0] "x": kv_snapshots "kv" must be an absolute path`},
		{"./kv", `engine[0] "x": kv_snapshots "./kv" must be an absolute path`},
	} {
		_, err := Load(write(t, "[[engine]]\nname = \"x\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nkv_snapshots = \""+tc.dir+"\"\n"))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("kv_snapshots %q: error lacks %q: %v", tc.dir, tc.wantErr, err)
		}
	}
}

func TestEngineFallbackParsedAndValidated(t *testing.T) {
	const engines = `
[[engine]]
name   = "strata"
type   = "llamacpp"
url    = "http://127.0.0.1:8080"
serves = ["qwen3.8"]
%s
[[engine]]
name   = "strata-cpu"
type   = "llamacpp"
url    = "http://127.0.0.1:8081"
serves = ["qwen3.8-cpu"]
`
	c, err := Load(write(t, fmt.Sprintf(engines, `
fallback = "strata-cpu"
fallback_after = "30s"
fallback_model = "qwen3.8-cpu"
`)))
	if err != nil {
		t.Fatalf("a valid fallback was refused: %v", err)
	}
	if c.Engine[0].Fallback != "strata-cpu" || c.Engine[0].FallbackAfter.Seconds() != 30 || c.Engine[0].FallbackModel != "qwen3.8-cpu" {
		t.Fatalf("fallback keys not loaded: %+v", c.Engine[0])
	}
	// Without fallback_after the gateway applies its own default.
	c, err = Load(write(t, fmt.Sprintf(engines, `fallback = "strata-cpu"`)))
	if err != nil || c.Engine[0].FallbackAfter.Duration != 0 {
		t.Fatalf("no fallback_after: %+v err=%v", c.Engine[0], err)
	}

	for _, tc := range []struct {
		name    string
		keys    string
		wantErr string
	}{
		{"unknown engine", `fallback = "nope"`, `engine[0] "strata": fallback "nope" is not an engine in this file`},
		{"itself", `fallback = "strata"`, `engine[0] "strata": fallback names the engine itself`},
		{"negative after", `fallback = "strata-cpu"
fallback_after = "-5s"`, `engine[0] "strata": fallback_after -5s must be >= 0`},
	} {
		_, err := Load(write(t, fmt.Sprintf(engines, tc.keys)))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error lacks %q: %v", tc.name, tc.wantErr, err)
		}
	}
	// A fallback that has a fallback of its own is refused, and the error
	// names both engines.
	_, err = Load(write(t, fmt.Sprintf(engines, `fallback = "strata-cpu"`)+`
[[engine]]
name   = "third"
type   = "llamacpp"
url    = "http://127.0.0.1:8082"
fallback = "strata"
`))
	if err == nil || !strings.Contains(err.Error(), `engine[2] "third": fallback "strata" has a fallback of its own; no chains`) {
		t.Errorf("a chain was accepted or names neither engine: %v", err)
	}
}

func TestEngineParkTTL(t *testing.T) {
	const entry = "[[engine]]\nname = \"comfyui\"\ntype = \"comfyui\"\nurl = \"http://127.0.0.1:8188\"\n"
	c, err := Load(write(t, entry+"park_ttl = \"10m\"\n"))
	if err != nil {
		t.Fatalf("a valid park_ttl was refused: %v", err)
	}
	if got := c.Engine[0].ParkTTL.Duration; got != 10*time.Minute {
		t.Errorf("park_ttl = %s, want 10m", got)
	}
	if _, err := Load(write(t, entry+"park_ttl = \"-1m\"\n")); err == nil || !strings.Contains(err.Error(), `engine[0] "comfyui": park_ttl -1m0s must be >= 0`) {
		t.Errorf("a negative park_ttl: %v", err)
	}
}

// A background engine needs a start command, runs at batch only, and
// idle_after belongs to it alone.
func TestBackgroundEngine(t *testing.T) {
	const entry = `[[engine]]
name = "tagger"
type = "custom"
url  = "http://127.0.0.1:8000"
health = "/health"
`
	c, err := Load(write(t, entry+`start = ["docker", "start", "-a", "tagger"]
background = true
idle_after = "2m"
`))
	if err != nil || !c.Engine[0].Background || c.Engine[0].IdleAfter.Duration != 2*time.Minute {
		t.Fatalf("a valid background engine: %+v err=%v", c.Engine[0], err)
	}
	for _, tc := range []struct{ keys, wantErr string }{
		{"background = true\n", "background needs start"},
		{"start = [\"x\"]\nbackground = true\npriority = \"foreground\"\n", "runs at batch priority, not foreground"},
		{"start = [\"x\"]\nidle_after = \"1m\"\n", "idle_after needs background"},
	} {
		if _, err := Load(write(t, entry+tc.keys)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: error lacks %q: %v", tc.keys, tc.wantErr, err)
		}
	}
}

// on_busy = "queue" is for a ComfyUI engine the gateway can start.
func TestOnBusyQueue(t *testing.T) {
	const comfy = `[[engine]]
name = "comfyui"
type = "comfyui"
url  = "http://127.0.0.1:8188"
`
	if _, err := Load(write(t, comfy+"start = [\"comfy\"]\non_busy = \"queue\"\n")); err != nil {
		t.Fatalf("a queueing ComfyUI was refused: %v", err)
	}
	for _, tc := range []struct{ entry, wantErr string }{
		{comfy + "start = [\"comfy\"]\non_busy = \"drop\"\n", `on_busy "drop" is not wait or queue`},
		{comfy + "on_busy = \"queue\"\n", `on_busy = "queue" needs start`},
		{"[[engine]]\nname = \"chat\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8080\"\nstart = [\"x\"]\nserves = [\"m\"]\non_busy = \"queue\"\n", `is for ComfyUI engines`},
	} {
		if _, err := Load(write(t, tc.entry)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("error lacks %q: %v", tc.wantErr, err)
		}
	}
}

// [notify] decodes for berth notify, and a level it does not know is
// refused at load.
func TestNotifyKeys(t *testing.T) {
	c, err := Load(write(t, "[notify]\nenabled = false\nlevel = \"info\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Notify.Enabled == nil || *c.Notify.Enabled || c.Notify.Level != "info" {
		t.Fatalf("notify = %+v", c.Notify)
	}
	_, err = Load(write(t, "[notify]\nlevel = \"loud\"\n"))
	if err == nil || !strings.Contains(err.Error(), `notify.level: notify level "loud": use error or info`) {
		t.Fatalf("load err = %v", err)
	}
}

// park = "checkpoint" belongs on a berth run line, where --front names the
// engine's door; an entry would hand one front address to every run.
func TestParkCheckpointIsARunFlag(t *testing.T) {
	_, err := Load(write(t, "[[engine]]\nname = \"llama-swap\"\ntype = \"llama-swap\"\nurl = \"http://127.0.0.1:9090\"\npark = \"checkpoint\"\n"))
	if err == nil || !strings.Contains(err.Error(), `park = "checkpoint" goes on the engine's berth run line with --front`) {
		t.Fatalf("load err = %v", err)
	}
}

func TestLendValidated(t *testing.T) {
	head := func(typ string) string {
		return "[[engine]]\nname = \"e\"\ntype = \"" + typ + "\"\nurl = \"http://127.0.0.1:9000\"\nhealth = \"/health\"\n"
	}
	c, err := Load(write(t, head("strata")+"lend = \"hook\"\n"))
	if err != nil {
		t.Fatalf("strata lends with no lend_url: %v", err)
	}
	if c.Engine[0].Lend != "hook" || c.Engine[0].LendURL != "" {
		t.Fatalf("lend keys not loaded: %+v", c.Engine[0])
	}
	c, err = Load(write(t, head("custom")+"lend = \"hook\"\nlend_url = \"http://127.0.0.1:9000\"\n"))
	if err != nil {
		t.Fatalf("a lend_url is valid on any engine: %v", err)
	}
	if c.Engine[0].LendURL != "http://127.0.0.1:9000" {
		t.Fatalf("lend_url = %q", c.Engine[0].LendURL)
	}
	for _, bad := range []struct {
		body string
		want string
	}{
		{head("custom") + "lend = \"hook\"\n", `lend = "hook" for a custom engine needs lend_url, the base of the Berth lend protocol`},
		{head("custom") + "lend_url = \"http://127.0.0.1:9000\"\n", `lend_url needs lend = "hook"`},
		{head("custom") + "lend = \"maybe\"\n", `lend "maybe" is not off or hook`},
		{head("custom") + "lend = \"hook\"\nlend_url = \"127.0.0.1:9000\"\n", `lend_url "127.0.0.1:9000" must be an absolute http(s) URL`},
	} {
		if _, err := Load(write(t, bad.body)); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("error lacks %q: %v", bad.want, err)
		}
	}
}

// A Compose engine is one absolute Compose file, never also a container: the
// project names its own containers.
func TestEngineComposeParsedAndValidated(t *testing.T) {
	const base = "[[engine]]\nname = \"tagger\"\ntype = \"custom\"\nurl = \"http://127.0.0.1:8000\"\nhealth = \"/healthz\"\n"
	abs := filepath.Join(t.TempDir(), "compose.yaml")
	c, err := Load(write(t, base+"compose = '"+abs+"'\n"))
	if err != nil {
		t.Fatalf("a valid compose was refused: %v", err)
	}
	if c.Engine[0].Compose != abs {
		t.Fatalf("compose = %q, want %q", c.Engine[0].Compose, abs)
	}

	for _, tc := range []struct {
		engine  string
		wantErr string
	}{
		{"compose = \"compose.yaml\"", `engine[0] "tagger": compose "compose.yaml" must be an absolute path`},
		{"compose = \"./compose.yaml\"", `engine[0] "tagger": compose "./compose.yaml" must be an absolute path`},
		{"compose = \"/srv/tagger/compose.yaml\"\ncontainer = \"tagger\"",
			`engine[0] "tagger": compose and container are mutually exclusive`},
	} {
		_, err := Load(write(t, base+tc.engine+"\n"))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error lacks %q: %v", tc.engine, tc.wantErr, err)
		}
	}
}

// gpu = names the card an engine runs on: a node card name or a PCI address,
// or a comma list of them for one lease on several cards.
func TestEngineGPUParsedAndValidated(t *testing.T) {
	const base = "[[engine]]\nname = \"maya\"\ntype = \"llamacpp\"\nurl = \"http://127.0.0.1:8000\"\n"
	for _, ok := range []string{"gpu1", "gpu12", "0000:41:00.0", "gpu0,gpu1", "gpu3,0000:41:00.0"} {
		c, err := Load(write(t, base+"gpu = \""+ok+"\"\n"))
		if err != nil || c.Engine[0].GPU != ok {
			t.Errorf("gpu = %q: %v, loaded %q", ok, err, c.Engine[0].GPU)
		}
	}
	for _, bad := range []string{"1", "gpu", "card1", "GPU1", "41:00.0", "gpu0,", "gpu0,,gpu1", "gpu0, gpu1", "gpu0,gpu0"} {
		_, err := Load(write(t, base+"gpu = \""+bad+"\"\n"))
		if err == nil || !strings.Contains(err.Error(), `engine[0] "maya": gpu "`+bad+`"`) {
			t.Errorf("gpu = %q: error %v, want it refused", bad, err)
		}
	}
}

func TestSquattersAllow(t *testing.T) {
	c, err := Load(write(t, "[squatters]\nallow = [\"chrome\", \"WindowsTerminal\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Squatters.Allow, ","); got != "chrome,WindowsTerminal" {
		t.Errorf("allow = %q, want chrome,WindowsTerminal", got)
	}
}
