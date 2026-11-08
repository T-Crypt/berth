// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"net/http"
	"testing"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/proto"
)

func batchKinds(b *broker.Broker) []string {
	evs, _, _ := b.Events(0, false)
	var out []string
	for _, e := range evs {
		if e.Kind == proto.EvBatchStarted || e.Kind == proto.EvBatchEnded {
			out = append(out, e.Kind+" "+e.Engine)
		}
	}
	return out
}

// One call before the jobs: the batch is on and the engine is started under
// a lease, as a switch would start it. End takes the batch off.
func TestBatchStartsTheEngineAndEnds(t *testing.T) {
	srv, b := switchGateway(t)

	resp := call(t, "POST", srv.URL+"/v1/berth/batch", `{"engine":"strata","grace":20}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("batch = %d", resp.StatusCode)
	}
	until(t, "strata to hold the card", func() bool {
		st := b.Status()
		return len(st.Leases) == 1 && st.Leases[0].Engine == "strata" && st.Leases[0].State == "active"
	})

	resp = call(t, "POST", srv.URL+"/v1/berth/batch", `{"engine":"strata","end":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("end = %d", resp.StatusCode)
	}
	got := batchKinds(b)
	if len(got) != 2 || got[0] != "batch.started strata" || got[1] != "batch.ended strata" {
		t.Fatalf("batch events = %q", got)
	}
}

func TestBatchRefusals(t *testing.T) {
	srv, _ := switchGateway(t)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"engine":"nope"}`, http.StatusNotFound},
		{`not json`, http.StatusBadRequest},
		{`{"engine":"strata","grace":-1}`, http.StatusBadRequest},
		{`{"engine":"strata","end":true}`, http.StatusConflict}, // no batch to end
	} {
		resp := call(t, "POST", srv.URL+"/v1/berth/batch", c.body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s = %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
}
