package cli

import (
	"errors"
	"os"
	"strings"

	"github.com/T-Crypt/berth/internal/wire"
)

// wireDoc is the machine document of `berth wire llama-swap`: the plan, what
// it did, or what a check found.
type wireDoc struct {
	Status  string       `json:"status" yaml:"status"` // ok | drift
	Server  string       `json:"server" yaml:"server"`
	Path    string       `json:"path" yaml:"path"`
	Mode    string       `json:"mode" yaml:"mode"` // plan | check | write
	Changed bool         `json:"changed" yaml:"changed"`
	Wired   int          `json:"wired" yaml:"wired"`   // already wrapped, nothing done
	Add     int          `json:"add" yaml:"add"`       // wrapper inserted (or to insert)
	Update  int          `json:"update" yaml:"update"` // wrapper realigned (or to realign)
	Skipped int          `json:"skipped" yaml:"skipped"`
	Models  []wire.Model `json:"models" yaml:"models"`
	Backup  string       `json:"backup,omitempty" yaml:"backup,omitempty"`
}

// wireView is one run over one llama-swap config.
type wireView struct {
	doc wireDoc
}

func (v wireView) Data() any { return v.doc }

func (v wireView) Human(c *Context, ok bool) error {
	if !ok {
		return nil // the failed check's [ERROR] block is the report
	}
	d := v.doc
	total := d.Wired + d.Add + d.Update + d.Skipped
	switch {
	case d.Mode == "write" && d.Changed:
		c.Printf("%s Wired %d of %d models in %s\n", c.Style.OK(markOK), d.Add+d.Update, total, d.Path)
		if d.Backup != "" {
			c.Bullet("backup: %s", d.Backup)
		}
	case d.Changed:
		c.Printf("%s %d of %d models in %s need a berth wrapper\n", c.Style.OK(markOK), d.Add+d.Update, total, d.Path)
	default:
		if d.Skipped > 0 {
			c.Printf("%s %d models in %s: %d wired, %d skipped\n", c.Style.OK(markOK), total, d.Path, d.Wired, d.Skipped)
		} else {
			c.Printf("%s All %d models in %s run under a berth lease\n", c.Style.OK(markOK), total, d.Path)
		}
	}
	for _, m := range d.Models {
		switch m.Action {
		case "add", "update":
			if c.Verbose && m.Line != "" {
				c.Bullet("%s %s: %s", m.Action, m.Name, strings.TrimSpace(m.Line))
			} else {
				c.Bullet("%s %s", m.Action, m.Name)
			}
		case "wired":
			if c.Verbose {
				c.Bullet("wired %s", m.Name)
			}
		case "skip":
			if c.Verbose {
				c.Bullet("skip %s: %s", m.Name, m.Reason)
			}
		}
	}
	if d.Skipped > 0 && !c.Verbose {
		c.Bullet("%d skipped (--verbose lists them)", d.Skipped)
	}
	if d.Mode == "plan" && d.Changed {
		c.Bullet("apply with: berth wire llama-swap --config %s --write", d.Path)
	}
	return nil
}

// patterns is a repeatable --exclude flag.
type patterns []string

func (p *patterns) String() string { return strings.Join(*p, ",") }
func (p *patterns) Set(v string) error {
	*p = append(*p, v)
	return nil
}

func runWireLlamaSwap(c *Context) int {
	fs := c.FlagSet()
	pathArg := fs.String("config", "", "llama-swap config file to edit (required)")
	api := fs.String("api", "http://127.0.0.1:9090", "llama-swap base URL; the unload endpoint hangs off it")
	binFlag := fs.String("bin", "", "berth binary the wrapper line runs (default: this executable)")
	group := fs.String("group", "llama-swap", "lease group every model counts for (\"\" leaves the flag out)")
	priority := fs.String("priority", "foreground", "lease priority: batch, interactive or foreground (\"\" leaves the flag out)")
	var exclude patterns
	fs.Var(&exclude, "exclude", "model name pattern to leave alone (repeatable, e.g. 'text-embedding-*')")
	update := fs.Bool("update", false, "rewrite existing berth run lines to these flags")
	write := fs.Bool("write", false, "apply the changes (default prints the plan)")
	check := fs.Bool("check", false, "no writes; exit 1 when any model needs wiring")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	if *pathArg == "" {
		return c.fail(Systemf("--config is required: the llama-swap config to wire").
			WithHint("for example: berth wire llama-swap --config /etc/llama-swap/config.yaml"))
	}
	if *write && *check {
		return c.fail(Systemf("--write and --check are exclusive").
			WithHint("--check only reads; drop --write to check, or drop --check to apply"))
	}

	bin := *binFlag
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return c.fail(Systemf("cannot resolve this berth binary: %v", err).
				WithHint("pass --bin with the installed berth path"))
		}
		if isTempBuild(exe) {
			return c.fail(Systemf("this berth runs from a temporary build: %s", exe).
				WithHint("the wrapper line must survive this process; pass --bin with the installed path, e.g. --bin ~/.local/bin/berth"))
		}
		bin = exe
	}

	mode := "plan"
	switch {
	case *write:
		mode = "write"
	case *check:
		mode = "check"
	}

	opts := wire.Options{Bin: bin, Group: *group, Priority: *priority, APIBase: *api, Exclude: exclude, Update: *update}
	if err := opts.Validate(); err != nil {
		return c.fail(Systemf("invalid options: %v", err).
			WithHint("run \"berth wire llama-swap -h\" for the flag meanings"))
	}

	res, err := wire.LlamaSwap(*pathArg, opts)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return c.fail(Systemf("cannot read %s: %v", *pathArg, err))
		}
		return c.fail(Failf("cannot plan the wire: %v", err))
	}

	doc := wireDoc{
		Status: "ok", Server: "llama-swap", Path: *pathArg, Mode: mode, Changed: res.Changed,
		Wired: res.Count("wired"), Add: res.Count("add"), Update: res.Count("update"),
		Skipped: res.Count("skip"), Models: res.Models,
	}
	if *write && res.Changed {
		backup, err := wire.Write(res)
		if err != nil {
			return c.fail(Systemf("writing %s: %v", *pathArg, err))
		}
		doc.Backup = backup
	}

	var failure *Error
	if *check && res.Changed {
		total := res.Count("wired") + res.Count("add") + res.Count("update") + res.Count("skip")
		doc.Status = "drift"
		failure = Failf("%d of %d models do not run under a berth lease: %s", doc.Add+doc.Update, total, *pathArg)
		for _, m := range res.Models {
			if m.Action == "add" || m.Action == "update" {
				failure.Detail("%s (%s)", m.Name, m.Action)
			}
		}
		failure.WithHint("run berth wire llama-swap --config %s --write", *pathArg)
	}
	return c.Write(wireView{doc: doc}, failure)
}

// isTempBuild says whether the executable runs from a path that will not outlive
// the process, like `go run`'s build directory. A wrapper line written from it
// would point at nothing the next day.
func isTempBuild(exe string) bool {
	if strings.Contains(exe, "go-build") {
		return true
	}
	if tmp := os.TempDir(); tmp != "" && strings.HasPrefix(exe, tmp) {
		return true
	}
	return false
}
