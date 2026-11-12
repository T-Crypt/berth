// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/runner"
)

// The node's two calls: the yield answers with its length, and its end
// answers once the engine holds the card again.
func TestYieldRoute(t *testing.T) {
	srv, b := switchGateway(t)
	resp := call(t, "POST", srv.URL+"/v1/berth/batch", `{"engine":"strata"}`)
	resp.Body.Close()
	until(t, "strata to hold the card", func() bool {
		st := b.Status()
		return len(st.Leases) == 1 && st.Leases[0].Engine == "strata" && st.Leases[0].State == "active"
	})

	var got struct {
		Yield   bool    `json:"yield"`
		Seconds float64 `json:"seconds"`
	}
	resp = call(t, "POST", srv.URL+"/v1/berth/yield", `{"engine":"strata"}`)
	err := json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || !got.Yield || got.Seconds != defaultYield.Seconds() {
		t.Fatalf("yield = %d %+v %v, want 200 and the default length", resp.StatusCode, got, err)
	}

	resp = call(t, "POST", srv.URL+"/v1/berth/yield", `{"engine":"strata","end":true}`)
	err = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || got.Yield {
		t.Fatalf("end = %d %+v %v", resp.StatusCode, got, err)
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].State != "active" {
		t.Fatalf("after the end, leases = %+v", st.Leases)
	}
}

func TestYieldRefusals(t *testing.T) {
	srv, _ := switchGateway(t)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"engine":"nope"}`, http.StatusNotFound},
		{`not json`, http.StatusBadRequest},
		{`{"engine":"strata","seconds":-1}`, http.StatusBadRequest},
		{`{"engine":"strata","seconds":7200}`, http.StatusBadRequest},
		{`{"engine":"strata","seconds":60}`, http.StatusConflict}, // strata holds no lease
	} {
		resp := call(t, "POST", srv.URL+"/v1/berth/yield", c.body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s = %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
}

// busyComfyStub is a ComfyUI whose one job waits on an LLM: its queue never
// empties, /free lands, and the run probes /system_stats after the park.
func busyComfyStub(t *testing.T, freed *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/queue":
			fmt.Fprint(w, `{"queue_running":[[1,"job"]],"queue_pending":[]}`)
		case "/free":
			freed.Store(true)
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{"devices":[{"vram_free":1,"vram_total":2}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// #299 through the gateway: the end of a yield answers only once ComfyUI
// holds the card again. A parked ComfyUI answers its queue the whole time, so
// a probe says nothing; the answer must wait for the wake, or the job's next
// node loads its models onto memory the LLM still holds.
func TestYieldEndWaitsForTheCardBack(t *testing.T) {
	card := &wakeCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	var freed atomic.Bool
	comfy := busyComfyStub(t, &freed)
	adapter, ok := park.For("comfyui", "", "")
	if !ok {
		t.Fatal("the comfyui adapter is missing")
	}
	engineLog := &syncBuf{}
	go runner.Run(runner.Options{Socket: sock, Engine: "comfyui", Priority: "interactive", Needs: []uint64{8 << 30},
		DrainComfyUI: comfy.URL, Park: &adapter, ParkBase: comfy.URL, ParkProbe: "/system_stats",
		Retry: 10 * time.Millisecond, Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: engineLog})
	var pid int
	until(t, "the run to attach", func() bool {
		if st := b.Status(); len(st.Leases) > 0 && st.Leases[0].PID != 0 {
			pid = st.Leases[0].PID
		}
		return pid != 0
	})
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 8 << 30})
	until(t, "the broker to measure ComfyUI", func() bool {
		st := b.Status()
		return len(st.Leases) > 0 && st.Leases[0].Used > 0
	})

	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slogDiscard(),
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: comfy.URL,
			DrainComfyUI: comfy.URL, KeepComfyUI: true}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	llm := make(chan *broker.Lease, 1)
	go func() {
		l, _ := b.Acquire(context.Background(), broker.Request{Engine: "gemma", Priority: broker.Foreground}, nil)
		llm <- l
	}()
	until(t, "the drain to wait on the job", func() bool {
		return strings.Contains(engineLog.String(), "waiting for ComfyUI to finish")
	})
	resp := call(t, "POST", gw.URL+"/v1/berth/yield", `{"engine":"comfyui","seconds":60}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("yield = %d", resp.StatusCode)
	}
	until(t, "the park unload", freed.Load)
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	hold := <-llm
	if hold == nil {
		t.Fatalf("gemma was never granted beside the parked job; engine log:\n%s", engineLog.String())
	}

	ended := make(chan time.Time, 1)
	go func() {
		resp := call(t, "POST", gw.URL+"/v1/berth/yield", `{"engine":"comfyui","end":true}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("end = %d", resp.StatusCode)
		}
		ended <- time.Now()
	}()
	time.Sleep(400 * time.Millisecond)
	select {
	case <-ended:
		t.Fatal("the end answered while gemma still held the card")
	default:
	}
	released := time.Now()
	hold.Release()
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	select {
	case at := <-ended:
		if at.Before(released) {
			t.Fatal("the end answered before gemma let go")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the end never answered; engine log:\n%s", engineLog.String())
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "comfyui" || st.Leases[0].State != "active" {
		t.Fatalf("after the end, leases = %+v", st.Leases)
	}
}
