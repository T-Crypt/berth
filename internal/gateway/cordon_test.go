// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

// A cordoned card is not free: its background engine waits until berth
// uncordon, and a ComfyUI job for it is told why it cannot run.
func TestCordonedCardIsNotFree(t *testing.T) {
	sock, n := twoCards(t)
	if _, err := n.Cordon("gpu1", true); err != nil {
		t.Fatal(err)
	}
	var st proto.Status
	until(t, "both cards read", func() bool {
		st = n.Status()
		return node.View(&st, "gpu0") != nil && node.View(&st, "gpu1") != nil
	})
	if why := outOfReach(nodeView(t, &st, "gpu1")); why != "the card is cordoned" {
		t.Errorf("outOfReach(gpu1) = %q, want the cordon", why)
	}
	if why := outOfReach(nodeView(t, &st, "gpu0")); why != "" {
		t.Errorf("outOfReach(gpu0) = %q, want nothing", why)
	}

	addr := freeAddr(t)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "tagger", Type: "llamacpp", URL: "http://" + addr, GPU: "gpu1",
			Start:      []string{"env", "BERTH_GW_ENGINE=" + addr + ",tagger-model", os.Args[0]},
			Background: true, IdleAfter: config.Duration{Duration: 50 * time.Millisecond}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Background(ctx, 20*time.Millisecond)

	time.Sleep(400 * time.Millisecond)
	if st := n.Cards()[1].B.Status(); len(st.Leases)+len(st.Waiting) != 0 {
		t.Fatalf("the gateway started a background run for the cordoned card: leases %+v, waiting %+v", st.Leases, st.Waiting)
	}
	if _, err := n.Cordon("gpu1", false); err != nil {
		t.Fatal(err)
	}
	until(t, "the tagger to start once gpu1 is uncordoned", func() bool {
		ls := n.Cards()[1].B.Status().Leases
		return len(ls) == 1 && ls[0].Engine == "tagger"
	})
}

func nodeView(t *testing.T, st *proto.Status, card string) *proto.Status {
	t.Helper()
	v := node.View(st, card)
	if v == nil {
		t.Fatalf("no card %s", card)
	}
	return v
}
