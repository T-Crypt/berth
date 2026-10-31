// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/T-Crypt/berth/internal/apikey"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/swapcost"
)

type Gateway struct {
	Socket  string          // the node's berthd socket
	Engines []config.Engine // configured engines; those with Start can be started on demand
	Ready   time.Duration   // how long a start may wait for a berth and then to answer; 0 = 10 min
	Key     string          // the node's API key; required on every request
	// AllowHosts are extra Host names the loopback gateway answers to: the name a
	// same-machine reverse proxy (tailscale serve) forwards. The listener stays on
	// loopback and the key and Origin checks still apply.
	AllowHosts []string
	Log        *slog.Logger
	Client     *http.Client
	// HeadroomFloor is the RAM kept for the rest of the machine on a cold
	// start; 0 = DefaultHeadroomFloor.
	HeadroomFloor uint64
	// MemAvailable is RAM available in bytes plus whether the box reports
	// it; nil reads the live machine. Tests inject a fake here.
	MemAvailable func() (avail uint64, ok bool)
	// Prefetch asks the kernel to read one file into the page cache while a
	// start waits for the card; nil uses pagecache.Prefetch. Tests inject a
	// fake here.
	Prefetch func(path string) error
	// StateFile records the engines this gateway started as their own
	// processes, so the next gateway adopts the ones still running
	// instead of starting them again. Empty disables the record.
	StateFile string
	// DeferredFile keeps the ComfyUI jobs queued while the card was out of
	// reach (on_busy = "queue"), so they survive a gateway restart. Empty
	// keeps them in memory only.
	DeferredFile string
	// CostFile is where the gateway's book of measured switch times
	// lives, so the times survive a restart of the gateway. Empty keeps
	// the book in memory only.
	CostFile string
	// ConfigPath is the berth.toml the spawned runs read their entry's
	// park settings from (the park hook is not a flag).
	ConfigPath string
	// LogMax caps an engine's log file before it starts over; 0 = 64 MiB.
	LogMax int64

	mu         sync.Mutex
	deferred   *deferredBook       // loaded on first use from DeferredFile
	submitting atomic.Bool         // a submit of the queued ComfyUI jobs is running
	running    map[string]*started // engines this gateway started, by name
	failed     map[string]*failure // engines whose last start died before answering
	lists      map[string]cached   // engine URL -> its /v1/models, briefly cached
	wakes      map[string]*wake    // the wake page's last start, by engine name

	adoptOnce sync.Once
	stateMu   sync.Mutex
	records   map[string]engineRec // the run processes still going, by engine name

	logOnce sync.Once
	logDir  string
	logErr  error // one engineLogs() decision per gateway

	costOnce sync.Once
	costs    *swapcost.Book
	costMu   sync.Mutex // one book access at a time: Record and Save together, Estimate undivided
}

type started struct {
	exited   chan struct{}
	code     int
	stderr   *lockedBuf // berth run's own lines: granted, waiting, draining
	output   *tailBuf   // the tail of the engine's own stdout and stderr
	answered bool       // the engine answered at least once (under g.mu)
	why      *startError

	// The switch cost of a start this gateway ran: the engine's position
	// when the start was decided, when its run printed its grant, and
	// whether the sample went into the book. All three under g.mu; pos
	// is set before the run spawns, so a grant the follower sees always
	// has somewhere to land.
	pos      *position
	grantAt  time.Time
	recorded bool
}

// failure is an engine whose start died before it answered. A start that
// fails the same way every time (a bad config, a card too full for it) is
// not run again on every client retry: for a while the gateway answers with
// the cause at once instead of paying another cold start to learn it again.
type failure struct {
	err   *startError
	n     int       // failed starts in a row
	until time.Time // no new start before this
}

// startCooldown is the pause after the first failed start; each failure in
// a row doubles it, up to maxStartCooldown. A start that answers clears it.
const (
	startCooldown    = 15 * time.Second
	maxStartCooldown = 5 * time.Minute
)

func cooldown(n int) time.Duration {
	d := startCooldown
	for i := 1; i < n && d < maxStartCooldown; i++ {
		d *= 2
	}
	return min(d, maxStartCooldown)
}

// DefaultHeadroomFloor is the RAM kept for the rest of the machine on a
// cold start, by the gateway's guard and by the park admission alike: a
// start that would leave less than this available is refused instead of
// risking system-oomd. Tunable per gateway via HeadroomFloor.
const DefaultHeadroomFloor = 2 << 30

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

