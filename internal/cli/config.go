package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
)

// configCheckDoc is what `berth config check --format json|yaml` prints: the
// verdict, the facts behind it, and the path they came from.
type configCheckDoc struct {
	Status   string       `json:"status" yaml:"status"` // ok | invalid
	Path     string       `json:"path" yaml:"path"`
	Source   string       `json:"source" yaml:"source"` // file | defaults
	Strict   bool         `json:"strict" yaml:"strict"`
	Engines  int          `json:"engines_count" yaml:"engines_count"`
	Metrics  metricsDoc   `json:"metrics" yaml:"metrics"`
	Warnings []warningDoc `json:"warnings,omitempty" yaml:"warnings,omitempty"`
	Errors   []string     `json:"errors,omitempty" yaml:"errors,omitempty"`
	Applied  bool         `json:"applied" yaml:"applied"` // always false: check never applies
}

type metricsDoc struct {
	Enabled  bool   `json:"enabled" yaml:"enabled"`
	Address  string `json:"address,omitempty" yaml:"address,omitempty"`   // where /metrics listens, when it is on
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"` // /metrics, when it is on
}

type warningDoc struct {
	Key     string `json:"key" yaml:"key"`
	Message string `json:"message" yaml:"message"`
}

func metricsOf(cfg config.Config) metricsDoc {
	if cfg.Metrics.Listen == "" {
		return metricsDoc{}
	}
	return metricsDoc{Enabled: true, Address: cfg.Metrics.Listen, Endpoint: "/metrics"}
}

func warningsDoc(ws []config.Warning) []warningDoc {
	if len(ws) == 0 {
		return nil
	}
	out := make([]warningDoc, len(ws))
	for i, w := range ws {
		out[i] = warningDoc{Key: w.Key, Message: w.Message}
	}
	return out
}

// loadConfig reads the config both config commands and engine check need: the
// resolved config, its warnings, and whether a file was there at all. A
// missing file is the built-in defaults; a file the operator named with
// --config and that is not there is an error.
func loadConfig(c *Context, path string) (config.Config, []config.Warning, bool, *Error) {
	fi, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		if c.Changed("config") {
			return config.Resolve(config.Config{}), nil, false,
				Systemf("config not found: %s", path).Detail("the file named by --config does not exist")
		}
		return config.Resolve(config.Config{}), nil, false, nil
	case statErr != nil:
		return config.Resolve(config.Config{}), nil, false,
			Systemf("config unreadable: %s", path).Detail("%v", statErr)
	case !fi.Mode().IsRegular():
		return config.Resolve(config.Config{}), nil, false,
			Systemf("config unreadable: %s", path).Detail("%s is not a regular file", path)
	}
	cfg, warns, err := config.Check(path)
	if err != nil {
		return config.Config{}, nil, true, Failf("config invalid: %s", path).Detail("%v", err)
	}
	return cfg, warns, true, nil
}

// reportWarnings prints each warning to stderr, where a diagnostic belongs.
// In a machine format they travel in the document instead.
func reportWarnings(c *Context, ws []config.Warning) {
	for _, w := range ws {
		c.Warnf("%s: %s", w.Key, w.Message)
	}
}

// configBullets is the body both config commands print: what decides the
// node's behavior, and in verbose mode the whole engine block.
func configBullets(c *Context, cfg config.Config) {
	enginesBlock(c, cfg)
	metricsBlock(c, cfg)
	if len(cfg.Priority.Order) > 0 || c.Verbose {
		priorityBullet(c, cfg)
	}
	if cfg.AlertsSet() || c.Verbose {
		alertsBullet(c, cfg)
	}
	if c.Verbose {
		discoveryBullet(c, cfg)
	}
}

func enginesBlock(c *Context, cfg config.Config) {
	if !c.Verbose {
		discovery := ""
		if cfg.Engines.Discover != nil && *cfg.Engines.Discover {
			discovery = ", discovery on"
		}
		c.Bullet("Engines: %d configured%s", len(cfg.Engine), discovery)
		return
	}
	if len(cfg.Engine) == 0 {
		c.Bullet("Engines: none configured")
		return
	}
	c.Bullet("Engines (%d):", len(cfg.Engine))
	for _, e := range cfg.Engine {
		c.Sub("%s (type: %s, url: %s%s)", e.Name, e.Type, e.URL, engineExtra(e))
	}
}

