// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// fakeSwap is llama-swap as the load sees it: a model list, and an upstream
// /health that holds until the test lets the model come up.
type fakeSwap struct {
	mu      sync.Mutex
	paths   []string // every /upstream request's escaped path
	release chan struct{}
	status  int
	state   string // the loaded model's state in /running; "" = not running
}

func (f *fakeSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen"},{"id":"org/gemma 4"}]}`)
		return
	case "/running":
		f.mu.Lock()
		state := f.state
		f.mu.Unlock()
		if state == "" {
			_, _ = io.WriteString(w, `{"running":[]}`)
		} else {
			_, _ = io.WriteString(w, `{"running":[{"model":"org/gemma 4","state":"`+state+`"}]}`)
		}
		return
	}
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.EscapedPath())
	f.state = "starting"
	f.mu.Unlock()
	<-f.release
	f.mu.Lock()
	f.state = "ready"
	f.mu.Unlock()
	w.WriteHeader(f.status)
}

func (f *fakeSwap) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func loadGateway(t *testing.T, status int) (*httptest.Server, *fakeSwap) {
	t.Helper()
	f := &fakeSwap{release: make(chan struct{}), status: status}
	swap := httptest.NewServer(f)
	t.Cleanup(func() { close(f.release); swap.Close() })
	g := &Gateway{Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "llama-swap", Type: "llama-swap", URL: swap.URL},
			{Name: "strata", Type: "strata", URL: "http://127.0.0.1:1"},
		}}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, f
}

func modelLoading(t *testing.T, url, model string) bool {
	t.Helper()
	for _, e := range readBoard(t, url).Engines {
		for _, m := range e.Models {
			if e.Name == "llama-swap" && m.ID == model {
				return m.Loading
			}
		}
	}
	t.Fatalf("no model %q on the board", model)
	return false
}

func modelLoaded(t *testing.T, url, model string) bool {
	t.Helper()
	for _, e := range readBoard(t, url).Engines {
		for _, m := range e.Models {
			if e.Name == "llama-swap" && m.ID == model {
				return m.Loaded
			}
		}
	}
	return false
}

func TestLoadRefusals(t *testing.T) {
	srv, f := loadGateway(t, http.StatusOK)
	for _, c := range []struct {
		name, body string
		want       int
		msg        string
	}{
		{"no model", `{"engine":"llama-swap"}`, 400, `the body must be JSON like {"engine":"NAME","model":"MODEL"}`},
		{"unknown engine", `{"engine":"nope","model":"qwen"}`, 404, `no engine "nope" in berth.toml`},
		{"not llama-swap", `{"engine":"strata","model":"qwen"}`, 409, "strata is not llama-swap; only llama-swap loads one model on request"},
		{"unlisted model", `{"engine":"llama-swap","model":"llama"}`, 404, `llama-swap does not list a model "llama"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := call(t, "POST", srv.URL+"/v1/berth/load", c.body)
			var e struct {
				Error struct{ Message string } `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			resp.Body.Close()
			if resp.StatusCode != c.want || e.Error.Message != c.msg {
				t.Fatalf("= %d %q, want %d %q", resp.StatusCode, e.Error.Message, c.want, c.msg)
			}
		})
	}
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("a refused load reached llama-swap: %q", got)
	}
}

// A load answers before the model is up, shows on the board until
// llama-swap answers, and a second ask joins the first.
func TestLoadThroughUpstream(t *testing.T) {
	srv, f := loadGateway(t, http.StatusOK)
	if e := readBoard(t, srv.URL).Engines; e[0].Type != "llama-swap" || e[1].Type != "strata" {
		t.Fatalf("board types = %q, %q", e[0].Type, e[1].Type)
	}
	for range 2 {
		resp := call(t, "POST", srv.URL+"/v1/berth/load", `{"engine":"llama-swap","model":"org/gemma 4"}`)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted || strings.TrimSpace(string(b)) != `{"engine":"llama-swap","model":"org/gemma 4"}` {
			t.Fatalf("load = %d %s", resp.StatusCode, b)
		}
	}
	until(t, "llama-swap to be asked", func() bool { return len(f.seen()) == 1 })
	if got := f.seen(); got[0] != "/upstream/org/gemma%204/health" {
		t.Fatalf("llama-swap was asked for %q", got[0])
	}
	if !modelLoading(t, srv.URL, "org/gemma 4") || modelLoading(t, srv.URL, "qwen") {
		t.Fatal("the board does not show the load under way")
	}
	f.release <- struct{}{}
	// Once llama-swap answers, the board reads the model loaded at once, not
	// cold until berthd's reader catches up.
	until(t, "the load to end", func() bool { return !modelLoading(t, srv.URL, "org/gemma 4") })
	if !modelLoaded(t, srv.URL, "org/gemma 4") {
		t.Fatal("the model read cold between llama-swap's answer and berthd's next read")
	}
	if got := f.seen(); len(got) != 1 {
		t.Fatalf("two asks loaded twice: %q", got)
	}
}
