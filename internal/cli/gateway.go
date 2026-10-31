// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

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
	"github.com/T-Crypt/berth/internal/suite"
)

// DefaultGatewayListen is the address the gateway listens on when neither its
// flag nor [gateway] listen in berth.toml names one.
const DefaultGatewayListen = "127.0.0.1:9799"

// resolveListen is the address a server runs on: the flag when the operator
// typed one, else the berth.toml key, else the built-in default.
func resolveListen(flag, key, def string) string {
	if flag != "" {
		return flag
	}
	if key != "" {
		return key
	}
	return def
}

// runGateway serves one OpenAI-compatible port in front of the node's engines
// (preview). It can load models, so until the API has authentication it listens
// on loopback only.
func runGateway(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "the node's berthd socket ($BERTH_SOCKET)")
	listen := fs.String("listen", "", "listen address, over [gateway] listen in berth.toml, itself 127.0.0.1:9799: loopback only in this release")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml: engines with start/serves can be started on demand ($BERTH_CONFIG)")
	ready := fs.Duration("ready", 10*time.Minute, "how long a start may wait for a berth and then to answer")
	headroomMiB := fs.Uint64("headroom-mib", 2048, "RAM kept for the machine on a cold start; a start leaving less available is refused")
	comfyListen := fs.String("comfyui-listen", "", "serve ComfyUI's own port behind Berth on this loopback address (web UI, API, websocket; a queued prompt wakes or starts ComfyUI first); empty = off")
	guiListen := fs.String("gui-listen", "", "serve the gui plane (/gui/ lists every engine with its state and one button; /gui/<engine> starts an engine with a gui URL, then opens its web UI) on this loopback address; empty = off")
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
	cfg, warns, err := suite.Merged(*cfgPath)
	if err != nil {
		return c.fail(Failf("config invalid: %s", *cfgPath).Detail("%v", err))
	}
	addr := resolveListen(*listen, cfg.Gateway.Listen, DefaultGatewayListen)
	if err := config.Loopback(addr); err != nil {
		return c.fail(Systemf("gateway listens on loopback only until the API has authentication: %v", err))
	}
	if *guiListen != "" {
		if err := config.Loopback(*guiListen); err != nil {
			return c.fail(Systemf("the gui plane listens on loopback only: %v", err))
		}
	}
	if *comfyListen != "" {
		if err := config.Loopback(*comfyListen); err != nil {
			return c.fail(Systemf("the ComfyUI port listens on loopback only: %v", err))
		}
	}
	for _, w := range warns {
		log.Warn(w.Key + ": " + w.Message)
	}
	log.Debug("config", "path", *cfgPath, "startable", len(cfg.Engine), "discover", *cfg.Engines.Discover)
	key, err := apikey.LoadOrCreate(*keyFile)
	if err != nil {
		return c.fail(Systemf("api key: %v", err))
	}
	g := &gateway.Gateway{Socket: *sock, Engines: cfg.Engine, Ready: *ready, ReplyTimeout: cfg.Gateway.ReplyTimeout.Duration, Key: key, Log: log, HeadroomFloor: *headroomMiB << 20,
		AllowHosts: allow, StateFile: defaultStateFile("gateway-engines.json"),
		CostFile: defaultStateFile("swapcost.json"), DeferredFile: defaultStateFile("comfyui-queue.json"), ConfigPath: *cfgPath}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go g.Background(ctx, 0)
	if *guiListen != "" {
		gui := &http.Server{Addr: *guiListen, Handler: g.GUIHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { <-ctx.Done(); _ = gui.Close() }()
		go func() {
			if err := gui.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("gui plane stopped", "err", err)
				stop()
			}
		}()
		log.Info("gui plane up", "url", "http://"+*guiListen+"/gui/")
	}
	if *comfyListen != "" {
		comfy := &http.Server{Addr: *comfyListen, Handler: g.ComfyUIHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { <-ctx.Done(); _ = comfy.Close() }()
		go func() {
			if err := comfy.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("ComfyUI port stopped", "err", err)
				stop()
			}
		}()
		log.Info("ComfyUI port up", "url", "http://"+*comfyListen+"/")
	}
	log.Info("gateway up", "listen", "http://"+addr+"/v1", "startable", len(cfg.Engine), "key_file", *keyFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return c.fail(Systemf("gateway: %v", err))
	}
	return ExitOK
}
