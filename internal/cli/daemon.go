package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/history"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
)

func runDaemon(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "Unix socket to listen on ($BERTH_SOCKET)")
	index := fs.Int("gpu", 0, "GPU index")
	poll := fs.Duration("poll", 2*time.Second, "how often the card is read")
	reserve := fs.Uint64("reserve-mib", 0, "memory never handed to sized leases")
	wait := fs.Duration("wait", 120*time.Second, "default time a request waits before it is refused")
	grace := fs.Duration("release-grace", 30*time.Second, "how long a released lease's processes may linger before they count as squatters")
	reattachWin := fs.Duration("reattach-window", 15*time.Second, "after a restart, how long new grants wait for engines that ran under the last daemon to re-attach")
	allow := fs.String("allow", "", "comma-separated process-name substrings that are never squatters (e.g. Xorg,Hyprland)")
	notify := fs.Bool("notify", false, "desktop notification (notify-send) when a squatter appears")
	webhook := fs.String("webhook", "", "POST squatter events here as JSON")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml: engines to read, /metrics listener ($BERTH_CONFIG); missing = none")
	countersFile := fs.String("counters", defaultStateFile("counters.json"), "keeps the collisions board across restarts; \"\" = start at zero every time")
	usageFile := fs.String("usage", defaultStateFile("usage.json"), "card-hours per engine, the default juggle ranking; \"\" = memory only")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}

	log := c.Log()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return c.fail(Failf("config invalid: %s", *cfgPath).Detail("%v", err))
	}
	log.Debug("config", "path", *cfgPath, "engines", len(cfg.Engine), "discover", *cfg.Engines.Discover,
		"metrics", cfg.Metrics.Listen, "juggle_window", cfg.Priority.JuggleWindow.String())

	probe := &gpu.Stream{Index: *index, Interval: *poll}
	rep := server.Reporter{Notify: *notify, Webhook: *webhook, Log: log}
	// The engine readers exist before the broker: a juggle asks them whether
	// the holder is busy. They start reading below, once the card is known.
	var watcher *engines.Watcher
	if len(cfg.Engine) > 0 || *cfg.Engines.Discover {
		watcher = &engines.Watcher{Engines: cfg.Engine, Every: cfg.Engines.Every.Duration,
			Discover: *cfg.Engines.Discover, Hosts: cfg.Engines.Hosts, Ports: cfg.Engines.Ports}
	}
	// The broker's squat-free path (ask a squatting ComfyUI to unload through
	// its own /free and hold the waiter) fires only for engines this list
	// names. Without it every whole-card request is granted over the squat
	// and dies on it.
	comfy := comfyEngines(cfg)
	b := broker.New(broker.Config{
		Poll:           *poll,
		Reserve:        *reserve * gpu.MiB,
		ReleaseGrace:   *grace,
		DefaultWait:    *wait,
		Allow:          splitList(*allow),
		RunsDir:        runs.Dir(*sock),
		CountersFile:   *countersFile,
		UsageFile:      *usageFile,
		Order:          cfg.Priority.Order,
		JuggleWindow:   cfg.Priority.JuggleWindow.Duration,
		Busy:           watcher.Busy,
		Comfy:          comfy,
		ReattachWindow: *reattachWin,
		Version:        c.Version,
		Reporter:       rep,
		Log:            log,
	}, probe, procs.System{})

	// The alert table is the node's own thresholds: the built-in danger
	// keys plus what the operator enabled. One level crossing is one
	// event and one notification through the same channels as squatters.
	ev := alerts.New(alerts.FromConfig(cfg.Alerts), *cfg.Alerts.ClearPolls,
		func(key alerts.Key, lv alerts.Level, raised bool, msg string) {
			if raised {
				b.Note(proto.Event{Kind: proto.EvAlertRaised, Detail: lv.Name() + ": " + msg})
				rep.Alert(lv.Name(), string(key), msg)
			} else {
				b.Note(proto.Event{Kind: proto.EvAlertCleared, Detail: msg})
			}
		})

	// Inventory every card with its real limits before taking any lease, and
	// refuse to schedule a card that does not exist.
	snap, err := probe.Read(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		return c.fail(Systemf("nvidia-smi not found: Berth needs an NVIDIA driver (Linux + NVIDIA is the supported platform)"))
	} else if err != nil {
		return c.fail(Systemf("gpu inventory: %v", err))
	}
	for _, d := range snap.Devices {
		log.Info("gpu found", "index", d.Index, "name", d.Name, "total_mib", d.Total/gpu.MiB, "scheduled", d.Index == *index)
	}

	ln, err := server.Listen(*sock)
	if err != nil {
		return c.fail(Systemf("listen on %s: %v", *sock, err))
	}
	defer os.Remove(*sock)
	log.Info("berthd up", "version", c.Version, "socket", *sock, "gpu", *index)
	go probe.Run(ctx)
	go b.Run(ctx)
	if watcher != nil {
		go watcher.Run(ctx)
		for _, e := range cfg.Engine {
			log.Info("reading engine", "name", e.Name, "type", e.Type, "url", e.URL)
		}
		if *cfg.Engines.Discover {
			log.Info("discovering engines", "every", engines.DiscoveryEvery, "hosts", cfg.Engines.Hosts, "extra_ports", cfg.Engines.Ports)
		}
	}
	logComfyArmed(log, comfy)
	if cfg.Metrics.Listen != "" {
		go func() {
			if err := server.ServeMetrics(ctx, cfg.Metrics.Listen, b, watcher.Snapshot, log); err != nil {
				log.Error("metrics", "err", err)
			}
		}()
	}
	// The sampler feeds the graphs, caches host stats for status, so a
	// status read never resets the CPU interval, and runs the alert
	// evaluator on the same sample it records: one clock for the levels,
	// the events and the notifications.
	hr := &host.Reader{}
	rec := &history.Recorder{Every: 5 * time.Second}
	var lastHost atomic.Pointer[proto.Host]
	var lastAlerts atomic.Pointer[proto.Alerts]
	lastAlerts.Store(ev.Snapshot())
	go samplerLoop(ctx, rec, hr, b, watcher, ev, &lastHost, &lastAlerts)
	x := server.Extras{Engines: watcher.Snapshot, Host: lastHost.Load, History: rec.Get,
		Insights: func() *proto.Insights { return watcher.Insights(time.Now()) },
		Requests: watcher.Requests, Alerts: lastAlerts.Load}
	if err := server.Serve(ctx, ln, b, x, log); err != nil {
		return c.fail(Systemf("serve: %v", err))
	}
	return ExitOK
}

