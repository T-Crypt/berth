// Package gateway is `berth gateway`: one local port in front of every
// engine on the node. A request names a model; the gateway finds the engine
// that serves it, starts that engine under a lease if it is not running
// (draining the holder like any other request), waits until it answers, and
// proxies the request to it, streaming. Engines keep their own ports, and
// stay leased when called directly.
//
// The gateway can load models, so every request must carry the node's API
// key as a bearer token, name a loopback Host (DNS rebinding) and come from no
// browser origin (a page cannot forge the key, and is refused before it is
// even asked for).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/apikey"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
)

type Gateway struct {
	Socket  string          // the node's berthd socket
	Engines []config.Engine // configured engines; those with Start can be started on demand
	Ready   time.Duration   // how long a start may wait for a berth and then to answer; 0 = 10 min
	Key     string          // the node's API key; required on every request
	Log     *slog.Logger
	Client  *http.Client
	// HeadroomFloor is the RAM kept for the rest of the machine on a cold
	// start; 0 = defaultHeadroomFloor.
	HeadroomFloor uint64
	// MemAvailable is RAM available in bytes plus whether the box reports
	// it; nil reads the live machine. Tests inject a fake here.
	MemAvailable func() (avail uint64, ok bool)

	mu      sync.Mutex
	running map[string]*started // engines this gateway started, by name
	lists   map[string]cached   // engine URL -> its /v1/models, briefly cached
}

type started struct {
	exited chan struct{}
	code   int
	stderr *lockedBuf
}

// defaultHeadroomFloor is the RAM the gateway keeps for the rest of the
// machine when an engine cold-starts: a start that would leave less than
// this available is refused instead of risking system-oomd. Tunable per
// gateway via HeadroomFloor.
const defaultHeadroomFloor = 2 << 30

// guardDecision is what the cold-start headroom guard decided, and why.
// The handler only maps allow/deny to HTTP; the reason lives here.
type guardDecision struct {
	allow bool
	code  string // set on deny, e.g. "no_headroom"
	msg   string
	avail uint64 // RAM available when decided
	need  uint64 // engine estimate plus the floor
}

// decideGuard is pure: deny when starting an engine estimated at estimate
// would leave less than floor available. A zero estimate (an engine with
// no configured footprint) allows: refusing on unknown data strands users.
func decideGuard(avail, floor, estimate uint64) guardDecision {
	if estimate == 0 {
		return guardDecision{allow: true, avail: avail}
	}
	need := estimate + floor
	if avail < need {
		return guardDecision{
			allow: false,
			code:  "no_headroom",
			msg: fmt.Sprintf("only %s RAM available, the engine needs %s plus a %s floor",
				server.MiB(avail), server.MiB(estimate), server.MiB(floor)),
			avail: avail,
			need:  need,
		}
	}
	return guardDecision{allow: true, avail: avail, need: need}
}

type cached struct {
	at     time.Time
	models []string
}

func (g *Gateway) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (g *Gateway) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

// Handler serves the OpenAI-compatible surface.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.models)
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		mux.HandleFunc("POST "+p, g.forward)
	}
	return g.guard(mux)
}

// guard admits a request only with the key, a loopback Host and no Origin.
// OpenAI clients send no Origin; browsers always do on a cross-site POST.
func (g *Gateway) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !loopbackHost(r.Host):
			apiError(w, http.StatusForbidden, "call the gateway at 127.0.0.1 or localhost")
		case r.Header.Get("Origin") != "":
			apiError(w, http.StatusForbidden, "the gateway does not answer browser pages")
		case !apikey.Bearer(r.Header.Get("Authorization"), g.Key):
			w.Header().Set("WWW-Authenticate", `Bearer realm="berth"`)
			apiError(w, http.StatusUnauthorized, "send the node's API key as a bearer token (`berth key` prints it)")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// target is where a model's requests go, and how to start it if it is down.
type target struct {
	url    string
	engine *config.Engine // set when the gateway may start it
	live   bool           // an engine already answering lists the model
}

