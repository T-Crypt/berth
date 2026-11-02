// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runner

// The runner answers a lend ask with the engine's own call: the entry's
// lend_url on the generic protocol, the built-in endpoints for a kind that has
// them, the run's credential on every one, and the engine's answer back to
// berthd unchanged. The run also keeps the ledger of what it has lent: the
// open tickets enter it on a lend the engine granted and leave it on a
// take-back it confirmed, a re-attach names its one open ticket, and a lend
// the engine answered 503 is watched for landing and handed straight back.
// These drive one step against an httptest engine, with a pipe standing in
// for the daemon's connection.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/lend"
	"github.com/T-Crypt/berth/internal/proto"
)

// lendResp is one answer: a status and the body that goes with it.
type lendResp struct {
	status  int
	payload string
}

// lendStub is an engine's lend endpoints: it records every request in order
// and answers by path when byPath is set, else with the single status and
// payload the test hands it.
type lendStub struct {
	mu       sync.Mutex
	requests []string // every call, each as "path auth body"
	path     string   // the last call, as the older tests read it
	auth     string
	body     string
	status   int
	payload  string
	byPath   map[string]lendResp
}

func (s *lendStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.path, s.auth, s.body = r.URL.Path, r.Header.Get("Authorization"), string(raw)
		s.requests = append(s.requests, s.path+" "+s.auth+" "+s.body)
		resp, routed := s.byPath[r.URL.Path]
		if !routed {
			resp = lendResp{s.status, s.payload}
		}
		s.mu.Unlock()
		if resp.status != 0 && resp.status != http.StatusOK {
			w.WriteHeader(resp.status)
			fmt.Fprint(w, resp.payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp.payload)
	})
}

// saw is what the last request looked like.
func (s *lendStub) saw() (path, auth, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path, s.auth, s.body
}

// calls lists every request the stub has seen, each as "path auth body".
func (s *lendStub) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// lendEntry is a run of a vLLM-shaped engine with the generic lend endpoints,
// as its berth.toml entry and the daemon's ask would give them.
func lendEntry(url string) Options {
	o := OptionsFor(&config.Engine{Type: "vllm", Name: "maya", URL: url,
		Lend: "hook", LendURL: url}, Over{})
	o.APIKey = "sk-test"
	return o
}

// daemonPipe stands a pipe in for the daemon's connection: the runner end
// rides a fresh line, the daemon end reads the answers.
func daemonPipe(t *testing.T) (*leaseLine, *proto.Conn) {
	t.Helper()
	runnerEnd, daemonEnd := net.Pipe()
	t.Cleanup(func() {
		_ = runnerEnd.Close()
		_ = daemonEnd.Close()
	})
	return newLine(proto.NewConn(runnerEnd)), proto.NewConn(daemonEnd)
}

// askStep hands one ask to the runner's step on an existing line and reads
// the answer the daemon end gets: two asks on one line are one run's life.
func askStep(t *testing.T, line *leaseLine, daemon *proto.Conn, o Options, ask proto.Msg) proto.Msg {
	t.Helper()
	say := func(format string, args ...any) { t.Logf(format, args...) }
	reply := make(chan proto.Msg, 1)
	go func() {
		m, err := daemon.Recv()
		if err == nil {
			reply <- m
		}
	}()
	switch ask.Op {
	case proto.OpLend:
		go line.lendStep(o, ask, say)
	case proto.OpReturn:
		go line.returnStep(o, ask.LendID, say)
	}
	select {
	case m := <-reply:
		return m
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the runner never answered", ask.Op)
	}
	return proto.Msg{}
}

// askOn stands a fresh pipe in for the daemon's connection, hands it one ask,
// and returns the answer and the line it rode: the line's ledger is what the
// run remembers across a restart.
func askOn(t *testing.T, o Options, ask proto.Msg) (proto.Msg, *leaseLine) {
	t.Helper()
	line, daemon := daemonPipe(t)
	return askStep(t, line, daemon, o, ask), line
}

