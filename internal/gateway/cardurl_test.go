// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// cardEngine is an engine that serves qwen plus a model of its own, and
// answers a chat request with its name and the path it was sent to.
func cardEngine(t *testing.T, name string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":"qwen"},{"id":%q}]}`, name+"-only")
		default:
			fmt.Fprintf(w, "%s %s", name, r.URL.Path)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// One model served on two cards: a /cards/{card}/v1 base URL reaches the
// engine on that card, a card the node lacks is a 404 naming the cards, and
// that card's model list names only what its engines serve.
func TestCardBaseURLPicksTheEngineOnThatCard(t *testing.T) {
	sock, _ := twoCards(t)
	a, b := cardEngine(t, "on-gpu0"), cardEngine(t, "on-gpu1")
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "qwen-a", Type: "llamacpp", URL: a.URL, GPU: "gpu0", Serves: []string{"qwen", "on-gpu0-only"}},
			{Name: "qwen-b", Type: "llamacpp", URL: b.URL, GPU: "0000:09:00.0", Serves: []string{"qwen", "on-gpu1-only"}},
		}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	chat := func(path, model string) (int, string) {
		resp := call(t, "POST", gw.URL+path, fmt.Sprintf(`{"model":%q}`, model))
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for _, c := range []struct{ path, model, want string }{
		{"/cards/gpu1/v1/chat/completions", "qwen", "on-gpu1 /v1/chat/completions"},
		{"/cards/gpu0/v1/chat/completions", "qwen", "on-gpu0 /v1/chat/completions"},
		{"/cards/0000:09:00.0/v1/embeddings", "qwen", "on-gpu1 /v1/embeddings"},
		{"/v1/chat/completions", "qwen", "on-gpu0 /v1/chat/completions"},
	} {
		if code, body := chat(c.path, c.model); code != http.StatusOK || body != c.want {
			t.Errorf("POST %s: %d %q, want 200 %q", c.path, code, body, c.want)
		}
	}
	if code, body := chat("/cards/gpu0/v1/chat/completions", "on-gpu1-only"); code != http.StatusNotFound || !strings.Contains(body, "gpu0") {
		t.Errorf("a model only gpu1 serves, asked on gpu0: %d %q, want a 404 naming gpu0", code, body)
	}
	if code, body := chat("/cards/gpu7/v1/chat/completions", "qwen"); code != http.StatusNotFound || !strings.Contains(body, "cards: gpu0, gpu1") {
		t.Errorf("an unknown card: %d %q, want a 404 listing the cards", code, body)
	}

	resp := call(t, "GET", gw.URL+"/cards/gpu1/v1/models", "")
	defer resp.Body.Close()
	var list struct{ Data []struct{ ID string } }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"on-gpu1-only", "qwen"}) {
		t.Errorf("gpu1's models = %v, want [on-gpu1-only qwen]", ids)
	}
}
