// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/schedule"
	"github.com/T-Crypt/berth/internal/server"
)

func scheduleGateway(t *testing.T) (*httptest.Server, *schedule.Store) {
	t.Helper()
	s, err := schedule.Open(filepath.Join(t.TempDir(), "schedule.json"))
	if err != nil {
		t.Fatal(err)
	}
	sock, _ := daemonServing(t, broker.Config{}, server.Extras{Schedule: s})
	g := &Gateway{Socket: sock, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// An agent books its run, reads it on the board, moves it and cancels it;
// a clash is a 409 naming the booking it hits.
func TestScheduleRoutes(t *testing.T) {
	srv, _ := scheduleGateway(t)
	resp := call(t, "POST", srv.URL+"/v1/berth/schedule", `{"who":"pi-pocket","engine":"pocket","start":"23:00","for":"5h","priority":"batch"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("book = %d %s", resp.StatusCode, readAll(resp))
	}
	made := decode[map[string][]proto.Booking](t, resp)["bookings"]
	if len(made) != 1 || made[0].ID != "b1" || made[0].Card != "gpu0" || made[0].End.Sub(made[0].Start) != 5*time.Hour {
		t.Fatalf("booked = %+v", made)
	}

	resp = call(t, "POST", srv.URL+"/v1/berth/schedule", `{"who":"train","engine":"sft","start":"23:30","for":"1h"}`)
	if body := readAll(resp); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "clashes with b1") {
		t.Fatalf("clash = %d %s", resp.StatusCode, body)
	}

	board := decode[proto.ScheduleBoard](t, call(t, "GET", srv.URL+"/v1/berth/schedule", ""))
	if len(board.Bookings) != 1 || board.Bookings[0].Who != "pi-pocket" || board.Running == nil || board.Queued == nil {
		t.Fatalf("board = %+v", board)
	}

	resp = call(t, "PATCH", srv.URL+"/v1/berth/schedule/b1", `{"for":"2h"}`)
	moved := decode[map[string][]proto.Booking](t, resp)["bookings"]
	if resp.StatusCode != http.StatusOK || len(moved) != 1 || moved[0].End.Sub(moved[0].Start) != 2*time.Hour {
		t.Fatalf("move = %d %+v", resp.StatusCode, moved)
	}

	resp = call(t, "DELETE", srv.URL+"/v1/berth/schedule/b1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel = %d %s", resp.StatusCode, readAll(resp))
	}
	resp.Body.Close()
	if board := decode[proto.ScheduleBoard](t, call(t, "GET", srv.URL+"/v1/berth/schedule", "")); len(board.Bookings) != 0 {
		t.Fatalf("after cancel = %+v", board.Bookings)
	}
}

// Downtime and priority changes are the operator's: the gateway books only
// runs, and will not move or cancel anything else.
func TestScheduleRoutesLeaveTheOperatorsBookings(t *testing.T) {
	srv, s := scheduleGateway(t)
	now := time.Now()
	if _, err := s.Book(proto.Booking{Card: "gpu0", Kind: proto.BookDown, Who: "operator", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}, 1, false, now); err != nil {
		t.Fatal(err)
	}
	resp := call(t, "POST", srv.URL+"/v1/berth/schedule", `{"who":"agent","engine":"x","start":"now","for":"30m","kind":"down"}`)
	made := decode[map[string][]proto.Booking](t, resp)["bookings"]
	if resp.StatusCode != http.StatusCreated || made[0].Kind != proto.BookRun {
		t.Fatalf("a kind in the body booked %+v (%d), want a run", made, resp.StatusCode)
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		resp := call(t, method, srv.URL+"/v1/berth/schedule/b1", `{"for":"3h"}`)
		if body := readAll(resp); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "only the operator") {
			t.Errorf("%s downtime = %d %s, want 403", method, resp.StatusCode, body)
		}
	}
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
