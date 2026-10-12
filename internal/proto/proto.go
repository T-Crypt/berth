// Package proto is the wire format between berth run and berthd:
// newline-delimited JSON over a Unix socket. A lease lives exactly as long
// as the connection that acquired it, so a wrapper that dies, however it
// dies, releases its berth.
package proto

import (
	"bufio"
	"encoding/json"
	"io"
	"time"
)

const (
	OpAcquire = "acquire" // client: ask for a berth; blocks until granted or refused
	OpAttach  = "attach"  // client: the engine is running as this PID
	// OpReattach: client: an engine already running under berth run (PID plus
	// PIDStart) lost its daemon and asks the new one to lease it the card again.
	OpReattach = "reattach"
	OpStatus   = "status"  // client: one Status reply, then the server closes
	OpEvents   = "events"  // client: events after Since; with Watch, then every new one as it happens
	OpHistory  = "history" // client: one History reply (the graphs), then the server closes

	OpRequests = "requests" // client: one reply of the requests after SinceT, then the server closes
	OpWaiting  = "waiting"  // server: still queued, with the reason
	OpGranted  = "granted"  // server: the berth is yours
	OpRefused  = "refused"  // server: gave up waiting, with the reason
	OpDrain    = "drain"    // server: give the berth back now
	OpEvent    = "event"    // server: one event (a reply to OpEvents)
)