// Handler serves the OpenAI-compatible surface plus the ComfyUI
// passthrough: /prompt queues a workflow (starting ComfyUI under a lease
// when it is down), while /ws /queue /history /view /upload proxy to a
// running ComfyUI.
func (g *Gateway) Handler() http.Handler {
	g.adoptOnce.Do(g.adopt)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.models)
	mux.HandleFunc("GET /cards/{card}/v1/models", g.onCard(g.models))
	mux.HandleFunc("GET /v1/berth/wait", g.wait)
	mux.HandleFunc("GET /cards/{card}/v1/berth/wait", g.onCard(g.wait))
	mux.HandleFunc("GET /v1/berth/cost", g.cost)
	mux.HandleFunc("POST /v1/berth/plan", g.plan)
	mux.HandleFunc("GET /v1/berth/board", g.board)
	mux.HandleFunc("POST /v1/berth/switch", g.apiSwitch)
	mux.HandleFunc("POST /v1/berth/stop", g.apiStop)
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		mux.HandleFunc("POST "+p, g.forward)
		mux.HandleFunc("POST /cards/{card}"+p, g.onCard(g.forward))
	}
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) { g.comfyPrompt(w, r, false) })
	mux.HandleFunc("/ws", g.comfyProxy)
	mux.HandleFunc("/queue", g.comfyProxy)
	mux.HandleFunc("/history/", g.comfyProxy)
	mux.HandleFunc("/view", g.comfyProxy)
	mux.HandleFunc("/upload/", g.comfyProxy)
	return g.guard(mux)
}

// guard admits a request only with the key, a loopback Host and no Origin.
// OpenAI clients send no Origin; browsers always do on a cross-site POST.
func (g *Gateway) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !loopbackHost(r.Host) && !g.allowedHost(r.Host):
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

// allowedHost reports whether host, port stripped, is one of AllowHosts.
func (g *Gateway) allowedHost(host string) bool {
	name := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		name = hh
	}
	for _, a := range g.AllowHosts {
		if a != "" && strings.EqualFold(name, a) {
			return true
		}
	}
	return false
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

// engineEntry is the configured entry behind a target: the one the
// gateway may start it when that is set, otherwise the entry whose url
// is the target's (an already-running configured engine). A discovered
// engine has no entry.
func (g *Gateway) engineEntry(t target) *config.Engine {
	if t.engine != nil {
		return t.engine
	}
	want := strings.TrimRight(t.url, "/")
	for i := range g.Engines {
		if strings.TrimRight(g.Engines[i].URL, "/") == want {
			return &g.Engines[i]
		}
	}
	return nil
}

// engineAuth is the Authorization a proxied request to the engine
// carries: the engine's own key when it has one, none otherwise. The
// client's node key is a credential for this gateway and goes no
// further: the engine, its logs and any proxy in front of it would see
// it. A credential that will not resolve sends nothing and logs one
// WARN line naming the engine and the error, never the key.
func (g *Gateway) engineAuth(entry *config.Engine, r *http.Request) {
	r.Header.Del("Authorization")
	if entry == nil {
		return
	}
	key, err := entry.APIKey()
	if err != nil {
		g.log().Warn("engine key not resolved", "engine", entry.Name, "err", err)
		return
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
}

// live is every engine the node reads that is up, configured or discovered.
func (g *Gateway) live() []proto.Engine {
	if st := g.nodeStatus(); st != nil {
		return st.Engines
	}
	return nil
}

// nodeStatus is berthd's whole view, or nil when it does not answer.
func (g *Gateway) nodeStatus() *proto.Status {
	conn, raw, err := server.Dial(g.Socket)
	if err != nil {
		return nil
	}
	defer raw.Close()
	if conn.Send(proto.Msg{Op: proto.OpStatus}) != nil {
		return nil
	}
	m, err := conn.Recv()
	if err != nil {
		return nil
	}
	return m.Status
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

// onCard serves a /cards/{card}/v1/... route: the card is named by its
// name or PCI address, the path the engine sees loses the prefix, and the
// handler finds the card's name under PathValue("card").
func (g *Gateway) onCard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("card")
		st := g.nodeStatus()
		if st == nil {
			apiErrorCode(w, http.StatusServiceUnavailable, "no_berth", "berthd is not answering, so the gateway cannot tell which card "+key+" is")
			return
		}
		cards := node.CardsOf(st, key)
		if len(cards) != 1 {
			var names []string
			for _, c := range st.GPUs {
				if c.Scheduled {
					names = append(names, c.Card)
				}
			}
			apiError(w, http.StatusNotFound, fmt.Sprintf("no card %s on this node (cards: %s)", key, strings.Join(names, ", ")))
			return
		}
		r.SetPathValue("card", cards[0])
		r.URL.Path = r.URL.Path[strings.Index(r.URL.Path, "/v1/"):]
		r.URL.RawPath = ""
		h(w, r)
	}
}

