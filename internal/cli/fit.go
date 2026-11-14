// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/contract"
	"github.com/T-Crypt/berth/internal/fit"
	"github.com/T-Crypt/berth/internal/gateway"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/swapcost"
)

// fitDoc is berth fit's answer: what Fit would pick for the name, and every
// candidate it ranked, cheapest first.
type fitDoc struct {
	Name       string          `json:"name" yaml:"name"`
	Pick       fit.Candidate   `json:"pick" yaml:"pick"`
	Candidates []fit.Candidate `json:"candidates" yaml:"candidates"`
}

type fitView struct {
	doc     fitDoc
	explain bool
	offline string
}

func (v fitView) Data() any { return v.doc }

func (v fitView) Human(c *Context, ok bool) error {
	p := v.doc.Pick
	if p.Engine == v.doc.Name {
		c.Printf("%s: %s, %s\n", p.Engine, p.Path, p.Why)
	} else {
		c.Printf("%s: %s, %s, %s\n", v.doc.Name, p.Engine, p.Path, p.Why)
	}
	c.Bullet("start %s", startText(p))
	if v.offline != "" {
		c.Bullet("berthd unreachable (%s): every card reads as out of reach", v.offline)
	}
	if !v.explain && len(v.doc.Candidates) < 2 {
		return nil
	}
	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	fmt.Fprint(tw, "\nENGINE\tPATH\tTIER\tSTART\tWHY\n")
	for _, k := range v.doc.Candidates {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.Engine, k.Path, k.Tier, startText(k), k.Why)
	}
	return tw.Flush()
}

// startText is a candidate's start for people: the time and where it came from.
func startText(k fit.Candidate) string {
	s := k.Start
	if s.Source == contract.Unknown {
		return "unknown (" + s.Why + ")"
	}
	t := fmt.Sprintf("%.1f s %s", s.Seconds, s.Source)
	if s.Why != "" {
		t += " (" + s.Why + ")"
	}
	return t
}

func runFit(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml ($BERTH_CONFIG): the engines and what they serve")
	costs := fs.String("costs", defaultStateFile("swapcost.json"), "the gateway's measured switch times")
	explain := fs.Bool("explain", false, "list every candidate, cheapest first, with its path and reason")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	if len(c.Args) != 1 {
		return c.fail(Systemf("berth fit takes one engine or model name").
			WithHint("for example: berth fit comfyui, or berth fit qwen3-30b"))
	}
	name := c.Args[0]

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return c.fail(Systemf("%s: %v", *cfgPath, err).WithHint("run berth config check"))
	}
	st, offline := contractStatus(*sock)
	cands := fitCandidates(cfg, name)
	if len(cands) == 0 && st != nil && slices.ContainsFunc(st.Engines, func(e proto.Engine) bool { return e.Name == name }) {
		cands = []*config.Engine{{Name: name}}
	}
	if len(cands) == 0 {
		cands = listedBy(context.Background(), st, cfg, name)
	}
	if len(cands) == 0 {
		e := Failf("no engine named %q, and none serves it", name)
		var known []string
		for _, en := range cfg.Engine {
			known = append(known, en.Name)
		}
		if st != nil {
			for _, en := range st.Engines {
				if !slices.Contains(known, en.Name) {
					known = append(known, en.Name)
				}
			}
		}
		if len(known) > 0 {
			e.WithHint("engines: %s", strings.Join(known, ", "))
		}
		return c.fail(e)
	}

	book, err := swapcost.NewBook(*costs)
	if err != nil {
		book = nil
	}
	ranked := fit.Rank(st, cands, book)
	return c.Write(fitView{doc: fitDoc{Name: name, Pick: ranked[0], Candidates: ranked}, explain: *explain, offline: offline}, nil)
}

// listedBy is a model no berth.toml engine names or serves, the way the
// gateway resolves it: an engine that is up, not parked, and lists it on its
// /v1/models. llama-swap lists every model it can load and starts the model
// under the model's own lease, on the lister's card.
func listedBy(ctx context.Context, st *proto.Status, cfg config.Config, name string) []*config.Engine {
	if st == nil {
		return nil
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, e := range st.Engines {
		if !e.Up || fit.Parked(st, e.Name) || !slices.Contains(gateway.ListModels(ctx, client, e.URL), name) {
			continue
		}
		m := &config.Engine{Name: name}
		if i := slices.IndexFunc(cfg.Engine, func(c config.Engine) bool { return c.Name == e.Name }); i >= 0 {
			m.GPU = cfg.Engine[i].GPU
		}
		return []*config.Engine{m}
	}
	return nil
}

// fitCandidates is the engines a name means: the [[engine]] of that name, else
// every engine whose serves lists it, in berth.toml order, which is what the
// gateway ranks for a request naming that model.
func fitCandidates(cfg config.Config, name string) []*config.Engine {
	for i := range cfg.Engine {
		if cfg.Engine[i].Name == name {
			return []*config.Engine{&cfg.Engine[i]}
		}
	}
	var out []*config.Engine
	for i := range cfg.Engine {
		if slices.Contains(cfg.Engine[i].Serves, name) {
			out = append(out, &cfg.Engine[i])
		}
	}
	return out
}
