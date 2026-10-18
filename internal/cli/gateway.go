package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/apikey"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gateway"
)

// runGateway serves one OpenAI-compatible port in front of the node's engines
// (preview). It can load models, so until the API has authentication it listens
// on loopback only.
func runGateway(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "the node's berthd socket ($BERTH_SOCKET)")
	listen := fs.String("listen", "127.0.0.1:9799", "listen address; loopback only in this release")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml: engines with start/serves can be started on demand ($BERTH_CONFIG)")
	ready := fs.Duration("ready", 10*time.Minute, "how long a start may wait for a berth and then to answer")
	headroomMiB := fs.Uint64("headroom-mib", 2048, "RAM kept for the machine on a cold start; a start leaving less available is refused")
	keyFile := fs.String("key-file", apikey.DefaultPath(), "the node's API key, created on first start ($BERTH_API_KEY_FILE)")
	var allow hostList
	fs.Var(&allow, "allow-host", "extra Host name the loopback gateway answers to, for a same-machine reverse proxy such as tailscale serve (repeatable or comma-separated)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	log := c.Log()
	if err := config.Loopback(*listen); err != nil {
		return c.fail(Systemf("gateway listens on loopback only until the API has authentication: %v", err))
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return c.fail(Failf("config invalid: %s", *cfgPath).Detail("%v", err))
	}
	log.Debug("config", "path", *cfgPath, "startable", len(cfg.Engine), "discover", *cfg.Engines.Discover)
	key, err := apikey.LoadOrCreate(*keyFile)
	if err != nil {
		return c.fail(Systemf("api key: %v", err))
	}
	g := &gateway.Gateway{Socket: *sock, Engines: cfg.Engine, Ready: *ready, Key: key, Log: log, HeadroomFloor: *headroomMiB << 20, AllowHosts: allow}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	log.Info("gateway up", "listen", "http://"+*listen+"/v1", "startable", len(cfg.Engine), "key_file", *keyFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return c.fail(Systemf("gateway: %v", err))
	}
	return ExitOK
}