// engineOn is whether the engine runs on the card named card; "" (no card
// asked) takes every engine. An engine berthd sees that has no entry in
// berth.toml is on no card the gateway knows.
func (g *Gateway) engineOn(st *proto.Status, name, card string) bool {
	if card == "" {
		return true
	}
	e := g.engineByName(name)
	return e != nil && st != nil && slices.Contains(node.CardsOf(st, e.GPU), card)
}

// resolve finds the engine for model, on card when one is named: an engine
// that is up and lists it first (it loads the model itself, under its own
// leases), else a configured engine that serves it and can be started.
func (g *Gateway) resolve(ctx context.Context, model, card string) (target, bool) {
	st := g.nodeStatus()
	for _, e := range g.live() {
		// A parked engine answers its list with the weights in RAM; the
		// resume, not the list, says when it may be proxied.
		if e.Up && g.engineOn(st, e.Name, card) && !parkedIn(st, e.Name) && slices.Contains(g.listed(ctx, e.URL), model) {
			return target{url: e.URL, live: true}, true
		}
	}
	var serving []*config.Engine
	for i := range g.Engines {
		if e := &g.Engines[i]; g.engineOn(st, e.Name, card) && slices.Contains(e.Serves, model) {
			serving = append(serving, e)
		}
	}
	if len(serving) == 0 {
		return target{}, false
	}
	// One engine per card serves the model: the cheapest card to reach
	// wins, and berth.toml's order breaks a tie.
	e := slices.MinFunc(serving, func(a, b *config.Engine) int { return reach(st, a) - reach(st, b) })
	return target{url: e.URL, engine: e}, true
}

// reach ranks what it takes to bring e up on its card, cheapest first: a
// resume from RAM, a free card, a drain of the holder, then a wait for a
// card a VM or a pinned lease (a game, a hold) keeps.
func reach(st *proto.Status, e *config.Engine) int {
	v := node.View(st, e.GPU)
	switch {
	case parkedIn(st, e.Name):
		return 0
	case cardFree(v):
		return 1
	case v != nil && outOfReach(v) == "":
		return 2
	default:
		return 3
	}
}

// notServed is the 404 for a model no engine serves, on card when one is
// named.
func notServed(w http.ResponseWriter, model, card string) {
	if card == "" {
		apiError(w, http.StatusNotFound, fmt.Sprintf("no engine on this node serves %q (GET /v1/models lists what does)", model))
		return
	}
	apiError(w, http.StatusNotFound, fmt.Sprintf("no engine on %s serves %q (GET /cards/%s/v1/models lists what does)", card, model, card))
}