// live is every engine the node reads that is up, configured or discovered.
func (g *Gateway) live() []proto.Engine {
	conn, raw, err := server.Dial(g.Socket)
	if err != nil {
		return nil
	}
	defer raw.Close()
	if conn.Send(proto.Msg{Op: proto.OpStatus}) != nil {
		return nil
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		return nil
	}
	return m.Status.Engines
}

// listed is what an engine's own /v1/models says it can serve. llama-swap and
// Ollama list every model they can load, not only the loaded ones, which is
// what lets them answer for a cold model themselves.
func (g *Gateway) listed(ctx context.Context, base string) []string {
	g.mu.Lock()
	if c, ok := g.lists[base]; ok && time.Since(c.at) < 30*time.Second {
		g.mu.Unlock()
		return c.models
	}
	g.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return nil
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil
	}
	out := make([]string, 0, len(r.Data))
	for _, d := range r.Data {
		out = append(out, d.ID)
	}
	g.mu.Lock()
	if g.lists == nil {
		g.lists = map[string]cached{}
	}
	g.lists[base] = cached{time.Now(), out}
	g.mu.Unlock()
	return out
}

// resolve finds the engine for model: an engine that is up and lists it
// first (it loads the model itself, under its own leases), else a configured
// engine that serves it and can be started.
func (g *Gateway) resolve(ctx context.Context, model string) (target, bool) {
	for _, e := range g.live() {
		if e.Up && slices.Contains(g.listed(ctx, e.URL), model) {
			return target{url: e.URL, live: true}, true
		}
	}
	for i := range g.Engines {
		e := &g.Engines[i]
		if slices.Contains(e.Serves, model) {
			return target{url: e.URL, engine: e}, true
		}
	}
	return target{}, false
}