// engineExtra names the gateway keys of one engine, when it has any.
func engineExtra(e config.Engine) string {
	var parts []string
	if e.Priority != "" {
		parts = append(parts, "priority: "+e.Priority)
	}
	if len(e.Serves) > 0 {
		parts = append(parts, "serves: "+strings.Join(e.Serves, ", "))
	}
	if e.NeedsMiB > 0 {
		parts = append(parts, fmt.Sprintf("needs_mib: %d", e.NeedsMiB))
	}
	if len(e.Start) > 0 {
		parts = append(parts, "start: "+e.Start[0])
	}
	if e.DrainComfyUI != "" {
		parts = append(parts, "drain_comfyui: "+e.DrainComfyUI)
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

func metricsBlock(c *Context, cfg config.Config) {
	if !c.Verbose {
		if cfg.Metrics.Listen == "" {
			c.Bullet("Metrics: off")
			return
		}
		c.Bullet("Metrics: %s (/metrics)", cfg.Metrics.Listen)
		return
	}
	address, enabled := cfg.Metrics.Listen, "true"
	if address == "" {
		address, enabled = "off", "false"
	}
	c.Bullet("Metrics Listener:")
	c.Sub("Address: %s", address)
	c.Sub("Path:    /metrics")
	c.Sub("Enabled: %s", enabled)
}

func priorityBullet(c *Context, cfg config.Config) {
	rank := "ranked by card-hours over 7 days"
	if len(cfg.Priority.Order) > 0 {
		rank = "order [" + strings.Join(cfg.Priority.Order, " ") + "]"
	}
	window := cfg.Priority.JuggleWindow.String()
	if cfg.Priority.JuggleWindow.Duration < 0 {
		window = "off"
	}
	c.Bullet("Priority: %s, juggle window %s", rank, window)
}

func alertsBullet(c *Context, cfg config.Config) {
	var parts []string
	for _, k := range []struct {
		name string
		t    proto.Threshold
	}{
		{"queue", cfg.Alerts.Queue}, {"ttft", cfg.Alerts.TTFT}, {"tpot", cfg.Alerts.TPOT},
		{"p95", cfg.Alerts.P95}, {"cache", cfg.Alerts.Cache}, {"reingest", cfg.Alerts.Reingest},
		{"vram", cfg.Alerts.VRAM}, {"temp", cfg.Alerts.Temp}, {"power", cfg.Alerts.Power},
		{"kv", cfg.Alerts.KV},
	} {
		if k.t.Warn == 0 && k.t.Crit == 0 {
			continue
		}
		switch {
		case k.t.Warn == 0:
			parts = append(parts, fmt.Sprintf("%s crit %g", k.name, k.t.Crit))
		case k.t.Crit == 0:
			parts = append(parts, fmt.Sprintf("%s warn %g", k.name, k.t.Warn))
		default:
			parts = append(parts, fmt.Sprintf("%s warn %g crit %g", k.name, k.t.Warn, k.t.Crit))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "none")
	}
	polls := 0
	if cfg.Alerts.ClearPolls != nil {
		polls = *cfg.Alerts.ClearPolls
	}
	c.Bullet("Alerts: %s, clear polls %d", strings.Join(parts, "; "), polls)
}

func discoveryBullet(c *Context, cfg config.Config) {
	state, every := "off", cfg.Engines.Every.String()
	if cfg.Engines.Discover != nil && *cfg.Engines.Discover {
		state = "on"
	}
	line := fmt.Sprintf("Discovery: %s, every %s", state, every)
	if len(cfg.Engines.Hosts) > 0 {
		line += ", hosts [" + strings.Join(cfg.Engines.Hosts, " ") + "]"
	}
	if len(cfg.Engines.Ports) > 0 {
		ports := make([]string, len(cfg.Engines.Ports))
		for i, p := range cfg.Engines.Ports {
			ports[i] = fmt.Sprintf("%d", p)
		}
		line += ", ports [" + strings.Join(ports, " ") + "]"
	}
	c.Bullet("%s", line)
}

// configCheckView is `berth config check`: the file's verdict. A failed check
// prints nothing here, since the [ERROR] block is the report then.
type configCheckView struct {
	cfg config.Config
	doc configCheckDoc
}

func (v configCheckView) Data() any { return v.doc }

func (v configCheckView) Human(c *Context, ok bool) error {
	if !ok {
		return nil
	}
	c.Printf("%s Config valid: %s\n", c.Style.OK(markOK), v.doc.Path)
	if v.doc.Source == "defaults" {
		c.Bullet("No file: berth runs on its built-in defaults and discovery")
	}
	configBullets(c, v.cfg)
	return nil
}

func runConfigCheck(c *Context) int {
	fs := c.FlagSet()
	path := fs.String("config", config.DefaultPath(), "berth.toml ($BERTH_CONFIG; missing = built-in defaults)")
	strict := fs.Bool("strict", false, "a warning fails the check")
	if code, done := c.Parse(fs); done {
		return code
	}
	if len(c.Args) > 0 {
		return c.fail(Systemf("unexpected argument %q", c.Args[0]).WithHint("use --config %s", c.Args[0]))
	}
	cfg, warns, fromFile, e := loadConfig(c, *path)
	doc := configCheckDoc{
		Status:  "ok",
		Path:    *path,
		Source:  "file",
		Strict:  *strict,
		Engines: len(cfg.Engine),
		Metrics: metricsOf(cfg),
	}
	if !fromFile {
		doc.Source = "defaults"
	}
	if e != nil {
		if e.Code == ExitSystem {
			return c.fail(e)
		}
		doc.Status = "invalid"
		doc.Errors = strings.Split(strings.Join(e.Details, "\n"), "\n")
		if c.Machine() {
			return c.Write(configCheckView{doc: doc}, e)
		}
		return c.Write(nil, e)
	}
	doc.Warnings = warningsDoc(warns)
	strictFail := *strict && len(warns) > 0
	if !c.Machine() && !strictFail {
		reportWarnings(c, warns)
	}
	var failure *Error
	if strictFail {
		failure = Failf("config valid with %d warning%s and --strict is set: %s", len(warns), plural(len(warns)), *path)
		for _, w := range warns {
			failure.Detail("%s: %s", w.Key, w.Message)
		}
	}
	return c.Write(configCheckView{cfg: cfg, doc: doc}, failure)
}

// configShowView is `berth config show`: the config the node would run with,
// with every default materialised.
type configShowView struct {
	cfg      config.Config
	path     string
	fromFile bool
}

func (v configShowView) Data() any {
	source := "file"
	if !v.fromFile {
		source = "defaults"
	}
	return struct {
		Path   string        `json:"path" yaml:"path"`
		Source string        `json:"source" yaml:"source"`
		Config config.Config `json:"config" yaml:"config"`
	}{Path: v.path, Source: source, Config: v.cfg}
}

func (v configShowView) Human(c *Context, ok bool) error {
	line := fmt.Sprintf("%s Config: %s", c.Style.OK(markOK), v.path)
	if !v.fromFile {
		line += " (built-in defaults, no file)"
	}
	c.Printf("%s\n", line)
	configBullets(c, v.cfg)
	return nil
}

func runConfigShow(c *Context) int {
	fs := c.FlagSet()
	path := fs.String("config", config.DefaultPath(), "berth.toml ($BERTH_CONFIG; missing = built-in defaults)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	cfg, warns, fromFile, e := loadConfig(c, *path)
	if e != nil {
		return c.fail(e)
	}
	// A TOML document is the config's own file format, so it prints alone and
	// saves straight back to berth.toml.
	if c.Format == FormatTOML {
		if err := c.Emit(cfg); err != nil {
			return c.fail(Systemf("%v", err))
		}
		return ExitOK
	}
	if !c.Machine() {
		reportWarnings(c, warns)
	}
	return c.Write(configShowView{cfg: cfg, path: *path, fromFile: fromFile}, nil)
}