// samplerLoop builds one status per tick, records it in the in-memory
// history and runs the alert evaluator on that same sample, so the
// levels, the events and the notifications all come from one clock. The
// first tick fires immediately, then every rec.Every until ctx is done.
func samplerLoop(ctx context.Context, rec *history.Recorder, hr *host.Reader, b *broker.Broker,
	watcher *engines.Watcher, ev *alerts.Evaluator,
	hostOut *atomic.Pointer[proto.Host], alertsOut *atomic.Pointer[proto.Alerts]) {
	t := time.NewTicker(rec.Every)
	defer t.Stop()
	for {
		hs := proto.Host(hr.Read())
		hostOut.Store(&hs)
		st := b.Status()
		st.Engines, st.Host, st.Insights = watcher.Snapshot(), &hs, watcher.Insights(time.Now())
		rec.Add(history.Sample(st))
		ev.Tick(alerts.FromStatus(st))
		alertsOut.Store(ev.Snapshot())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// comfyEngines names the configured ComfyUI engines the broker may ask to
// unload their models when a waiter needs the card they squat.
func comfyEngines(cfg config.Config) []broker.ComfyEngine {
	var out []broker.ComfyEngine
	for _, e := range cfg.Engine {
		if e.Type == "comfyui" {
			out = append(out, broker.ComfyEngine{Name: e.Name, URL: e.URL})
		}
	}
	return out
}

// logComfyArmed reports the squat-free arming at startup. The empty case is
// logged too: an engine found only by discovery is never armed (comfyEngines
// reads cfg.Engine), and that has to be visible, because a silently empty list
// turns every ComfyUI squat into a contested grant.
func logComfyArmed(log *slog.Logger, comfy []broker.ComfyEngine) {
	if len(comfy) == 0 {
		log.Info("comfy squat-free not armed: no [[engine]] with type \"comfyui\"")
		return
	}
	for _, e := range comfy {
		log.Info("comfy squat-free armed", "engine", e.Name, "url", e.URL)
	}
}

// splitList splits a comma-separated flag value, dropping the empties.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
