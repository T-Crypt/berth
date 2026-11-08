// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

// The gateway's resume: a parked engine's lease is queued and woken, and
// nothing is proxied before the wake answers, even though the engine's
// model list answers the whole time.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
)

// wakeCard fakes the card the broker reads.
type wakeCard struct {
	mu    sync.Mutex
	total uint64
	procs []gpu.Proc
}

func (c *wakeCard) set(ps ...gpu.Proc) {
	c.mu.Lock()
	c.procs = ps
	c.mu.Unlock()
}

func (c *wakeCard) Read(context.Context) (gpu.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var used uint64
	for _, p := range c.procs {
		used += p.Used
	}
	return gpu.Snapshot{Total: c.total, Used: used, Free: c.total - used,
		Procs: append([]gpu.Proc(nil), c.procs...), At: time.Now()}, nil
}

// parkDaemon is a berthd with RAM readings the test holds.
func parkDaemon(t *testing.T, card *wakeCard, avail uint64) (string, *broker.Broker) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slogDiscard()
	b := broker.New(broker.Config{Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second,
		JuggleWindow: -1, ParkFloor: 1 << 30, ParkResidualMax: 2 << 30,
		MemAvailable: func() uint64 { return avail }, Log: log}, card, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	go server.Serve(ctx, ln, node.One(b), server.Extras{}, log)
	return sock, b
}

// wakeStub is the parked-capable engine: the sleep-mode endpoints, the
// model list that answers while parked, and every hit in order, so the
// test proves the wake ran before the proxy.
type wakeStub struct {
	sleeps    atomic.Int64
	wakes     atomic.Int64
	wakeDelay time.Duration

	mu    sync.Mutex
	order []string
}

func (s *wakeStub) hit(what string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, what)
	return len(s.order)
}

func (s *wakeStub) nth(what string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.order {
		if w == what {
			return i + 1
		}
	}
	return 0
}

