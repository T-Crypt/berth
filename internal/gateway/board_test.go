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
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
)

func readBoard(t *testing.T, url string) boardNode {
	t.Helper()
	resp := call(t, "GET", url+"/v1/berth/board", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("board = %d", resp.StatusCode)
	}
	var n boardNode
	if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The board is one node: its card with who holds it, and every engine with
// its state, what it can do, and its models.
func TestBoard(t *testing.T) {
	sock, _ := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",strata-model,300ms")
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "strata", Type: "llamacpp", URL: "http://" + addr, Start: []string{os.Args[0]}, Serves: []string{"strata-model"}},
			{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:1", Start: []string{"false"}, Serves: []string{"sdxl"}, GUI: "https://box/"},
		}}
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	n := readBoard(t, srv.URL)
	if !n.Reachable || len(n.Cards) != 1 || n.Cards[0].State != "free" || len(n.Cards[0].Holders) != 0 {
		t.Fatalf("idle node = %+v", n)
	}

	resp := call(t, "POST", srv.URL+"/v1/berth/switch", `{"engine":"strata"}`)
	resp.Body.Close()
	until(t, "strata up on the board", func() bool {
		for _, e := range readBoard(t, srv.URL).Engines {
			if e.Name == "strata" && e.State == "up" {
				return true
			}
		}
		return false
	})

	n = readBoard(t, srv.URL)
	if c := n.Cards[0]; c.State != "held" || !reflect.DeepEqual(c.Holders, []guiHold{{Name: "strata", State: "active"}}) {
		t.Fatalf("held card = %+v", c)
	}
	want := []struct {
		name, state  string
		launch, stop bool
		models       []boardModel
	}{
		{"strata", "up", true, true, []boardModel{{ID: "strata-model"}}},
		{"comfyui", "offline", true, false, []boardModel{{ID: "sdxl"}}},
	}
	if len(n.Engines) != len(want) {
		t.Fatalf("engines = %+v", n.Engines)
	}
	for i, w := range want {
		e := n.Engines[i]
		if e.Name != w.name || e.State != w.state || e.Launch != w.launch || e.Stoppable != w.stop || !reflect.DeepEqual(e.Models, w.models) {
			t.Errorf("engine %d = %+v, want %+v", i, e, w)
		}
	}
}

// boardOf reads the card's state and each model's loaded flag from berthd's
// status alone; berthd not answering leaves the cards unknown.
func TestBoardOf(t *testing.T) {
	gpu := []proto.GPU{{Index: 0, Name: "RTX 4090", Scheduled: true, Total: 24, Used: 23}}
	rows := []guiRow{{Name: "llama-swap", State: "up"}, {Name: "game", State: "offline"}}
	listed := map[string][]string{"llama-swap": {"qwen", "gemma"}}
	for _, c := range []struct {
		name      string
		node      *proto.Status
		card      string
		stoppable []bool
		loaded    []boardModel
		inFlight  map[loadKey]bool
	}{
		{"unreachable", nil, "", []bool{false, false}, []boardModel{{ID: "qwen"}, {ID: "gemma"}}, nil},
		{"held, one model ready", &proto.Status{GPUs: gpu,
			Leases:  []proto.Lease{{Engine: "llama-swap", Group: "qwen", State: "active"}},
			Engines: []proto.Engine{{Name: "llama-swap", Models: []proto.Model{{Name: "qwen", State: "ready"}, {Name: "gemma", State: "starting"}}}}},
			"held", []bool{true, false}, []boardModel{{ID: "qwen", Loaded: true}, {ID: "gemma", Loading: true}}, nil},
		{"a switchboard load under way", &proto.Status{GPUs: gpu},
			"free", []bool{false, false}, []boardModel{{ID: "qwen"}, {ID: "gemma", Loading: true}}, map[loadKey]bool{{"llama-swap", "gemma"}: true}},
		{"a load under way for a model already ready", &proto.Status{GPUs: gpu,
			Engines: []proto.Engine{{Name: "llama-swap", Models: []proto.Model{{Name: "qwen", State: "ready"}}}}},
			"free", []bool{false, false}, []boardModel{{ID: "qwen", Loaded: true}, {ID: "gemma"}}, map[loadKey]bool{{"llama-swap", "qwen"}: true}},
		{"pinned game", &proto.Status{GPUs: gpu, Leases: []proto.Lease{{Engine: "game", State: "active", Pinned: true}}},
			"held", []bool{false, false}, []boardModel{{ID: "qwen"}, {ID: "gemma"}}, nil},
		{"draining", &proto.Status{GPUs: gpu, Leases: []proto.Lease{{Engine: "llama-swap", State: "draining"}}},
			"draining", []bool{false, false}, []boardModel{{ID: "qwen"}, {ID: "gemma"}}, nil},
		{"in a VM", &proto.Status{GPUs: gpu, Passthrough: &proto.Passthrough{State: proto.CardPassthrough}},
			"passthrough", []bool{false, false}, []boardModel{{ID: "qwen"}, {ID: "gemma"}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := boardOf("box", c.node, rows, listed, c.inFlight)
			if n.Reachable != (c.node != nil) {
				t.Fatalf("reachable = %v", n.Reachable)
			}
			if c.card == "" && len(n.Cards) != 0 || c.card != "" && (len(n.Cards) != 1 || n.Cards[0].State != c.card) {
				t.Fatalf("cards = %+v, want state %q", n.Cards, c.card)
			}
			for i, s := range c.stoppable {
				if n.Engines[i].Stoppable != s {
					t.Errorf("%s stoppable = %v, want %v", n.Engines[i].Name, n.Engines[i].Stoppable, s)
				}
			}
			if !reflect.DeepEqual(n.Engines[0].Models, c.loaded) {
				t.Errorf("models = %+v, want %+v", n.Engines[0].Models, c.loaded)
			}
		})
	}
}
