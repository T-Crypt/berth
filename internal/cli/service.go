// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/T-Crypt/berth/internal/gpu"
)

// Unit is one berth service: what it runs and where it sits in the
// services' own ordering.
type Unit struct {
	Name        string
	Exec        []string
	After       []string
	Description string
}

// serviceUnits is the list the flags select: the node always, the gateway
// and the harbor when asked. No unit carries --listen: the address is
// [harbor] listen and [gateway] listen in berth.toml, so a port change
// needs a restart of the service, not a reinstall, and the units in
// contrib/ stay the source of the words.
func serviceUnits(exe string, gateway, harbor bool) []Unit {
	units := []Unit{{
		Name:        "berthd",
		Exec:        []string{exe, "daemon", "--gpu", "0", "--poll", "2s"},
		Description: "berth - GPU lease broker (one card, many inference engines)",
	}}
	if gateway {
		units = append(units, Unit{
			Name:        "berth-gateway",
			Exec:        []string{exe, "gateway"},
			After:       []string{"berthd"},
			Description: "berth gateway - one port that starts engines on demand under a lease",
		})
	}
	if harbor {
		units = append(units, Unit{
			Name:        "berth-harbor",
			Exec:        []string{exe, "harbor"},
			After:       []string{"berthd"},
			Description: "berth harbor - web UI for the local berthd",
		})
	}
	return units
}

// notifyUnit is berth notify as a service. It shows notifications on the
// desktop of the user who runs it, so it is a Linux user unit only: a
// Windows service runs in session 0 and has no desktop.
func notifyUnit(exe string) Unit {
	return Unit{
		Name:        "berth-notify",
		Exec:        []string{exe, "notify"},
		After:       []string{"berthd"},
		Description: "berth notify - desktop notifications for what berthd does",
	}
}

// allUnits is the list uninstall and status report on: every service the
// install could have written.
func allUnits() []Unit {
	all := serviceUnits("", true, true)
	if runtime.GOOS == "linux" {
		all = append(all, notifyUnit(""))
	}
	return all
}

func serviceUnitsForGPU(exe string, gateway, harbor bool, backend, card string) []Unit {
	units := serviceUnits(exe, gateway, harbor)
	units[0].Exec[3] = card
	if backend != "" && backend != "auto" {
		units[0].Exec = append(units[0].Exec, "--gpu-backend", backend)
	}
	return units
}

// serviceUnit is one unit's outcome in the install, uninstall and status
// documents. Content is the file the install writes, set on install rows so
// a dry run shows it. Installed and Running are set by status only.
type serviceUnit struct {
	Unit    string `json:"unit" yaml:"unit"`
	Target  string `json:"target" yaml:"target"`
	Action  string `json:"action" yaml:"action"`
	Content string `json:"content,omitempty" yaml:"content,omitempty"`
	// BackedUp is the .bak a replaced, differing copy was kept as.
	BackedUp string `json:"backed_up,omitempty" yaml:"backed_up,omitempty"`
	// Log is the file one of the services appends its output to, set on
	// Windows.
	Log string `json:"log,omitempty" yaml:"log,omitempty"`

	Installed bool `json:"installed,omitempty" yaml:"installed,omitempty"`
	Running   bool `json:"running,omitempty" yaml:"running,omitempty"`
}

// serviceDoc is what a service command reports: one row per unit and the
// commands it ran, or would have run with --dry-run.
type serviceDoc struct {
	DryRun   bool          `json:"dry_run" yaml:"dry_run"`
	Units    []serviceUnit `json:"units" yaml:"units"`
	Commands []string      `json:"commands" yaml:"commands"`
}

// serviceOps is the seam the platform code runs its external commands
// through, and where it writes its files. Tests replace run, so a test
// never reaches systemctl or schtasks, and point unitDir at a temporary
// directory.
type serviceOps struct {
	run     func(ctx context.Context, name string, args ...string) (string, error)
	unitDir func() (string, error)
}

// liveServiceOps is the real seam: exec for the commands, and liveUnitDir
// (one definition per platform) for the files.
func liveServiceOps() serviceOps {
	return serviceOps{
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
		unitDir: liveUnitDir,
	}
}

// serviceView prints one service command's report.
type serviceView struct {
	verb string // install, uninstall or status
	doc  serviceDoc
}

