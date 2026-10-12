// Package store is the harbor's persistence: the node keeps one hour of
// samples and a day of requests in RAM, the harbor folds the same readings
// into SQLite so the UI can show days and months. The node never writes to
// disk. In the fleet phase the nodes push these samples and requests over a
// stream into this same store, so the API is "ingest", not "poll".
//
// One file, four tables. samples_1m holds one row per closed minute of
// 5 s samples (per-GPU and per-engine figures as compact JSON); samples_1h
// is the same shape rolled up from the minute rows. requests keeps every
// completed request the node reported, deduped on the engine's own id.
// model_spans keeps the intervals each model was resident on each engine
// (the model timeline), folded from the samples' per-engine model tables.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/T-Crypt/berth/internal/insights"
	"github.com/T-Crypt/berth/internal/proto"
)

const (
	// DefaultRetention is how long samples, requests and model spans are kept.
	DefaultRetention = 30 * 24 * time.Hour
	// hourRetention keeps the one-hour rollups far longer than the 1 m rows.
	hourRetention = 400 * 24 * time.Hour
	// schemaVersion is recorded in meta; the only migration from v1 is the
	// additive kv column added by migrate() in Open - never a rewrite. The
	// counters_1m table is new in place too (CREATE IF NOT EXISTS).
	schemaVersion = 2
	// spanGapSplit is the largest hole between two sightings of a model at
	// which its span stays one interval. The node samples at 5 s and the
	// harbor folds at most once a minute, so a larger hole means the harbor
	// or the node was down, and the down time must not be drawn as loaded.
	spanGapSplit = 3 * time.Minute
	// metaSpansWatermark stores, in meta, the newest sample t already folded
	// into model_spans.
	metaSpansWatermark = "spans_watermark"
)

