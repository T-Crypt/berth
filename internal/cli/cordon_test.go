// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// berth cordon and uncordon against a live daemon: the report names the
// card, status shows the cordon, and an unknown card fails with exit 1.
func TestCordonAgainstADaemon(t *testing.T) {
	sock, _, _ := startStopDaemon(t, time.Second)
	cordoned := func() bool {
		code, out, _ := runCLI(t, "status", "--socket", sock, "--format", "json")
		if code != 0 {
			t.Fatalf("status exit %d", code)
		}
		var st proto.Status
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			t.Fatal(err)
		}
		return len(st.GPUs) > 0 && st.GPUs[0].Cordoned
	}

	code, out, errOut := runCLI(t, "cordon", "gpu0", "--socket", sock)
	if code != 0 || !strings.Contains(out, "gpu0") {
		t.Fatalf("cordon: exit %d, out %q, err %q", code, out, errOut)
	}
	if !cordoned() {
		t.Error("status does not show gpu0 cordoned")
	}
	if _, out, _ := runCLI(t, "status", "--socket", sock); !strings.Contains(out, "CORDONED") {
		t.Errorf("berth status does not say CORDONED:\n%s", out)
	}
	if code, out, errOut = runCLI(t, "uncordon", "gpu0", "--socket", sock); code != 0 || !strings.Contains(out, "gpu0") {
		t.Fatalf("uncordon: exit %d, out %q, err %q", code, out, errOut)
	}
	if cordoned() {
		t.Error("status still shows gpu0 cordoned")
	}
	if code, _, errOut = runCLI(t, "cordon", "gpu7", "--socket", sock); code != ExitFail || !strings.Contains(errOut, "no card gpu7") {
		t.Errorf("unknown card: exit %d, err %q, want exit %d naming the card", code, errOut, ExitFail)
	}
}
