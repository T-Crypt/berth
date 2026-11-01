// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package sysd

import (
	"strings"
	"testing"
)

// slice placement is the whole point: the engine scope lands under the
// operator's slice, so the slice unit's resource policy covers it.
func TestScopeArgvPlacesTheScopeUnderTheSlice(t *testing.T) {
	got := scopeArgv("berth-run-strata-0000ffff", "strata.slice", []string{"run.sh"})
	want := []string{"systemd-run", "--user", "--scope", "--unit", "berth-run-strata-0000ffff", "--collect", "--quiet", "--slice", "strata.slice", "--", "run.sh"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %q, want %q", got, want)
	}
	bare := scopeArgv("berth-run-strata-0000ffff", "", []string{"run.sh"})
	if strings.Contains(strings.Join(bare, " "), "--slice") {
		t.Errorf("a run with no slice gained one: %q", bare)
	}
}
