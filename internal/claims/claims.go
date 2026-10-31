// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package claims turns work Berth does not run into claims on the card, so a
// game, a render or a capture is never lost in the shuffle between engines.
//
// A rule matches running processes by name, command line or launcher
// ancestor. Its action is one of:
//
//   - hold: while a match runs, berthd holds the card for it (a pinned lease
//     at hold priority), so engines are drained and none starts until the
//     match has been gone for the rule's linger. A game is the typical case:
//     it rarely shows up in the driver's compute list, so only the process
//     table can see it.
//   - track: a match's card memory is named for the rule instead of being
//     reported as a stowaway. It still counts against the card; nothing waits
//     on it and nothing is drained for it.
//
// Claims never stop or signal a process.
package claims

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
)

const (
	ActionHold  = "hold"
	ActionTrack = "track"

	// DefaultLinger is how long a hold outlives its last match: launchers
	// restart the game process during start-up, and the card should not
	// bounce back to an engine in between.
	DefaultLinger = 15 * time.Second
)

// Rule is one [[claim]] entry, or a built-in.
type Rule struct {
	Name    string   // shown on the lease: "claim:<name>"
	Exe     []string // globs on the process name (comm) or argv[0]'s base name; case-insensitive
	Cmdline string   // regex on the full command line (on Windows: the executable's full path)
	Not     string   // regex on the same; a match here excludes the process (a launcher beside its games)
	Parent  []string // globs on any ancestor's name: the launcher (Steam's reaper, lutris-wrapper)
	// ParentCmdline is a regex the matched ancestor's command line must also
	// match, for a launcher whose name another program shares (Steam's
	// reaper and the REAPER DAW are both "reaper").
	ParentCmdline string
	Action        string        // hold | track
	NeedsMiB      uint64        // hold only: 0 = the whole card
	Linger        time.Duration // hold only: 0 = DefaultLinger
	// GPU is the cards a hold takes, as berth run --gpu names them; "" is
	// every card. Cards is GPU resolved to positions in the watcher's
	// cards, nil for every card.
	GPU     string
	Cards   []int
	Builtin bool

	cmdline       *regexp.Regexp
	not           *regexp.Regexp
	parentCmdline *regexp.Regexp
}

// Proc is one process as the scanner reads it.
type Proc struct {
	PID     int
	Start   uint64
	Name    string // comm
	Argv0   string // base name of argv[0]
	Cmdline string // argv joined with spaces
	Parent  int
}

// Builtin is the pack that ships on (berth.toml `[claims] builtin = false`
// turns it off). Games hold the card; creative and capture apps are tracked.
func Builtin() []Rule {
	return []Rule{
		// Every Steam game, native or Proton, runs under Steam's reaper,
		// which Steam starts as "reaper SteamLaunch AppId=N -- ..."; the
		// REAPER DAW is also "reaper" and never passes SteamLaunch (#171).
		{Name: "steam-game", Parent: []string{"reaper"}, ParentCmdline: `(^|\s)SteamLaunch(\s|$)`, Action: ActionHold, Builtin: true},
		{Name: "lutris-game", Parent: []string{"lutris-wrapper*"}, Action: ActionHold, Builtin: true},
		{Name: "gamescope", Exe: []string{"gamescope"}, Action: ActionHold, Builtin: true},
		// Windows (and Proton, whose paths look the same): games by install folder.
		// Wallpaper Engine and Lossless Scaling live in the Steam library but run all
		// day beside other work; holding the card for them would starve every engine.
		{Name: "steam-library", Cmdline: `(?i)\\steamapps\\common\\`,
			Not: `(?i)\\steamapps\\common\\(wallpaper_engine|Lossless Scaling|Steamworks Shared)\\`, Action: ActionHold, Builtin: true},
		{Name: "epic-game", Cmdline: `(?i)\\Epic Games\\`, Not: `(?i)\\Epic Games\\(Launcher|Epic Online Services|EpicOnlineServices)\\`, Action: ActionHold, Builtin: true},
		{Name: "gog-game", Cmdline: `(?i)\\GOG (Galaxy\\)?Games\\`, Action: ActionHold, Builtin: true},
		{Name: "ea-game", Cmdline: `(?i)\\EA Games\\`, Action: ActionHold, Builtin: true},
		{Name: "ubisoft-game", Cmdline: `(?i)\\Ubisoft Game Launcher\\games\\`, Action: ActionHold, Builtin: true},
		{Name: "xbox-game", Cmdline: `(?i)\\XboxGames\\`, Action: ActionHold, Builtin: true},
		{Name: "battlenet-game", Parent: []string{"battle.net"}, Not: `(?i)\\Battle\.net\\`, Action: ActionHold, Builtin: true},
		{Name: "blender", Exe: []string{"blender"}, Action: ActionTrack, Builtin: true},
		{Name: "davinci-resolve", Exe: []string{"resolve"}, Action: ActionTrack, Builtin: true},
		{Name: "obs", Exe: []string{"obs", "obs64"}, Action: ActionTrack, Builtin: true},
		{Name: "premiere", Exe: []string{"adobe premiere pro"}, Action: ActionTrack, Builtin: true},
		{Name: "after-effects", Exe: []string{"afterfx"}, Action: ActionTrack, Builtin: true},
		{Name: "kdenlive", Exe: []string{"kdenlive"}, Action: ActionTrack, Builtin: true},
	}
}

