package cli

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/T-Crypt/berth/internal/harbor"
	"github.com/T-Crypt/berth/internal/store"
)

func runHarbor(c *Context) int {
	fs := c.FlagSet()
	sock := fs.String("socket", defaultSocket(), "the node's berthd socket ($BERTH_SOCKET)")
	listen := fs.String("listen", "127.0.0.1:9798", "web UI listen address: loopback (default) or a LAN address, which exposes the harbor (warning at startup)")
	db := fs.String("db", defaultHarborDB(), "SQLite file the harbor persists the node's history in")
	retention := fs.Duration("retention", 720*time.Hour, "how long the store keeps samples and requests")
	var allow hostList
	fs.Var(&allow, "allow-host", "extra Host name a loopback harbor answers to, for a same-machine reverse proxy such as tailscale serve (repeatable or comma-separated)")
	if code, done := c.Parse(fs); done {
		return code
	}
	if code, done := c.noArgs(); done {
		return code
	}
	log := c.Log()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(*db, *retention)
	if err != nil {
		return c.fail(Systemf("store %s: %v", *db, err))
	}
	defer st.Close()
	h := &harbor.Harbor{Socket: *sock, Version: c.Version, Log: log, Store: st, AllowHosts: allow}
	h.RunCollector(ctx)
	if err := h.Serve(ctx, *listen); err != nil {
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