func (s *wakeStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sleep", func(w http.ResponseWriter, r *http.Request) {
		s.sleeps.Add(1)
		s.hit("sleep")
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/wake_up", func(w http.ResponseWriter, r *http.Request) {
		s.hit("wake")
		time.Sleep(s.wakeDelay)
		s.wakes.Add(1)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		s.hit("models")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"vllm-model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.hit("generate")
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range []string{"back", "on the", "card"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	return mux
}

// A parked engine is resumed, not started: the lease is queued at its own
// priority, the keep-alive carries the wait, the wake runs, and only
// after it does the request reach the engine.
func TestGatewayResumesAParkedEngine(t *testing.T) {
	shortKeepAlive(t, 100*time.Millisecond)
	card := &wakeCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	stub := &wakeStub{wakeDelay: 600 * time.Millisecond}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	// The engine's run: a parked-capable vLLM under a lease.
	adapter, ok := park.For("vllm", "", "")
	if !ok {
		t.Fatal("the vllm adapter is missing")
	}
	engineLog := &syncBuf{}
	go runner.Run(runner.Options{Socket: sock, Engine: "vllm", Priority: "interactive", Needs: []uint64{8 << 30},
		Park: &adapter, ParkBase: srv.URL, ParkProbe: "/v1/models",
		Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: engineLog})

	// Measure, then park it behind a foreground holder that stays.
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the run never attached")
		}
		if st := b.Status(); len(st.Leases) > 0 && st.Leases[0].PID != 0 {
			pid = st.Leases[0].PID
		}
		time.Sleep(10 * time.Millisecond)
	}
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 8 << 30})
	// Admission parks only what a card read measured: wait for the broker
	// to see the 8 GiB before anything drains the engine.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if st := b.Status(); len(st.Leases) > 0 && st.Leases[0].Used > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the broker never measured the engine")
		}
		time.Sleep(10 * time.Millisecond)
	}
	holderDone := make(chan *broker.Lease, 1)
	go func() {
		l, err := b.Acquire(context.Background(), broker.Request{Engine: "game", Priority: broker.Foreground}, nil)
		if err != nil {
			holderDone <- nil
			return
		}
		holderDone <- l
	}()
	// The park call runs after the drain; the residual leaves the card
	// with it, and the lease parks.
	deadline = time.Now().Add(5 * time.Second)
	for {
		st := b.Status()
		if len(st.Leases) > 0 && st.Leases[0].State == "parked" {
			break
		}
		if stub.sleeps.Load() > 0 {
			card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine never parked: %+v\nengine log:\n%s", st.Leases, engineLog.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The gateway: park = "hook" in the entry; the engine answers its
	// model list the whole time.
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slogDiscard(),
		Engines: []config.Engine{{Name: "vllm", Type: "vllm", URL: srv.URL,
			Park: "hook", Serves: []string{"vllm-model"}}}}
	gw := httptest.NewServer(g.Handler())
	defer gw.Close()

	resp := call(t, "POST", gw.URL+"/v1/chat/completions",
		`{"model":"vllm-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// The wake is held behind the foreground holder; let it go and the
	// wake runs, then the request is proxied.
	go func() {
		hold := <-holderDone
		if hold == nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
		hold.Release()
	}()
	var comments []string
	var data []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": berth: loading"):
			comments = append(comments, line)
		case strings.HasPrefix(line, "data: "):
			data = append(data, line)
		}
	}
	if len(comments) == 0 {
		t.Error("no keep-alive comment while the parked engine woke")
	}
	if len(data) != 4 || data[3] != "data: [DONE]" {
		t.Fatalf("engine data = %q", data)
	}
	if gen := stub.nth("generate"); gen == 0 || stub.nth("wake") == 0 || gen < stub.nth("wake") {
		t.Fatalf("the request reached the engine before the wake: wake %d, generate %d", stub.nth("wake"), gen)
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "vllm" {
		t.Fatalf("leases = %+v, want the resumed engine's one lease", st.Leases)
	}
	g.mu.Lock()
	n := len(g.running)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("the gateway started %d engine(s) for a parked one", n)
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// statusDaemon is a berthd that answers every status with st.
func statusDaemon(t *testing.T, st proto.Status) string {
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
				_ = conn.Send(proto.Msg{Op: proto.OpStatus, Status: &st})
			}
			c.Close()
		}
	}()
	return sock
}

// A parked model behind llama-swap wakes at its own front (berth run
// --front), so the gateway sends the request to llama-swap. Another model's
// parked lease in the llama-swap family does not make llama-swap parked.
// An engine whose own lease is parked is still not proxied live: the gateway
// is its front and resumes it first.
func TestParkedLeaseSkipsOnlyItsOwnEngine(t *testing.T) {
	const model = "NInfer-Qwen3.8-27B"
	addr := freeAddr(t)
	go fakeEngine(addr, model, 0)
	for _, tc := range []struct {
		engine, lease, group string
		want                 int
	}{
		{"llama-swap", model, "llama-swap", http.StatusOK},
		{"vllm", "vllm", "vllm", http.StatusNotFound},
	} {
		sock := statusDaemon(t, proto.Status{
			Engines: []proto.Engine{{Name: tc.engine, Type: "llama-swap", URL: "http://" + addr, Up: true, State: "up"}},
			Leases:  []proto.Lease{{ID: tc.lease + "-1", Engine: tc.lease, Group: tc.group, State: "parked"}},
		})
		g := &Gateway{Socket: sock, Ready: 5 * time.Second, Key: testKey}
		gw := httptest.NewServer(g.Handler())
		resp := call(t, "POST", gw.URL+"/v1/chat/completions", fmt.Sprintf(`{"model":%q}`, model))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		gw.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s with %s parked: %d %s, want %d", tc.engine, tc.lease, resp.StatusCode, body, tc.want)
		}
		if tc.want == http.StatusOK && !strings.Contains(string(body), "never collide") {
			t.Fatalf("%s with %s parked: the reply did not come from the engine: %s", tc.engine, tc.lease, body)
		}
	}
}
