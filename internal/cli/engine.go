package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/proto"
)

// engineCheckDoc is what `berth engine check --format json|yaml` prints: one
// reading per engine, which is the engine's own report, never the driver's.
type engineCheckDoc struct {
	Status   string         `json:"status" yaml:"status"` // ok | failed
	Checked  int            `json:"checked" yaml:"checked"`
	Answered int            `json:"answered" yaml:"answered"`
	Engines  []proto.Engine `json:"engines" yaml:"engines"`
}

// engineCheckView is one read of every engine the config names.
type engineCheckView struct {
	doc     engineCheckDoc
	cfg     []config.Engine // the configured engines, for the gateway keys
	checked bool
}

func (v engineCheckView) Data() any { return v.doc }

func (v engineCheckView) Human(c *Context, ok bool) error {
	if !v.checked {
		c.Printf("%s No engines configured\n", c.Style.OK(markOK))
		c.Bullet("Add [[engine]] entries to berth.toml, or run berth engine check --discover to probe the well-known ports")
		return nil
	}
	if ok {
		c.Printf("%s %d of %d engine%s answer\n", c.Style.OK(markOK), v.doc.Answered, v.doc.Checked, plural(v.doc.Checked))
	}
	for _, e := range v.doc.Engines {
		c.Bullet("%s", engineLine(c, e))
		if c.Verbose {
			v.engineDetail(c, e)
		}
	}
	return nil
}

// engineLine is one engine's reading on one line: who answered, what it holds.
// The reason a down engine did not answer is the [ERROR] block's to report, so
// it does not land here twice.
func engineLine(c *Context, e proto.Engine) string {
	word := c.Style.OK("up")
	if !e.Up {
		word = c.Style.Err("DOWN")
	}
	line := fmt.Sprintf("%s (%s) %s: %s", c.Style.Bold(e.Name), e.Type, e.URL, word)
	if !e.Up {
		return line
	}
	switch len(e.Models) {
	case 0:
		return line + ", no model loaded"
	case 1:
		m := e.Models[0]
		if m.Window > 0 {
			return fmt.Sprintf("%s, 1 model (%s, window %d)", line, m.Name, m.Window)
		}
		return fmt.Sprintf("%s, 1 model (%s)", line, m.Name)
	}
	names := make([]string, len(e.Models))
	for i, m := range e.Models {
		names[i] = m.Name
	}
	return fmt.Sprintf("%s, %d models (%s)", line, len(e.Models), strings.Join(names, ", "))
}

// engineDetail is the verbose block under one engine: what it reports about
// itself and, when it is a gateway engine, how the gateway would start it.
func (v engineCheckView) engineDetail(c *Context, e proto.Engine) {
	if e.Up {
		m := e.Metrics
		if m.PromptTokens+m.OutputTokens+m.Processing+m.Waiting == 0 {
			c.Sub("metrics: none reported")
		} else {
			c.Sub("metrics: prompt %g, output %g, processing %g, waiting %g", m.PromptTokens, m.OutputTokens, m.Processing, m.Waiting)
		}
		if e.Window > 0 {
			c.Sub("window: %d", e.Window)
		}
		if len(e.Slots) > 0 {
			live, busy := len(e.Slots), 0
			for _, s := range e.Slots {
				if s.Processing {
					busy++
				}
			}
			c.Sub("slots: %d live (%d processing)", live, busy)
		}
	}
	for _, cfgEngine := range v.cfg {
		if cfgEngine.Name != e.Name {
			continue
		}
		var parts []string
		if cfgEngine.Priority != "" {
			parts = append(parts, "priority "+cfgEngine.Priority)
		}
		if len(cfgEngine.Serves) > 0 {
			parts = append(parts, "serves "+strings.Join(cfgEngine.Serves, ", "))
		}
		if len(cfgEngine.Start) > 0 {
			parts = append(parts, "start "+cfgEngine.Start[0])
		}
		if len(parts) > 0 {
			c.Sub("gateway: %s", strings.Join(parts, ", "))
		}
	}
}

func runEngineCheck(c *Context) int {
	fs := c.FlagSet()
	path := fs.String("config", config.DefaultPath(), "berth.toml ($BERTH_CONFIG) with the engines to read")
	discover := fs.Bool("discover", false, "also probe the well-known engine ports and discover_hosts")
	timeout := fs.Duration("timeout", 3*time.Second, "how long one engine's read may take")
	if code, done := c.Parse(fs); done {
		return code
	}
	name := ""
	if len(c.Args) > 0 {
		name = c.Args[0]
	}
	if len(c.Args) > 1 {
		return c.fail(Systemf("unexpected argument %q", c.Args[1]))
	}
	cfg, _, fromFile, e := loadConfig(c, *path)
	if e != nil {
		return c.fail(e)
	}
	if name != "" {
		filtered := cfg.Engine[:0:0]
		for _, eng := range cfg.Engine {
			if eng.Name == name {
				filtered = append(filtered, eng)
			}
		}
		if len(filtered) == 0 {
			from := *path
			if !fromFile {
				from += " (no such file)"
			}
			return c.fail(Systemf("no engine named %q in %s", name, from))
		}
		cfg.Engine = filtered
	}
	if len(cfg.Engine) == 0 && !*discover {
		return c.Write(engineCheckView{doc: engineCheckDoc{Status: "ok", Engines: []proto.Engine{}}}, nil)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w := &engines.Watcher{
		Engines:  cfg.Engine,
		Discover: *discover,
		Hosts:    cfg.Engines.Hosts,
		Ports:    cfg.Engines.Ports,
		Client:   &http.Client{Timeout: *timeout},
	}
	w.PollOnce(ctx)

	snap := w.Snapshot()
	doc := engineCheckDoc{Status: "ok", Checked: len(snap), Engines: snap}
	for _, eng := range snap {
		if eng.Up {
			doc.Answered++
		}
	}
	var failure *Error
	if down := doc.Checked - doc.Answered; down > 0 {
		doc.Status = "failed"
		failure = Failf("%d of %d engine%s did not answer", down, doc.Checked, plural(doc.Checked))
		for _, eng := range snap {
			if !eng.Up {
				failure.Detail("%s: %s", eng.Name, eng.Error)
			}
		}
	}
	return c.Write(engineCheckView{doc: doc, cfg: cfg.Engine, checked: true}, failure)
}
