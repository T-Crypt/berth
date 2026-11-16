// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// shortKeepAlive speeds the comment cadence up for one test and puts it
// back when it ends.
func shortKeepAlive(t *testing.T, every time.Duration) {
	t.Helper()
	old := keepAliveEvery
	keepAliveEvery = every
	t.Cleanup(func() { keepAliveEvery = old })
}

// While a streaming request waits for its cold engine, the SSE headers and
// the loading comments hold the connection, then the engine's own stream
// follows through the already-sent headers.
func TestKeepAliveWhileLoading(t *testing.T) {
	shortKeepAlive(t, 100*time.Millisecond)
	sock, _ := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",slow,400ms")
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "loading-engine", Type: "llamacpp", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Serves: []string{"slow"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "POST", gw.URL+"/v1/chat/completions",
		`{"model":"slow","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control %q, want no-cache", cc)
	}
	var comments, data []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		switch {
		case strings.HasPrefix(sc.Text(), ": berth: loading"):
			comments = append(comments, sc.Text())
		case strings.HasPrefix(sc.Text(), "data: "):
			data = append(data, sc.Text())
		}
	}
	commentShape := regexp.MustCompile(`^: berth: loading loading-engine, \d+ s$`)
	for _, c := range comments {
		if !commentShape.MatchString(c) {
			t.Errorf("comment %q does not match %s", c, commentShape)
		}
	}
	if len(comments) == 0 {
		t.Error("no loading comment arrived while the engine loaded")
	}
	if len(data) != 4 || data[3] != "data: [DONE]" || !strings.Contains(data[2], "never collide") {
		t.Fatalf("engine data = %q", data)
	}
}

// A start that fails after the SSE headers went out answers with one
// OpenAI-shaped error event naming the 503 path's code, then DONE.
func TestKeepAliveFailureEmitsErrorEvent(t *testing.T) {
	shortKeepAlive(t, 100*time.Millisecond)
	sock, _ := daemon(t)
	crash := `echo "prefill gemm: cublasCreate: cuBLAS status 1" >&2; exit 1`
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "crashy", Type: "llamacpp", URL: "http://" + freeAddr(t),
			Start: []string{"sh", "-c", crash}, Serves: []string{"m"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"m","stream":true}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"code":"engine_oom"`) || !strings.Contains(s, "cuBLAS status 1") {
		t.Fatalf("error event = %s", s)
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "data: [DONE]") {
		t.Fatalf("no DONE after the error event: %s", s)
	}
	if strings.Count(s, "data: [DONE]") != 1 {
		t.Fatalf("more than one DONE: %s", s)
	}
	// An SSE client joins the data lines of one event with "\n" (the
	// blank line ends the event), so the error object and DONE must be
	// two events, or the client reads one event that is not JSON.
	for ev := range strings.SplitSeq(strings.TrimSpace(s), "\n\n") {
		var data []string
		for line := range strings.SplitSeq(ev, "\n") {
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				data = append(data, d)
			}
		}
		if len(data) == 0 {
			continue
		}
		if d := strings.Join(data, "\n"); d != "[DONE]" && !json.Valid([]byte(d)) {
			t.Errorf("event data is neither JSON nor [DONE]: %q", d)
		}
	}
}

// /v1/berth/wait blocks until the engine answers, 404s a model nobody
// serves, and answers a failed start with the 503 the request path uses.
func TestWaitForModel(t *testing.T) {
	sock, b := daemon(t)
	// No engine anywhere serves "nope" from a bare gateway.
	gw := httptest.NewServer((&Gateway{Socket: sock, Key: testKey}).Handler())
	resp := call(t, "GET", gw.URL+"/v1/berth/wait?model=nope", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model: %d %s", resp.StatusCode, body)
	}
	resp = call(t, "GET", gw.URL+"/v1/berth/wait", "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no model named: %d %s", resp.StatusCode, body)
	}
	gw.Close()

	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",late,300ms")
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "wait-engine", Type: "llamacpp", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Serves: []string{"late"}}}}
	gw = httptest.NewServer(g.Handler())
	defer gw.Close()
	start := time.Now()
	resp = call(t, "GET", gw.URL+"/v1/berth/wait?model=late", "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ready":true`) {
		t.Fatalf("wait = %d %s", resp.StatusCode, body)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatalf("wait returned after %s, before the engine answered", time.Since(start))
	}
	if n := len(b.Status().Leases); n != 1 {
		t.Fatalf("leases after the wait = %d, want the engine's one lease", n)
	}
}