func (g *Gateway) catalog(ctx context.Context) []string {
	seen := map[string]bool{}
	for _, e := range g.live() {
		if e.Up {
			for _, m := range g.listed(ctx, e.URL) {
				seen[m] = true
			}
		}
	}
	for _, e := range g.Engines {
		for _, m := range e.Serves {
			seen[m] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, m := range g.catalog(r.Context()) {
		out.Data = append(out.Data, model{ID: m, Object: "model", OwnedBy: "berth"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	apiErrorCode(w, code, "", msg)
}

// apiErrorCode is an OpenAI-shaped error with a reason code a client can
// branch on; a 503 also says when to try again.
func apiErrorCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	e := map[string]string{"message": msg, "type": "berth"}
	if code != "" {
		e["code"] = code
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}

// startError is why an engine could not be brought up for a request.
type startError struct {
	code string // no_start, outranked, drained_while_starting, no_berth, start_failed, not_ready, no_headroom
	msg  string
}

func (e *startError) Error() string { return e.msg }

// classify reads why a start ended before its engine answered from what the
// in-process berth run printed.
func classify(name string, exit int, stderr string) *startError {
	last := lastLine(stderr)
	switch {
	case strings.Contains(stderr, "juggle:"):
		return &startError{"outranked", fmt.Sprintf("%s lost a juggle for the card: %s", name, last)}
	case strings.Contains(stderr, "berth: draining"):
		return &startError{"drained_while_starting", fmt.Sprintf("%s was drained before it answered: %s", name, last)}
	case exit == runner.ExitRefused:
		return &startError{"no_berth", fmt.Sprintf("%s got no berth: %s", name, last)}
	}
	return &startError{"start_failed", fmt.Sprintf("%s did not start (exit %d): %s", name, exit, last)}
}

// forward reads the model from the body, makes sure its engine is up (under
// a lease), then proxies the request unchanged.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		apiError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
		apiError(w, http.StatusBadRequest, "the request needs a model")
		return
	}
	t, ok := g.resolve(r.Context(), req.Model)
	if !ok {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine on this node serves %q (GET /v1/models lists what does)", req.Model))
		return
	}
	if !t.live {
		if err := g.ensure(r.Context(), t.engine); err != nil {
			code := ""
			var se *startError
			if errors.As(err, &se) {
				code = se.code
			}
			g.log().Warn("gateway start failed", "engine", t.engine.Name, "model", req.Model, "code", code, "err", err)
			apiErrorCode(w, http.StatusServiceUnavailable, code, err.Error())
			return
		}
	}
	u, err := url.Parse(t.url)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "engine url: "+err.Error())
		return
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1 // stream tokens as the engine sends them
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Host = u.Host
	p.ServeHTTP(w, r)
}

// ensure starts e under a lease unless this gateway already has, then waits
// until it answers. A start the broker refuses comes back as the reason.
func (g *Gateway) ensure(ctx context.Context, e *config.Engine) error {
	if g.answers(ctx, e.URL) {
		return nil // already up (started by hand or by its own manager)
	}
	if len(e.Start) == 0 {
		return &startError{"no_start", fmt.Sprintf("%s is not answering at %s and has no start command in berth.toml", e.Name, e.URL)}
	}
	if err := g.checkHeadroom(e); err != nil {
		g.log().Warn("gateway start refused", "engine", e.Name, "err", err)
		return err
	}
	ready := g.ready()
	g.mu.Lock()
	if g.running == nil {
		g.running = map[string]*started{}
	}
	s, ok := g.running[e.Name]
	if !ok {
		s = &started{exited: make(chan struct{}), stderr: &lockedBuf{}}
		g.running[e.Name] = s
		go g.run(e, s, ready)
	}
	g.mu.Unlock()

	deadline := time.Now().Add(ready)
	for {
		if g.answers(ctx, e.URL) {
			return nil
		}
		select {
		case <-s.exited:
			return classify(e.Name, s.code, s.stderr.String())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return &startError{"not_ready", fmt.Sprintf("%s did not answer within %s", e.Name, ready)}
		}
	}
}

func (g *Gateway) ready() time.Duration {
	if g.Ready > 0 {
		return g.Ready
	}
	return 10 * time.Minute
}

// checkHeadroom refuses a cold start that would leave the machine without
// its RAM floor. It never caps, throttles or nices an engine: it only
// refuses to start one. Unknown footprints (no estimate) and boxes that do
// not report memory fail open toward starting.
func (g *Gateway) checkHeadroom(e *config.Engine) error {
	estimate := e.NeedsRAMMiB << 20
	if estimate == 0 {
		return nil
	}
	avail, ok := g.memAvailable()
	if !ok {
		return nil
	}
	d := decideGuard(avail, g.headroomFloor(), estimate)
	if !d.allow {
		return &startError{code: d.code, msg: d.msg}
	}
	return nil
}

func (g *Gateway) headroomFloor() uint64 {
	if g.HeadroomFloor > 0 {
		return g.HeadroomFloor
	}
	return defaultHeadroomFloor
}

func (g *Gateway) memAvailable() (uint64, bool) {
	if g.MemAvailable != nil {
		return g.MemAvailable()
	}
	s := new(host.Reader).Read()
	if !s.Supported {
		return 0, false
	}
	return s.RAMTotal - s.RAMUsed, true
}

// run is one `berth run` of e, in process: it waits for a berth, starts the
// engine, and hands the berth back when the engine exits or is drained. It
// waits for the berth no longer than the request that asked for it, so a start
// the client gave up on cannot fire hours later and take the card.
func (g *Gateway) run(e *config.Engine, s *started, wait time.Duration) {
	prio := e.Priority
	if prio == "" {
		prio = "interactive"
	}
	g.log().Info("gateway starting engine", "engine", e.Name, "priority", prio)
	s.code = runner.Run(runner.Options{
		Socket: g.Socket, Engine: e.Name, Priority: prio, Needs: e.NeedsMiB << 20,
		Wait: wait, DrainPost: e.DrainPost, DrainComfyUI: e.DrainComfyUI,
		Grace: 30 * time.Second, Argv: e.Start, Stderr: s.stderr,
	})
	g.log().Info("gateway engine exited", "engine", e.Name, "code", s.code)
	g.mu.Lock()
	delete(g.running, e.Name)
	g.mu.Unlock()
	close(s.exited)
}

// answers is true when the engine's model list responds.
func (g *Gateway) answers(ctx context.Context, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
