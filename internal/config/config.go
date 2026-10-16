// Package config reads berth.toml. The file is optional: without one, the
// daemon runs on its flags and watches no engines. Engines use the same
// [[engine]] shape as Speculum's config, so a Speculum user can copy theirs.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/T-Crypt/berth/internal/proto"
)

// Engine is one inference engine this node reads metrics from. Reading is
// all it does: Berth never sends an engine a prompt.
type Engine struct {
	Name string `toml:"name" json:"name" yaml:"name"`
	Type string `toml:"type" json:"type" yaml:"type"` // one of Types
	URL  string `toml:"url" json:"url" yaml:"url"`    // any host and port
	// APIKeyEnv names an environment variable holding a bearer token for
	// engines that want one; the token itself never goes in the file.
	APIKeyEnv string `toml:"api_key_env,omitempty" json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	// Custom engines: every path is optional. Health decides up/down (else
	// metrics or models do, else GET /); Models is an OpenAI-style list;
	// Slots is a llama.cpp-style /slots; Map names the metrics to read.
	Health  string            `toml:"health,omitempty" json:"health,omitempty" yaml:"health,omitempty"`
	Models  string            `toml:"models,omitempty" json:"models,omitempty" yaml:"models,omitempty"`
	Metrics string            `toml:"metrics,omitempty" json:"metrics,omitempty" yaml:"metrics,omitempty"` // path of Prometheus text; custom default /metrics when Map is set
	Slots   string            `toml:"slots,omitempty" json:"slots,omitempty" yaml:"slots,omitempty"`
	Map     map[string]string `toml:"map,omitempty" json:"map,omitempty" yaml:"map,omitempty"` // field -> metric name
	// Gateway: how `berth gateway` starts this engine when a request names a
	// model it serves and it is not running. Start is the engine's command;
	// it runs under a lease exactly as `berth run` would.
	Start    []string `toml:"start,omitempty" json:"start,omitempty" yaml:"start,omitempty"`
	Serves   []string `toml:"serves,omitempty" json:"serves,omitempty" yaml:"serves,omitempty"`         // model names this engine answers for
	Priority string   `toml:"priority,omitempty" json:"priority,omitempty" yaml:"priority,omitempty"`   // batch, interactive (default) or foreground
	NeedsMiB uint64   `toml:"needs_mib,omitzero" json:"needs_mib,omitempty" yaml:"needs_mib,omitempty"` // 0 = the whole card
	// NeedsRAMMiB estimates the engine's host RAM spike at cold start, for
	// the gateway headroom guard. 0 = unknown, and the guard fails open.
	NeedsRAMMiB  uint64 `toml:"needs_ram_mib,omitzero" json:"needs_ram_mib,omitempty" yaml:"needs_ram_mib,omitempty"`
	DrainPost    string `toml:"drain_post,omitempty" json:"drain_post,omitempty" yaml:"drain_post,omitempty"`          // as berth run --drain-post
	DrainComfyUI string `toml:"drain_comfyui,omitempty" json:"drain_comfyui,omitempty" yaml:"drain_comfyui,omitempty"` // as berth run --drain-comfyui
	// KeepComfyUI is berth run --keep-comfyui-up: unload and stay up.
	KeepComfyUI bool `toml:"keep_comfyui_up,omitempty" json:"keep_comfyui_up,omitempty" yaml:"keep_comfyui_up,omitempty"`
}

// Types are the engine readers that exist.
var Types = []string{"llama-swap", "llamacpp", "ninfer", "strata", "ollama", "vllm", "sglang", "koboldcpp",
	"tabbyapi", "localai", "lmstudio", "unsloth", "freetoken", "openai", "comfyui", "custom"}

// MapFields are the fields a custom engine may map to its metric names.
var MapFields = []string{"prompt_tokens", "output_tokens", "prompt_seconds", "output_seconds", "processing", "waiting"}

type Config struct {
	Metrics struct {
		// Listen is where /metrics is served for Prometheus. Loopback only;
		// empty = off.
		Listen string `toml:"listen" json:"listen,omitempty" yaml:"listen,omitempty"`
	} `toml:"metrics,omitempty" json:"metrics" yaml:"metrics"`
	Engines struct {
		Every Duration `toml:"every" json:"every" yaml:"every"` // how often engines are read; default 5 s
		// Discover probes the well-known engine ports on this machine (and
		// on Hosts) every minute and reads whatever answers; default on.
		// Ports adds more ports to probe with every reader.
		Discover *bool    `toml:"discover" json:"discover" yaml:"discover"`
		Hosts    []string `toml:"discover_hosts,omitempty" json:"discover_hosts,omitempty" yaml:"discover_hosts,omitempty"`
		Ports    []int    `toml:"discover_ports,omitempty" json:"discover_ports,omitempty" yaml:"discover_ports,omitempty"`
	} `toml:"engines" json:"engines" yaml:"engines"`
	Engine   []Engine       `toml:"engine,omitempty" json:"engine,omitempty" yaml:"engine,omitempty"`
	Alerts   AlertsConfig   `toml:"alerts" json:"alerts" yaml:"alerts"`
	Priority PriorityConfig `toml:"priority,omitempty" json:"priority" yaml:"priority"`
}

// PriorityConfig decides juggles: two engines at the same priority asking for
// the card within JuggleWindow of each other while the holder is busy.
type PriorityConfig struct {
	// Order ranks engines, first is best: a lease's --group, else its
	// engine name. Empty = the engine with the most card-hours over the
	// last 7 days ranks first.
	Order []string `toml:"order,omitempty" json:"order,omitempty" yaml:"order,omitempty"`
	// JuggleWindow: default 15s; "0s" keeps the default, a negative value
	// turns juggle handling off (last switch always wins).
	JuggleWindow Duration `toml:"juggle_window" json:"juggle_window" yaml:"juggle_window"`
}

// AlertsConfig is the threshold table the node evaluates on its sample cycle.
// A key with no entry is not watched. probe and stuck are built in; they
// cannot be configured off.
type AlertsConfig struct {
	ClearPolls *int            `toml:"clear_polls,omitempty" json:"clear_polls,omitempty" yaml:"clear_polls,omitempty"` // consecutive clean polls before a level drops; nil = 3
	Queue      proto.Threshold `toml:"queue,omitempty" json:"queue" yaml:"queue"`                                       // waiting leases
	TTFT       proto.Threshold `toml:"ttft,omitempty" json:"ttft" yaml:"ttft"`                                          // prefill time, ms
	TPOT       proto.Threshold `toml:"tpot,omitempty" json:"tpot" yaml:"tpot"`                                          // ms per output token
	P95        proto.Threshold `toml:"p95,omitempty" json:"p95" yaml:"p95"`                                             // request duration, ms
	Cache      proto.Threshold `toml:"cache,omitempty" json:"cache" yaml:"cache"`                                       // cache share, %; low is bad
	Reingest   proto.Threshold `toml:"reingest,omitempty" json:"reingest" yaml:"reingest"`                              // re-ingest share, %
	VRAM       proto.Threshold `toml:"vram,omitempty" json:"vram" yaml:"vram"`                                          // fraction of the card
	Temp       proto.Threshold `toml:"temp,omitempty" json:"temp" yaml:"temp"`                                          // °C
	Power      proto.Threshold `toml:"power,omitempty" json:"power" yaml:"power"`                                       // fraction of the power limit
	KV         proto.Threshold `toml:"kv,omitempty" json:"kv" yaml:"kv"`                                                // deepest slot fill, 0..1
}

// Duration is a TOML string like "5s". It keeps that spelling in JSON and
// YAML too, so the three encodings of the config read the same.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a duration is a string like \"5s\"")
	}
	return d.UnmarshalText([]byte(s))
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("a duration is a string like \"5s\"")
	}
	return d.UnmarshalText([]byte(node.Value))
}

// DefaultPath is ~/.config/berth/berth.toml, or $BERTH_CONFIG.
func DefaultPath() string {
	if p := os.Getenv("BERTH_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "berth.toml"
	}
	return filepath.Join(dir, "berth", "berth.toml")
}

// Load reads and validates path, with the defaults of the file's schema
// materialised. A missing file is an empty config, not an error; an unreadable
// or invalid one is.
func Load(path string) (Config, error) {
	c, _, err := Check(path)
	return c, err
}

// Validate reports every problem at once, so one run of `berth config check`
// shows everything to fix.
func (c *Config) Validate() error {
	var errs []error
	if c.Engines.Every.Duration < 0 {
		errs = append(errs, errors.New("engines.every must be positive"))
	}
	if c.Metrics.Listen != "" {
		if err := Loopback(c.Metrics.Listen); err != nil {
			errs = append(errs, fmt.Errorf("metrics.listen: %w", err))
		}
	}
	if cp := c.Alerts.ClearPolls; cp != nil && *cp < 1 {
		errs = append(errs, errors.New("alerts.clear_polls must be >= 1"))
	}
	// frac: a fraction of a limit (vram, power, kv) - any set level in (0, 1].
	// low: lower is worse (cache) - when both levels are set, crit < warn.
	alertKeys := []struct {
		name string
		frac bool
		low  bool
		thr  proto.Threshold
	}{
		{"queue", false, false, c.Alerts.Queue},
		{"ttft", false, false, c.Alerts.TTFT},
		{"tpot", false, false, c.Alerts.TPOT},
		{"p95", false, false, c.Alerts.P95},
		{"cache", false, true, c.Alerts.Cache},
		{"reingest", false, false, c.Alerts.Reingest},
		{"vram", true, false, c.Alerts.VRAM},
		{"temp", false, false, c.Alerts.Temp},
		{"power", true, false, c.Alerts.Power},
		{"kv", true, false, c.Alerts.KV},
	}
	for _, k := range alertKeys {
		if k.thr.Warn == 0 && k.thr.Crit == 0 {
			continue // the key is not watched
		}
		for _, l := range []struct {
			name string
			v    float64
		}{
			{"warn", k.thr.Warn},
			{"crit", k.thr.Crit},
		} {
			if l.v == 0 {
				continue // level not set
			}
			where := fmt.Sprintf("alerts.%s: %s %g", k.name, l.name, l.v)
			if k.frac && (l.v <= 0 || l.v > 1) {
				errs = append(errs, errors.New(where+" must be in (0, 1]"))
			} else if l.v < 0 {
				errs = append(errs, errors.New(where+" must be > 0"))
			}
		}
		if k.thr.Warn != 0 && k.thr.Crit != 0 {
			if k.low && k.thr.Crit >= k.thr.Warn {
				errs = append(errs, fmt.Errorf("alerts.%s: crit %g must be < warn %g", k.name, k.thr.Crit, k.thr.Warn))
			} else if !k.low && !k.frac && k.thr.Crit < k.thr.Warn {
				errs = append(errs, fmt.Errorf("alerts.%s: crit %g must be >= warn %g", k.name, k.thr.Crit, k.thr.Warn))
			}
		}
	}
	ordered := map[string]bool{}
	for i, o := range c.Priority.Order {
		switch {
		case strings.TrimSpace(o) == "":
			errs = append(errs, fmt.Errorf("priority.order[%d] is empty", i))
		case ordered[o]:
			errs = append(errs, fmt.Errorf("priority.order lists %q twice", o))
		}
		ordered[o] = true
	}
	seen := map[string]bool{}
	served := map[string]string{} // model -> the engine that serves it
	for i, e := range c.Engine {
		where := fmt.Sprintf("engine[%d] %q", i, e.Name)
		if e.Name == "" {
			errs = append(errs, fmt.Errorf("engine[%d]: name is required", i))
		} else if seen[e.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", where))
		}
		seen[e.Name] = true
		if !contains(Types, e.Type) {
			errs = append(errs, fmt.Errorf("%s: type %q is not one of %v", where, e.Type, Types))
		}
		if u, err := url.Parse(e.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s: url %q must be http(s)://host:port", where, e.URL))
		}
		if e.Type == "custom" && len(e.Map) == 0 && e.Health == "" && e.Models == "" && e.Metrics == "" && e.Slots == "" {
			errs = append(errs, fmt.Errorf("%s: a custom engine needs at least one of health, models, metrics, slots or [engine.map]", where))
		}
		for _, m := range e.Serves {
			if other, ok := served[m]; ok {
				errs = append(errs, fmt.Errorf("%s: serves %q, which %q already serves; the gateway would never pick this one", where, m, other))
			} else {
				served[m] = e.Name
			}
		}
		if len(e.Start) > 0 && len(e.Serves) == 0 && e.Type != "comfyui" {
			errs = append(errs, fmt.Errorf("%s: start needs serves, the models the gateway may start it for", where))
		}
		if e.Type == "comfyui" && len(e.Serves) > 0 {
			errs = append(errs, fmt.Errorf("%s: a comfyui engine is reached at the gateway's /prompt, not through serves (ComfyUI is not OpenAI-compatible)", where))
		}
		if e.KeepComfyUI && e.DrainComfyUI == "" {
			errs = append(errs, fmt.Errorf("%s: keep_comfyui_up needs drain_comfyui, the ComfyUI base URL to unload on drain", where))
		}
		switch e.Priority {
		case "", "batch", "interactive", "foreground":
		default:
			errs = append(errs, fmt.Errorf("%s: priority %q is not batch, interactive or foreground", where, e.Priority))
		}
		for k := range e.Map {
			if !contains(MapFields, k) {
				errs = append(errs, fmt.Errorf("%s: map field %q is not one of %v", where, k, MapFields))
			}
		}
	}
	for _, p := range c.Engines.Ports {
		if p <= 0 || p > 65535 {
			errs = append(errs, fmt.Errorf("engines.discover_ports: %d is not a port", p))
		}
	}
	return errors.Join(errs...)
}

// Loopback accepts host:port only when host is a loopback address or
// localhost. Berth never serves on a LAN address by accident.
func Loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q is not a loopback address (use 127.0.0.1 or [::1])", host)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
