// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
)

// A paired device books downtime, moves it and cancels it through the
// harbor, straight to berthd; the board reads it from /v1/status; an
// unpaired device changes nothing; nothing reaches the gateway.
func TestBoardSchedule(t *testing.T) {
	base, gw, token, _ := brokerHarbor(t, testKey)
	paired := map[string]string{"Cookie": cookieName + "=" + token, "X-Berth": "1", "Sec-Fetch-Site": "same-origin"}
	unpaired := map[string]string{"X-Berth": "1", "Sec-Fetch-Site": "same-origin"}
	start := time.Now().Add(time.Hour).Truncate(time.Minute)
	down := func(id string, from time.Time, d time.Duration) string {
		b, _ := json.Marshal(proto.Booking{ID: id, Kind: proto.BookDown, Who: "operator", Start: from, End: from.Add(d), Note: "driver update"})
		return string(b)
	}

	if code, body := call(t, "POST", base+"/v1/board/schedule", `{"action":"book","booking":`+down("", start, time.Hour)+`}`, unpaired); code != 401 {
		t.Fatalf("unpaired book = %d %s, want 401", code, body)
	}
	code, body := call(t, "POST", base+"/v1/board/schedule", `{"action":"book","days":2,"booking":`+down("", start, time.Hour)+`}`, paired)
	var got struct{ Bookings []proto.Booking }
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || len(got.Bookings) != 2 || got.Bookings[0].Card != "gpu0" {
		t.Fatalf("book = %d %s", code, body)
	}

	code, body = call(t, "GET", base+"/v1/status", "", nil)
	var st proto.Status
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || len(st.Bookings) != 2 || st.Bookings[0].Note != "driver update" {
		t.Fatalf("status bookings = %d %s", code, body)
	}

	if code, body = call(t, "POST", base+"/v1/board/schedule", `{"action":"move","booking":`+down("b1", start.Add(30*time.Minute), time.Hour)+`}`, paired); code != 200 {
		t.Fatalf("move = %d %s", code, body)
	}
	if code, body = call(t, "POST", base+"/v1/board/schedule", `{"action":"move","booking":`+down("b1", start.AddDate(0, 0, 1), time.Hour)+`}`, paired); code != 409 || !strings.Contains(body, "clashes with b2") {
		t.Fatalf("move onto b2 = %d %s, want 409", code, body)
	}
	if code, body = call(t, "POST", base+"/v1/board/schedule", `{"action":"cancel","booking":{"id":"b2"}}`, paired); code != 200 {
		t.Fatalf("cancel = %d %s", code, body)
	}
	for _, bad := range []string{`{"action":"nap","booking":{}}`, `{"action":"cancel","booking":{}}`, `{"action":"book","booking":{"id":"b1"}}`, `{"action":"book"}`} {
		if code, body = call(t, "POST", base+"/v1/board/schedule", bad, paired); code != 400 {
			t.Fatalf("%s = %d %s, want 400", bad, code, body)
		}
	}
	if gw.last() != "" {
		t.Fatalf("a schedule action reached the gateway: %q", gw.last())
	}
}
