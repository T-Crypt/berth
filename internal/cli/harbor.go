// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/apikey"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/harbor"
	"github.com/T-Crypt/berth/internal/models"
	"github.com/T-Crypt/berth/internal/store"
)

// DefaultHarborListen is the address the harbor listens on when neither its
// flag nor [harbor] listen in berth.toml names one.
const DefaultHarborListen = "127.0.0.1:9798"

func runHarbor(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "the node's berthd socket ($BERTH_SOCKET)")
	listen := fs.String("listen", "", "web UI listen address, over [harbor] listen in berth.toml, itself 127.0.0.1:9798: a LAN address exposes the harbor (warning at startup)")
	db := fs.String("db", defaultHarborDB(), "SQLite file the harbor persists the node's history in")
	retention := fs.Duration("retention", 720*time.Hour, "how long the store keeps samples and requests")
	var allow hostList
	fs.Var(&allow, "allow-host", "extra Host name a loopback harbor answers to, for a same-machine reverse proxy such as tailscale serve (repeatable or comma-separated)")
	gatewayURL := fs.String("gateway", "", "the gateway's base URL for the SWITCH tab; empty = http:// plus [gateway] listen in berth.toml, itself 127.0.0.1:9799")
	keyFile := fs.String("key-file", apikey.DefaultPath(), "the node's API key the SWITCH tab sends to the gateway; read, never created ($BERTH_API_KEY_FILE)")
	cfgPath := fs.String("config", config.DefaultPath(), "berth.toml ($BERTH_CONFIG): the model roots and the [update] check switch")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	log := c.Log()
	// One read of the config feeds the library's roots and the update
	// check. A config that cannot be read leaves the harbor up on the
	// default roots, with the update check off: no reading the operator's
	// intent, no contacting GitHub.
	updateCheck := false
	cfg, _, _, e := loadConfig(c, *cfgPath)
	if e != nil {
		log.Warn("harbor: the config cannot be read; default model roots, update check off", "path", *cfgPath, "err", e)
		cfg = config.Config{}
	} else {
		updateCheck = cfg.UpdateCheck()
	}
	var shelves []models.RootShelf
	for _, r := range cfg.Library.Root {
		if r.Shelf != "" {
			sh, _ := models.ParseShelf(r.Shelf)
			shelves = append(shelves, models.RootShelf{Path: r.Path, Shelf: sh})
		}
	}
	addr := resolveListen(*listen, cfg.Harbor.Listen, DefaultHarborListen)
	gw := *gatewayURL
	if gw == "" {
		gw = "http://" + resolveListen("", cfg.Gateway.Listen, DefaultGatewayListen)
	}
	key, err := apikey.Load(*keyFile)
	if err != nil {
		log.Warn("harbor: no node API key, so the SWITCH tab cannot reach the gateway", "path", *keyFile, "err", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(*db, *retention)
	if err != nil {
		return c.fail(Systemf("store %s: %v", *db, err))
	}
	defer st.Close()
	h := &harbor.Harbor{Socket: *sock, Version: c.Version, Log: log, Store: st, AllowHosts: allow,
		UpdateCheck: updateCheck, UpdateCache: defaultStateFile("update.json"),
		Dirs: cfg.Models.Dirs, Shelves: shelves, Gateway: gw, GatewayKey: key}
	h.RunCollector(ctx)
	h.RunUpdate(ctx)
	if err := h.Serve(ctx, addr); err != nil {
		return c.fail(Systemf("harbor: %v", err))
	}
	return ExitOK
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
