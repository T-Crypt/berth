// Package harbor is the web UI's server. At basement scale it runs on the
// same box as the node and reads it over the node's socket; the fleet phase
// adds remote nodes. In P2 it is read-only: no endpoint changes anything,
// so the browser is never a path to controlling the daemon.
package harbor

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/store"
)

//go:embed web
var webFS embed.FS

// The guard modes. The zero value means loopback, so test harbors built
// with no fields keep the full loopback guard.
const (
	ModeLoopback = "loopback"
	ModeLAN      = "lan"
)

type Harbor struct {
	Socket  string // the node's socket
	Version string
	Log     *slog.Logger
	Store   *store.Store // history persistence; nil = live node only
	Mode    string       // ModeLoopback (zero value) or ModeLAN, set by Serve
	// AllowHosts are extra Host names a loopback harbor answers to: the name a
	// same-machine reverse proxy (tailscale serve, caddy) forwards with. The
	// listener stays on loopback; only the proxy can reach it.
	AllowHosts []string
}

// Serve listens on addr. A loopback address keeps the harbor on this machine;
// a non-loopback address is the operator's explicit opt-in to expose it on
// the network, announced loudly at startup.
func (h *Harbor) Serve(ctx context.Context, addr string) error {
	mode, err := listenMode(addr)
	if err != nil {
		return err
	}
	h.Mode = mode
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if mode == ModeLAN {
		h.Log.Warn("harbor is exposed on the LAN", "addr", addr,
			"detail", "read-only, no authentication: anyone who can reach this address can read the node")
	}
	h.Log.Info("harbor up", "url", "http://"+addr+"/")
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// listenMode decides which guard the harbor runs in from the address the
// operator typed: a well-formed host:port is required; a loopback host keeps
// it on this machine, and any other well-formed address is the consent.
func listenMode(addr string) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return "", fmt.Errorf("%q is not a host:port address", addr)
	}
	if loopbackHost(addr) {
		return ModeLoopback, nil
	}
	return ModeLAN, nil
}

func (h *Harbor) Handler() http.Handler {
	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /v1/status", h.status)
	mux.HandleFunc("GET /v1/events", h.events)
	mux.HandleFunc("GET /v1/history", h.history)
	mux.HandleFunc("GET /v1/insights", h.insights)
	mux.HandleFunc("GET /v1/counters", h.counters)
	mux.HandleFunc("GET /v1/spans", h.spans)
	mux.HandleFunc("GET /v1/storage", h.storage)
	mux.HandleFunc("GET /v1/mode", h.mode)
	return h.guard(mux)
}

// guard is the browser boundary: GET only, and the page may load nothing
// from anywhere else. In loopback mode the Host must be a loopback name too
// (DNS rebinding); the Host check is the one thing a LAN-exposed harbor
// (ModeLAN, the operator's explicit opt-in) must drop, or no phone on the
// network could reach it at all. A loopback harbor behind a same-machine
// reverse proxy also accepts the names in AllowHosts, which the operator
// typed; a rebinding page cannot make the browser send one of those.
func (h *Harbor) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			apiError(w, http.StatusMethodNotAllowed, "read_only", "the harbor is read-only in this release")
			return
		}
		if h.Mode != ModeLAN && !loopbackHost(r.Host) && !h.allowedHost(r.Host) {
			apiError(w, http.StatusForbidden, "forbidden_host", "open the harbor at 127.0.0.1 or localhost")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mode tells the page which guard this harbor runs under. It is a harbor
// fact, stable for the process lifetime, so the page fetches it once: unlike
// /v1/status it does not 502 when the node is down, and the banner must not
// vanish on a node flap.
func (h *Harbor) mode(w http.ResponseWriter, _ *http.Request) {
	m := h.Mode
	if m == "" {
		m = ModeLoopback
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]string{"mode": m})
}

// allowedHost reports whether host, port stripped, is one of AllowHosts.
func (h *Harbor) allowedHost(host string) bool {
	name := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		name = hh
	}
	for _, a := range h.AllowHosts {
		if a != "" && strings.EqualFold(name, a) {
			return true
		}
	}
	return false
}

func loopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// apiError is the /v1 error envelope.
func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// nodeStatus is one status read off the node's socket.
func (h *Harbor) nodeStatus() (*proto.Status, error) {
	conn, raw, err := server.Dial(h.Socket)
	if err != nil {
		return nil, fmt.Errorf("berthd is not answering on %s", h.Socket)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
		return nil, err
	}
	m, err := conn.Recv()
	if err != nil || m.Status == nil {
		return nil, errors.New("no status from berthd")
	}
	return m.Status, nil
}

func (h *Harbor) status(w http.ResponseWriter, r *http.Request) {
	st, err := h.nodeStatus()
	if err != nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(struct {
		*proto.Status
		Harbor string `json:"harbor_version"`
	}{st, h.Version})
}

