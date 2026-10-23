// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import "testing"

func TestLocalURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:9798":  "http://127.0.0.1:9798/",
		"0.0.0.0:50980":   "http://127.0.0.1:50980/",
		"[::]:9798":       "http://127.0.0.1:9798/",
		"10.0.0.200:9798": "http://10.0.0.200:9798/",
	} {
		if got := localURL(in); got != want {
			t.Errorf("localURL(%q) = %q, want %q", in, got, want)
		}
	}
}