// waitForCall waits until the stub has seen a request naming want, and fails
// with the calls it saw instead.
func waitForCall(t *testing.T, stub *lendStub, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range stub.calls() {
			if strings.Contains(c, want) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the engine never saw %q; calls: %v", want, stub.calls())
}

// A lend ask on an entry with lend_url reaches the engine's lend endpoint with
// run's key and the shortfall, the ticket comes back, and the run's ledger
// holds the ticket: it is what a later re-attach names.
func TestLendAskReachesTheEngineAndAnswersWithItsTicket(t *testing.T) {
	stub := &lendStub{payload: `{"id":"17","miB":4096,"gpu":"0000:09:00.0"}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	got, line := askOn(t, lendEntry(srv.URL), proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"})

	if got.Op != proto.OpLent || got.LendID != "17" || got.Needs != 4<<30 {
		t.Fatalf("the runner answered op=%s ticket=%q needs=%d, want op=lent ticket=17 needs=%d",
			got.Op, got.LendID, got.Needs, int64(4)<<30)
	}
	path, auth, body := stub.saw()
	if path != "/vram/lend" {
		t.Fatalf("the engine was called at %s, want /vram/lend", path)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("the engine saw %q, want the run's bearer key", auth)
	}
	if body != `{"gpu":"0000:09:00.0","mib":4096}` {
		t.Fatalf("the engine was asked %s", body)
	}
	tk, ok := line.openLend()
	if !ok || tk.ID != "17" || tk.MiB != 4096 || tk.GPU != "0000:09:00.0" {
		t.Fatalf("the ledger = %+v (open: %v), want the ticket 17 of 4096 MiB on 0000:09:00.0", tk, ok)
	}
}

// A strata entry needs no lend_url: the ask becomes one reserve_vram call that
// carries the whole request (free plus the shortfall), and the synthetic ticket
// comes back.
func TestLendAskOnStrataReservesTheWholeRequest(t *testing.T) {
	stub := &lendStub{payload: `{"vram_free_mib":24576}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := OptionsFor(&config.Engine{Type: "strata", Name: "strata", URL: srv.URL, Lend: "hook"}, Over{})
	o.APIKey = "sk-test"
	got, _ := askOn(t, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 20 << 10})

	if got.Op != proto.OpLent || got.LendID != "reserve" || got.Needs != 4<<30 {
		t.Fatalf("the runner answered op=%s ticket=%q needs=%d, want op=lent ticket=reserve needs=%d",
			got.Op, got.LendID, got.Needs, int64(4)<<30)
	}
	path, auth, body := stub.saw()
	if path != "/v1/vram" {
		t.Fatalf("the engine was called at %s, want /v1/vram", path)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("the engine saw %q, want the run's bearer key", auth)
	}
	if body != `{"reserve_mib":24576}` {
		t.Fatalf("the engine was asked %s", body)
	}
}

// A take-back the engine completes leaves the run owing nothing: the ticket
// entered the ledger with the lend and leaves it with the take-back, and a
// re-attach would name no lend.
func TestTakeBackClearsTheLedger(t *testing.T) {
	stub := &lendStub{byPath: map[string]lendResp{
		"/vram/lend":   {payload: `{"id":"17","miB":4096,"gpu":"0000:09:00.0"}`},
		"/vram/return": {payload: `{"id":"17","gpu":"0000:09:00.0","mib":4096}`},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := lendEntry(srv.URL)
	line, daemon := daemonPipe(t)
	if got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"}); got.Op != proto.OpLent {
		t.Fatalf("the lend answered op=%s, want lent", got.Op)
	}
	got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpReturn, LendID: "17"})
	if got.Op != proto.OpReturned || got.LendID != "17" {
		t.Fatalf("the runner answered op=%s ticket=%q, want returned ticket=17", got.Op, got.LendID)
	}
	if _, ok := line.openLend(); ok {
		t.Fatalf("the ledger still holds %+v after a completed take-back", line.lends)
	}
}

// A take-back the engine answers 404 counts as done: the lend is not open,
// so the ticket leaves the ledger just as on a 200.
func TestReturnOfATicketTheEngineLostClearsTheLedger(t *testing.T) {
	stub := &lendStub{byPath: map[string]lendResp{
		"/vram/lend":   {payload: `{"id":"17","miB":4096,"gpu":"0000:09:00.0"}`},
		"/vram/return": {status: http.StatusNotFound, payload: `{"error":"no open lease 17"}`},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := lendEntry(srv.URL)
	line, daemon := daemonPipe(t)
	if got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"}); got.Op != proto.OpLent {
		t.Fatalf("the lend answered op=%s, want lent", got.Op)
	}
	got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpReturn, LendID: "17"})
	if got.Op != proto.OpReturned || got.LendID != "17" {
		t.Fatalf("the runner answered op=%s ticket=%q, want returned ticket=17: a 404 is done", got.Op, got.LendID)
	}
	if _, ok := line.openLend(); ok {
		t.Fatalf("the ledger still holds %+v after a 404", line.lends)
	}
}