// counters is the collisions board over one window (5m, 15m, 1h, 24h, 7d):
// the node's live totals against the totals the store kept at the window's
// start. The node's own board (all time) is in /v1/status.
func (h *Harbor) counters(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		apiError(w, http.StatusServiceUnavailable, "no_store", "the harbor has no history store")
		return
	}
	name := r.URL.Query().Get("range")
	d, ok := store.ParseCounterRange(name)
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_range", "range must be 5m, 15m, 1h, 24h or 7d")
		return
	}
	st, err := h.nodeStatus()
	if err != nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", err.Error())
		return
	}
	cw, err := h.Store.CountersOver(name, d, time.Now(), st.Counters)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(cw)
}

// history is the graphs' data. The default (no range) and 1h read the node's
// in-memory window live off the socket; 6h and longer read the store, which
// keeps the same samples at one-minute resolution.
func (h *Harbor) history(w http.ResponseWriter, r *http.Request) {
	d := time.Hour
	if v := r.URL.Query().Get("range"); v != "" {
		var ok bool
		d, ok = store.ParseRange(v)
		if !ok {
			apiError(w, http.StatusBadRequest, "bad_range", "range must be 1h, 6h, 24h, 7d or 30d")
			return
		}
	}
	if d > time.Hour {
		if h.Store == nil {
			apiError(w, http.StatusServiceUnavailable, "no_store", "the harbor has no history store")
			return
		}
		hh, err := h.Store.History(d, time.Now())
		if err != nil {
			apiError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(hh)
		return
	}
	conn, raw, err := server.Dial(h.Socket)
	if err != nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", fmt.Sprintf("berthd is not answering on %s", h.Socket))
		return
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := conn.Send(proto.Msg{Op: proto.OpHistory}); err != nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", err.Error())
		return
	}
	m, err := conn.Recv()
	if err != nil || m.History == nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", "no history from berthd (an older node?)")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(m.History)
}

// events is a server-sent event stream of the node's events. A browser
// reconnecting sends Last-Event-ID and resumes without gaps while the node
// still holds those events in its ring. If the ID is above everything this
// node instance has ever emitted, the node restarted (Seq begins again at 1):
// the harbor rebases with a rebase frame and replays the new ring.
func (h *Harbor) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "no_stream", "streaming unsupported")
		return
	}
	since, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = strconv.ParseUint(v, 10, 64)
	}
	// Both dials happen before WriteHeader, so a 502 here just makes the
	// browser's EventSource retry. LastEvent is the node's total event count
	// since this instance started; a since above it is an old generation.
	rebase := uint64(0)
	if since > 0 {
		conn, raw, err := server.Dial(h.Socket)
		if err != nil {
			apiError(w, http.StatusBadGateway, "node_unreachable", fmt.Sprintf("berthd is not answering on %s", h.Socket))
			return
		}
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		if err := conn.Send(proto.Msg{Op: proto.OpStatus}); err != nil {
			raw.Close()
			apiError(w, http.StatusBadGateway, "node_unreachable", err.Error())
			return
		}
		m, err := conn.Recv()
		raw.Close()
		if err != nil || m.Status == nil {
			apiError(w, http.StatusBadGateway, "node_unreachable", "no status from berthd")
			return
		}
		if since > m.Status.LastEvent {
			rebase = m.Status.LastEvent
			since = 0 // replay the new generation's full ring
		}
	}
	conn, raw, err := server.Dial(h.Socket)
	if err != nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", fmt.Sprintf("berthd is not answering on %s", h.Socket))
		return
	}
	defer raw.Close()
	if err := conn.Send(proto.Msg{Op: proto.OpEvents, Since: since, Watch: true}); err != nil {
		apiError(w, http.StatusBadGateway, "node_unreachable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	if rebase > 0 {
		// The server is replaying the new generation's ring; the client resets
		// its dedupe and its log, and refetches status and history.
		fmt.Fprintf(w, "event: rebase\ndata: {\"last_event\":%d}\n\n", rebase)
		fl.Flush()
	}

	msgs := make(chan proto.Msg)
	go func() {
		defer close(msgs)
		for {
			m, err := conn.Recv()
			if err != nil {
				return
			}
			msgs <- m
		}
	}()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case m, ok := <-msgs:
			if !ok || m.Op == proto.OpRefused {
				return // the browser's EventSource reconnects with Last-Event-ID
			}
			if m.Event == nil {
				continue
			}
			b, _ := json.Marshal(m.Event)
			fmt.Fprintf(w, "id: %d\nevent: berth\ndata: %s\n\n", m.Event.Seq, b)
			fl.Flush()
		}
	}
}
