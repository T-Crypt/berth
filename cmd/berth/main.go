// berth leases one GPU to one inference engine at a time.
//
//	berth daemon                         run the broker (one per card)
//	berth run --as NAME [flags] -- CMD   start CMD once it holds a berth
//	berth status [--json]                who is docked, waiting, squatting
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/apikey"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/export"
	"github.com/T-Crypt/berth/internal/gateway"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/harbor"
	"github.com/T-Crypt/berth/internal/history"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/store"
)

// version is set by the release build (-X main.version=); otherwise it
// comes from the module version Go records (go install ...@v0.1.0), and
// a plain build from a checkout reads "dev".
var version = ""

func init() {
	if version != "" {
		return
	}
	version = "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = strings.TrimPrefix(bi.Main.Version, "v")
	}
}

func defaultSocket() string {
	if s := os.Getenv("BERTH_SOCKET"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "berth.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("berth-%d.sock", os.Getuid()))
}

func usage() {
	fmt.Fprint(os.Stderr, `berth `+version+` - one GPU, many inference engines, taking turns

  berth daemon [flags]                 run the broker for one card
  berth run --as NAME [flags] -- CMD   start CMD once it holds a berth; stop it when asked
  berth status [--json]                leases, waiters, squatters, collisions board
  berth events [-f] [--since N]        what berthd saw and did; -f follows
  berth config check [PATH]            validate berth.toml without applying it
  berth harbor [flags]                 the web UI, read-only; loopback by default,
                                       LAN exposure via --listen; persists node
                                       history in its own SQLite db
  berth export [flags]                 dump the stored history for analysis: the
                                       requests and the samples, CSV or JSON
  berth gateway [flags]                (preview) one OpenAI-compatible port for
                                       every engine; starts cold engines under
                                       a lease; loopback only, needs the API key
  berth key [--file PATH]              print the node's API key, creating it on
                                       first use (clients: apiKey "!berth key")

Run "berth <command> -h" for flags.
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(daemon(os.Args[2:]))
	case "run":
		os.Exit(run(os.Args[2:]))
	case "status":
		os.Exit(status(os.Args[2:]))
	case "events":
		os.Exit(events(os.Args[2:]))
	case "config":
		os.Exit(configCmd(os.Args[2:]))
	case "harbor":
		os.Exit(harborCmd(os.Args[2:]))
	case "export":
		os.Exit(exportCmd(os.Args[2:]))
	case "gateway":
		os.Exit(gatewayCmd(os.Args[2:]))
	case "key":
		os.Exit(keyCmd(os.Args[2:]))
	case "version", "--version":
		fmt.Println(version)
	default:
		usage()
	}
}

func daemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
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
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}

	probe := &gpu.Stream{Index: *index, Interval: *poll}
	rep := server.Reporter{Notify: *notify, Webhook: *webhook, Log: log}
	// The engine readers exist before the broker: a juggle asks them whether
	// the holder is busy. They start reading below, once the card is known.
	var watcher *engines.Watcher
	discover := cfg.Engines.Discover == nil || *cfg.Engines.Discover
	if len(cfg.Engine) > 0 || discover {
		watcher = &engines.Watcher{Engines: cfg.Engine, Every: cfg.Engines.Every.Duration,
			Discover: discover, Hosts: cfg.Engines.Hosts, Ports: cfg.Engines.Ports}
	}
	var comfyEngines []broker.ComfyEngine
	for _, e := range cfg.Engine {
		if e.Type == "comfyui" {
			comfyEngines = append(comfyEngines, broker.ComfyEngine{Name: e.Name, URL: e.URL})
		}
	}
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
		Comfy:          comfyEngines,
		ReattachWindow: *reattachWin,
		Version:        version,
		Reporter:       rep,
		Log:            log,
	}, probe, procs.System{})

	// The alert table is the node's own thresholds: the built-in danger
	// keys plus what the operator enabled. One level crossing is one
	// event and one notification through the same channels as squatters.
	table := alerts.FromConfig(cfg.Alerts)
	clearPolls := 3
	if cfg.Alerts.ClearPolls != nil {
		clearPolls = *cfg.Alerts.ClearPolls
	}
	ev := alerts.New(table, clearPolls, func(key alerts.Key, lv alerts.Level, raised bool, msg string) {
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
		log.Error("nvidia-smi not found: Berth needs an NVIDIA driver (Linux + NVIDIA is the supported platform)")
		return 1
	} else if err != nil {
		log.Error("gpu inventory", "err", err)
		return 1
	}
	for _, d := range snap.Devices {
		log.Info("gpu found", "index", d.Index, "name", d.Name, "total_mib", d.Total/gpu.MiB, "scheduled", d.Index == *index)
	}

	ln, err := server.Listen(*sock)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	defer os.Remove(*sock)
	log.Info("berthd up", "version", version, "socket", *sock, "gpu", *index)
	go probe.Run(ctx)
	go b.Run(ctx)
	if watcher != nil {
		go watcher.Run(ctx)
		for _, e := range cfg.Engine {
			log.Info("reading engine", "name", e.Name, "type", e.Type, "url", e.URL)
		}
		if discover {
			log.Info("discovering engines", "every", engines.DiscoveryEvery, "hosts", cfg.Engines.Hosts, "extra_ports", cfg.Engines.Ports)
		}
	}
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
		log.Error("serve", "err", err)
		return 1
	}
	return 0
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

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	engine := fs.String("as", "", "engine name shown in status and drain messages (required)")
	group := fs.String("group", "", "engine family for juggles and the usage ranking, e.g. llama-swap for each of its models (default: --as)")
	prio := fs.String("priority", "interactive", "batch, interactive or foreground")
	needs := fs.Uint64("needs-mib", 0, "memory this engine needs; 0 = the whole card")
	wait := fs.Duration("wait", 0, "how long to wait for a berth; 0 = berthd's default")
	drainPost := fs.String("drain-post", "", "URL to POST when asked to drain, before signalling (e.g. llama-swap's unload)")
	drainComfy := fs.String("drain-comfyui", "", "ComfyUI base URL: on drain, wait until its queue is empty before stopping it (never cuts a job off)")
	keepComfy := fs.Bool("keep-comfyui-up", false, "with --drain-comfyui: unload its models via POST /free and leave the server up instead of stopping it")
	grace := fs.Duration("grace", 30*time.Second, "time each drain step gets before the next")
	failClosed := fs.Bool("fail-closed", false, "do not start CMD when berthd is unreachable")
	fs.Parse(args)
	if *engine == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: berth run --as NAME [flags] -- CMD [ARGS...]")
		fs.PrintDefaults()
		return 2
	}
	if _, err := broker.ParsePriority(*prio); err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		return 2
	}
	if err := checkRunFlags(*keepComfy, *drainComfy); err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		return 2
	}
	return runner.Run(runner.Options{
		Socket: *sock, Engine: *engine, Group: *group, Priority: *prio, Needs: *needs * gpu.MiB, Wait: *wait,
		DrainPost: *drainPost, DrainComfyUI: *drainComfy, KeepComfyUI: *keepComfy, Grace: *grace, FailClosed: *failClosed, Argv: fs.Args(), Stderr: os.Stderr,
	})
}

// checkRunFlags rejects flag combinations that can never work, mirroring
// the berth.toml validation, before anything starts.
func checkRunFlags(keepComfy bool, drainComfy string) error {
	if keepComfy && drainComfy == "" {
		return fmt.Errorf("--keep-comfyui-up needs --drain-comfyui, the ComfyUI base URL to unload on drain")
	}
	return nil
}

func status(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)

	conn, raw, err := server.Dial(*sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "berth: berthd unreachable on %s: %v\n", *sock, err)
		return 1
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		return 1
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		fmt.Fprintln(os.Stderr, "berth: no status from berthd:", err)
		return 1
	}
	st := m.Status
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(st)
		return 0
	}

	if len(st.GPUs) == 0 {
		fmt.Println("gpu     no reading yet")
	}
	for _, g := range st.GPUs {
		role := "inventory only"
		if g.Scheduled {
			role = "leased by berth, baseline " + server.MiB(g.Baseline)
		}
		fmt.Printf("gpu%d    %s: %s used of %s, %s free (%s, read %s ago)\n", g.Index, g.Name,
			server.MiB(g.Used), server.MiB(g.Total), server.MiB(g.Free), role, age(g.Age))
	}
	if st.ProbeErr != "" {
		fmt.Printf("probe   FAILING: %s\n", st.ProbeErr)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	section := func(title string, ls []proto.Lease) {
		if len(ls) == 0 {
			return
		}
		fmt.Fprintf(tw, "\n%s\tENGINE\tPRIORITY\tSTATE\tNEEDS\tHOLDS\tPIDS\tAGE\n", title)
		for _, l := range ls {
			needs := "card"
			if l.Needs > 0 {
				needs = server.MiB(l.Needs)
			}
			state := l.State
			if l.Contested > 0 {
				state += " (contested " + server.MiB(l.Contested) + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n", l.ID, l.Engine, l.Priority, state, needs, server.MiB(l.Used), l.GPUPIDs, age(l.Age))
		}
	}
	section("LEASE", st.Leases)
	section("WAITING", st.Waiting)
	if len(st.Squatters) > 0 {
		fmt.Fprintf(tw, "\nSQUATTER\tPID\tHOLDS\tSEEN\tUNPROTECTED RUN\n")
		for _, s := range st.Squatters {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", s.Name, s.PID, server.MiB(s.Used), age(s.Age), s.Run)
		}
	}
	if len(st.Unprotected) > 0 {
		fmt.Fprintf(tw, "\nUNPROTECTED\tPID\tSINCE\tWHY\n")
		for _, r := range st.Unprotected {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", r.Engine, r.PID, age(r.Age), r.Reason)
		}
	}
	tw.Flush()
	if len(st.Engines) > 0 {
		fmt.Fprintf(tw, "\nENGINE\tTYPE\tUP\tMODELS\tDECODE\tPREFILL\tOUT TOKENS\tQUEUE\n")
		for _, e := range st.Engines {
			up := "yes"
			if !e.Up {
				up = "NO: " + e.Error
				if e.State == "latched" {
					up = "LATCHED: " + e.Error
				}
			}
			var names []string
			for _, m := range e.Models {
				names = append(names, m.Name)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f t/s\t%.0f t/s\t%.0f\t%.0f\n", e.Name, e.Type, up, strings.Join(names, ","),
				e.DecodeTPS, e.PrefillTPS, e.Metrics.OutputTokens, e.Metrics.Processing+e.Metrics.Waiting)
		}
		tw.Flush()
	}
	c := st.Counters
	fmt.Printf("\nboard   %d collisions avoided, %s handed off, %d drains, %d stuck caught, %d squatters caught, %d contested grants, %d refused, %d juggles stopped (since %s)\n",
		c.CollisionsAvoided, server.MiB(c.VRAMHandedOff), c.Drains, c.StuckCaught, c.SquattersCaught, c.ContestedGrants, c.Refused, c.JugglesStopped, c.Since.Local().Format("Jan 2 15:04"))
	if p := st.Priority; p != nil && len(p.Engines) > 0 {
		parts := make([]string, 0, len(p.Engines))
		for _, e := range p.Engines {
			parts = append(parts, fmt.Sprintf("%d %s (%.1f h/7d)", e.Rank, e.Engine, e.Hours7d))
		}
		win := "off"
		if p.JuggleWindow > 0 {
			win = fmt.Sprintf("%gs", p.JuggleWindow)
		}
		fmt.Printf("juggles ranked by %s, window %s: %s\n", p.Source, win, strings.Join(parts, ", "))
	}
	if len(st.Leases)+len(st.Waiting)+len(st.Squatters)+len(st.Unprotected) == 0 {
		fmt.Println("no leases, nobody waiting, no squatters")
	}
	return 0
}

// gatewayCmd serves one OpenAI-compatible port in front of the node's
// engines (preview). It can load models, so until the API has authentication
// it listens on loopback only.
func gatewayCmd(args []string) int {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	sock := fs.String("socket", defaultSocket(), "the node's berthd socket ($BERTH_SOCKET)")
	listen := fs.String("listen", "127.0.0.1:9799", "listen address; loopback only in this release")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml: engines with start/serves can be started on demand ($BERTH_CONFIG)")
	ready := fs.Duration("ready", 10*time.Minute, "how long a start may wait for a berth and then to answer")
	headroomMiB := fs.Uint64("headroom-mib", 2048, "RAM kept for the machine on a cold start; a start leaving less available is refused")
	keyFile := fs.String("key-file", apikey.DefaultPath(), "the node's API key, created on first start ($BERTH_API_KEY_FILE)")
	fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := config.Loopback(*listen); err != nil {
		log.Error("gateway listens on loopback only until the API has authentication", "err", err)
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		return 1
	}
	key, err := apikey.LoadOrCreate(*keyFile)
	if err != nil {
		log.Error("api key", "err", err)
		return 1
	}
	g := &gateway.Gateway{Socket: *sock, Engines: cfg.Engine, Ready: *ready, Key: key, Log: log, HeadroomFloor: *headroomMiB << 20}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	log.Info("gateway up", "listen", "http://"+*listen+"/v1", "startable", len(cfg.Engine), "key_file", *keyFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("gateway", "err", err)
		return 1
	}
	return 0
}

// keyCmd prints the node's API key for a client to send as its bearer token.
func keyCmd(args []string) int {
	fs := flag.NewFlagSet("key", flag.ExitOnError)
	file := fs.String("file", apikey.DefaultPath(), "the key file ($BERTH_API_KEY_FILE)")
	fs.Parse(args)
	key, err := apikey.LoadOrCreate(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth key:", err)
		return 1
	}
	fmt.Println(key)
	return 0
}

func harborCmd(args []string) int {
	fs := flag.NewFlagSet("harbor", flag.ExitOnError)
	sock := fs.String("socket", defaultSocket(), "the node's berthd socket ($BERTH_SOCKET)")
	listen := fs.String("listen", "127.0.0.1:9798", "web UI listen address: loopback (default) or a LAN address, which exposes the harbor (warning at startup)")
	db := fs.String("db", defaultHarborDB(), "SQLite file the harbor persists the node's history in")
	retention := fs.Duration("retention", 720*time.Hour, "how long the store keeps samples and requests")
	var allow hostList
	fs.Var(&allow, "allow-host", "extra Host name a loopback harbor answers to, for a same-machine reverse proxy such as tailscale serve (repeatable or comma-separated)")
	fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(*db, *retention)
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}
	defer st.Close()
	h := &harbor.Harbor{Socket: *sock, Version: version, Log: log, Store: st, AllowHosts: allow}
	h.RunCollector(ctx)
	if err := h.Serve(ctx, *listen); err != nil {
		log.Error("harbor", "err", err)
		return 1
	}
	return 0
}

// hostList is the --allow-host flag: repeatable, each value comma-separated.
type hostList []string

func (l *hostList) String() string { return strings.Join(*l, ",") }

func (l *hostList) Set(v string) error {
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			*l = append(*l, h)
		}
	}
	return nil
}

// exportCmd dumps the harbor's stored history to the operator's own
// analysis: the requests table and the history samples of one range, as CSV
// (wide and flattened) or JSON (the store's wire shape verbatim). It reads
// through the store's read methods only and never creates a store file.
func exportCmd(args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	db := fs.String("db", defaultHarborDB(), "the harbor's SQLite file (what berth harbor persists to)")
	rng := fs.String("range", "30d", "how far back: 1h, 6h, 24h, 7d or 30d")
	format := fs.String("format", "csv", "csv or json")
	out := fs.String("out", ".", "directory the dump is written to")
	fs.Parse(args)
	d, ok := store.ParseRange(*rng)
	if !ok {
		fmt.Fprintln(os.Stderr, "berth: range must be 1h, 6h, 24h, 7d or 30d")
		usage()
	}
	if *format != "csv" && *format != "json" {
		fmt.Fprintln(os.Stderr, "berth: format must be csv or json")
		usage()
	}
	if _, err := os.Stat(*db); err != nil {
		fmt.Fprintf(os.Stderr, "berth: no harbor store at %s (run `berth harbor` first)\n", *db)
		return 1
	}
	// retention 0 takes the default; irrelevant, the command never prunes.
	st, err := store.Open(*db, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	defer st.Close()
	now := time.Now()
	reqs, err := st.Requests(d, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	hh, err := st.History(d, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	ext := "csv"
	if *format == "json" {
		ext = "json"
	}
	write := func(file string, csvW func(io.Writer) error, jsonW func(*json.Encoder) error) error {
		f, err := os.Create(file)
		if err != nil {
			return err
		}
		var w error
		if *format == "csv" {
			w = csvW(f)
		} else {
			w = jsonW(json.NewEncoder(f))
		}
		if cerr := f.Close(); w == nil {
			w = cerr
		}
		return w
	}
	if err := write(filepath.Join(*out, "requests."+ext),
		func(w io.Writer) error { return export.RequestsCSV(reqs, w) },
		func(e *json.Encoder) error { return e.Encode(reqs) }); err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	if err := write(filepath.Join(*out, "samples."+ext),
		func(w io.Writer) error { return export.SamplesCSV(hh, w) },
		func(e *json.Encoder) error { return e.Encode(hh) }); err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	return 0
}

// defaultHarborDB is $XDG_STATE_HOME/berth/harbor.db, falling back to
// ~/.local/state/berth/harbor.db.
func defaultHarborDB() string {
	return defaultStateFile("harbor.db")
}

// defaultStateFile is $XDG_STATE_HOME/berth/NAME, falling back to
// ~/.local/state/berth/NAME: kept across reboots, unlike the runtime dir.
func defaultStateFile(name string) string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "berth", name)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "berth", name)
	}
	return filepath.Join(os.TempDir(), "berth", name)
}

func configCmd(args []string) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(os.Stderr, "usage: berth config check [PATH]")
		return 2
	}
	path := config.DefaultPath()
	if len(args) > 1 {
		path = args[1]
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) && len(args) == 1 {
		// No file is a valid setup: defaults, and discovery finds the engines.
		fmt.Printf("%s: no config file; berth runs on its defaults and discovers engines on their usual ports\n", path)
		return 0
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "berth: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth: invalid config\n"+err.Error())
		return 1
	}
	fmt.Printf("%s: ok, %d engine(s)", path, len(cfg.Engine))
	if cfg.Metrics.Listen != "" {
		fmt.Printf(", /metrics on %s", cfg.Metrics.Listen)
	}
	fmt.Println(" (validated only; nothing was applied)")
	return 0
}

func events(args []string) int {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	sock := fs.String("socket", defaultSocket(), "berthd socket ($BERTH_SOCKET)")
	follow := fs.Bool("f", false, "keep following new events")
	since := fs.Uint64("since", 0, "only events after this sequence number")
	asJSON := fs.Bool("json", false, "one JSON event per line")
	fs.Parse(args)

	conn, raw, err := server.Dial(*sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "berth: berthd unreachable on %s: %v\n", *sock, err)
		return 1
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpEvents, Since: *since, Watch: *follow}); err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	for {
		m, err := conn.Recv()
		if err != nil {
			return 0
		}
		if m.Op == proto.OpRefused {
			fmt.Fprintln(os.Stderr, "berth:", m.Reason)
			return 1
		}
		e := m.Event
		if e == nil {
			continue
		}
		if *asJSON {
			enc.Encode(e)
			continue
		}
		line := fmt.Sprintf("%6d  %s  %-18s", e.Seq, e.At.Local().Format("15:04:05.000"), e.Kind)
		if e.Engine != "" {
			line += "  " + e.Engine
		}
		if e.Lease != "" && e.Lease != e.Engine {
			line += " (" + e.Lease + ")"
		}
		if e.PID != 0 {
			line += fmt.Sprintf("  pid %d", e.PID)
		}
		if e.Bytes != 0 {
			line += "  " + server.MiB(e.Bytes)
		}
		if e.Detail != "" {
			line += "  " + e.Detail
		}
		fmt.Println(line)
	}
}

func age(s float64) string {
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