const schema = `
CREATE TABLE IF NOT EXISTS samples_1m (
	minute INTEGER PRIMARY KEY,   -- unix second at the minute's start
	gpus TEXT NOT NULL,           -- json: per index avg used/util/temp/power, max used
	holders TEXT NOT NULL,        -- json: engine -> avg bytes on the card
	squat_avg REAL NOT NULL,
	waiting_max INTEGER NOT NULL,
	cpu_avg REAL NOT NULL,
	ram_avg REAL NOT NULL,
	decode TEXT NOT NULL,         -- json: engine -> avg tok/s
	rpm_avg REAL NOT NULL,
	p95_max REAL NOT NULL,
	kv TEXT NOT NULL,             -- json: engine -> {window, pressure, models}
	n INTEGER NOT NULL            -- samples folded into the row
);
CREATE TABLE IF NOT EXISTS samples_1h (
	hour INTEGER PRIMARY KEY,     -- unix second at the hour's start
	gpus TEXT NOT NULL,
	holders TEXT NOT NULL,
	squat_avg REAL NOT NULL,
	waiting_max INTEGER NOT NULL,
	cpu_avg REAL NOT NULL,
	ram_avg REAL NOT NULL,
	decode TEXT NOT NULL,
	rpm_avg REAL NOT NULL,
	p95_max REAL NOT NULL,
	kv TEXT NOT NULL,
	n INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS requests (
	id TEXT PRIMARY KEY,          -- the engine's own id, unique per request
	t INTEGER NOT NULL,           -- unix seconds
	engine TEXT NOT NULL,
	model TEXT,
	status INTEGER,
	prompt REAL,
	cache REAL,
	output REAL,
	draft REAL,
	draft_acc REAL,
	prefill_tps REAL,
	decode_tps REAL,
	duration_ms REAL,
	prefill_ms REAL
);
CREATE INDEX IF NOT EXISTS ix_requests_t ON requests (t);
CREATE TABLE IF NOT EXISTS model_spans (
	id INTEGER PRIMARY KEY,
	engine TEXT NOT NULL,
	model TEXT NOT NULL,
	start INTEGER NOT NULL,   -- unix seconds
	end INTEGER,              -- unix seconds; NULL = still open
	last_seen INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_model_spans_end ON model_spans (end);
CREATE INDEX IF NOT EXISTS ix_model_spans_start ON model_spans (start);
CREATE TABLE IF NOT EXISTS counters_1m (
	minute INTEGER PRIMARY KEY,   -- unix second at the minute's start
	since INTEGER NOT NULL,       -- the node's board start (unix s); a new value = a board reset
	collisions INTEGER NOT NULL,  -- the node's running totals at that minute
	handed_off INTEGER NOT NULL,
	drains INTEGER NOT NULL,
	stuck INTEGER NOT NULL,
	squatters INTEGER NOT NULL,
	contested INTEGER NOT NULL,
	refused INTEGER NOT NULL,
	juggles INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Store is one harbor database. The collector is its only writer; the API
// handlers read. All access goes through one connection, serialized by mu,
// so a read never tears a write and the write load is one transaction per
// minute.
type Store struct {
	path      string
	retention time.Duration
	db        *sql.DB
	mu        sync.Mutex
}

// gpuStat is one GPU's folded figures inside a row's gpus column.
type gpuStat struct {
	Index   int     `json:"index"`
	Used    float64 `json:"used"`     // avg bytes
	UsedMax float64 `json:"used_max"` // peak bytes
	Util    float64 `json:"util"`     // avg %
	Temp    float64 `json:"temp"`     // avg C
	Power   float64 `json:"power"`    // avg W
}

// Storage is what the /v1/storage panel shows. Retentions are whole seconds
// on the wire: a duration would encode as a nanosecond integer no reader can
// eyeball.
type Storage struct {
	Path          string           `json:"path"`
	Bytes         int64            `json:"bytes"`
	Retention     int64            `json:"retention_seconds"`
	HourRetention int64            `json:"hour_retention_seconds"` // fixed: how long the 1 h rollup is kept
	Rows          map[string]int64 `json:"rows"`                   // per table
	OldestSample  time.Time        `json:"oldest_sample,omitempty"`
	Created       time.Time        `json:"created,omitempty"`
}

// Open opens path, creating its directory, schema and pragmas on first use.
// retention <= 0 means DefaultRetention.
func Open(path string, retention time.Duration) (*Store, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	if d := filepath.Dir(path); d != "" && d != "." {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: the collector's one-transaction-per-minute writes and
	// the API reads all queue on it, so nothing interleaves.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Databases written before the kv column exists are migrated in place:
	// one ADD COLUMN per table, old rows reading back as '{}'. Never a
	// drop, recreate or rewrite.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %s: %w", path, p, err)
		}
	}
	_, _ = db.Exec(`INSERT INTO meta (key, value) VALUES ('schema', ?), ('created', ?) ON CONFLICT(key) DO NOTHING`,
		schemaVersion, time.Now().UTC().Format(time.RFC3339))
	return &Store{path: path, retention: retention, db: db}, nil
}

// migrate brings a pre-v2 database up in place: the kv column is added to
// the two sample tables where a v1 schema left it out. Both table names are
// code literals, not input, so they are inlined in the pragma query.
func migrate(db *sql.DB) error {
	// counters_1m shipped without juggles; old rows read back as 0.
	var have int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('counters_1m') WHERE name = 'juggles'`).Scan(&have); err != nil {
		return err
	}
	if have == 0 {
		if _, err := db.Exec(`ALTER TABLE counters_1m ADD COLUMN juggles INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	for _, table := range []string{"samples_1m", "samples_1h"} {
		var have int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name = 'kv'`).Scan(&have); err != nil {
			return err
		}
		if have == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN kv TEXT NOT NULL DEFAULT '{}'`); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close closes the database file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// Path and Retention are as opened.
func (s *Store) Path() string             { return s.path }
func (s *Store) Retention() time.Duration { return s.retention }

// IngestSamples folds the node's samples into samples_1m. Only minutes that
// are complete -- strictly before the newest sample's minute -- are stored,
// and a minute already in the table is never rewritten, so re-ingesting an
// overlapping window changes nothing.
func (s *Store) IngestSamples(samples []proto.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	newest := samples[0].T
	for _, sp := range samples {
		if sp.T.After(newest) {
			newest = sp.T
		}
	}
	open := newest.Truncate(time.Minute)
	minutes := map[int64][]proto.Sample{}
	for _, sp := range samples {
		m := sp.T.Truncate(time.Minute)
		if !m.Before(open) {
			continue // the newest sample's minute is still open
		}
		minutes[m.Unix()] = append(minutes[m.Unix()], sp)
	}
	if len(minutes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, min := range sortedKeys(minutes) {
		var stored int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM samples_1m WHERE minute = ?`, min).Scan(&stored); err != nil {
			return err
		}
		if stored > 0 {
			continue
		}
		r := foldMinute(minutes[min])
		_, err = tx.Exec(`INSERT OR IGNORE INTO samples_1m
			(minute, gpus, holders, squat_avg, waiting_max, cpu_avg, ram_avg, decode, rpm_avg, p95_max, kv, n)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			min, r.gpus, r.holders, r.squatAvg, r.waitingMax, r.cpuAvg, r.ramAvg, r.decode, r.rpmAvg, r.p95Max, r.kv, r.n)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IngestRequests records completed requests. The id is the engine's and is
// unique per request, so re-ingested rows are dropped at the table level
// and never double counted. It returns how many rows were new.
func (s *Store) IngestRequests(reqs []proto.Request) (int, error) {
	if len(reqs) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	new := 0
	for i := range reqs {
		q := &reqs[i]
		res, err := tx.Exec(`INSERT OR IGNORE INTO requests
			(id, t, engine, model, status, prompt, cache, output, draft, draft_acc, prefill_tps, decode_tps, duration_ms, prefill_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			q.ID, q.T.Unix(), q.Engine, q.Model, q.Status, q.Prompt, q.Cache, q.Output,
			q.Draft, q.DraftAcc, q.PrefillTPS, q.DecodeTPS, q.DurationMS, q.PrefillMS)
		if err != nil {
			return 0, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			new++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return new, nil
}

// Span is one interval a model was resident on an engine. End is nil while
// the span is open: the model was still loaded at the last folded sample,
// and the UI runs such a span to now.
type Span struct {
	Engine string     `json:"engine"`
	Model  string     `json:"model"`
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end"` // nil = still loaded
}

// spanKey is the (engine, model) pair as a map key.
func spanKey(engine, model string) string { return engine + "\x00" + model }

// spanLive is an open span as the fold walks the samples.
type spanLive struct {
	id       int64
	engine   string
	model    string
	start    int64 // unix seconds
	lastSeen int64
}

// IngestSpans folds the node's samples into model_spans, oldest first: a
// model seen with no open span for its engine opens one at the sample's t;
// a model seen with an open span bumps its last_seen; an open span absent
// from a sample -- whether the sample reports its engine with the model gone
// or has no data for the engine at all -- closes at its last_seen. Samples at
// or before the newest t already folded (kept in meta) are ignored, so the
// collector's overlapping re-reads change nothing. If the hole between a
// span's last sighting and its next one exceeds spanGapSplit (the harbor or
// the node was down), the old span closes at its last_seen and a new one
// opens, so the down time is not drawn as loaded.
func (s *Store) IngestSpans(samples []proto.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].T.Before(samples[j].T) })
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	wm, err := s.metaInt(tx, metaSpansWatermark)
	if err != nil {
		return err
	}
	fresh := []proto.Sample{}
	for _, sp := range samples {
		if sp.T.Unix() > wm {
			fresh = append(fresh, sp)
		}
	}
	if len(fresh) == 0 {
		return tx.Commit()
	}
	for _, sp := range fresh {
		if t := sp.T.Unix(); t > wm {
			wm = t
		}
	}
	open := map[string]*spanLive{}
	rows, err := tx.Query(`SELECT id, engine, model, start, last_seen FROM model_spans WHERE end IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		l := &spanLive{}
		if err := rows.Scan(&l.id, &l.engine, &l.model, &l.start, &l.lastSeen); err != nil {
			rows.Close()
			return err
		}
		open[spanKey(l.engine, l.model)] = l
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	closeSpan := func(l *spanLive) error {
		if _, err := tx.Exec(`UPDATE model_spans SET end = last_seen WHERE id = ?`, l.id); err != nil {
			return err
		}
		delete(open, spanKey(l.engine, l.model))
		return nil
	}
	openSpan := func(engine, model string, at int64) error {
		res, err := tx.Exec(`INSERT INTO model_spans (engine, model, start, end, last_seen) VALUES (?,?,?,NULL,?)`, engine, model, at, at)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		l := &spanLive{id: id, engine: engine, model: model, start: at, lastSeen: at}
		open[spanKey(engine, model)] = l
		return nil
	}
	for _, sp := range fresh {
		at := sp.T.Unix()
		seen := map[string]bool{}
		for engine, models := range sp.Models {
			for _, model := range models {
				k := spanKey(engine, model)
				if seen[k] {
					continue // a duplicate name in one reading counts once
				}
				seen[k] = true
				l := open[k]
				if l == nil {
					if err := openSpan(engine, model, at); err != nil {
						return err
					}
					continue
				}
				if at-l.lastSeen > int64(spanGapSplit/time.Second) {
					// a down harbor or node sat between the sightings
					if err := closeSpan(l); err != nil {
						return err
					}
					if err := openSpan(engine, model, at); err != nil {
						return err
					}
					continue
				}
				l.lastSeen = at
				if _, err := tx.Exec(`UPDATE model_spans SET last_seen = ? WHERE id = ?`, at, l.id); err != nil {
					return err
				}
			}
		}
		for k, l := range open {
			if seen[k] {
				continue
			}
			if err := closeSpan(l); err != nil {
				return err
			}
		}
	}
	if err := s.setMeta(tx, metaSpansWatermark, wm); err != nil {
		return err
	}
	return tx.Commit()
}

// RollupHours recomputes every closed hour's samples_1h row from the 1 m
// rows: the hour in progress is never touched, and a repeat pass writes the
// same values back, so the table self-heals after a long stop.
func (s *Store) RollupHours(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM samples_1m`).Scan(&have); err != nil {
		return err
	}
	if have == 0 {
		return tx.Commit()
	}
	latest := now.Truncate(time.Hour).Unix() - int64(time.Hour/time.Second)
	last := map[int64]bool{}
	rows, err := tx.Query(`SELECT hour FROM samples_1h`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var h int64
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		last[h] = true
	}
	rows.Close()
	// Start at the first hour not yet folded, or at the oldest stored hour
	// when the 1 h table is empty; a gap longer than the hour retention is
	// capped, so a year-long stop costs a bounded walk.
	start := latest - int64(hourRetention/time.Second)
	if len(last) == 0 {
		var oldest int64
		if err := tx.QueryRow(`SELECT MIN(minute) FROM samples_1m`).Scan(&oldest); err != nil {
			return err
		}
		if oh := (oldest / 3600) * 3600; start < oh {
			start = oh
		}
	} else {
		for h := range last {
			if h+3600 > start {
				start = h + 3600
			}
		}
	}
	if start > latest {
		return tx.Commit()
	}
	for h := start; h <= latest; h += 3600 {
		if _, ok := last[h]; ok {
			continue // already folded; its minutes are inside a later hour's range too, but the upsert would write identical values, so skip the work
		}
		if err := s.rollupHour(tx, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rollupHour folds one hour's 1 m rows into its 1 h row. Averages average
// the minute rows' averages; maxima and n take the hour's max and sum.
func (s *Store) rollupHour(tx *sql.Tx, h int64) error {
	rows, err := tx.Query(`SELECT gpus, holders, squat_avg, waiting_max, cpu_avg, ram_avg, decode, rpm_avg, p95_max, kv, n
		FROM samples_1m WHERE minute >= ? AND minute < ? ORDER BY minute`, h, h+3600)
	if err != nil {
		return err
	}
	type acc struct {
		used, usedMax, util, temp, power float64
	}
	gm := map[int]*acc{}
	hm := map[string]float64{}
	dm := map[string]float64{}
	var kvM map[string]proto.KVStat
	var squat, cpu, ram, rpm, p95 float64
	var waiting, n, mins int
	for rows.Next() {
		var g, ho, dec, kvs string
		var sq, cp, rm, rp, pm float64
		var wm, nn int
		// column order: gpus, holders, squat_avg, waiting_max, cpu_avg,
		// ram_avg, decode, rpm_avg, p95_max, kv, n
		if err := rows.Scan(&g, &ho, &sq, &wm, &cp, &rm, &dec, &rp, &pm, &kvs, &nn); err != nil {
			rows.Close()
			return err
		}
		var gs []gpuStat
		if err := json.Unmarshal([]byte(g), &gs); err != nil {
			rows.Close()
			return err
		}
		for _, x := range gs {
			a := gm[x.Index]
			if a == nil {
				a = &acc{}
				gm[x.Index] = a
			}
			a.used += x.Used
			a.util += x.Util
			a.temp += x.Temp
			a.power += x.Power
			if x.UsedMax > a.usedMax {
				a.usedMax = x.UsedMax
			}
		}
		var hd map[string]float64
		_ = json.Unmarshal([]byte(ho), &hd)
		for e, v := range hd {
			hm[e] += v
		}
		var dd map[string]float64
		_ = json.Unmarshal([]byte(dec), &dd)
		for e, v := range dd {
			dm[e] += v
		}
		var km map[string]proto.KVStat
		if err := json.Unmarshal([]byte(kvs), &km); err != nil {
			km = nil // pre-kv rows read back as '{}': no data then
		}
		foldKV(&kvM, km)
		squat += sq
		if wm > waiting {
			waiting = wm
		}
		cpu += cp
		ram += rm
		rpm += rp
		if pm > p95 {
			p95 = pm
		}
		n += nn
		mins++
	}
	err = rows.Close()
	if err != nil {
		return err
	}
	if mins == 0 {
		return nil
	}
	idx := make([]int, 0, len(gm))
	for i := range gm {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	gs := make([]gpuStat, 0, len(idx))
	for _, i := range idx {
		a := gm[i]
		gs = append(gs, gpuStat{Index: i, Used: a.used / float64(mins), UsedMax: a.usedMax,
			Util: a.util / float64(mins), Temp: a.temp / float64(mins), Power: a.power / float64(mins)})
	}
	hb := map[string]float64{}
	for e, v := range hm {
		hb[e] = v / float64(mins)
	}
	db := map[string]float64{}
	for e, v := range dm {
		db[e] = v / float64(mins)
	}
	_, err = tx.Exec(`INSERT INTO samples_1h
		(hour, gpus, holders, squat_avg, waiting_max, cpu_avg, ram_avg, decode, rpm_avg, p95_max, kv, n)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hour) DO UPDATE SET
		gpus=excluded.gpus, holders=excluded.holders, squat_avg=excluded.squat_avg,
		waiting_max=excluded.waiting_max, cpu_avg=excluded.cpu_avg, ram_avg=excluded.ram_avg,
		decode=excluded.decode, rpm_avg=excluded.rpm_avg, p95_max=excluded.p95_max,
		kv=excluded.kv, n=excluded.n`,
		h, marshalJSON(gs), marshalJSON(hb), squat/float64(mins), waiting, cpu/float64(mins),
		ram/float64(mins), marshalJSON(db), rpm/float64(mins), p95, marshalJSON(pruneKV(kvM)), n)
	return err
}

// Prune drops samples_1m and requests rows older than retention,
// samples_1h rows older than the fixed hour retention, and model spans that
// ended before the retention (an open span is kept until it closes).
func (s *Store) Prune(now time.Time, retention time.Duration) error {
	if retention <= 0 {
		retention = s.retention
	}
	cut := now.Add(-retention).Unix()
	cutH := now.Add(-hourRetention).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM samples_1m WHERE minute < ?`, cut); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM samples_1h WHERE hour < ?`, cutH); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM counters_1m WHERE minute < ?`, cut); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM requests WHERE t < ?`, cut); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM model_spans WHERE end IS NOT NULL AND end < ?`, cut); err != nil {
		return err
	}
	return tx.Commit()
}

// History is the stored samples for the range ending at now: 1 m rows for
// ranges up to a day, 1 h rows beyond. Every says the spacing.
// ParseRange maps the range names the harbor's UI uses (1h, 6h, 24h, 7d,
// 30d) to durations. The store's readers and the CLI's export share this
// one grammar; anything else is false.
func ParseRange(s string) (time.Duration, bool) {
	switch s {
	case "1h":
		return time.Hour, true
	case "6h":
		return 6 * time.Hour, true
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
}

func (s *Store) History(rangeDur time.Duration, now time.Time) (proto.History, error) {
	every, table, col := float64(time.Minute/time.Second), "samples_1m", "minute"
	bucket := time.Minute
	if rangeDur > 24*time.Hour {
		every, table, col = float64(time.Hour/time.Second), "samples_1h", "hour"
		bucket = time.Hour
	}
	// The range starts at the first bucket boundary at or after the cutoff,
	// so every returned row lies fully inside the range.
	cut := now.Add(-rangeDur).Truncate(bucket).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(fmt.Sprintf(`SELECT %s, gpus, holders, squat_avg, waiting_max, cpu_avg, ram_avg, decode, rpm_avg, p95_max, kv FROM %s WHERE %s >= ? ORDER BY %s`, col, table, col, col), cut)
	if err != nil {
		return proto.History{}, err
	}
	defer rows.Close()
	h := proto.History{Every: every, Samples: []proto.Sample{}}
	for rows.Next() {
		var ts int64
		var g, ho, dec, kvs string
		var sq, cp, rm, rp, pm float64
		var wm int
		// column order: minute, gpus, holders, squat_avg, waiting_max,
		// cpu_avg, ram_avg, decode, rpm_avg, p95_max, kv
		if err := rows.Scan(&ts, &g, &ho, &sq, &wm, &cp, &rm, &dec, &rp, &pm, &kvs); err != nil {
			return proto.History{}, err
		}
		// The stored figures are averages, so they come back through the
		// store's own types before narrowing to the wire types.
		var gs []gpuStat
		if err := json.Unmarshal([]byte(g), &gs); err != nil {
			return proto.History{}, err
		}
		s := proto.Sample{T: time.Unix(ts, 0), Waiting: wm, CPU: cp, RAMUsed: uint64(rm), RPM: rp, P95MS: pm, Squat: uint64(sq)}
		for _, x := range gs {
			s.GPUs = append(s.GPUs, proto.GPUSample{Index: x.Index, Used: uint64(x.Used), Util: x.Util, Temp: x.Temp, Power: x.Power})
		}
		var hd map[string]float64
		if err := json.Unmarshal([]byte(ho), &hd); err != nil {
			return proto.History{}, err
		}
		for e, b := range hd {
			s.Holders = addHolder(s.Holders, e, b)
		}
		if err := json.Unmarshal([]byte(dec), &s.Decode); err != nil {
			return proto.History{}, err
		}
		// Pre-kv rows read back as '{}': no data then, and the panels show
		// nothing rather than a lie.
		var kv map[string]proto.KVStat
		if err := json.Unmarshal([]byte(kvs), &kv); err != nil {
			return proto.History{}, err
		}
		if len(kv) > 0 {
			s.KV = kv
		}
		h.Samples = append(h.Samples, s)
	}
	return h, rows.Err()
}

// Spans returns the model intervals overlapping the range ending at now,
// oldest first per engine and model. Open spans come back with End nil; the
// UI runs them to the right edge of the chart.
func (s *Store) Spans(rangeDur time.Duration, now time.Time) ([]Span, error) {
	cut := now.Add(-rangeDur).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT engine, model, start, end FROM model_spans WHERE start < ? AND (end IS NULL OR end >= ?) ORDER BY engine, model, start`, now.Unix(), cut)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Span{}
	for rows.Next() {
		sp := Span{}
		var st int64
		var en sql.NullInt64
		if err := rows.Scan(&sp.Engine, &sp.Model, &st, &en); err != nil {
			return nil, err
		}
		sp.Start = time.Unix(st, 0)
		if en.Valid {
			t := time.Unix(en.Int64, 0)
			sp.End = &t
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Insights is the KPIs, ledger and leaderboard over the stored requests of
// the range ending at now, from the same math the node uses for its own log.
func (s *Store) Insights(rangeDur time.Duration, now time.Time) (*proto.Insights, error) {
	cut := now.Add(-rangeDur).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	var since time.Time
	var oldest int64
	if err := s.db.QueryRow(`SELECT MIN(t) FROM requests`).Scan(&oldest); err == nil && oldest > 0 {
		since = time.Unix(oldest, 0)
	}
	reqs, err := s.requestsLocked(cut)
	if err != nil {
		return nil, err
	}
	cols := []insights.Col{{Label: "1h", Window: time.Hour}}
	cols = append(cols, insights.Col{Label: rangeLabel(rangeDur), Window: rangeDur})
	return insights.Compute(reqs, nil, cols, since, now), nil
}

// Requests returns the stored requests of the range ending at now, oldest
// first. An empty range returns an empty slice, not nil, so a JSON dump
// reads as [] rather than null.
func (s *Store) Requests(rangeDur time.Duration, now time.Time) ([]proto.Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestsLocked(now.Add(-rangeDur).Unix())
}

// requestsLocked is the range's request read; the caller holds s.mu.
func (s *Store) requestsLocked(cut int64) ([]proto.Request, error) {
	rows, err := s.db.Query(`SELECT t, engine, model, status, prompt, cache, output, draft, draft_acc, prefill_tps, decode_tps, duration_ms, prefill_ms, id FROM requests WHERE t >= ? ORDER BY t`, cut)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.Request{}
	for rows.Next() {
		var q proto.Request
		var ts int64
		if err := rows.Scan(&ts, &q.Engine, &q.Model, &q.Status, &q.Prompt, &q.Cache, &q.Output,
			&q.Draft, &q.DraftAcc, &q.PrefillTPS, &q.DecodeTPS, &q.DurationMS, &q.PrefillMS, &q.ID); err != nil {
			return nil, err
		}
		q.T = time.Unix(ts, 0)
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// rangeLabel is how the store's whole-range ledger column is labelled, the
// same words the node uses for its own 24 h column.
func rangeLabel(d time.Duration) string {
	switch d {
	case time.Hour:
		return "1h"
	case 24 * time.Hour:
		return "24h"
	default:
		if d%24*time.Hour == 0 {
			return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
		}
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
}

// Storage reports the file, its size (WAL sidecars included), per-table row
// counts, the retentions and when the stored history starts.
func (s *Store) Storage() (Storage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Storage{Path: s.path, Retention: int64(s.retention / time.Second), HourRetention: int64(hourRetention / time.Second), Rows: map[string]int64{}}
	if fi, err := os.Stat(s.path); err == nil {
		st.Bytes = fi.Size()
	}
	for _, suf := range []string{"-wal", "-shm"} {
		if fi, err := os.Stat(s.path + suf); err == nil {
			st.Bytes += fi.Size()
		}
	}
	for _, t := range []string{"samples_1m", "samples_1h", "requests", "model_spans"} {
		var c int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&c); err != nil {
			return st, err
		}
		st.Rows[t] = c
	}
	var oldest int64
	if err := s.db.QueryRow(`SELECT MIN(minute) FROM samples_1m`).Scan(&oldest); err != nil {
		return st, err
	}
	if oldest > 0 {
		st.OldestSample = time.Unix(oldest, 0)
	}
	var created string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'created'`).Scan(&created); err == nil {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			st.Created = t
		}
	}
	return st, nil
}

// foldMinute is one minute's samples as a samples_1m row: means of the
// gauges, the minute's peak for used, waiting and p95, the sample count, and
// the minute's KV fold: per engine the last sample's window and models, the
// peak slot pressure, and engines reporting nothing dropped.
func foldMinute(samples []proto.Sample) (r struct {
	gpus, holders, decode, kv                string
	squatAvg, cpuAvg, ramAvg, rpmAvg, p95Max float64
	waitingMax                               int
	n                                        int
}) {
	n := len(samples)
	// The kv fold reads "the last sample's" window and models, so the
	// samples must be in time order: the node sends them oldest first, and
	// the sort keeps the fold right if a caller ever does not.
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].T.Before(samples[j].T) })
	type acc struct {
		used, usedMax, util, temp, power float64
	}
	gm := map[int]*acc{}
	hm := map[string]float64{}
	dm := map[string]float64{}
	var kvM map[string]proto.KVStat
	var squat, cpu, ram, rpm, p95 float64
	var waiting int
	for _, sp := range samples {
		for _, g := range sp.GPUs {
			a := gm[g.Index]
			if a == nil {
				a = &acc{}
				gm[g.Index] = a
			}
			a.used += float64(g.Used)
			if float64(g.Used) > a.usedMax {
				a.usedMax = float64(g.Used)
			}
			a.util += g.Util
			a.temp += g.Temp
			a.power += g.Power
		}
		for e, b := range sp.Holders {
			hm[e] += float64(b)
		}
		for e, v := range sp.Decode {
			dm[e] += v
		}
		foldKV(&kvM, sp.KV)
		squat += float64(sp.Squat)
		if sp.Waiting > waiting {
			waiting = sp.Waiting
		}
		cpu += sp.CPU
		ram += float64(sp.RAMUsed)
		rpm += sp.RPM
		if sp.P95MS > p95 {
			p95 = sp.P95MS
		}
	}
	idx := make([]int, 0, len(gm))
	for i := range gm {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	gs := make([]gpuStat, 0, len(idx))
	for _, i := range idx {
		a := gm[i]
		gs = append(gs, gpuStat{Index: i, Used: a.used / float64(n), UsedMax: a.usedMax,
			Util: a.util / float64(n), Temp: a.temp / float64(n), Power: a.power / float64(n)})
	}
	hb := map[string]float64{}
	for e, v := range hm {
		hb[e] = v / float64(n)
	}
	db := map[string]float64{}
	for e, v := range dm {
		db[e] = v / float64(n)
	}
	r.gpus, r.holders, r.decode = marshalJSON(gs), marshalJSON(hb), marshalJSON(db)
	r.squatAvg, r.cpuAvg, r.ramAvg, r.rpmAvg, r.p95Max = squat/float64(n), cpu/float64(n), ram/float64(n), rpm/float64(n), p95
	r.waitingMax, r.n = waiting, n
	r.kv = marshalJSON(pruneKV(kvM))
	return r
}

// foldKV merges one row's or sample's per-engine KV table into m in time
// order: the engine's window and models come from the newest row that
// reported it, its pressure takes the max across rows. A nil table - a
// pre-kv row, or an engine that reported nothing - changes nothing.
func foldKV(m *map[string]proto.KVStat, other map[string]proto.KVStat) {
	for e, st := range other {
		cur := (*m)[e]
		if st.Window > 0 || len(st.Models) > 0 {
			cur.Window = st.Window
			cur.Models = st.Models
		}
		if st.Pressure > cur.Pressure {
			cur.Pressure = st.Pressure
		}
		if *m == nil {
			*m = map[string]proto.KVStat{}
		}
		(*m)[e] = cur
	}
}

// pruneKV drops engines with no window, no pressure and no models. It always
// returns a non-nil map, so an empty fold marshals to '{}' - the value
// migrated pre-kv rows carry - not 'null', keeping the column's
// representation uniform across pre-kv, new-empty and populated rows.
func pruneKV(m map[string]proto.KVStat) map[string]proto.KVStat {
	if m == nil {
		m = map[string]proto.KVStat{}
	}
	for e, st := range m {
		if st.Window == 0 && st.Pressure == 0 && len(st.Models) == 0 {
			delete(m, e)
		}
	}
	return m
}

func addHolder(m map[string]uint64, engine string, b float64) map[string]uint64 {
	if m == nil {
		m = map[string]uint64{}
	}
	m[engine] = uint64(b)
	return m
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func sortedKeys(m map[int64][]proto.Sample) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// metaInt reads a whole-second value kept in the meta table; missing is 0.
func (s *Store) metaInt(tx *sql.Tx, key string) (int64, error) {
	var v string
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// setMeta writes a whole-second value into the meta table.
func (s *Store) setMeta(tx *sql.Tx, key string, v int64) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, strconv.FormatInt(v, 10))
	return err
}
