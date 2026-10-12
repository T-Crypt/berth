package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
	"github.com/T-Crypt/berth/internal/store"
)

// reqOverlap is how far behind its cursor the collector re-reads the
// node's request log each tick.
const reqOverlap = 15 * time.Minute

// insights is the stored KPIs, ledger and leaderboard of one range.
func (h *Harbor) insights(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		apiError(w, http.StatusServiceUnavailable, "no_store", "the harbor has no history store")
		return
	}
	d, ok := store.ParseRange(r.URL.Query().Get("range"))
	if !ok || d < 6*time.Hour {
		apiError(w, http.StatusBadRequest, "bad_range", "range must be 6h, 24h, 7d or 30d")
		return
	}
	in, err := h.Store.Insights(d, time.Now())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(in)
}

// spans is the model timeline: the stored intervals each model was resident
// on each engine, over the tide's range. A span still open comes back with
// end null; the UI runs it to the right edge of the chart.
func (h *Harbor) spans(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		apiError(w, http.StatusServiceUnavailable, "no_store", "the harbor has no history store")
		return
	}
	d, ok := store.ParseRange(r.URL.Query().Get("range"))
	if !ok {
		apiError(w, http.StatusBadRequest, "bad_range", "range must be 1h, 6h, 24h, 7d or 30d")
		return
	}
	now := time.Now()
	ss, err := h.Store.Spans(d, now)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(struct {
		From  string       `json:"from"`
		To    string       `json:"to"`
		Spans []store.Span `json:"spans"`
	}{From: now.Add(-d).Format(time.RFC3339), To: now.Format(time.RFC3339), Spans: ss})
}

// storage is the store's disk and row accounting.
func (h *Harbor) storage(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		apiError(w, http.StatusServiceUnavailable, "no_store", "the harbor has no history store")
		return
	}
	st, err := h.Store.Storage()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(st)
}

// RunCollector reads the node's in-memory history and request log over the
// socket every 60 s and folds both into the store: the node holds an hour of
// samples and a day of requests, the store keeps them for the retention. A
// down node is logged once and retried on the next tick; a late or repeated
// read is harmless because every ingest is idempotent.
func (h *Harbor) RunCollector(ctx context.Context) {
	if h.Store == nil {
		return
	}
	go func() {
		_ = h.Store.Prune(time.Now(), h.Store.Retention()) // once at start, then every 24 h
		nextPrune := time.Now().Add(24 * time.Hour)
		var lastReq time.Time // newest request t already stored; zero = take the node's whole log
		down := false         // a failure is logged once until the node answers again
		fail := func(err error) {
			if err == nil {
				err = errors.New("the node's reply is missing data")
			}
			if !down {
				h.Log.Warn("harbor: node unreachable, retrying in a minute", "err", err)
				down = true
			}
		}
		// The node answers one op per connection and closes it, so each read
		// is its own dial.
		roundtrip := func(m proto.Msg) (proto.Msg, error) {
			conn, raw, err := server.Dial(h.Socket)
			if err != nil {
				return proto.Msg{}, err
			}
			defer raw.Close()
			raw.SetDeadline(time.Now().Add(5 * time.Second))
			if err := conn.Send(m); err != nil {
				return proto.Msg{}, err
			}
			return conn.Recv()
		}
		tick := func() {
			now := time.Now()
			if !now.Before(nextPrune) {
				if err := h.Store.Prune(now, h.Store.Retention()); err != nil {
					h.Log.Warn("harbor: prune", "err", err)
				}
				nextPrune = now.Add(24 * time.Hour)
			}
			m, err := roundtrip(proto.Msg{Op: proto.OpHistory})
			if err != nil || m.History == nil {
				fail(err)
				return
			}
			if down {
				h.Log.Info("harbor: node back, resuming history collection")
				down = false
			}
			if err := h.Store.IngestSamples(m.History.Samples); err != nil {
				h.Log.Warn("harbor: store samples", "err", err)
			}
			// The same samples carry the per-engine model table, so the
			// timeline folds from them too; its watermark makes the
			// collector's overlapping re-reads a no-op, like the minutes.
			if err := h.Store.IngestSpans(m.History.Samples); err != nil {
				h.Log.Warn("harbor: store spans", "err", err)
			}
			// The node's log is in arrival order and a request's t is the
			// engine's own timestamp, so one can land behind the cursor. Re-read
			// an overlap and let the id dedupe drop the repeats.
			since := lastReq
			if !since.IsZero() {
				since = since.Add(-reqOverlap)
			}
			m, err = roundtrip(proto.Msg{Op: proto.OpRequests, SinceT: since})
			if err != nil {
				fail(err)
				return
			}
			if len(m.Requests) > 0 {
				n, err := h.Store.IngestRequests(m.Requests)
				if err != nil {
					h.Log.Warn("harbor: store requests", "err", err)
				} else {
					for _, q := range m.Requests {
						if q.T.After(lastReq) {
							lastReq = q.T
						}
					}
					if n > 0 {
						h.Log.Debug("harbor: stored new requests", "n", n)
					}
				}
			}
			// The board's running totals, one reading a minute, so the
			// harbor can say what it counted over any window.
			if st, err := h.nodeStatus(); err == nil {
				if err := h.Store.IngestCounters(time.Now(), st.Counters); err != nil {
					h.Log.Warn("harbor: store counters", "err", err)
				}
			}
			// A no-op until the hour the tick ran in has closed.
			if err := h.Store.RollupHours(time.Now()); err != nil {
				h.Log.Warn("harbor: rollup hours", "err", err)
			}
		}
		tick()
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick()
			}
		}
	}()
}