// Compile checks a rule and readies its regex.
func (r *Rule) Compile() error {
	if r.Name == "" {
		return fmt.Errorf("claim: name is required")
	}
	switch r.Action {
	case "":
		r.Action = ActionHold
	case ActionHold, ActionTrack:
	default:
		return fmt.Errorf("claim %q: action %q (hold, track)", r.Name, r.Action)
	}
	if r.GPU != "" {
		if r.Action != ActionHold {
			return fmt.Errorf("claim %q: gpu names the cards a hold takes; a %s claim holds none", r.Name, r.Action)
		}
		if err := gpu.CheckCard(r.GPU); err != nil {
			return fmt.Errorf("claim %q: %v", r.Name, err)
		}
	}
	if len(r.Exe) == 0 && r.Cmdline == "" && len(r.Parent) == 0 {
		return fmt.Errorf("claim %q: give exe, cmdline or parent to match on", r.Name)
	}
	for _, g := range append(append([]string{}, r.Exe...), r.Parent...) {
		if _, err := path.Match(strings.ToLower(g), ""); err != nil {
			return fmt.Errorf("claim %q: bad glob %q: %v", r.Name, g, err)
		}
	}
	if r.Cmdline != "" {
		re, err := regexp.Compile(r.Cmdline)
		if err != nil {
			return fmt.Errorf("claim %q: cmdline: %v", r.Name, err)
		}
		r.cmdline = re
	}
	if r.Not != "" {
		re, err := regexp.Compile(r.Not)
		if err != nil {
			return fmt.Errorf("claim %q: not_cmdline: %v", r.Name, err)
		}
		r.not = re
	}
	if r.ParentCmdline != "" {
		if len(r.Parent) == 0 {
			return fmt.Errorf("claim %q: parent_cmdline needs parent", r.Name)
		}
		re, err := regexp.Compile(r.ParentCmdline)
		if err != nil {
			return fmt.Errorf("claim %q: parent_cmdline: %v", r.Name, err)
		}
		r.parentCmdline = re
	}
	if r.Linger <= 0 {
		r.Linger = DefaultLinger
	}
	return nil
}

// Matches reports whether p, with its process table, is this rule's. Every
// matcher the rule gives must agree.
func (r *Rule) Matches(p Proc, table map[int]Proc) bool {
	if len(r.Exe) > 0 && !globAny(r.Exe, p.Name) && !globAny(r.Exe, p.Argv0) {
		return false
	}
	if r.cmdline != nil && !r.cmdline.MatchString(p.Cmdline) {
		return false
	}
	if r.not != nil && r.not.MatchString(p.Cmdline) {
		return false
	}
	if len(r.Parent) > 0 {
		found := false
		seen := map[int]bool{p.PID: true}
		for pid := p.Parent; pid > 1 && !seen[pid]; {
			seen[pid] = true
			a, ok := table[pid]
			if !ok {
				break
			}
			if (globAny(r.Parent, a.Name) || globAny(r.Parent, a.Argv0)) &&
				(r.parentCmdline == nil || r.parentCmdline.MatchString(a.Cmdline)) {
				found = true
				break
			}
			pid = a.Parent
		}
		if !found {
			return false
		}
	}
	return true
}

func globAny(globs []string, name string) bool {
	if name == "" {
		return false
	}
	name = strings.ToLower(name)
	bare := strings.TrimSuffix(name, ".exe") // one rule for blender and blender.exe
	for _, g := range globs {
		g = strings.ToLower(g)
		if ok, _ := path.Match(g, name); ok {
			return true
		}
		if ok, _ := path.Match(g, bare); ok {
			return true
		}
	}
	return false
}

// Match returns, per rule name, the processes it matches. A process is the
// first matching rule's only, so two rules never claim the same work twice.
func Match(rules []Rule, ps []Proc) map[string][]Proc {
	table := make(map[int]Proc, len(ps))
	for _, p := range ps {
		table[p.PID] = p
	}
	out := map[string][]Proc{}
	for _, p := range ps {
		for i := range rules {
			if rules[i].Matches(p, table) {
				out[rules[i].Name] = append(out[rules[i].Name], p)
				break
			}
		}
	}
	return out
}
