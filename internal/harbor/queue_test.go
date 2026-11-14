// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
)

// A paired device reorders and drops waiters through the harbor, straight to
// berthd; an unpaired one is refused and the queue is unchanged; nothing goes
// through the gateway, whose key agents hold.
func TestBoardQueue(t *testing.T) {
	base, gw, token, b := brokerHarbor(t, testKey)
	if _, err := b.Acquire(context.Background(), broker.Request{Engine: "game", Priority: broker.Hold, Pinned: true}, nil); err != nil {
		t.Fatal(err)
	}
	refused := make(chan error, 2)
	for _, e := range []string{"first", "second"} {
		go func() {
			_, err := b.Acquire(context.Background(), broker.Request{Engine: e, Priority: broker.Interactive, Wait: time.Minute}, nil)
			refused <- err
		}()
		time.Sleep(20 * time.Millisecond)
	}
	order := func() (ids, names []string) {
		for _, l := range b.Status().Waiting {
			ids, names = append(ids, l.ID), append(names, l.Engine)
		}
		return
	}
	ids, names := order()
	if strings.Join(names, ",") != "first,second" {
		t.Fatalf("queue = %v", names)
	}
	paired := map[string]string{"Cookie": cookieName + "=" + token, "X-Berth": "1", "Sec-Fetch-Site": "same-origin"}
	unpaired := map[string]string{"X-Berth": "1", "Sec-Fetch-Site": "same-origin"}

	code, body := call(t, "POST", base+"/v1/board/queue", `{"lease":"`+ids[1]+`","action":"bump"}`, unpaired)
	if code != 401 {
		t.Fatalf("unpaired bump = %d %s, want 401", code, body)
	}
	if _, names = order(); strings.Join(names, ",") != "first,second" {
		t.Fatalf("an unpaired bump moved the queue: %v", names)
	}

	code, body = call(t, "POST", base+"/v1/board/queue", `{"lease":"`+ids[1]+`","action":"bump"}`, paired)
	var got struct{ Lease, Change string }
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got.Lease != ids[1] ||
		got.Change != "moved to the front of its class by the operator" {
		t.Fatalf("bump = %d %s", code, body)
	}
	if _, names = order(); strings.Join(names, ",") != "second,first" {
		t.Fatalf("after bump: %v", names)
	}

	for _, bad := range []string{`{"lease":"x","action":"jump"}`, `{"lease":"x","action":"prio"}`, `{"action":"drop"}`} {
		if code, body = call(t, "POST", base+"/v1/board/queue", bad, paired); code != 400 {
			t.Fatalf("%s = %d %s, want 400", bad, code, body)
		}
	}
	if code, body = call(t, "POST", base+"/v1/board/queue", `{"lease":"`+ids[0]+`","action":"prio","priority":"hold"}`, paired); code != 409 {
		t.Fatalf("prio hold = %d %s, want 409", code, body)
	}

	if code, body = call(t, "POST", base+"/v1/board/queue", `{"lease":"`+ids[0]+`","action":"drop"}`, paired); code != 200 {
		t.Fatalf("drop = %d %s", code, body)
	}
	select {
	case err := <-refused:
		if err == nil || !strings.Contains(err.Error(), "dropped from the queue by the operator") {
			t.Fatalf("dropped waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dropped waiter still waiting")
	}
	if gw.last() != "" {
		t.Fatalf("a queue action reached the gateway: %q", gw.last())
	}
}
