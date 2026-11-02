// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
)

func TestRunAuthenticatesDrainRequests(t *testing.T) {
	for _, kind := range []string{"slots", "post", "comfy", "file"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("FIXTURE_DRAIN_KEY", "fixture-boundary-token")
			var busy atomic.Bool
			busy.Store(true)
			var reads, unauthorized, actions atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-boundary-token" {
					unauthorized.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/slots":
					fmt.Fprintf(w, `[{"is_processing":%t}]`, busy.Load())
				case "/queue":
					if busy.Load() {
						fmt.Fprint(w, `{"queue_running":[[1]],"queue_pending":[]}`)
					} else {
						fmt.Fprint(w, `{"queue_running":[],"queue_pending":[]}`)
					}
				case "/unload", "/free":
					actions.Add(1)
					fmt.Fprint(w, `{}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			card := &parkCard{total: 24 << 30}
			sock, b := parkDaemon(t, card, 16<<30)
			e := config.Engine{Name: "fixture", Priority: "interactive", NeedsMiB: config.MiBs{8192}, APIKeyEnv: "FIXTURE_DRAIN_KEY"}
			switch kind {
			case "file":
				e.APIKeyEnv = ""
				e.APIKeyFile = filepath.Join(t.TempDir(), "key")
				if err := os.WriteFile(e.APIKeyFile, []byte("fixture-boundary-token"), 0600); err != nil {
					t.Fatal(err)
				}
				e.DrainSlots = srv.URL
			case "slots":
				e.DrainSlots = srv.URL
			case "post":
				e.DrainPost = srv.URL + "/unload"
			case "comfy":
				e.DrainComfyUI = srv.URL
			}
			var logs syncBuf
			exited := make(chan int, 1)
			o := OptionsFor(&e, Over{Socket: sock, Grace: 20 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: &logs})
			o.Retry = time.Millisecond
			go func() { exited <- Run(o) }()
			pid := attachAndMeasure(t, b, card)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			holder := acquireIn(b, broker.Request{Engine: "next", Priority: broker.Foreground})
			t.Cleanup(func() {
				card.set()
				select {
				case l := <-holder:
					if l != nil {
						l.Release()
					}
				case <-time.After(time.Second):
				}
			})
			waitFor(t, "a boundary request", func() bool { return reads.Load() > 0 })
			if unauthorized.Load() != 0 {
				t.Fatal("runner omitted the configured bearer token")
			}
			select {
			case code := <-exited:
				t.Fatalf("stopped at a busy boundary: %d", code)
			default:
			}
			busy.Store(false)
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				t.Fatalf("runner did not finish: %s", logs.String())
			}
			if (kind == "post" || kind == "comfy") && actions.Load() != 1 {
				t.Fatalf("authenticated actions = %d, want 1", actions.Load())
			}
		})
	}
}

func TestRunRejectsMissingConfiguredCredential(t *testing.T) {
	t.Setenv("FIXTURE_MISSING_KEY", "")
	e := config.Engine{Name: "fixture", APIKeyEnv: "FIXTURE_MISSING_KEY"}
	var logs syncBuf
	o := OptionsFor(&e, Over{Socket: filepath.Join(t.TempDir(), "none.sock"), Argv: []string{"true"}, Stderr: &logs})
	if got := Run(o); got != ExitRefused {
		t.Fatalf("exit = %d, want refusal before fail-open startup", got)
	}
}
