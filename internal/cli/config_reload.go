// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
)

// reloadEvery is how often the daemon asks whether berth.toml changed. The
// claim watcher's own 2 s scan sets the floor for how quickly a new rule can
// matter, so a second is plenty, and a stat loop needs no new dependency.
const reloadEvery = time.Second

// configReloader applies a changed berth.toml to the running daemon through
// apply: the claim rules, the brokers' scheduling fields and the engine
// list. A file that fails to validate is left in place - the applied config
// keeps - and said so on the event stream. Start-up facts stay put and need
// a restart: the scan and poll intervals (their tickers were built at
// start-up), the metrics listener, and the gateway's own copy of the config.
type configReloader struct {
	path     string
	merged   func(string) (config.Config, []config.Warning, error)
	apply    func(cfg config.Config) string // returns the event detail
	kind     func(kind, detail string)      // one event on the node's stream
	log      *slog.Logger
	every    time.Duration
	lastMod  time.Time
	lastSize int64
}

// Run watches until ctx ends. A config that is absent at start-up is absent
// for good: the daemon runs on its flags, and a file appearing mid-run is a
// restart's job.
func (r *configReloader) Run(ctx context.Context) {
	armed := false
	if fi, err := os.Stat(r.path); err == nil {
		r.lastMod, r.lastSize, armed = fi.ModTime(), fi.Size(), true
	}
	if !armed {
		return
	}
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fi, err := os.Stat(r.path)
		if err != nil {
			return // deleted or moved mid-run: stop watching, keep what applied
		}
		if fi.ModTime().Equal(r.lastMod) && fi.Size() == r.lastSize {
			continue
		}
		cfg, warns, err := r.merged(r.path)
		if err != nil {
			r.lastMod, r.lastSize = fi.ModTime(), fi.Size() // do not re-reject the same file
			r.kind(proto.EvConfigRejected, err.Error())
			r.log.Error("config reload rejected; the applied config keeps", "path", r.path, "err", err)
			continue
		}
		for _, w := range warns {
			r.log.Warn(w.Key + ": " + w.Message)
		}
		detail := r.apply(cfg)
		r.lastMod, r.lastSize = fi.ModTime(), fi.Size()
		r.kind(proto.EvConfigReloaded, detail)
		r.log.Info("config reloaded", "path", r.path, "what", detail)
	}
}
