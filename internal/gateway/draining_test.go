// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// liveStatusDaemon is a berthd whose status the test changes while the
// gateway reads it.
func liveStatusDaemon(t *testing.T, st *atomic.Pointer[proto.Status]) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conn := proto.NewConn(c)
			if m, err := conn.Recv(); err == nil && m.Op == proto.OpStatus {
				_ = conn.Send(proto.Msg{Op: proto.OpStatus, Status: st.Load()})
			}
			c.Close()
		}
	}()
	return sock
}

// An engine whose lease is draining still answers, and on aphotic Strata took
// a request in the seconds it spent stopping and ended it with 0 tokens. The
// gateway holds a request for a draining engine and sends it on only once
// the lease is active again (or gone, and the engine is started anew).
func TestGatewayHoldsARequestWhileItsEngineDrains(t *testing.T) {
	const model = "qwen3.8-flash"
	var served atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q}]}`, model)
		case "/v1/chat/completions":
			served.Add(1)
			fmt.Fprint(w, `{"choices":[{"message":{"content":"from the engine"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()

	var st atomic.Pointer[proto.Status]
	up := []proto.Engine{{Name: "strata", Type: "strata", URL: engine.URL, Up: true, State: "up"}}
	st.Store(&proto.Status{Engines: up, Leases: []proto.Lease{{ID: "strata-58", Engine: "strata", Group: "strata", State: "draining"}}})
	g := &Gateway{Socket: liveStatusDaemon(t, &st), Ready: 5 * time.Second, Key: testKey,
		Engines: []config.Engine{{Name: "strata", Type: "strata", URL: engine.URL, Serves: []string{model}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	type reply struct {
		code int
		body string
	}
	done := make(chan reply, 1)
	go func() {
		resp := call(t, "POST", gw.URL+"/v1/chat/completions", fmt.Sprintf(`{"model":%q}`, model))
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- reply{resp.StatusCode, string(b)}
	}()

	select {
	case r := <-done:
		t.Fatalf("answered while the engine drained: %d %s", r.code, r.body)
	case <-time.After(700 * time.Millisecond):
	}
	if n := served.Load(); n != 0 {
		t.Fatalf("the draining engine got %d request(s)", n)
	}

	st.Store(&proto.Status{Engines: up, Leases: []proto.Lease{{ID: "strata-58", Engine: "strata", Group: "strata", State: "active"}}})
	select {
	case r := <-done:
		if r.code != http.StatusOK || !strings.Contains(r.body, "from the engine") {
			t.Fatalf("after the drain: %d %s", r.code, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held request never went on after the lease was active again")
	}
	if n := served.Load(); n != 1 {
		t.Fatalf("the engine served %d request(s), want 1", n)
	}
}
