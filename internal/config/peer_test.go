// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// A peer's node key goes with every request to it, so its URL must stay on
// the machine or the tailnet.
func TestPeerURLStaysOnTheTailnet(t *testing.T) {
	ok := []string{
		"https://galaxy.tail2a8bb.ts.net:9799",
		"http://100.101.5.7:9799",
		"http://[fd7a:115c:a1e0::12]:9799",
		"http://127.0.0.1:19799/",
		"http://localhost:19799",
	}
	for _, u := range ok {
		c := Config{Peer: []Peer{{Name: "p", URL: u, KeyFile: "/k"}}}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := map[string]string{
		"https://berth.example.com:9799":     "not on the tailnet",
		"http://192.168.1.20:9799":           "not on the tailnet",
		"http://100.128.0.1:9799":            "not on the tailnet",
		"ftp://galaxy.tail2a8bb.ts.net":      "not an http or https URL",
		"https://galaxy.tail2a8bb.ts.net/v1": "names a path",
	}
	for u, want := range bad {
		c := Config{Peer: []Peer{{Name: "p", URL: u, KeyFile: "/k"}}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", u, err, want)
		}
	}
}

func TestPeerNeedsANameAKeyFileAndNoTwin(t *testing.T) {
	u := "http://127.0.0.1:19799"
	cases := map[string][]Peer{
		"needs a name":      {{URL: u, KeyFile: "/k"}},
		"key_file must be":  {{Name: "p", URL: u, KeyFile: "k"}},
		"is already a peer": {{Name: "p", URL: u, KeyFile: "/k"}, {Name: "p", URL: u, KeyFile: "/k"}},
	}
	for want, ps := range cases {
		c := Config{Peer: ps}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}
