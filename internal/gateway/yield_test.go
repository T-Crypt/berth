// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
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
