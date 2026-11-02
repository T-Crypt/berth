// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/pair"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/server"
)

const testKey = "berth-test-key-0123456789"

// fakeGateway answers the board and the actions, and records what it was sent.
type fakeGateway struct {
	mu   sync.Mutex
	seen []string // "METHOD PATH AUTH BODY"
	stop int      // status for /v1/berth/stop
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(b))
	stop := f.stop
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/berth/board":
		_, _ = io.WriteString(w, `{"name":"aphotic","reachable":true}`)
	case "/v1/berth/switch", "/v1/berth/load":
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"state":"waiting"}`)
	case "/v1/berth/stop":
		w.WriteHeader(stop)
		_, _ = io.WriteString(w, `{"error":{"message":"game is pinned; Berth never stops it"}}`)
	}
}

func (f *fakeGateway) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return ""
	}
	return f.seen[len(f.seen)-1]
}

// switchHarbor is a harbor in front of a real berthd that owns a pairing
// file and a fake gateway, with one paired device's token.
func switchHarbor(t *testing.T, key string) (base string, gw *fakeGateway, token string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, Log: log}, idleCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &pair.Store{Path: filepath.Join(dir, "harbor-devices.json")}
	go server.Serve(ctx, ln, node.One(b), server.Extras{Pair: s}, log)
	code, err := s.Code()
	if err != nil {
		t.Fatal(err)
	}
	if token, err = s.Redeem(code, "iPhone"); err != nil {
		t.Fatal(err)
	}
	gw = &fakeGateway{stop: http.StatusAccepted}
	gsrv := httptest.NewServer(gw)
	t.Cleanup(gsrv.Close)
	srv := httptest.NewServer((&Harbor{Socket: sock, Log: log, Gateway: gsrv.URL, GatewayKey: key}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, gw, token
}

func call(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestBoardReadIsOpen(t *testing.T) {
	base, gw, _ := switchHarbor(t, testKey)
	code, body := call(t, "GET", base+"/v1/board", "", nil)
	if code != http.StatusOK || body != `{"name":"aphotic","reachable":true}` {
		t.Fatalf("unpaired board read = %d %s", code, body)
	}
	if got := gw.last(); got != "GET /v1/berth/board Bearer "+testKey+" " {
		t.Fatalf("gateway saw %q", got)
	}
}

func TestBoardActionGates(t *testing.T) {
	base, gw, token := switchHarbor(t, testKey)
	cookie := cookieName + "=" + token
	ok := map[string]string{"Cookie": cookie, "X-Berth": "1", "Sec-Fetch-Site": "same-origin"}
	with := func(k, v string) map[string]string {
		h := map[string]string{}
		for a, b := range ok {
			h[a] = b
		}
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
		return h
	}
	cases := []struct {
		name, path, body string
		hdr              map[string]string
		want             int
		wantCode         string
	}{
		{"cross-site", "/v1/board/switch", `{"engine":"x"}`, with("Sec-Fetch-Site", "cross-site"), 403, "cross_site"},
		{"no X-Berth", "/v1/board/switch", `{"engine":"x"}`, with("X-Berth", ""), 403, "no_x_berth"},
		{"not paired", "/v1/board/switch", `{"engine":"x"}`, with("Cookie", ""), 401, "not_paired"},
		{"forged token", "/v1/board/stop", `{"engine":"x"}`, with("Cookie", cookieName+"=forged"), 401, "not_paired"},
		{"bad body", "/v1/board/switch", `{"engine":""}`, ok, 400, "bad_body"},
		{"load without a model", "/v1/board/load", `{"engine":"llama-swap"}`, ok, 400, "bad_body"},
		{"load not paired", "/v1/board/load", `{"engine":"llama-swap","model":"qwen"}`, with("Cookie", ""), 401, "not_paired"},
		{"pair code unpaired", "/v1/pair/code", ``, with("Cookie", ""), 401, "not_paired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := gw.last()
			code, body := call(t, "POST", base+tc.path, tc.body, tc.hdr)
			var e struct{ Code string }
			_ = json.Unmarshal([]byte(body), &e)
			if code != tc.want || e.Code != tc.wantCode {
				t.Fatalf("= %d %s, want %d %s", code, body, tc.want, tc.wantCode)
			}
			if gw.last() != before {
				t.Fatalf("a refused action reached the gateway: %q", gw.last())
			}
		})
	}
}

func TestBoardActionForwards(t *testing.T) {
	base, gw, token := switchHarbor(t, testKey)
	hdr := map[string]string{"Cookie": cookieName + "=" + token, "X-Berth": "1", "Sec-Fetch-Site": "same-origin"}
	code, body := call(t, "POST", base+"/v1/board/switch", `{"engine":"strata","extra":1}`, hdr)
	if code != http.StatusAccepted || body != `{"state":"waiting"}` {
		t.Fatalf("switch = %d %s", code, body)
	}
	if got := gw.last(); got != `POST /v1/berth/switch Bearer `+testKey+` {"engine":"strata"}` {
		t.Fatalf("gateway saw %q", got)
	}
	// A load carries its model; a switch never passes one on.
	code, _ = call(t, "POST", base+"/v1/board/load", `{"engine":"llama-swap","model":"org/qwen"}`, hdr)
	if got := gw.last(); code != http.StatusAccepted || got != `POST /v1/berth/load Bearer `+testKey+` {"engine":"llama-swap","model":"org/qwen"}` {
		t.Fatalf("load = %d, gateway saw %q", code, got)
	}
	_, _ = call(t, "POST", base+"/v1/board/switch", `{"engine":"strata","model":"x"}`, hdr)
	if got := gw.last(); got != `POST /v1/berth/switch Bearer `+testKey+` {"engine":"strata"}` {
		t.Fatalf("switch passed a model on: %q", got)
	}
	// A bearer token works for scripts as the cookie does for a browser.
	gw.mu.Lock()
	gw.stop = http.StatusConflict
	gw.mu.Unlock()
	code, body = call(t, "POST", base+"/v1/board/stop", `{"engine":"game"}`, map[string]string{"Authorization": "Bearer " + token, "X-Berth": "1"})
	if code != http.StatusConflict || body != `{"error":{"message":"game is pinned; Berth never stops it"}}` {
		t.Fatalf("stop of a pinned holder = %d %s", code, body)
	}
}

func TestPairCodeForPairedDevice(t *testing.T) {
	base, _, token := switchHarbor(t, testKey)
	code, body := call(t, "POST", base+"/v1/pair/code", "", map[string]string{"Cookie": cookieName + "=" + token, "X-Berth": "1", "X-Forwarded-Proto": "https"})
	var got struct{ Link, QR string }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("= %d %s", code, body)
	}
	host := strings.TrimPrefix(base, "http://")
	if !strings.HasPrefix(got.Link, "https://"+host+"/pair?code=") || len(got.Link) <= len("https://"+host+"/pair?code=") {
		t.Fatalf("link = %q", got.Link)
	}
	if !strings.HasPrefix(got.QR, "data:image/svg+xml;base64,") {
		t.Fatalf("qr = %.40q", got.QR)
	}
}

func TestBoardWithoutKey(t *testing.T) {
	base, gw, _ := switchHarbor(t, "")
	code, body := call(t, "GET", base+"/v1/board", "", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"no_gateway_key"`) {
		t.Fatalf("= %d %s", code, body)
	}
	if gw.last() != "" {
		t.Fatalf("the gateway was called without a key: %q", gw.last())
	}
}