func (v serviceView) Data() any { return v.doc }

func (v serviceView) Human(c *Context, ok bool) error {
	if v.doc.DryRun {
		c.Printf("%s (dry run: nothing changed)\n", c.Style.Bold("berth service "+v.verb))
	} else {
		c.Printf("%s", c.Style.OK("[OK] berth service "+v.verb))
	}
	for _, u := range v.doc.Units {
		line := u.Unit + ": " + u.Action
		if u.Installed {
			if u.Running {
				line = u.Unit + ": installed, running"
			} else {
				line = u.Unit + ": installed, not running"
			}
		} else if u.Action == "" {
			line = u.Unit + ": not installed"
		}
		if u.BackedUp != "" {
			line += " (previous copy kept as " + u.BackedUp + ")"
		}
		if u.Log != "" {
			line += " (log: " + u.Log + ")"
		}
		c.Bullet("%s", line)
		if v.doc.DryRun && u.Content != "" {
			for _, textLine := range strings.Split(strings.TrimSuffix(u.Content, "\n"), "\n") {
				c.Sub("%s", textLine)
			}
		}
	}
	for _, cmd := range v.doc.Commands {
		c.Sub("%s", cmd)
	}
	return nil
}

func runServiceInstall(c *Context) int {
	fs := c.FlagSet()
	gateway := fs.Bool("gateway", false, "also install the gateway")
	harbor := fs.Bool("harbor", false, "also install the harbor")
	notifyToo := fs.Bool("notify", false, "also start berth notify, desktop notifications: a user unit on Linux, the tray at logon on Windows")
	dryRun := fs.Bool("dry-run", false, "print what would be written and run, change nothing")
	backend := fs.String("gpu-backend", "auto", "daemon GPU probe: auto, nvidia or amd")
	card := fs.String("gpu", "0", "daemon GPU index (AMD: DRM cardN number), or an NVIDIA card's PCI address (0000:02:00.0)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	index, addr, err := gpu.ParseCard(*card)
	if err != nil {
		return c.fail(Failf("%v", err))
	}
	if err := gpu.ValidateSelection(*backend, index); err != nil {
		return c.fail(Failf("%v", err))
	}
	if addr != "" && *backend == "amd" {
		return c.fail(Failf("an AMD card is selected by its DRM cardN number, not a PCI address"))
	}
	if addr != "" {
		*card = addr
	}
	exe, err := os.Executable()
	if err != nil {
		return c.fail(Systemf("find the berth binary: %v", err))
	}
	units := serviceUnitsForGPU(exe, *gateway, *harbor, *backend, *card)
	switch {
	case !*notifyToo:
	case runtime.GOOS == "linux":
		units = append(units, notifyUnit(exe))
	case runtime.GOOS != "windows":
		return c.fail(Failf("--notify is for Linux and Windows; on %s, run berth notify in your own session", runtime.GOOS))
	}
	doc, e := serviceInstall(context.Background(), liveServiceOps(), units, *dryRun)
	if e == nil && *notifyToo && runtime.GOOS == "windows" {
		line, err := notifyAtLogon(exe, true, *dryRun)
		doc.Commands = append(doc.Commands, line)
		if err != nil {
			e = Systemf("start berth notify at logon: %v", err)
		}
	}
	return c.Write(serviceView{verb: "install", doc: *doc}, e)
}

func runServiceUninstall(c *Context) int {
	fs := c.FlagSet()
	dryRun := fs.Bool("dry-run", false, "print what would be run, change nothing")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	doc, e := serviceUninstall(context.Background(), liveServiceOps(), allUnits(), *dryRun)
	if e == nil && runtime.GOOS == "windows" {
		line, err := notifyAtLogon("", false, *dryRun)
		doc.Commands = append(doc.Commands, line)
		if err != nil {
			e = Systemf("stop starting berth notify at logon: %v", err)
		}
	}
	return c.Write(serviceView{verb: "uninstall", doc: *doc}, e)
}

func runServiceStatus(c *Context) int {
	fs := c.FlagSet()
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	doc, e := serviceStatus(context.Background(), liveServiceOps(), allUnits())
	return c.Write(serviceView{verb: "status", doc: *doc}, e)
}
