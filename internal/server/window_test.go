// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server_test

import (
	"testing"

	"github.com/T-Crypt/berth/internal/proto"
)

// A window reaches berthd as the string its entry or its flag wrote, and the
// socket is where it becomes a window. One it cannot read is refused with the
// reason, the way a card the node does not have is: an empty window must
// never be what a typo turns into.
func TestAcquireRefusesAWindowItCannotRead(t *testing.T) {
	sock := start(t)
	m := dial(t, sock, proto.Msg{Op: proto.OpAcquire, Engine: "vllm", Priority: "batch", Window: "11pm-7"})
	if m.Op != proto.OpRefused || m.Reason != `window "11pm-7": "11pm" is not HH:MM` {
		t.Fatalf("acquire with window 11pm-7 = %s %q, want refused naming what is wrong with it", m.Op, m.Reason)
	}
	if m := dial(t, sock, proto.Msg{Op: proto.OpAcquire, Engine: "vllm", Priority: "batch"}); m.Op != proto.OpGranted {
		t.Fatalf("the next acquire after a refused window = %s %q, want granted", m.Op, m.Reason)
	}
}