// A take-back the engine refuses with a 409 is reported with a retry, so
// berthd keeps the lend counted and asks again later. The ledger keeps the
// ticket: it is still open.
func TestTakeBackRefusedReportsARetry(t *testing.T) {
	stub := &lendStub{byPath: map[string]lendResp{
		"/vram/lend":   {payload: `{"id":"17","miB":4096,"gpu":"0000:09:00.0"}`},
		"/vram/return": {status: http.StatusConflict, payload: `{"error":{"message":"the lent memory is still in use"}}`},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := lendEntry(srv.URL)
	line, daemon := daemonPipe(t)
	if got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"}); got.Op != proto.OpLent {
		t.Fatalf("the lend answered op=%s, want lent", got.Op)
	}
	got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpReturn, LendID: "17"})

	if got.Op != proto.OpReturnFailed || !got.Retry || got.LendID != "17" {
		t.Fatalf("the runner answered op=%s retry=%v ticket=%q, want return_failed retry=true ticket=17",
			got.Op, got.Retry, got.LendID)
	}
	if got.Reason != "the lent memory is still in use" {
		t.Fatalf("the refusal reads %q, want the engine's reason", got.Reason)
	}
	path, auth, body := stub.saw()
	if path != "/vram/return" || auth != "Bearer sk-test" || body != `{"id":"17"}` {
		t.Fatalf("the engine saw %s with %q and %s, want /vram/return with the run's key and the ticket", path, auth, body)
	}
	if tk, ok := line.openLend(); !ok || tk.ID != "17" {
		t.Fatalf("the ledger dropped a refused take-back: %+v (open: %v)", line.lends, ok)
	}
}

// A run whose entry has no lend calls says so instead of pretending, and never
// touches the engine: the broker falls back to the drain ladder.
func TestLendAskOnARunWithoutLendCallsIsRefused(t *testing.T) {
	o := OptionsFor(&config.Engine{Type: "vllm", Name: "maya", URL: "http://127.0.0.1:1"}, Over{})
	got, _ := askOn(t, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048})

	if got.Op != proto.OpLendFailed || got.Kind != "Unsupported" {
		t.Fatalf("the runner answered op=%s kind=%q, want lend_failed kind=Unsupported", got.Op, got.Kind)
	}
}

// The re-attach message carries the run's one open lend: the ticket and its
// bytes, with the lease's needs untouched, and nothing at all when the run
// has lent nothing.
func TestReattachNamesTheOpenLend(t *testing.T) {
	o := lendEntry("http://127.0.0.1:1")
	o.Needs = []uint64{16 << 30}

	line := newLine(nil)
	line.record(lend.Ticket{ID: "17", GPU: "0000:09:00.0", MiB: 4096})
	m := reattachMsg(o, 101, 7, &containerRef{}, false, line)
	if m.Op != proto.OpReattach || m.Needs != 16<<30 {
		t.Fatalf("reattach msg = op %s needs %d, want reattach with the lease's 16 GiB", m.Op, m.Needs)
	}
	if m.LendID != "17" || m.Lent != 4<<30 {
		t.Fatalf("reattach msg names ticket %q of %d bytes, want ticket 17 of %d bytes", m.LendID, m.Lent, int64(4)<<30)
	}

	fresh := newLine(nil)
	m2 := reattachMsg(o, 101, 7, &containerRef{}, false, fresh)
	if m2.LendID != "" || m2.Lent != 0 {
		t.Fatalf("a run that lent nothing re-attaches with ticket %q of %d bytes, want none", m2.LendID, m2.Lent)
	}
}

