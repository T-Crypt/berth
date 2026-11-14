// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
)

// A render asked for while a game holds the card is queued, answered at
// once with a berth id, kept across a gateway restart, submitted when the
// game lets go, and read back from /history under the berth id.
func TestComfyJobQueuedWhileTheCardIsHeldRunsWhenItIsFree(t *testing.T) {
	sock, b := daemon(t)
	game, err := b.Acquire(context.Background(), broker.Request{Engine: "game", Priority: broker.Hold, Pinned: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	comfy := freeAddr(t)
	queue := filepath.Join(t.TempDir(), "comfyui-queue.json")
	gateway := func() *Gateway {
		return &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			DeferredFile: queue,
			Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://" + comfy,
				Start: []string{"env", "BERTH_GW_COMFY=" + comfy, os.Args[0]}, OnBusy: "queue"}}}
	}
	first := httptest.NewServer(gateway().Handler())

	resp := call(t, "POST", first.URL+"/prompt", `{"prompt":{"3":{"class_type":"KSampler"}}}`)
	var got struct {
		PromptID string `json:"prompt_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	why := resp.Header.Get("X-Berth-Deferred")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(got.PromptID, "berth-") || !strings.Contains(why, "holds the card") {
		t.Fatalf("prompt = %d %+v, X-Berth-Deferred %q", resp.StatusCode, got, why)
	}
	for _, l := range b.Status().Leases {
		if l.Engine == "comfyui" {
			t.Fatalf("ComfyUI started while the game held the card: %+v", b.Status().Leases)
		}
	}
	history := func(srv *httptest.Server) string {
		resp := call(t, "GET", srv.URL+"/history/"+got.PromptID, "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}
	if h := history(first); strings.TrimSpace(h) != "{}" {
		t.Fatalf("history while queued = %s, want {}", h)
	}
	first.Close()

	// The gateway restarts; the job is on disk and the new one picks it up.
	g := gateway()
	second := httptest.NewServer(g.Handler())
	defer second.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Background(ctx, 50*time.Millisecond)

	game.Release()
	until(t, "the queued render to come back from ComfyUI under its berth id", func() bool {
		h := history(second)
		return strings.Contains(h, `"`+got.PromptID+`"`) && strings.Contains(h, "history-marker")
	})
}
