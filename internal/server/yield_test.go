// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
)

// #299 end to end: a ComfyUI job waits on an LLM node, and the LLM waits for
// the card ComfyUI holds. Its drain waits on the job, so neither moves. A
// yield from inside the job lets the drain through, and the LLM gets the card.
func TestYieldBreaksTheComfyUILLMDeadlock(t *testing.T) {
	sock := start(t)
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/queue" {
			fmt.Fprint(w, `{"queue_running":[["1","job"]],"queue_pending":[]}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer comfy.Close()

	comfyui, comfyOut := launch(runner.Options{Socket: sock, Engine: "comfyui", Priority: "foreground",
		DrainComfyUI: comfy.URL, Retry: 20 * time.Millisecond, Grace: 200 * time.Millisecond, Argv: []string{"sleep", "30"}})
	waitFor(t, "comfyui grant", func() bool { return strings.Contains(comfyOut.String(), "granted") })

	llm, llmOut := launch(runner.Options{Socket: sock, Engine: "gemma", Priority: "foreground", Argv: []string{"true"}})
	waitFor(t, "the drain waiting on the job", func() bool {
		return strings.Contains(comfyOut.String(), "waiting for ComfyUI to finish")
	})
	select {
	case code := <-llm:
		t.Fatalf("gemma ended (%d) while the job held the card; stderr:\n%s", code, llmOut.String())
	case <-time.After(500 * time.Millisecond):
	}

	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpYield, Engine: "comfyui", Yield: 60}); err != nil {
		t.Fatal(err)
	}
	if m, err := conn.Recv(); err != nil || m.Op != proto.OpYield || len(m.Leases) != 1 {
		t.Fatalf("yield reply = %+v, %v", m, err)
	}

	select {
	case code := <-llm:
		if code != 0 {
			t.Fatalf("gemma exit = %d; stderr:\n%s", code, llmOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("gemma never got the card after the yield; comfyui:\n%s", comfyOut.String())
	}
	if code := <-comfyui; code != 128+15 {
		t.Fatalf("comfyui exit = %d, want 143 after its drain; stderr:\n%s", code, comfyOut.String())
	}
	if !strings.Contains(comfyOut.String(), "yielded the card from inside its job") {
		t.Fatalf("the run never heard the yield:\n%s", comfyOut.String())
	}
}

// A yield names a leased engine; one that has no lease is refused, so a
// node pointed at the wrong name hears it at once.
func TestYieldWithNoLeaseIsRefused(t *testing.T) {
	sock := start(t)
	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpYield, Engine: "comfyui", Yield: 60}); err != nil {
		t.Fatal(err)
	}
	m, err := conn.Recv()
	if err != nil || m.Op != proto.OpRefused || !strings.Contains(m.Reason, "no running engine named comfyui") {
		t.Fatalf("reply = %+v, %v", m, err)
	}
}
