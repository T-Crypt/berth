// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

// The tray starts at logon through the same hidden launcher as the other
// units: the Run value is wscript on a stub, never the console exe, so no
// window opens and closing one cannot end the tray (issue 344).
func TestNotifyAtLogonStartsTheTrayHidden(t *testing.T) {
	exe := `C:\Program Files\berth\berth.exe`
	run, stub, err := notifyLauncher(exe)
	if err != nil {
		t.Fatal(err)
	}
	if want := `wscript.exe //B //Nologo "` + unitStubPath("berth-notify") + `"`; run != want {
		t.Errorf("Run value = %s, want %s", run, want)
	}
	if strings.Contains(run, "berth.exe") {
		t.Errorf("the Run value starts the console exe: %s", run)
	}
	for _, want := range []string{`""` + exe + `"" notify`, unitLogPath("berth-notify"), ", 0, True)"} {
		if !strings.Contains(stub, want) {
			t.Errorf("stub lacks %q:\n%s", want, stub)
		}
	}
}
