package config

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func TestResolveMaterialisesDefaults(t *testing.T) {
	c := Resolve(Config{})
	if c.Engines.Every.Duration != DefaultEvery {
		t.Errorf("every = %s, want %s", c.Engines.Every.Duration, DefaultEvery)
	}
	if c.Engines.Discover == nil || !*c.Engines.Discover {
		t.Errorf("discover = %v, want true", c.Engines.Discover)
	}
	if c.Alerts.ClearPolls == nil || *c.Alerts.ClearPolls != DefaultClearPolls {
		t.Errorf("clear polls = %v, want %d", c.Alerts.ClearPolls, DefaultClearPolls)
	}
	if c.Priority.JuggleWindow.Duration != DefaultJuggleWindow {
		t.Errorf("juggle window = %s, want %s", c.Priority.JuggleWindow.Duration, DefaultJuggleWindow)
	}
}

// A negative juggle window is a decision (juggles off), not a missing value:
// Resolve must leave it alone.
func TestResolveKeepsNegativeJuggleWindow(t *testing.T) {
	c := Config{}
	c.Priority.JuggleWindow.Duration = -1 * time.Second
	if got := Resolve(c); got.Priority.JuggleWindow.Duration != -1*time.Second {
		t.Errorf("juggle window = %s, want -1s", got.Priority.JuggleWindow.Duration)
	}
}

func TestResolveKeepsDiscoveryOff(t *testing.T) {
	off := false
	c := Config{}
	c.Engines.Discover = &off
	if got := Resolve(c); got.Engines.Discover == nil || *got.Engines.Discover {
		t.Errorf("discover = %v, want false", got.Engines.Discover)
	}
}

func TestCheckMissingFileIsDefaults(t *testing.T) {
	c, warns, err := Check(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
	if c.Engines.Every.Duration != DefaultEvery {
		t.Errorf("every = %s, want the default %s", c.Engines.Every.Duration, DefaultEvery)
	}
}

func TestLoadResolvesDefaults(t *testing.T) {
	c, err := Load(write(t, "[metrics]\nlisten = \"127.0.0.1:9797\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Engines.Every.Duration != DefaultEvery || c.Priority.JuggleWindow.Duration != DefaultJuggleWindow {
		t.Errorf("unresolved defaults: every %s, window %s", c.Engines.Every.Duration, c.Priority.JuggleWindow.Duration)
	}
}

func TestWarnings(t *testing.T) {
	cases := []struct {
		name string
		body string
		key  string
		want string // a substring of the message; "" means no warning
	}{
		{
			name: "discovery off with nothing configured",
			body: "[engines]\ndiscover = false\n",
			key:  "engines.discover", want: "will read nothing",
		},
		{
			name: "discovery off with an engine configured",
			body: "[engines]\ndiscover = false\n[[engine]]\n" + `name = "a"` + "\ntype = \"openai\"\nurl = \"http://127.0.0.1:8080\"\n",
		},
		{
			name: "a sub-second re-read",
			body: "[engines]\nevery = \"500ms\"\n",
			key:  "engines.every", want: "500ms",
		},
		{
			name: "a one second re-read",
			body: "[engines]\nevery = \"1s\"\n",
		},
		{
			name: "two engines on one url",
			body: "[engines]\ndiscover = false\n[[engine]]\n" + `name = "a"` + "\ntype = \"openai\"\nurl = \"http://127.0.0.1:8080\"\n[[engine]]\n" + `name = "b"` + "\ntype = \"openai\"\nurl = \"http://127.0.0.1:8080/\"\n",
			key:  "engine[1].url", want: "one server, two cards",
		},
		{
			name: "comfyui without a job boundary",
			body: "[engines]\ndiscover = false\n[[engine]]\n" + `name = "comfyui"` + "\ntype = \"comfyui\"\nurl = \"http://127.0.0.1:8188\"\n",
			key:  "engine[0]", want: "drain_comfyui waits for the job boundary",
		},
		{
			name: "comfyui with a job boundary",
			body: "[engines]\ndiscover = false\n[[engine]]\n" + `name = "comfyui"` + "\ntype = \"comfyui\"\nurl = \"http://127.0.0.1:8188\"\ndrain_comfyui = \"http://127.0.0.1:8188\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, warns, err := Check(write(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(warns) != 0 {
					t.Fatalf("warnings = %v, want none", warns)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warns)
			}
			if warns[0].Key != tc.key {
				t.Errorf("key = %q, want %q", warns[0].Key, tc.key)
			}
			if !strings.Contains(warns[0].Message, tc.want) {
				t.Errorf("message = %q, want it to contain %q", warns[0].Message, tc.want)
			}
		})
	}
}

// One duration spelling in all three encodings: the file's own.
func TestDurationMarshalsAsString(t *testing.T) {
	c, err := Load(write(t, "[engines]\nevery = \"90s\"\ndiscover = false\n[priority]\njuggle_window = \"-2s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"every":"1m30s"`, `"juggle_window":"-2s"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json %s lacks %s", b, want)
		}
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Engines.Every.Duration != 90*time.Second || back.Priority.JuggleWindow.Duration != -2*time.Second {
		t.Errorf("round trip = %s / %s", back.Engines.Every, back.Priority.JuggleWindow)
	}

	y, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "every: 1m30s") {
		t.Errorf("yaml %s lacks every: 1m30s", y)
	}
	if err := yaml.Unmarshal(y, &Config{}); err != nil {
		t.Fatal(err)
	}

	var tomlBuf bytes.Buffer
	if err := toml.NewEncoder(&tomlBuf).Encode(c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tomlBuf.String(), `every = "1m30s"`) {
		t.Errorf("toml %s lacks every = \"1m30s\"", tomlBuf.String())
	}
	var tomlBack Config
	if _, err := toml.Decode(tomlBuf.String(), &tomlBack); err != nil {
		t.Fatal(err)
	}
	if tomlBack.Engines.Every.Duration != 90*time.Second {
		t.Errorf("toml round trip every = %s, want 90s", tomlBack.Engines.Every)
	}
}

func TestDurationRejectsANumber(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte("5\n"), &d); err == nil {
		t.Error("yaml: a bare number should not load as a duration")
	}
	if err := json.Unmarshal([]byte("5"), &d); err == nil {
		t.Error("json: a bare number should not load as a duration")
	}
}
