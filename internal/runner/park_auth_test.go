// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
)

func TestRunAuthenticatesParkAndResume(t *testing.T) {
	for _, kind := range []string{"comfyui", "vllm", "sglang", "custom"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("FIXTURE_PARK_KEY", "fixture-park-token")
			var unauthorized, parks, wakes, ready atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-park-token" {
					unauthorized.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/free", "/sleep", "/release_memory_occupation", "/berth/park":
					parks.Add(1)
				case "/wake_up", "/resume_memory_occupation", "/berth/resume":
					wakes.Add(1)
				case "/system_stats", "/v1/models":
					ready.Add(1)
				}
				if r.URL.Path == "/berth/state" {
					fmt.Fprint(w, `{"state":"hot"}`)
				} else {
					fmt.Fprint(w, `{}`)
				}
			}))
			defer srv.Close()
			card := &parkCard{total: 24 << 30}
			sock, b := parkDaemon(t, card, 24<<30)
			e := config.Engine{Name: "fixture", Type: kind, URL: srv.URL, Park: "hook", NeedsMiB: config.MiBs{8192}, APIKeyEnv: "FIXTURE_PARK_KEY"}
			var logs syncBuf
			exited := make(chan int, 1)
			o := OptionsFor(&e, Over{Socket: sock, Grace: 20 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: &logs})
			go func() { exited <- Run(o) }()
			pid := attachAndMeasure(t, b, card)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			holder := acquireIn(b, broker.Request{Engine: "next", Priority: broker.Foreground})
			t.Cleanup(func() {
				if holder == nil {
					return
				}
				card.set()
				select {
				case lease := <-holder:
					if lease != nil {
						lease.Release()
					}
				case <-time.After(time.Second):
				}
			})
			waitFor(t, "park request or authentication refusal", func() bool { return parks.Load() > 0 || unauthorized.Load() > 0 })
			if unauthorized.Load() > 0 {
				t.Fatal("native parking omitted the configured bearer token")
			}
			card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
			waitFor(t, "parked lease", func() bool {
				for _, lease := range b.Status().Leases {
					if lease.Engine == "fixture" {
						return lease.State == "parked"
					}
				}
				return false
			})
			if err := b.Resume("fixture", false); err != nil {
				t.Fatal(err)
			}
			select {
			case lease := <-holder:
				if lease == nil {
					t.Fatal("waiter refused")
				}
				lease.Release()
				holder = nil
			case <-time.After(3 * time.Second):
				t.Fatal("waiter not granted")
			}
			waitFor(t, "authenticated wake", func() bool { return ready.Load() > 0 || unauthorized.Load() > 0 })
			if unauthorized.Load() > 0 || (kind != "comfyui" && wakes.Load() != 1) {
				t.Fatalf("wake auth: unauthorized=%d wakes=%d", unauthorized.Load(), wakes.Load())
			}
			waitFor(t, "native resume completed", func() bool { return strings.Contains(logs.String(), "resumed fixture") })
			_ = syscall.Kill(pid, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				t.Fatal("runner did not exit")
			}
		})
	}
}
