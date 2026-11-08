// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

const peerKey = "berth-peer-key-9876543210"

// peerNode is a second node's gateway, with its own berthd and its own key,
// serving llama3 from an engine that is already up.
func peerNode(t *testing.T) *httptest.Server {
	t.Helper()
	addr := freeAddr(t)
	go fakeEngine(addr, "llama3", 0)
	sock, _ := daemon(t)
	g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: peerKey,
		Engines: []config.Engine{{Name: "ollama", Type: "ollama", URL: "http://" + addr, Serves: []string{"llama3"}}}}
	deadline := time.Now().Add(3 * time.Second)
	for !g.answers(context.Background(), "http://"+addr) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	gw := httptest.NewServer(g.Handler())
	t.Cleanup(gw.Close)
	return gw
}

// A model only a peer serves is listed under the peer's name, and a chat
// for it is answered by the peer, which is called with its own key: the
// client's key is this node's.
func TestAModelOnlyAPeerServesGoesToThePeer(t *testing.T) {
	b := peerNode(t)
	sock, local := daemon(t)
	g := &Gateway{Socket: sock, Key: testKey, Peers: []Peer{{Name: "galaxy", URL: b.URL, Key: peerKey}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "GET", gw.URL+"/v1/models", "")
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || len(list.Data) != 1 ||
		list.Data[0].ID != "llama3" || list.Data[0].OwnedBy != "galaxy" {
		t.Fatalf("models = %+v (%v)", list, err)
	}
	resp.Body.Close()

	resp = call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"llama3","stream":true}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "never collide") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := len(local.Status().Leases); n != 0 {
		t.Fatalf("this node took %d lease(s) for a model its peer serves", n)
	}
}

// Two nodes that name each other as peers answer a model neither serves
// with a 404 at once: a request a peer sent is never sent on.
func TestPeersDoNotPassARequestBackAndForth(t *testing.T) {
	sockA, _ := daemon(t)
	sockB, _ := daemon(t)
	a := &Gateway{Socket: sockA, Key: testKey}
	b := &Gateway{Socket: sockB, Key: peerKey}
	ga := httptest.NewServer(a.Handler())
	defer ga.Close()
	gb := httptest.NewServer(b.Handler())
	defer gb.Close()
	a.Peers = []Peer{{Name: "b", URL: gb.URL, Key: peerKey}}
	b.Peers = []Peer{{Name: "a", URL: ga.URL, Key: testKey}}

	start := time.Now()
	resp := call(t, "POST", ga.URL+"/v1/chat/completions", `{"model":"nope"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || time.Since(start) > 2*time.Second {
		t.Fatalf("%d after %s: %s", resp.StatusCode, time.Since(start), body)
	}
}

// A peer that does not answer leaves this node's own list and 404s as they
// were.
func TestAPeerThatIsDownListsNothing(t *testing.T) {
	sock, _ := daemon(t)
	g := &Gateway{Socket: sock, Key: testKey, Peers: []Peer{{Name: "galaxy", URL: "http://" + freeAddr(t), Key: peerKey}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()
	resp := call(t, "GET", gw.URL+"/v1/models", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "{\"object\":\"list\",\"data\":[]}\n" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp = call(t, "POST", gw.URL+"/v1/chat/completions", `{"model":"llama3"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}
