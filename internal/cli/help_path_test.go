// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

// A subcommand's help and hints name its group: "berth engine show", not
// "berth show", which is no command.
func TestSubcommandHelpNamesItsGroup(t *testing.T) {
	for _, args := range [][]string{{"engine", "show", "--help"}, {"help", "engine", "show"}} {
		code, out, _ := runCLI(t, args...)
		if code != 0 || !strings.HasPrefix(out, "berth engine show ") {
			t.Errorf("berth %s (exit %d) starts %q, want \"berth engine show \"", strings.Join(args, " "), code, firstLine(out))
		}
	}
	_, _, errOut := runCLI(t, "engine", "show", "--nope")
	if !strings.Contains(errOut, `berth engine show -h`) {
		t.Errorf("bad flag hint = %q, want it to name berth engine show -h", errOut)
	}
}
