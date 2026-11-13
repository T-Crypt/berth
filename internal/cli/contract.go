// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/contract"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/swapcost"
)

// contractView is one engine's contract.
type contractView struct {
	c       contract.Contract
	offline string // why berthd gave no status, "" when it did
}

func (v contractView) Data() any { return v.c }

func (v contractView) Human(c *Context, ok bool) error {
	k := v.c
	where := "not in berth.toml"
	if k.InConfig {
		where = "in berth.toml"
	}
	if k.Type != "" {
		where = k.Type + ", " + where
	}
	c.Printf("%s (%s)\n", k.Engine, where)
	if v.offline != "" {
		c.Bullet("berthd unreachable (%s): measured numbers are unknown", v.offline)
	}
	tw := tabwriter.NewWriter(c.Out, 0, 4, 2, ' ', 0)
	row := func(name, value string, src contract.Source, why string) {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, value, src, why)
	}
	row("priority", k.Priority.Value, k.Priority.Source, "")
	row("vram", contractSize(k.VRAM, "whole card"), k.VRAM.Source, k.VRAM.Why)
	row("last peak", contractSize(k.LastPeak, "-"), k.LastPeak.Source, k.LastPeak.Why)
	row("ram", contractSize(k.RAM, "-"), k.RAM.Source, k.RAM.Why)
	row("parked ram", contractSize(k.ParkedRAM, "-"), k.ParkedRAM.Source, k.ParkedRAM.Why)
	minTurn := "off"
	if k.MinTurn.Seconds > 0 {
		minTurn = (time.Duration(k.MinTurn.Seconds) * time.Second).String()
	}
	row("min turn", minTurn, k.MinTurn.Source, "")
	row("window", k.Window.Value, k.Window.Source, "")
	for _, s := range k.Switch {
		took, why := "-", s.Took.Why
		switch s.Took.Source {
		case contract.Measured:
			took, why = fmt.Sprintf("%.1f s", s.Took.Seconds), fmt.Sprintf("median of %d", s.Samples)
		case contract.Default:
			took = "0 s"
		}
		row("switch "+string(s.Tier), took, s.Took.Source, why)
	}
	return tw.Flush()
}

// contractSize is a contract size for people: MiB, or none when it carries no bytes.
func contractSize(b contract.Bytes, none string) string {
	if b.Bytes == 0 {
		return none
	}
	return server.MiB(b.Bytes)
}

func runContract(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml ($BERTH_CONFIG): the engine's declared terms")
	costs := fs.String("costs", defaultStateFile("swapcost.json"), "the gateway's measured switch times")
	if code, done := c.ParseInterspersed(fs); done {
		return code
	}
	if len(c.Args) != 1 {
		return c.fail(Systemf("berth contract takes one engine name").
			WithHint("for example: berth contract comfyui"))
	}
	name := c.Args[0]

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return c.fail(Systemf("%s: %v", *cfgPath, err).WithHint("run berth config check"))
	}
	var entry *config.Engine
	for i := range cfg.Engine {
		if cfg.Engine[i].Name == name {
			entry = &cfg.Engine[i]
		}
	}

	st, offline := contractStatus(*sock)
	if entry == nil && !knownTo(st, name) {
		known := knownNames(cfg, st)
		e := Failf("no engine named %q in berth.toml or berthd's status", name)
		if len(known) > 0 {
			e.WithHint("known: %s", strings.Join(known, ", "))
		}
		return c.fail(e)
	}

	book, err := swapcost.NewBook(*costs)
	if err != nil {
		book = nil
	}
	return c.Write(contractView{c: contract.Build(name, entry, st, book), offline: offline}, nil)
}

// contractStatus is berthd's status, or nil and why when it did not answer.
func contractStatus(sock string) (*proto.Status, string) {
	conn, raw, err := server.Dial(sock)
	if err != nil {
		return nil, err.Error()
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		return nil, err.Error()
	}
	m, err := conn.Recv()
	if err != nil {
		return nil, err.Error()
	}
	if m.Status == nil {
		return nil, "empty reply"
	}
	return m.Status, ""
}

// knownTo says whether berthd's status names the engine: discovered, or
// measured under its own name (a llama-swap model).
func knownTo(st *proto.Status, name string) bool {
	for _, n := range statusNames(st) {
		if n == name {
			return true
		}
	}
	return false
}

func statusNames(st *proto.Status) []string {
	if st == nil {
		return nil
	}
	var out []string
	for _, e := range st.Engines {
		out = append(out, e.Name)
	}
	for _, f := range st.Footprints {
		out = append(out, f.Engine)
	}
	return out
}

func knownNames(cfg config.Config, st *proto.Status) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range cfg.Engine {
		if !seen[e.Name] {
			seen[e.Name] = true
			out = append(out, e.Name)
		}
	}
	for _, n := range statusNames(st) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
