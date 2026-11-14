// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// busyComfy is a ComfyUI whose one job never ends: the job is waiting on an
// LLM node that waits on the card (#299).
func busyComfy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/queue":
			fmt.Fprint(w, `{"queue_running":[["1","job"]],"queue_pending":[]}`)
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// #299: a ComfyUI job that waits on an LLM never reaches its job boundary,
// so a drain for that LLM waited on the job and the job waited on the drain.
// A yield from inside the job is a boundary for as long as it lasts.
func TestYieldIsAJobBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before time.Duration // a yield set before the drain starts
		during time.Duration // a yield set while the drain waits
		passes bool
	}{
		{name: "no yield", passes: false},
		{name: "yield before the drain", before: time.Minute, passes: true},
		{name: "yield while the drain waits", during: time.Minute, passes: true},
		{name: "yield that expired", before: time.Nanosecond, passes: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := busyComfy(t)
			cmd, waited := startSleep(t)
			line := newLine(nil)
			if tc.before > 0 {
				line.setYield(tc.before)
				time.Sleep(time.Millisecond)
			}
			done := make(chan struct{})
			var logs strings.Builder
			go drain(Options{Engine: "comfyui", DrainComfyUI: url, Retry: 5 * time.Millisecond,
				Grace: 50 * time.Millisecond}, cmd, line, done, "test", sayTo(&logs))
			if tc.during > 0 {
				time.Sleep(30 * time.Millisecond)
				line.setYield(tc.during)
			}
			select {
			case <-waited:
				if !tc.passes {
					t.Fatalf("the drain stopped ComfyUI mid-job with no yield")
				}
			case <-time.After(time.Second):
				if tc.passes {
					t.Fatalf("the drain still waits on the job after a yield")
				}
				close(done)
			}
		})
	}
}

// sendYield is the call a ComfyUI node makes through berthd: a yield of d,
// or its end when d is 0.
func sendYield(t *testing.T, sock string, d time.Duration) proto.Msg {
	t.Helper()
	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpYield, Engine: "comfyui", Yield: d.Seconds()}); err != nil {
		t.Fatal(err)
	}
	m, err := conn.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// #299, the whole cycle with ComfyUI's park: the yield lets the drain park
// ComfyUI mid-job, the LLM runs in the room the card shows free, and the end
// of the yield gives the job its card back.
func TestYieldParksAndItsEndResumes(t *testing.T) {
	card := &parkCard{total: 24 << 30}
	sock, b := parkDaemon(t, card, 16<<30)
	stub := &comfyStub{}
	stub.busy.Store(true)
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	adapter, ok := park.For("comfyui", "", "")
	if !ok {
		t.Fatal("the comfyui adapter is missing")
	}
	log := &syncBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.String())
		}
	})
	exited := make(chan int, 1)
	go func() {
		exited <- Run(Options{Socket: sock, Engine: "comfyui", Priority: "interactive", Needs: []uint64{8 << 30},
			DrainComfyUI: srv.URL, Park: &adapter, ParkBase: srv.URL, ParkProbe: "/system_stats",
			Retry: 10 * time.Millisecond, Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}, Stderr: log})
	}()
	pid := attachAndMeasure(t, b, card)
	stopAtEnd(t, pid, exited)

	holder := acquireIn(b, broker.Request{Engine: "gemma", Priority: broker.Foreground})
	waitFor(t, "the drain waiting on the job", func() bool {
		return strings.Contains(log.String(), "waiting for ComfyUI to finish")
	})

	if m := sendYield(t, sock, time.Minute); m.Op != proto.OpYield {
		t.Fatalf("yield reply = %+v", m)
	}
	waitFor(t, "the park unload", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.frees >= 1
	})
	card.set(gpu.Proc{PID: pid, Name: "sleep", Used: 512 << 20})
	waitFor(t, "parked", func() bool { return b.Status().Leases[0].State == "parked" })
	hold := <-holder
	if hold == nil {
		t.Fatal("gemma was never granted beside the parked job")
	}
	hold.Release()

	if m := sendYield(t, sock, 0); m.Op != proto.OpYield {
		t.Fatalf("yield end reply = %+v", m)
	}
	waitFor(t, "the job's card back", func() bool {
		st := b.Status()
		return len(st.Leases) == 1 && st.Leases[0].Engine == "comfyui" && st.Leases[0].State == "active"
	})
	// The job runs again: its run must not still count it as at its boundary.
	waitFor(t, "the run hearing the end", func() bool { return strings.Contains(log.String(), "comfyui ended its yield") })
	select {
	case code := <-exited:
		t.Fatalf("ComfyUI exited (%d); a yield must park it, not stop it", code)
	default:
	}
}