// Msg is every message in both directions; unused fields are omitted.
type Msg struct {
	Op       string    `json:"op"`
	Engine   string    `json:"engine,omitempty"`
	Group    string    `json:"group,omitempty"` // the engine family the lease counts for in juggles and usage
	Priority string    `json:"priority,omitempty"`
	Needs    uint64    `json:"needs,omitempty"` // bytes; 0 = the whole card
	Wait     float64   `json:"wait,omitempty"`  // seconds to wait before refusing; 0 = server default
	PID      int       `json:"pid,omitempty"`
	PIDStart uint64    `json:"pid_start,omitempty"` // OpReattach: the engine's start time, so a reused PID is refused
	Lease    string    `json:"lease,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Status   *Status   `json:"status,omitempty"`
	Since    uint64    `json:"since,omitempty"` // OpEvents: only events with a higher Seq
	Watch    bool      `json:"watch,omitempty"` // OpEvents: keep streaming
	Event    *Event    `json:"event,omitempty"`
	History  *History  `json:"history,omitempty"`
	SinceT   time.Time `json:"since_t,omitempty"`  // OpRequests: only requests strictly after this time
	Requests []Request `json:"requests,omitempty"` // OpRequests: the reply's log, oldest first
}

// Event is one thing berthd saw or did. Seq is gap-free and increases by one,
// so a client that reconnects asks for Since = the last Seq it saw. Events
// live in memory only (by design for now): a restart starts again at 1.
type Event struct {
	Seq    uint64    `json:"seq"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Lease  string    `json:"lease,omitempty"`
	Engine string    `json:"engine,omitempty"`
	PID    int       `json:"pid,omitempty"`
	Bytes  uint64    `json:"bytes,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Event kinds.
const (
	EvRequested      = "lease.requested"
	EvWaiting        = "lease.waiting"
	EvGranted        = "lease.granted"
	EvDraining       = "lease.draining"
	EvReleased       = "lease.released"
	EvFreed          = "lease.freed"
	EvStuck          = "lease.stuck"
	EvRefused        = "lease.refused"
	EvReattached     = "lease.reattached"
	EvSquatter       = "squatter.appeared"
	EvSquatterGone   = "squatter.gone"
	EvProbeFailed    = "probe.failed"
	EvProbeRecovered = "probe.recovered"
	EvAlertRaised    = "alert.raised"
	EvAlertCleared   = "alert.cleared"
	EvJuggle         = "lease.juggle" // two engines at one priority fought over the card; the rank decided
)

// Counters are the collisions board. Every count is an event berthd can
// show, never an estimate of what might have happened.
type Counters struct {
	// CollisionsAvoided: grants that had to wait because the card was held,
	// instead of loading on top of the holder.
	CollisionsAvoided uint64 `json:"collisions_avoided"`
	// VRAMHandedOff: peak measured bytes of every lease whose memory the card
	// showed freed.
	VRAMHandedOff   uint64 `json:"vram_handed_off"`
	Drains          uint64 `json:"drains"`
	StuckCaught     uint64 `json:"stuck_caught"`
	SquattersCaught uint64 `json:"squatters_caught"`
	ContestedGrants uint64 `json:"contested_grants"`
	Refused         uint64 `json:"refused"`
	// JugglesStopped: equal-priority handoffs inside the juggle window that
	// the engine ranking decided instead of flipping the card.
	JugglesStopped uint64    `json:"juggles_stopped"`
	Since          time.Time `json:"since"`
}

// APIVersion is the schema version of Status. Fields are only added
// within a version; a removal or a change of meaning bumps it.
const APIVersion = 1

// Status is berthd's view: the body of the socket status reply and of
// GET /v1/status. Memory is bytes, ages are seconds.
type Status struct {
	API         int             `json:"api"`
	Version     string          `json:"version"`
	GPUs        []GPU           `json:"gpus"`
	Leases      []Lease         `json:"leases"`
	Waiting     []Lease         `json:"waiting"`
	Squatters   []Squatter      `json:"squatters"`
	Unprotected []Run           `json:"unprotected"`
	Engines     []Engine        `json:"engines"`
	Host        *Host           `json:"host,omitempty"`
	Insights    *Insights       `json:"insights,omitempty"`
	Counters    Counters        `json:"counters"`
	LastEvent   uint64          `json:"last_event"`
	ProbeErr    string          `json:"probe_error,omitempty"`
	Alerts      *Alerts         `json:"alerts,omitempty"`
	Priority    *PriorityStatus `json:"priority,omitempty"`
}

// PriorityStatus is how the node ranks engines when two at one priority
// juggle the card: the configured order, else card-hours over 7 days.
type PriorityStatus struct {
	Source       string       `json:"source"` // "order" or "usage"
	Order        []string     `json:"order,omitempty"`
	JuggleWindow float64      `json:"juggle_window"` // seconds; 0 = off
	Engines      []EngineRank `json:"engines"`
}

type EngineRank struct {
	Engine  string  `json:"engine"`
	Rank    int     `json:"rank"`
	Hours7d float64 `json:"hours_7d"`
}

// Threshold is one key's alert levels. Warn 0 = that level is not set.
type Threshold struct {
	Warn float64 `json:"warn"` // 0 = level not set
	Crit float64 `json:"crit,omitempty"`
}

// Alerts is the node's alert state: the threshold table it evaluates (every
// active key, always sent) and the sticky levels currently held (key ->
// "warn"|"crit"; only non-ok keys appear). The node is the source of truth;
// the harbor paints exactly these levels and never re-derives them.
type Alerts struct {
	Table  map[string]Threshold `json:"table"`            // every active key, always sent
	Levels map[string]string    `json:"levels,omitempty"` // key -> "warn"|"crit" (sticky, non-ok only)
}

// GPU is the last measurement of one device. Age says how stale it is.
// Scheduled marks the card this berthd leases; the others are inventory.
type GPU struct {
	Index     int     `json:"index"`
	Name      string  `json:"name"`
	Scheduled bool    `json:"scheduled"`
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Free      uint64  `json:"free"`
	Baseline  uint64  `json:"baseline"`
	Age       float64 `json:"age_seconds"`

	// Telemetry as the driver reports it; -1 = not reported.
	Util       float64 `json:"util_pct"`
	Temp       float64 `json:"temp_c"`
	Power      float64 `json:"power_w"`
	PowerLimit float64 `json:"power_limit_w"`
	Fan        float64 `json:"fan_pct"`
	ClockSM    float64 `json:"clock_sm_mhz"`
	ClockMem   float64 `json:"clock_mem_mhz"`
	PCIeGen    float64 `json:"pcie_gen"`
	PCIeWidth  float64 `json:"pcie_width"`
}

// Host is the machine the node runs on.
type Host struct {
	Supported bool       `json:"supported"`
	CPU       float64    `json:"cpu_pct"`
	Cores     int        `json:"cores"`
	RAMTotal  uint64     `json:"ram_total"`
	RAMUsed   uint64     `json:"ram_used"`
	SwapTotal uint64     `json:"swap_total"`
	SwapUsed  uint64     `json:"swap_used"`
	Load      [3]float64 `json:"load"`
}

// Sample is one point of the node's in-memory history (the graphs).
type Sample struct {
	T       time.Time          `json:"t"`
	GPUs    []GPUSample        `json:"gpus"`
	Holders map[string]uint64  `json:"holders,omitempty"` // engine -> measured bytes on the scheduled card
	Squat   uint64             `json:"squat,omitempty"`   // bytes held without a lease
	Waiting int                `json:"waiting,omitempty"`
	CPU     float64            `json:"cpu_pct"`
	RAMUsed uint64             `json:"ram_used"`
	Decode  map[string]float64 `json:"decode_tps,omitempty"` // engine -> tok/s
	RPM     float64            `json:"rpm"`
	P95MS   float64            `json:"p95_ms"`
	// KV is the per-engine context window and KV pressure at this moment:
	// engine -> state. Engines that report nothing are absent.
	KV map[string]KVStat `json:"kv,omitempty"`
	// Models says which models each engine reports resident (loaded/ready)
	// at this moment. An engine present with an empty slice is up with
	// nothing loaded; an absent engine is down. Either way the store closes
	// any open span the sample does not list.
	Models map[string][]string `json:"models,omitempty"`
}

// KVStat is one engine's KV state at a sample instant: the context window
// in play, the windows of the resident models, and the deepest slot fill
// (used over window, 0..1) the engine reported via /slots.
type KVStat struct {
	Window   int            `json:"window"`           // max resident model window; 0 = none
	Pressure float64        `json:"pressure"`         // max slot used/window in the sample window, 0..1
	Models   map[string]int `json:"models,omitempty"` // resident model -> window
}

type GPUSample struct {
	Index int     `json:"index"`
	Used  uint64  `json:"used"`
	Util  float64 `json:"util_pct"`
	Temp  float64 `json:"temp_c"`
	Power float64 `json:"power_w"`
}

// History is the node's recent samples, oldest first.
type History struct {
	Every   float64  `json:"every_seconds"`
	Samples []Sample `json:"samples"`
}

type Lease struct {
	ID        string  `json:"id"`
	Engine    string  `json:"engine"`
	Priority  string  `json:"priority"`
	State     string  `json:"state"`
	Needs     uint64  `json:"needs"`
	PID       int     `json:"pid,omitempty"`
	PIDStart  uint64  `json:"pid_start,omitempty"`
	GPUPIDs   []int   `json:"gpu_pids,omitempty"`
	Used      uint64  `json:"used"`
	Contested uint64  `json:"contested,omitempty"`
	Age       float64 `json:"age_seconds"`
}

type Squatter struct {
	PID      int     `json:"pid"`
	PIDStart uint64  `json:"pid_start,omitempty"`
	Name     string  `json:"name"`
	Used     uint64  `json:"used"`
	Run      string  `json:"unprotected_run,omitempty"`
	Age      float64 `json:"age_seconds"`
}

// Engine is what an inference engine reports about itself. These are the
// engine's own figures (ReportedVRAM included), never driver-measured.
type Engine struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Up    bool   `json:"up"`
	Error string `json:"error,omitempty"`
	// State is the latched engine state: up = answers, latched = was up on a
	// previous read and recent reads fail, down = never confirmed up. Up is
	// the boolean form (Up = State == "up"); the node computes it.
	State      string        `json:"state"`
	Models     []Model       `json:"models"`
	Metrics    EngineMetrics `json:"metrics"`
	DecodeTPS  float64       `json:"decode_tps"`
	PrefillTPS float64       `json:"prefill_tps"`
	// Window is the engine-level context budget: the /props n_ctx for the
	// llama.cpp family, the max model window for llama-swap. 0 = not reported.
	Window int `json:"window,omitempty"`
	// Slots are the live KV sessions the engine reports via /slots; an
	// engine without the endpoint has none.
	Slots []Slot    `json:"slots,omitempty"`
	At    time.Time `json:"at"`
	Age   float64   `json:"age_seconds"`
}

// Slot is one live KV session (a llama.cpp slot) the engine reports via
// /slots. Used and Cached are prompt tokens: what the session has in its
// context, and how much of it came from the cache.
type Slot struct {
	ID         string `json:"id"`
	Model      string `json:"model,omitempty"`  // llama-swap: whose proxy the slot is on
	Window     int    `json:"window,omitempty"` // the slot's n_ctx
	Used       int    `json:"used"`             // n_prompt_tokens
	Cached     int    `json:"cached,omitempty"` // n_prompt_tokens_cache
	Processing bool   `json:"processing,omitempty"`
}

type Model struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	ReportedVRAM uint64 `json:"reported_vram,omitempty"`
	// Window is the model's context window in tokens; 0 = not reported.
	Window int `json:"window,omitempty"`
}

// EngineMetrics are counters (totals since the engine started) and gauges.
type EngineMetrics struct {
	PromptTokens  float64 `json:"prompt_tokens"`
	OutputTokens  float64 `json:"output_tokens"`
	PromptSeconds float64 `json:"prompt_seconds"`
	OutputSeconds float64 `json:"output_seconds"`
	Processing    float64 `json:"processing"`
	Waiting       float64 `json:"waiting"`
	DraftTokens   float64 `json:"draft_tokens,omitempty"`
	DraftAccepted float64 `json:"draft_accepted,omitempty"`
}

func (m EngineMetrics) Add(o EngineMetrics) EngineMetrics {
	return EngineMetrics{
		PromptTokens: m.PromptTokens + o.PromptTokens, OutputTokens: m.OutputTokens + o.OutputTokens,
		PromptSeconds: m.PromptSeconds + o.PromptSeconds, OutputSeconds: m.OutputSeconds + o.OutputSeconds,
		Processing: m.Processing + o.Processing, Waiting: m.Waiting + o.Waiting,
		DraftTokens: m.DraftTokens + o.DraftTokens, DraftAccepted: m.DraftAccepted + o.DraftAccepted,
	}
}

// Request is one completed inference request as an engine reports it
// (llama-swap's activity log). Token counts and rates are the engine's.
type Request struct {
	ID         string    `json:"id"`
	Engine     string    `json:"engine"`
	Model      string    `json:"model"`
	T          time.Time `json:"t"`
	Status     int       `json:"status"`
	Prompt     float64   `json:"prompt_tokens"`
	Cache      float64   `json:"cache_tokens"`
	Output     float64   `json:"output_tokens"`
	Draft      float64   `json:"draft_tokens,omitempty"`
	DraftAcc   float64   `json:"draft_accepted,omitempty"`
	PrefillTPS float64   `json:"prefill_tps"`
	DecodeTPS  float64   `json:"decode_tps"`
	DurationMS float64   `json:"duration_ms"`
	// PrefillMS is derived: uncached prompt tokens / prefill rate. It stands
	// in for time-to-first-token where the engine does not report TTFT.
	PrefillMS float64 `json:"prefill_ms"`
}

// Insights are the request KPIs, token ledger and leaderboard, computed
// from the request log the node keeps in memory (24 h).
type Insights struct {
	Requests5m  int         `json:"requests_5m"`
	RPM         float64     `json:"rpm"`    // requests per minute over the last 5 min
	P50MS       float64     `json:"p50_ms"` // request duration, last 15 min
	P95MS       float64     `json:"p95_ms"`
	PrefillMS   float64     `json:"prefill_ms"`   // mean derived prefill time, last 15 min
	TPOTMS      float64     `json:"tpot_ms"`      // time per output token, from decode rate, last 15 min
	DecodeTPS   float64     `json:"decode_tps"`   // median, last 15 min
	PrefillTPS  float64     `json:"prefill_tps"`  // median, last 15 min
	CachePct    float64     `json:"cache_pct"`    // cached / prompt tokens, last hour
	ReingestPct float64     `json:"reingest_pct"` // uncached / prompt tokens, last hour
	MTPPct      float64     `json:"mtp_pct"`      // accepted / drafted tokens, last hour
	Ledger      []LedgerCol `json:"ledger"`
	Leaderboard []BoardRow  `json:"leaderboard"` // last 24 h
	Recent      []Request   `json:"recent"`      // newest first, up to 50
	Since       time.Time   `json:"since"`       // when the node started keeping requests
}

type LedgerCol struct {
	Label     string  `json:"label"`
	Requests  int     `json:"requests"`
	Generated float64 `json:"generated"`
	Fresh     float64 `json:"fresh"`
	Cached    float64 `json:"cached"`
}

type BoardRow struct {
	Engine       string  `json:"engine"`
	Model        string  `json:"model"`
	Requests     int     `json:"requests"`
	Prompt       float64 `json:"prompt_tokens"`
	Output       float64 `json:"output_tokens"`
	DecodeMedian float64 `json:"decode_tps_median"`
	CachePct     float64 `json:"cache_pct"`
	MTPPct       float64 `json:"mtp_pct"`
}

// Run is an engine started without a lease.
type Run struct {
	Engine string  `json:"engine"`
	PID    int     `json:"pid"`
	Reason string  `json:"reason"`
	Age    float64 `json:"age_seconds"`
}

// Conn reads and writes Msgs on one stream.
type Conn struct {
	r *bufio.Reader
	w io.Writer
}

func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReader(rw), w: rw}
}

func (c *Conn) Send(m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

func (c *Conn) Recv() (Msg, error) {
	var m Msg
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(line, &m)
}