func (g *Gateway) catalog(ctx context.Context, card string) []string {
	st := g.nodeStatus()
	seen := map[string]bool{}
	for _, e := range g.live() {
		if e.Up && g.engineOn(st, e.Name, card) {
			for _, m := range g.listed(ctx, e.URL) {
				seen[m] = true
			}
		}
	}
	for _, e := range g.Engines {
		if !g.engineOn(st, e.Name, card) {
			continue
		}
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
	for _, m := range g.catalog(r.Context(), r.PathValue("card")) {
		out.Data = append(out.Data, model{ID: m, Object: "model", OwnedBy: "berth"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	apiErrorCode(w, code, "", msg)
}

// apiErrorCode is an OpenAI-shaped error with a reason code a client can
// branch on; a 503 also says when to try again (30 s unless the caller
// already set a time it knows).
func apiErrorCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "30")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(berthError(code, msg))
}

// berthError is the OpenAI-shaped error body, with the reason code when
// there is one. The JSON path encodes it; the streaming path sends it as
// one SSE event.
func berthError(code, msg string) map[string]any {
	e := map[string]any{"message": msg, "type": "berth"}
	if code != "" {
		e["code"] = code
	}
	return map[string]any{"error": e}
}

// startError is why an engine could not be brought up for a request.
type startError struct {
	code  string // no_start, outranked, drained_while_starting, no_berth, start_failed, engine_oom, not_ready, no_headroom
	msg   string
	retry time.Duration // when set, the client is told to try again after this (Retry-After)
}

func (e *startError) Error() string { return e.msg }

// classify reads why a start ended before its engine answered: the lease
// outcomes from what the in-process berth run printed (stderr), a crash from
// the engine's own output, where the cause is.
func classify(name string, exit int, stderr, output string) *startError {
	last := lastLine(stderr)
	switch {
	case strings.Contains(stderr, "juggle:"):
		return &startError{code: "outranked", msg: fmt.Sprintf("%s lost a juggle for the card: %s", name, last)}
	case strings.Contains(stderr, "berth: draining"):
		return &startError{code: "drained_while_starting", msg: fmt.Sprintf("%s was drained before it answered: %s", name, last)}
	case exit == runner.ExitRefused:
		return &startError{code: "no_berth", msg: fmt.Sprintf("%s got no berth: %s", name, last)}
	}
	cause := causeLine(output)
	if cause == "" {
		cause = last
	}
	if oomLine.MatchString(output) {
		return &startError{code: "engine_oom", msg: fmt.Sprintf("%s ran out of GPU memory while starting (exit %d): %s", name, exit, cause)}
	}
	return &startError{code: "start_failed", msg: fmt.Sprintf("%s did not start (exit %d): %s", name, exit, cause)}
}

// errLine marks a line of engine output that reads as a failure; oomLine one
// that names a GPU allocation failure, in the words the common engines use
// (CUDA and PyTorch, cuBLAS handle creation, llama.cpp, Vulkan, ROCm).
var (
	errLine = regexp.MustCompile(`(?i)error|fail|fatal|panic|exception|traceback|abort|denied|cannot|unable|no such|not found|out of memory|status [1-9]`)
	oomLine = regexp.MustCompile(`(?i)out of memory|CUDA_ERROR_OUT_OF_MEMORY|cudaErrorMemoryAllocation|OutOfMemoryError|CUBLAS_STATUS_ALLOC_FAILED|cublasCreate\S*:? cuBLAS status [13]\b|failed to allocate .*buffer|ErrorOutOfDeviceMemory|hipErrorOutOfMemory`)
)

// causeLine is the last line of the engine's output that reads as a
// failure, else its last line; empty when it printed nothing. Long lines
// are cut so an error body stays readable.
func causeLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	pick := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if pick == "" {
			pick = l
		}
		if errLine.MatchString(l) {
			pick = l
			break
		}
	}
	if len(pick) > 300 {
		pick = pick[:300] + "..."
	}
	return pick
}

// forward reads the model from the body, makes sure its engine is up (under
// a lease), then proxies the request unchanged. A streaming request whose
// engine is down gets the SSE preamble first, so the wait for the cold
// start carries keep-alive comments and holds clients with idle timeouts.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		apiError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
		apiError(w, http.StatusBadRequest, "the request needs a model")
		return
	}
	t, ok := g.resolve(r.Context(), req.Model, r.PathValue("card"))
	if !ok {
		notServed(w, req.Model, r.PathValue("card"))
		return
	}
	preamble := false
	var fb *config.Engine
	var waited time.Duration
	if !t.live {
		if req.Stream {
			preamble = true
			ssePreamble(w)
			var err error
			fb, waited, err = g.waitPrimary(r.Context(), t.engine, w)
			if err != nil {
				g.sseFailure(w, t.engine.Name, req.Model, err)
				return
			}
		} else {
			var err error
			fb, waited, err = g.waitPrimary(r.Context(), t.engine, nil)
			if err != nil {
				g.startFailed(w, t.engine.Name, req.Model, err)
				return
			}
		}
	}
	base := t.url
	// The request ends at the fallback when it switched to it, else the
	// engine that serves the model. Either way it carries that engine's
	// own key when it has one: the client's node key is a credential for
	// this gateway and goes no further, a fallback included.
	if fb != nil {
		base, body = g.tookFallback(w, preamble, t.engine, fb, waited, body)
		g.engineAuth(fb, r)
	} else {
		g.engineAuth(g.engineEntry(t), r)
	}
	u, err := url.Parse(base)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "engine url: "+err.Error())
		return
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1 // stream tokens as the engine sends them
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Host = u.Host
	if preamble {
		p.ServeHTTP(bodyOnly{w}, r) // the SSE preamble owns the headers
		return
	}
	p.ServeHTTP(w, r)
}

