// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// A changed berth.toml applies once per change; a file that fails to
// validate keeps what applied and says so; a later good file applies again.
func TestConfigReloaderAppliesChangesAndKeepsOldOnReject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "berth.toml")
	writeFile := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("[[claim]]\nname = 'a'\npaths = ['/g/a']\n")

	var mu sync.Mutex
	var applied []config.Config
	var events [][2]string
	rel := &configReloader{
		path: path,
		merged: func(string) (config.Config, []config.Warning, error) {
			c, err := config.Load(path)
			return c, nil, err
		},
		apply: func(cfg config.Config) string {
			mu.Lock()
			applied = append(applied, cfg)
			mu.Unlock()
			return "ok"
		},
		kind: func(k, d string) {
			mu.Lock()
			events = append(events, [2]string{k, d})
			mu.Unlock()
		},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		every: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rel.Run(ctx)

	// Nothing changes until the file does.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(applied) != 0 {
		t.Fatalf("applied before a change: %d times", len(applied))
	}
	mu.Unlock()

	writeFile("[[claim]]\nname = 'a'\npaths = ['/g/a', '/g/b']\n")
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			ok := len(applied) == n
			mu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		t.Fatalf("applied %d times, want %d", len(applied), n)
		mu.Unlock()
	}
	waitFor(1)

	// A file that fails to validate keeps what applied.
	writeFile("x = \n")
	waitFor(1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		rejected := len(events) > 0 && events[len(events)-1][0] == "config.rejected"
		mu.Unlock()
		if rejected {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("no rejection event: %v", events)
			mu.Unlock()
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A later good file applies again.
	writeFile("[[claim]]\nname = 'a'\npaths = ['/g/c']\n")
	waitFor(2)
	mu.Lock()
	kinds := make([]string, len(events))
	for i, e := range events {
		kinds[i] = e[0]
	}
	mu.Unlock()
	if len(kinds) != 3 || kinds[0] != "config.reloaded" || kinds[1] != "config.rejected" || kinds[2] != "config.reloaded" {
		t.Fatalf("events = %v, want reloaded, rejected, reloaded", kinds)
	}
}
