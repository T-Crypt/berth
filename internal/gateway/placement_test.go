// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/procs"
)

// A plain /v1 request for a model served on two cards goes to the card that
// costs least to reach: a parked copy before a free card, a free card before
// one a lease holds, and a held card before one a game has pinned. With
// nothing to tell them apart, the first entry in berth.toml wins.
func TestPlainRequestPicksTheCheapestCard(t *testing.T) {
	for _, c := range []struct {
		name string
		hold func(t *testing.T, gpu0, gpu1 *broker.Broker)
		want string
	}{
		{"both free", nil, "on-gpu0"},
		{"a game pins gpu0", func(t *testing.T, gpu0, _ *broker.Broker) {
			acquire(t, gpu0, broker.Request{Engine: "game", Priority: broker.Hold, Pinned: true})
		}, "on-gpu1"},
		{"a lease holds gpu0", func(t *testing.T, gpu0, _ *broker.Broker) {
			acquire(t, gpu0, broker.Request{Engine: "tts", Priority: broker.Batch})
		}, "on-gpu1"},
		{"a game pins gpu0, a lease holds gpu1", func(t *testing.T, gpu0, gpu1 *broker.Broker) {
			acquire(t, gpu0, broker.Request{Engine: "game", Priority: broker.Hold, Pinned: true})
			acquire(t, gpu1, broker.Request{Engine: "tts", Priority: broker.Batch})
		}, "on-gpu1"},
		{"qwen parked on gpu1", func(t *testing.T, _, gpu1 *broker.Broker) {
			pid := os.Getpid()
			l, err := gpu1.Reattach(broker.Request{Engine: "qwen-b", Parked: true}, pid, procs.System{}.StartTime(pid))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(l.Release)
		}, "on-gpu1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sock, n := twoCards(t)
			if c.hold != nil {
				c.hold(t, n.Cards()[0].B, n.Cards()[1].B)
			}
			a, b := cardEngine(t, "on-gpu0"), cardEngine(t, "on-gpu1")
			g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				Engines: []config.Engine{
					{Name: "qwen-a", Type: "llamacpp", URL: a.URL, GPU: "gpu0", Serves: []string{"qwen"}},
					{Name: "qwen-b", Type: "llamacpp", URL: b.URL, GPU: "gpu1", Serves: []string{"qwen"}},
				}}
			gw := httptest.NewServer(g.Handler())
			defer gw.Close()
			resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"qwen"}`)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if want := c.want + " /v1/chat/completions"; resp.StatusCode != http.StatusOK || string(body) != want {
				t.Errorf("POST /v1/chat/completions: %d %q, want 200 %q", resp.StatusCode, body, want)
			}
		})
	}
}

// acquire takes a lease on b for the rest of the test.
func acquire(t *testing.T, b *broker.Broker, r broker.Request) {
	t.Helper()
	r.Wait = 5 * time.Second
	l, err := b.Acquire(context.Background(), r, nil)
	if err != nil {
		t.Fatal(fmt.Errorf("acquire %s: %w", r.Engine, err))
	}
	t.Cleanup(l.Release)
}