// keepAliveEvery is how often a streaming request that waits for its
// engine gets an SSE comment. A var so tests need not wait 10 s.
var keepAliveEvery = 10 * time.Second

// ssePreamble sends a streaming response's headers before the engine is
// up, so the keep-alive comments can start while it loads.
func ssePreamble(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
}

// sseFailure answers a streaming request whose engine would not come up:
// the same OpenAI-shaped error the 503 path carries, as one SSE event.
func (g *Gateway) sseFailure(w http.ResponseWriter, engine, what string, err error) {
	code := ""
	var se *startError
	if errors.As(err, &se) {
		code = se.code
	}
	g.log().Warn("gateway start failed", "engine", engine, "request", what, "code", code, "err", err)
	b, _ := json.Marshal(berthError(code, err.Error()))
	fmt.Fprintf(w, "data: %s\ndata: [DONE]\n\n", b)
	_ = http.NewResponseController(w).Flush()
}

// bodyOnly is a ResponseWriter whose headers are already on the wire: the
// SSE preamble went out while the engine loaded. The engine's body streams
// through and header changes are dropped.
type bodyOnly struct{ http.ResponseWriter }

func (bodyOnly) WriteHeader(int)               {}
func (b bodyOnly) Flush()                      { _ = http.NewResponseController(b.ResponseWriter).Flush() }
func (b bodyOnly) Unwrap() http.ResponseWriter { return b.ResponseWriter }

// wait blocks until the engine for the model named in the query answers,
// for harnesses that pre-warm before their first real request. A failed
// start is the same 503 the request path answers with.
func (g *Gateway) wait(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		apiError(w, http.StatusBadRequest, "name the model to wait for: /v1/berth/wait?model=NAME")
		return
	}
	t, ok := g.resolve(r.Context(), model, r.PathValue("card"))
	if !ok {
		notServed(w, model, r.PathValue("card"))
		return
	}
	if !t.live {
		if err := g.ensure(r.Context(), t.engine, false); err != nil {
			g.startFailed(w, t.engine.Name, model, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"model": model, "ready": true})
}

// startFailed answers a request whose engine would not come up: a 503
// carrying the reason code a client can branch on.
func (g *Gateway) startFailed(w http.ResponseWriter, engine, what string, err error) {
	code := ""
	var se *startError
	if errors.As(err, &se) {
		code = se.code
	}
	g.log().Warn("gateway start failed", "engine", engine, "request", what, "code", code, "err", err)
	if se != nil && se.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((se.retry+time.Second-1)/time.Second)))
	}
	apiErrorCode(w, http.StatusServiceUnavailable, code, err.Error())
}

// comfyTarget finds ComfyUI: a live one first (it takes the prompt
// itself), else a configured one the gateway may start. A parked ComfyUI
// is not the live one: the prompt must resume it first.
func (g *Gateway) comfyTarget(ctx context.Context) (target, bool) {
	st := g.nodeStatus()
	for _, e := range g.live() {
		if e.Up && e.Type == "comfyui" && !parkedIn(st, e.Name) && g.comfyAnswers(ctx, e.URL) {
			return target{url: e.URL, live: true}, true
		}
	}
	for i := range g.Engines {
		e := &g.Engines[i]
		if e.Type == "comfyui" {
			return target{url: e.URL, engine: e}, true
		}
	}
	return target{}, false
}

// comfyAnswers is true when ComfyUI's queue endpoint responds. The
// /v1/models probe the chat path uses does not exist on ComfyUI.
func (g *Gateway) comfyAnswers(ctx context.Context, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/queue", nil)
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

// ComfyUIHandler is ComfyUI's own port behind Berth: the web UI, its API
// and websocket pass through unchanged, and a queued workflow (/prompt, or
// /api/prompt from the web UI) first wakes or starts ComfyUI under its
// lease as a person's switch, so a client here never meets a parked
// ComfyUI with no card. It takes no key: it serves what ComfyUI's own
// port served.
func (g *Gateway) ComfyUIHandler() http.Handler {
	g.adoptOnce.Do(g.adopt)
	prompt := func(w http.ResponseWriter, r *http.Request) { g.comfyPrompt(w, r, true) }
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", prompt)
	mux.HandleFunc("POST /api/prompt", prompt)
	mux.HandleFunc("/", g.comfyProxy)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keepHost{}, true)))
	})
}

