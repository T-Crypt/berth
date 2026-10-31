// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An engine that takes the request and sends no headers within ReplyTimeout
// is reported silent and nothing reaches the client, so the caller can send
// the buffered request again; an engine that replies in time is proxied.
func TestProxyToReportsASilentEngine(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select { // the first request is swallowed
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, "pong")
	}))
	defer engine.Close()
	defer close(release)
	u, _ := url.Parse(engine.URL)
	g := &Gateway{ReplyTimeout: 150 * time.Millisecond}
	body := []byte(`{"model":"m"}`)

	var silent bool
	rec := httptest.NewRecorder()
	g.proxyTo(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))), u, body, &silent)
	if !silent {
		t.Fatalf("a silent engine was not reported (code %d, body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("the client was written to: %q", rec.Body.String())
	}

	silent = false
	rec = httptest.NewRecorder()
	g.proxyTo(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))), u, body, &silent)
	if silent || rec.Body.String() != "pong" {
		t.Fatalf("replay: silent=%v body=%q, want the engine's reply", silent, rec.Body.String())
	}
}

// With no ReplyTimeout, proxyTo is the plain reverse proxy it was.
func TestProxyToWithoutATimeoutIsThePlainProxy(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	}))
	defer engine.Close()
	u, _ := url.Parse(engine.URL)
	rec := httptest.NewRecorder()
	(&Gateway{}).proxyTo(rec, httptest.NewRequest("POST", "/", nil), u, []byte("echo"), nil)
	if rec.Body.String() != "echo" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// The second silence is a 504 with a reason code, as JSON; a streaming
// request gets the error as one SSE event.
func TestReplayFailedAnswers504(t *testing.T) {
	g := &Gateway{}
	rec := httptest.NewRecorder()
	g.replayFailed(rec, false, "llm", "m", io.ErrUnexpectedEOF)
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), `"code":"no_reply"`) {
		t.Fatalf("code %d body %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	g.replayFailed(rec, true, "llm", "m", io.ErrUnexpectedEOF)
	if !strings.Contains(rec.Body.String(), "no_reply") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("sse body %q", rec.Body.String())
	}
}