// A wrapper that lost its own memory of the lend adopts what the engine still
// lists before re-attaching, so the new daemon counts the holder's budget less
// the lend instead of blocking waiters behind a full budget.
func TestReconnectAdoptsALendTheEngineStillLists(t *testing.T) {
	stub := &lendStub{byPath: map[string]lendResp{
		"/vram/state": {payload: `{"leases":[{"id":"9","gpu":"0000:09:00.0","mib":2048}]}`},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := lendEntry(srv.URL)
	line := newLine(nil)
	line.adoptLends(o, func(format string, args ...any) { t.Logf(format, args...) })

	tk, ok := line.openLend()
	if !ok || tk.ID != "9" || tk.MiB != 2048 {
		t.Fatalf("the ledger = %+v (open: %v), want the engine's ticket 9 of 2048 MiB", tk, ok)
	}
}

// A lend the engine answered 503 counts as refused for the waiter, but the
// engine may still lend it: when its ledger lists a ticket the run does not
// hold, the runner takes it straight back, and nothing was granted on it so
// the take-back overlaps no one.
func TestPendingLendIsTakenBackWhenItLands(t *testing.T) {
	stub := &lendStub{byPath: map[string]lendResp{
		"/vram/lend":   {status: http.StatusServiceUnavailable, payload: `{"error":"the engine did not answer LEND in 30 s"}`},
		"/vram/state":  {payload: `{"leases":[{"id":"9","gpu":"0000:09:00.0","mib":2048}]}`},
		"/vram/return": {payload: `{"id":"9","gpu":"0000:09:00.0","mib":2048}`},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := lendEntry(srv.URL)
	line, daemon := daemonPipe(t)
	line.lendEvery, line.lendWindow = time.Millisecond, 200*time.Millisecond
	got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"})
	if got.Op != proto.OpLendFailed || got.Kind != "Pending" {
		t.Fatalf("the runner answered op=%s kind=%q, want lend_failed kind=Pending", got.Op, got.Kind)
	}

	waitForCall(t, stub, `/vram/return Bearer sk-test {"id":"9"}`)
	if _, ok := line.openLend(); ok {
		t.Fatalf("the ledger holds %+v; a returned lend is not the run's to re-attach", line.lends)
	}
	// The watch reads until its window ends; outlast it here so the engine
	// closes after the run's last read.
	time.Sleep(line.lendWindow + 20*time.Millisecond)
}

// The 503 poll hands back only tickets the run did not already hold: a ticket
// it knows is a lend berthd is already watching, and the poll returns nothing.
func TestPendingLendWithNothingNewReturnsNothing(t *testing.T) {
	stub := &lendStub{byPath: map[string]lendResp{
		"/vram/lend":  {status: http.StatusServiceUnavailable, payload: `{"error":"the engine did not answer LEND in 30 s"}`},
		"/vram/state": {payload: `{"leases":[{"id":"17","gpu":"0000:09:00.0","mib":4096}]}`},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	o := lendEntry(srv.URL)
	line, daemon := daemonPipe(t)
	line.lendEvery, line.lendWindow = time.Millisecond, 30*time.Millisecond
	line.record(lend.Ticket{ID: "17", GPU: "0000:09:00.0", MiB: 4096})
	got := askStep(t, line, daemon, o, proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"})
	if got.Op != proto.OpLendFailed || got.Kind != "Pending" {
		t.Fatalf("the runner answered op=%s kind=%q, want lend_failed kind=Pending", got.Op, got.Kind)
	}

	// The poll ran (the engine was read), the window passed, and no return
	// went out: ticket 17 was the run's own before the call.
	waitForCall(t, stub, "/vram/state")
	deadline := time.Now().Add(line.lendWindow + 200*time.Millisecond)
	for time.Now().Before(deadline) {
		for _, c := range stub.calls() {
			if strings.HasPrefix(c, "/vram/return") {
				t.Fatalf("the 503 poll returned a ticket the run already held: %s", c)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := line.openLend(); !ok {
		t.Fatal("the poll dropped the run's own ticket from the ledger")
	}
}

// A new lend ends the 503 watch before it calls the engine. Otherwise the
// watch could read the engine's ledger after the engine listed the new
// ticket and before the run recorded it, and take back a lend berthd granted
// a borrower on (rule 2).
func TestNewLendEndsThePendingWatch(t *testing.T) {
	var mu sync.Mutex
	var lends int
	var returns []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vram/lend":
			mu.Lock()
			lends++
			n := lends
			mu.Unlock()
			if n == 1 {
				http.Error(w, `{"error":"the engine did not answer LEND in 30 s"}`, http.StatusServiceUnavailable)
				return
			}
			time.Sleep(100 * time.Millisecond)
			_, _ = io.WriteString(w, `{"id":"21","gpu":"0000:09:00.0","mib":4096}`)
		case "/vram/state":
			// The engine lists ticket 21 from the moment the second lend
			// reaches it, before its answer is back at the run.
			mu.Lock()
			second := lends >= 2
			mu.Unlock()
			if second {
				_, _ = io.WriteString(w, `{"leases":[{"id":"21","gpu":"0000:09:00.0","mib":4096}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"leases":[]}`)
		case "/vram/return":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			returns = append(returns, string(b))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"21"}`)
		}
	}))
	defer srv.Close()

	o := lendEntry(srv.URL)
	line, daemon := daemonPipe(t)
	line.lendEvery, line.lendWindow = time.Millisecond, time.Second
	ask := proto.Msg{Op: proto.OpLend, Needs: 4 << 30, Free: 2048, GPU: "0000:09:00.0"}
	if got := askStep(t, line, daemon, o, ask); got.Op != proto.OpLendFailed || got.Kind != "Pending" {
		t.Fatalf("first answer op=%s kind=%q, want lend_failed Pending", got.Op, got.Kind)
	}
	if got := askStep(t, line, daemon, o, ask); got.Op != proto.OpLent || got.LendID != "21" {
		t.Fatalf("second answer op=%s id=%q, want lent 21", got.Op, got.LendID)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(returns) != 0 {
		t.Fatalf("the 503 watch took back the new lend: %v", returns)
	}
}
