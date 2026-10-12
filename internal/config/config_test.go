package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "berth.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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
