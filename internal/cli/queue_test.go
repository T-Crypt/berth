// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// ask sends one acquire over the real socket and leaves the connection open,
// so the lease holds or waits for the rest of the test.
func ask(t *testing.T, sock string, m proto.Msg) *proto.Conn {
	t.Helper()
	conn, raw, err := server.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	m.Op = proto.OpAcquire
	if err := conn.Send(m); err != nil {
		t.Fatal(err)
	}
	return conn
}

type queued struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
}

func queueJSON(t *testing.T, sock string) []queued {
	t.Helper()
	code, out, errOut := runCLI(t, "queue", "--socket", sock, "--format", "json")
	if code != 0 {
		t.Fatalf("berth queue: exit %d: %s", code, errOut)
	}
	var w []queued
	if err := json.Unmarshal([]byte(out), &w); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return w
}

// berth queue lists the waiters in grant order over the real wire, and bump
// and drop change that order on a real berthd.
func TestQueueAgainstADaemon(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	holder := ask(t, sock, proto.Msg{Engine: "holder", Priority: "foreground", Pinned: true})
	if m, err := holder.Recv(); err != nil || m.Op != proto.OpGranted {
		t.Fatalf("holder: %+v, %v", m, err)
	}
	ask(t, sock, proto.Msg{Engine: "first", Priority: "interactive", Wait: 30})
	waitUntil(t, func() bool { return len(queueJSON(t, sock)) == 1 })
	ask(t, sock, proto.Msg{Engine: "second", Priority: "interactive", Wait: 30})
	waitUntil(t, func() bool { return len(queueJSON(t, sock)) == 2 })

	w := queueJSON(t, sock)
	if w[0].Engine != "first" || w[1].Engine != "second" {
		t.Fatalf("queue = %+v, want first then second", w)
	}

	code, out, errOut := runCLI(t, "queue", "bump", w[1].ID, "--socket", sock)
	if code != 0 || !strings.Contains(out, "moved to the front of its class by the operator") {
		t.Fatalf("bump (exit %d): %s %s", code, out, errOut)
	}
	if w = queueJSON(t, sock); w[0].Engine != "second" {
		t.Fatalf("after bump: %+v", w)
	}

	code, out, _ = runCLI(t, "queue", "--socket", sock)
	if code != 0 || !strings.Contains(out, "POS") || strings.Index(out, "second") > strings.Index(out, "first") {
		t.Fatalf("human list (exit %d):\n%s", code, out)
	}

	if code, _, errOut = runCLI(t, "queue", "drop", w[1].ID, "--socket", sock); code != 0 {
		t.Fatalf("drop: exit %d: %s", code, errOut)
	}
	waitUntil(t, func() bool { return len(queueJSON(t, sock)) == 1 })

	code, _, errOut = runCLI(t, "queue", "prio", w[0].ID, "hold", "--socket", sock)
	if code == 0 || !strings.Contains(errOut, "use batch, interactive or foreground") {
		t.Fatalf("prio hold (exit %d): %s", code, errOut)
	}
	code, _, errOut = runCLI(t, "queue", "bump", "nope-9", "--socket", sock)
	if code == 0 || !strings.Contains(errOut, "no waiter nope-9") {
		t.Fatalf("unknown id (exit %d): %s", code, errOut)
	}
}