// keepHost marks a request on ComfyUI's port: it reaches ComfyUI with the
// Host the browser sent, because ComfyUI refuses a request whose Origin
// does not match its Host.
type keepHost struct{}

// comfyPrompt queues a workflow: it makes sure ComfyUI is up under a lease
// (draining the holder like any other request, holding this request until
// the engine answers), then proxies the prompt unchanged. switched marks a
// person's switch, as ensure takes it.
func (g *Gateway) comfyPrompt(w http.ResponseWriter, r *http.Request, switched bool) {
	t, ok := g.comfyTarget(r.Context())
	if !ok {
		apiError(w, http.StatusNotFound, "no ComfyUI engine on this node (configure an engine of type comfyui)")
		return
	}
	if e := g.queueingComfy(); e != nil {
		if why := outOfReach(node.View(g.nodeStatus(), e.GPU)); why != "" {
			g.deferPrompt(w, r, e, why)
			return
		}
	}
	if !t.live {
		if err := g.ensure(r.Context(), t.engine, switched); err != nil {
			g.startFailed(w, t.engine.Name, "prompt", err)
			return
		}
	}
	g.comfyProxyTo(t, w, r)
}

// comfyProxy passes the rest of the ComfyUI surface (/ws, /history, /view,
// /upload, /queue) to the engine unchanged, streaming. /prompt is the entry
// point that starts a down engine; these answer 503 naming it instead, so
// a thumbnail fetch never cold-starts a 12 GB server by accident.
func (g *Gateway) comfyProxy(w http.ResponseWriter, r *http.Request) {
	if id, ok := strings.CutPrefix(r.URL.Path, "/history/"); ok && strings.HasPrefix(id, deferredPrefix) {
		g.deferredHistory(w, r, id)
		return
	}
	t, ok := g.comfyTarget(r.Context())
	if !ok {
		apiError(w, http.StatusNotFound, "no ComfyUI engine on this node (configure an engine of type comfyui)")
		return
	}
	if !t.live && !g.comfyAnswers(r.Context(), t.url) {
		apiError(w, http.StatusServiceUnavailable, "ComfyUI is not running; POST /prompt first to start it")
		return
	}
	g.comfyProxyTo(t, w, r)
}

func (g *Gateway) comfyProxyTo(t target, w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(t.url)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "engine url: "+err.Error())
		return
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.FlushInterval = -1 // stream progress as ComfyUI sends it
	g.engineAuth(g.engineEntry(t), r)
	if r.Context().Value(keepHost{}) == nil {
		r.Host = u.Host
	}
	p.ServeHTTP(w, r)
}

