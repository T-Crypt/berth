// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import "testing"

// --harbor is the harbor's base URL, so anything else is refused at startup
// rather than turning every old /gui/ bookmark into a dead redirect.
func TestGatewayHarborBase(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"https://box.tailnet.ts.net:9798", "https://box.tailnet.ts.net:9798"},
		{"https://box.tailnet.ts.net:9798/", "https://box.tailnet.ts.net:9798"},
		{"http://127.0.0.1:9798/#switch", "http://127.0.0.1:9798"},
		{"https://box.tailnet.ts.net/harbor/", "https://box.tailnet.ts.net/harbor"},
	} {
		got, err := harborBase(c.in)
		if err != nil || got != c.want {
			t.Errorf("harborBase(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"ftp://x", "notaurl", "https://", "//box"} {
		if got, err := harborBase(bad); err == nil {
			t.Errorf("harborBase(%q) = %q, accepted", bad, got)
		}
	}
}

// The gateway command takes the flag and refuses a value that is not an http
// or https address.
func TestGatewayHarborFlag(t *testing.T) {
	isolate(t)
	if code, _, errOut := runCLI(t, "gateway", "--harbor", "ftp://x"); code != ExitFail {
		t.Fatalf("--harbor ftp://x = exit %d, want %d:\n%s", code, ExitFail, errOut)
	}
	if code, _, errOut := runCLI(t, "gateway", "--harbor", "notaurl"); code != ExitFail {
		t.Fatalf("--harbor notaurl = exit %d, want %d:\n%s", code, ExitFail, errOut)
	}
}
