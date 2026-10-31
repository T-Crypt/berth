// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// On a node with two cards the board names each engine's card, so a phone
// picks the card by the engine it launches, and gives each card its base
// URL and its cordon. /gui/state carries the same cards per engine.
func TestBoardNamesEachEnginesCard(t *testing.T) {
	sock, n := twoCards(t)
	if _, err := n.Cordon("gpu1", true); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "qwen-a", Type: "llamacpp", URL: "http://127.0.0.1:1", GPU: "gpu0", Serves: []string{"qwen"}},
			{Name: "qwen-b", Type: "llamacpp", URL: "http://127.0.0.1:1", GPU: "0000:09:00.0", Serves: []string{"qwen"}},
			{Name: "tts", Type: "llamacpp", URL: "http://127.0.0.1:1", Serves: []string{"voice"}},
		}}
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	var b boardNode
	until(t, "both cards on the board", func() bool { b = readBoard(t, srv.URL); return len(b.Cards) == 2 })
	type card struct {
		Card, Base string
		Cordoned   bool
	}
	var cards []card
	for _, c := range b.Cards {
		cards = append(cards, card{c.Card, c.Base, c.Cordoned})
	}
	if want := []card{{"gpu0", "/cards/gpu0/v1", false}, {"gpu1", "/cards/gpu1/v1", true}}; !reflect.DeepEqual(cards, want) {
		t.Errorf("cards = %+v, want %+v", cards, want)
	}
	want := map[string][]string{"qwen-a": {"gpu0"}, "qwen-b": {"gpu1"}, "tts": {"gpu0"}}
	got := map[string][]string{}
	for _, e := range b.Engines {
		got[e.Name] = e.Cards
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("board engine cards = %v, want %v", got, want)
	}

	gui := httptest.NewServer(g.GUIHandler())
	defer gui.Close()
	resp := call(t, "GET", gui.URL+"/gui/state", "")
	defer resp.Body.Close()
	var rows []guiRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	got = map[string][]string{}
	for _, r := range rows {
		got[r.Name] = r.Cards
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/gui/state cards = %v, want %v", got, want)
	}
}