// ensure starts e under a lease unless this gateway already has, then waits
// until it answers. A start the broker refuses comes back as the reason.
// switched marks a person's switch, which wins a juggle.
func (g *Gateway) ensure(ctx context.Context, e *config.Engine, switched bool) error {
	// ComfyUI answers no /v1/models; its queue endpoint is the liveness
	// probe instead, so a cold start waits for the real server.
	probe := g.answers
	if e.Type == "comfyui" {
		probe = g.comfyAnswers
	}
	// A parked engine answers while the weights sit in RAM: the resume
	// goes before any probe, or the request is proxied to RAM.
	if parks(e) {
		switch err := g.resume(ctx, e, switched); {
		case err == nil:
			return nil
		case !errors.Is(err, errNotParked):
			return err
		}
	}
	if probe(ctx, e.URL) {
		return nil // already up (started by hand or by its own manager)
	}
	ready := g.ready()
	g.mu.Lock()
	if g.running == nil {
		g.running = map[string]*started{}
	}
	s, ok := g.running[e.Name]
	starting := false
	if !ok {
		if len(e.Start) == 0 {
			g.mu.Unlock()
			return &startError{code: "no_start", msg: fmt.Sprintf("%s is not answering at %s and has no start command in berth.toml", e.Name, e.URL)}
		}
		if err := g.checkHeadroom(e); err != nil {
			g.mu.Unlock()
			g.log().Warn("gateway start refused", "engine", e.Name, "err", err)
			return err
		}
		if f := g.failed[e.Name]; f != nil {
			if left := time.Until(f.until); left > 0 {
				g.mu.Unlock()
				return &startError{code: f.err.code, retry: left,
					msg: fmt.Sprintf("%s; %d start(s) in a row failed, the next is allowed in %s",
						f.err.msg, f.n, left.Round(time.Second))}
			}
		}
		s = &started{exited: make(chan struct{}), stderr: &lockedBuf{}, output: &tailBuf{}}
		g.running[e.Name] = s
		starting = true
	}
	g.mu.Unlock()
	if starting {
		// This gateway runs the start, so its switch gets measured. The
		// engine's position is read now, before its reads begin: the
		// prefetch below warms the page cache as soon as it can, and a
		// share taken after that would call an engine the start warmed
		// one that started warm.
		pos := g.readPosition(e)
		g.mu.Lock()
		s.pos = pos
		g.mu.Unlock()
		if len(e.Prefetch) > 0 {
			// The run takes the lease and the drain starts; the disk is
			// free while that happens, so the reads go first.
			go g.prefetch(e)
		}
		if e.KVSnapshots != "" {
			// The same wait warms the engine's saved prompt caches, so the
			// restore below reads the page cache where it can.
			go g.kvWarm(e, ready)
		}
		go g.run(e, s, ready, switched)
	} else if switched {
		// A start no person asked for is already queued, and may have
		// lost a juggle: the person's switch goes to its lease.
		g.switchQueued(e)
	}

	deadline := time.Now().Add(ready)
	for {
		if probe(ctx, e.URL) {
			g.markAnswered(e, s)
			if starting && e.KVSnapshots != "" {
				// Only the start this gateway ran restores: its slots are
				// empty, and the restore lands before the first request.
				g.kvRestore(e)
			}
			return nil
		}
		select {
		case <-s.exited:
			return s.why
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return &startError{code: "not_ready", msg: fmt.Sprintf("%s did not answer within %s", e.Name, ready)}
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
	return DefaultHeadroomFloor
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

// run starts e as its own `berth run` process: under the user manager's
// scope (or a new session), so a gateway restart or crash leaves the
// engine up. The process takes the lease and runs the drain ladder
// exactly as any other berth run; the gateway reads its output for the
// tail a failed start quotes, and records it, so the next gateway
// adopts the engine instead of starting it again. The start waits for
// the berth no longer than the request that asked for it, so a start
// the client gave up on cannot fire hours later and take the card.
func (g *Gateway) run(e *config.Engine, s *started, wait time.Duration, switched bool) {
	o := runner.OptionsFor(e, runner.Over{Socket: g.Socket, Group: e.Name, Wait: wait, Switched: switched})
	code, err := g.spawnRun(e.Name, s, o)
	if err != nil {
		g.log().Error("gateway could not start the engine process", "engine", e.Name, "err", err)
		s.why = &startError{code: "start_failed", msg: fmt.Sprintf("%s could not be started: %v", e.Name, err)}
	} else {
		s.code = code
		g.log().Info("gateway engine exited", "engine", e.Name, "code", code)
	}
	g.mu.Lock()
	delete(g.running, e.Name)
	if !s.answered {
		if s.why == nil {
			s.why = classify(e.Name, s.code, s.stderr.String(), s.output.String())
		}
		// Only a crash cools down: a lost juggle, a drain or a refused
		// lease is the broker's answer, and the next try may well win.
		if s.why.code == "start_failed" || s.why.code == "engine_oom" {
			if g.failed == nil {
				g.failed = map[string]*failure{}
			}
			f := g.failed[e.Name]
			if f == nil {
				f = &failure{}
				g.failed[e.Name] = f
			}
			f.n++
			f.err, f.until = s.why, time.Now().Add(cooldown(f.n))
			g.log().Warn("gateway start cooling down", "engine", e.Name, "failed_in_a_row", f.n,
				"next_start_in", cooldown(f.n), "cause", s.why.msg)
		}
	}
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

// tailBuf keeps the last tailMax bytes written to it. An engine can print
// for days (Strata logs every token batch); only the end says why it died.
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

const tailMax = 16 << 10

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - tailMax; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// ansiCSI matches a terminal escape sequence such as a color code.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// lastLine is the engine output's last line as a terminal would show it:
// color codes dropped, and a line redrawn with \r (a progress bar) read
// from its last redraw.
func lastLine(s string) string {
	s = strings.TrimSpace(ansiCSI.ReplaceAllString(s, ""))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	return s
}
