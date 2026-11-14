// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

func at(min int) time.Time { return time.Date(2026, 11, 12, 23, min, 0, 0, time.UTC) }

// The 11-12 night: strata waited for OrcaSAQ to drain, held the card, and
// was drained for Dirk-27B. Then berthd restarted and Dirk re-attached.
var night = []proto.Event{
	{Seq: 1, At: at(22), Kind: proto.EvRequested, Lease: "strata-24", Engine: "strata", Detail: "foreground"},
	{Seq: 2, At: at(22), Kind: proto.EvDraining, Lease: "OrcaSAQ-23", Engine: "OrcaSAQ", Detail: "strata (foreground) needs the card"},
	{Seq: 3, At: at(22), Kind: proto.EvWaiting, Lease: "strata-24", Engine: "strata", Detail: "next in line: draining OrcaSAQ [foreground, active]"},
	{Seq: 4, At: at(23), Kind: proto.EvGranted, Lease: "strata-24", Engine: "strata", Detail: "waited 1.631s"},
	{Seq: 5, At: at(25), Kind: proto.EvRequested, Lease: "Dirk-27B-25", Engine: "Dirk-27B", Detail: "foreground"},
	{Seq: 6, At: at(25), Kind: proto.EvDraining, Lease: "strata-24", Engine: "strata", Detail: "Dirk-27B (foreground) needs the card"},
	{Seq: 7, At: at(25), Kind: proto.EvReleased, Lease: "strata-24", Engine: "strata"},
	{Seq: 8, At: at(26), Kind: proto.EvFreed, Lease: "strata-24", Engine: "strata", Bytes: 16 << 30},
	{Seq: 9, At: at(26), Kind: proto.EvGranted, Lease: "Dirk-27B-25", Engine: "Dirk-27B", Detail: "waited 40s"},
	{Seq: 10, At: at(30), Kind: proto.EvNodeStarted, Detail: "berth 0.6.0"},
	{Seq: 11, At: at(30), Kind: proto.EvReattached, Lease: "Dirk-27B-26", Engine: "Dirk-27B", Detail: "foreground"},
}

func kindsOf(evs []proto.Event) string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return strings.Join(k, " ")
}

func TestWhyTellsOneLeasesStory(t *testing.T) {
	doc, ok := whyOf(night, nil, "strata")
	if !ok {
		t.Fatal("strata not found")
	}
	if doc.Lease != "strata-24" || doc.Engine != "strata" {
		t.Fatalf("lease = %q engine = %q", doc.Lease, doc.Engine)
	}
	if got, want := kindsOf(doc.Events), "lease.requested lease.waiting lease.granted lease.draining lease.released lease.freed"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if doc.Next != "" {
		t.Fatalf("an ended lease names a re-attach: %q", doc.Next)
	}
}

// A lease a restart cut off ends at the restart and names the lease its
// engine re-attached as.
func TestWhyFollowsARestart(t *testing.T) {
	doc, ok := whyOf(night, nil, "Dirk-27B-25")
	if !ok {
		t.Fatal("Dirk-27B-25 not found")
	}
	if got, want := kindsOf(doc.Events), "lease.requested lease.granted node.started"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if doc.Next != "Dirk-27B-26" {
		t.Fatalf("next = %q, want Dirk-27B-26", doc.Next)
	}

	doc, _ = whyOf(night, []proto.Lease{{ID: "Dirk-27B-26", State: "active", Priority: "foreground"}}, "Dirk-27B")
	if doc.Lease != "Dirk-27B-26" || doc.Now == nil || doc.Now.State != "active" {
		t.Fatalf("the engine's newest lease = %+v", doc)
	}
}

func TestWhyAgainstADaemon(t *testing.T) {
	isolate(t)
	sock := startDaemonWith(t, server.Extras{})
	holder := ask(t, sock, proto.Msg{Engine: "comfyui", Priority: "interactive"})
	if m, err := holder.Recv(); err != nil || m.Op != proto.OpGranted {
		t.Fatalf("holder: %+v, %v", m, err)
	}
	ask(t, sock, proto.Msg{Engine: "strata", Priority: "foreground", Wait: 30})
	waitUntil(t, func() bool { return len(queueJSON(t, sock)) == 1 })

	code, out, errOut := runCLI(t, "why", "comfyui", "--socket", sock)
	if code != 0 {
		t.Fatalf("why (exit %d): %s", code, errOut)
	}
	for _, want := range []string{"asked for the card", "granted", "asked to give the card back: strata (foreground) needs the card", "now "} {
		if !strings.Contains(out, want) {
			t.Fatalf("why comfyui = %q, want it to contain %q", out, want)
		}
	}

	code, out, _ = runCLI(t, "why", "strata", "--socket", sock, "--format", "json")
	var doc whyDoc
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.Now == nil || doc.Now.State != "waiting" {
		t.Fatalf("why strata json (exit %d): %s", code, out)
	}

	code, _, errOut = runCLI(t, "why", "nobody", "--socket", sock)
	if code != 1 || !strings.Contains(errOut, `no lease or engine named "nobody"`) {
		t.Fatalf("why nobody (exit %d): %s", code, errOut)
	}
}
