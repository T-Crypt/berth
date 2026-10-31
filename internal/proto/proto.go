// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package proto is the wire format between berth run and berthd:
// newline-delimited JSON over a Unix socket. A lease lives exactly as long
// as the connection that acquired it, so a wrapper that dies, however it
// dies, releases its berth.
package proto

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"time"
)

const (
	OpAcquire = "acquire" // client: ask for a berth; blocks until granted or refused
	OpAttach  = "attach"  // client: the engine is running as this PID
	// OpFinished: client: the engine stopped answering on purpose: it exited
	// 0, or the run ended it (a drain, a failed wake, a stop signal it
	// forwarded). Sent just before the hang-up that ends the lease.
	OpFinished = "finished"
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

	// OpReclaim: client: list (and, with Confirm, stop) the processes on the
	// card that the named lease, or every stuck and releasing lease, recorded.
	// The server answers once with Holders.
	OpReclaim = "reclaim"

	// OpStop: client: drain every active or draining lease of the named
	// engine (by name or group) that has an attached run, as a
	// higher-priority waiter would. The server answers once with the leases
	// it asked.
	OpStop = "stop"
	// OpPark: the runner holding a draining lease asks to park its engine
	// in RAM; the daemon answers OpParkOK or OpRefused with the why line.
	// OpParked: the runner's park call is done; the daemon watches for the
	// residual to leave the card. OpParkFailed: the park call failed; the
	// lease goes back to draining and the runner runs the stop ladder.
	// OpResume: any client puts a parked engine back on the card at its
	// own priority; the runner hears OpGranted{Resume: true} when it is
	// granted, or the resuming client gets OpRefused when it is not parked.
	// OpAwake: the runner's resume finished and the engine answers its
	// probe again; the daemon pushes lease.resumed for the harbor's warm
	// badge and for a gateway that waits for the wake before proxying.
	OpPark       = "park"
	OpParkOK     = "park_ok"
	OpParked     = "parked"
	OpParkFailed = "park_failed"
	OpResume     = "resume"
	// OpSwitch: a person switched to an engine whose lease is queued, so
	// that lease wins a juggle as a switched acquire does.
	OpSwitch = "switch"
	OpAwake  = "awake"

	// OpLend: the daemon asks a serving holder's runner to give part of its
	// memory on one card to a waiter that does not fit while the holder keeps
	// serving: Needs is the bytes to give, Free the card's driver free figure
	// in MiB, GPU the card's PCI address when the snapshot has one. OpLent
	// answers with the engine's ticket (LendID) and what it gave (Needs);
	// OpLendFailed answers with the Reason and its Kind (TooBig, Busy,
	// Pending, Unsupported). OpReturn asks the holder's runner to take the
	// memory back once the borrower is gone, naming its ticket (LendID); the
	// runner answers OpReturned, or OpReturnFailed with the Reason and Retry
	// set when Berth should ask again later.
	OpLend         = "lend"
	OpLent         = "lent"
	OpLendFailed   = "lend_failed"
	OpReturn       = "return"
	OpReturned     = "returned"
	OpReturnFailed = "return_failed"

	// Device pairing for the harbor's actions; berthd owns the file. A
	// client mints a code (OpPairCode, reply Code), the harbor trades one
	// for a device token (OpPairRedeem: Code and DeviceName in, Token out)
	// and checks a token on every write (OpPairCheck: Token in, Paired out).
	// OpPairList answers Devices; OpPairRevoke unpairs Device ("" = all)
	// and answers Revoked. A refusal is OpRefused with the reason.
	OpPairCode   = "pair_code"
	OpPairRedeem = "pair_redeem"
	OpPairCheck  = "pair_check"
	OpPairList   = "pair_list"
	OpPairRevoke = "pair_revoke"
)

// Msg is every message in both directions; unused fields are omitted.
type Msg struct {
	Op       string `json:"op"`
	Engine   string `json:"engine,omitempty"`
	Group    string `json:"group,omitempty"` // the engine family the lease counts for in juggles and usage
	Priority string `json:"priority,omitempty"`
	Needs    uint64 `json:"needs,omitempty"` // bytes; 0 = the whole card
	// Card is the card an OpAcquire or OpReattach is for: its name on the
	// node (gpu1) or its PCI address, or a comma list of them for one lease
	// on several cards. "" is the node's first card.
	Card string `json:"card,omitempty"`
	// CardUUID is the granted cards' NVIDIA UUIDs on OpGranted, comma
	// separated, "" when one is unknown: a run that named its cards sets
	// CUDA_VISIBLE_DEVICES to it.
	CardUUID string  `json:"card_uuid,omitempty"`
	Wait     float64 `json:"wait,omitempty"` // seconds to wait before refusing; 0 = server default
	PID      int     `json:"pid,omitempty"`
	PIDStart uint64  `json:"pid_start,omitempty"` // OpReattach: the engine's start time, so a reused PID is refused
	Pinned   bool    `json:"pinned,omitempty"`    // OpAcquire/OpReattach: never drained (holds, berth run --pinned)
	Parked   bool    `json:"parked,omitempty"`    // OpReattach: the engine's weights are parked in RAM; its lease comes back parked
	// Lend marks a run whose engine has lend calls (its entry says
	// lend = "hook"): OpAcquire and OpReattach declare it, and the broker
	// never asks a lease that did not.
	Lend bool `json:"lend,omitempty"`
	// WholeMachine marks a run whose engine needs the machine to itself
	// (its entry says whole_machine = true). OpAcquire and OpReattach.
	WholeMachine bool `json:"whole_machine,omitempty"`
	// Switched marks a person's switch (the wake page's Launch,
	// /v1/berth/switch): it wins a juggle. OpAcquire and OpResume.
	Switched bool `json:"switched,omitempty"`
	// MinTurn is the lease's minimum turn, in seconds: while the lease is
	// younger than it after the grant, no waiter drains it but a hold.
	// OpAcquire and OpReattach; 0 = off.
	MinTurn float64 `json:"min_turn,omitempty"`
	// ParkTTL is how long the lease may stay parked, in seconds, in place
	// of the node's [park] ttl. OpAcquire and OpReattach; 0 = the node's.
	ParkTTL float64 `json:"park_ttl,omitempty"`
	// Containers is the containers the engine runs in: OpAcquire and
	// OpReattach carry the names the operator gave (--container), OpAttach
	// the full IDs berth run resolved them to. A Compose project is a set;
	// a lone --container is a set of one, so the runner has one path. The
	// broker matches GPU processes by the IDs and reports them shortened.
	Containers []string `json:"containers,omitempty"`
	// Container is the one container a wrapper from before Compose support
	// sends. A berth run started before an upgrade re-attaches to the new
	// daemon with it, so the daemon still reads it; nothing sends it now.
	Container string `json:"container,omitempty"`
	Lease     string `json:"lease,omitempty"`
	// OpStop: Leases is the reply, the leases the daemon asked to drain.
	Leases   []string  `json:"leases,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Status   *Status   `json:"status,omitempty"`
	Since    uint64    `json:"since,omitempty"` // OpEvents: only events with a higher Seq
	Watch    bool      `json:"watch,omitempty"` // OpEvents: keep streaming
	Event    *Event    `json:"event,omitempty"`
	History  *History  `json:"history,omitempty"`
	SinceT   time.Time `json:"since_t,omitempty"`  // OpRequests: only requests strictly after this time
	Requests []Request `json:"requests,omitempty"` // OpRequests: the reply's log, oldest first
	// OpReclaim: Lease names the lease to look at ("" = every stuck and
	// releasing lease), Confirm stops the recorded ones, and Grace is the
	// seconds a SIGTERM has to work before SIGKILL (0 = the daemon's
	// default). Holders is the reply: the card processes in question.
	Confirm bool     `json:"confirm,omitempty"`
	Grace   float64  `json:"grace,omitempty"`
	Holders []Holder `json:"holders,omitempty"`
	// Resume is set on an OpGranted that hands a parked lease the card
	// back, so the runner knows to call the adapter's resume instead of
	// starting anything.
	Resume bool `json:"resume,omitempty"`
	// Lending (the OpLend / OpLent / OpLendFailed / OpReturn* ops). Needs
	// carries the bytes to give on OpLend and what the engine gave on OpLent;
	// Free is the card's driver free figure in MiB and GPU its PCI address,
	// both for the engine's lend call; LendID is the engine's ticket, on
	// OpLent, OpReturn, OpReturned, and OpReattach. Kind names a failed lend
	// (TooBig, Busy, Pending, Unsupported), and Retry says a return the
	// engine refused should be asked again. On OpReattach, LendID names the
	// one lend the run still holds open and Lent is the bytes the engine
	// gave with it, so the new daemon takes up the take-back where the old
	// one stopped; Needs stays the lease's budget.
	Free   uint64 `json:"free,omitempty"`
	GPU    string `json:"gpu,omitempty"`
	LendID string `json:"lend_id,omitempty"`
	Lent   uint64 `json:"lent,omitempty"` // bytes; OpReattach with an open lend
	Kind   string `json:"kind,omitempty"`
	Retry  bool   `json:"retry,omitempty"`
	// Pairing (the OpPair* ops). Code and Token are secrets: never logged.
	Code       string         `json:"code,omitempty"`
	Token      string         `json:"token,omitempty"`
	DeviceName string         `json:"device_name,omitempty"`
	Device     string         `json:"device,omitempty"`
	Devices    []PairedDevice `json:"devices,omitempty"`
	Paired     bool           `json:"paired,omitempty"`
	Revoked    int            `json:"revoked,omitempty"`
}

// PairedDevice is one browser paired to act on the node.
type PairedDevice struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// Event is one thing berthd saw or did. Seq is gap-free and increases by one,
// so a client that reconnects asks for Since = the last Seq it saw. Events
// live in memory only (by design for now): a restart starts again at 1.
type Event struct {
	Seq    uint64    `json:"seq"`
	At     time.Time `json:"at"`
	Card   string    `json:"card,omitempty"` // the card it happened on
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
	EvOrphaned       = "lease.orphaned" // the attached process is gone and the broker released the lease itself
	EvSquatter       = "squatter.appeared"
	EvSquatterGone   = "squatter.gone"
	EvProbeFailed    = "probe.failed"
	EvProbeRecovered = "probe.recovered"
	EvAlertRaised    = "alert.raised"
	EvAlertCleared   = "alert.cleared"
	EvJuggle         = "lease.juggle"       // two engines at one priority fought over the card; the rank decided
	EvClaimStarted   = "claim.started"      // a claim rule matched: a hold is requested, or memory is now named
	EvClaimEnded     = "claim.ended"        // its matches are gone (past the linger, for a hold)
	EvParked         = "lease.parked"       // the engine's weights are in RAM and its residual is on the card
	EvResumed        = "lease.resumed"      // a parked engine was granted the card back
	EvUnparked       = "lease.unparked"     // a parked engine loaded back onto the card with no resume; it is active again
	EvParkRefused    = "lease.park_refused" // the RAM could not take the park; the drain runs as before
	EvParkEvicted    = "lease.park_evicted" // the parked engine is being stopped: ttl, RAM floor or pressure
	EvLending        = "lease.lending"      // a serving holder was asked to give part of its card to a waiter
	EvLent           = "lease.lent"         // the engine gave it, and a card read after the answer shows it free
	EvReturned       = "lease.returned"     // the borrower is gone; the holder took its memory back
	EvLendFailed     = "lease.lend_failed"  // the lend did not free the memory; the waiter falls back to park or drain
	// EvFootprint: a freed run measured far more VRAM than the engine's
	// learned footprint, so the estimate a next run is offered grows.
	EvFootprint = "engine.footprint"
	// EvCardPassthrough: the governed card's PCI function is bound to
	// vfio-pci, a VM holds the card. EvCardReturned: it is back on its
	// host driver. Each fires once per transition.
	EvCardPassthrough = "card.passthrough"
	EvCardReturned    = "card.returned"
)

// Card states, the values Status.Passthrough.State takes. Normal is the
// zero state: no Passthrough in the status at all.
const (
	// CardPassthrough: the card is in a VM, its PCI function is bound to
	// vfio-pci.
	CardPassthrough = "passthrough"
	// CardDetaching: a VFIO hold is granted and the function is not bound
	// to vfio-pci (yet or any more); the switch is in progress.
	CardDetaching = "detaching"
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
	JugglesStopped uint64 `json:"juggles_stopped"`
	// SendTimeouts: fan-out writes that hit the per-message write
	// deadline on the socket. The server keeps this count, not the
	// broker: it stamps it into Status before each status reply.
	SendTimeouts uint64 `json:"send_timeouts"`
	// DroppedConsumers: socket consumers the server dropped for slow
	// reads. Stamped the same way; the harbor store keeps no column
	// for either field and ignores them.
	DroppedConsumers uint64    `json:"dropped_consumers"`
	Since            time.Time `json:"since"`
}

// APIVersion is the schema version of Status. Fields are only added
// within a version; a removal or a change of meaning bumps it.
const APIVersion = 1

// Status is berthd's view: the body of the socket status reply and of
// GET /v1/status. Memory is bytes, ages are seconds.
type Status struct {
	API         int               `json:"api"`
	Version     string            `json:"version"`
	GPUs        []GPU             `json:"gpus"`
	Leases      []Lease           `json:"leases"`
	Waiting     []Lease           `json:"waiting"`
	Squatters   []Squatter        `json:"squatters"`
	Unprotected []Run             `json:"unprotected"`
	Engines     []Engine          `json:"engines"`
	Host        *Host             `json:"host,omitempty"`
	Insights    *Insights         `json:"insights,omitempty"`
	Counters    Counters          `json:"counters"`
	LastEvent   uint64            `json:"last_event"`
	ProbeErr    string            `json:"probe_error,omitempty"`
	Alerts      *Alerts           `json:"alerts,omitempty"`
	Priority    *PriorityStatus   `json:"priority,omitempty"`
	Claims      []Claim           `json:"claims,omitempty"`
	Footprints  []EngineFootprint `json:"footprints,omitempty"`
	// ParkedBytes is the RAM the parked engines hold: each one's Berth
	// scope's anon plus shmem, or its VRAM footprint where Berth reads no
	// scope. A reading, never a reservation.
	ParkedBytes uint64 `json:"parked_bytes,omitempty"`
	// Passthrough is the VM state of the first scheduled card, set only
	// while every scheduled card in this status is bound to vfio-pci or
	// switching to or from it, so none serves. With one card of several in
	// a VM, only that card's GPU entry carries it.
	Passthrough *Passthrough `json:"passthrough,omitempty"`
}

// Passthrough is the card's VM passthrough state.
type Passthrough struct {
	// State is CardPassthrough or CardDetaching.
	State string `json:"state"`
	// Driver is the kernel driver bound to the card's PCI function at the
	// last look: vfio-pci, the host driver mid-switch, or "" unbound.
	Driver string `json:"driver"`
	// Addr is the card's PCI address in sysfs form, "" when unknown.
	Addr string `json:"addr,omitempty"`
}

// EngineFootprint is what the broker learned from one engine's leases: the
// last run's peak VRAM, an estimate of what a run needs. It is a measured
// figure, never a reservation of memory.
type EngineFootprint struct {
	Engine string    `json:"engine"`
	Bytes  uint64    `json:"estimated_vram_bytes"`
	Runs   int       `json:"runs"`
	At     time.Time `json:"at"`
}

// Claim is one claim rule and what it matches right now: work Berth does not
// run (a game, a render, a capture) that it holds the card for or names.
type Claim struct {
	Name    string `json:"name"`
	Action  string `json:"action"` // hold | track
	Builtin bool   `json:"builtin,omitempty"`
	Active  bool   `json:"active"`
	PIDs    []int  `json:"pids,omitempty"`
	Procs   string `json:"procs,omitempty"` // the matched process names, for people
	Lease   string `json:"lease,omitempty"` // hold: the lease it holds or waits on
	Used    uint64 `json:"used,omitempty"`  // track: card memory under the matches
	GTTUsed uint64 `json:"gtt_used,omitempty"`
	// Cards names the cards where the claim is active or holds memory.
	Cards []string `json:"cards,omitempty"`
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
	Warn float64 `json:"warn" toml:"warn,omitzero" yaml:"warn"` // 0 = level not set
	Crit float64 `json:"crit,omitempty" toml:"crit,omitzero" yaml:"crit,omitempty"`
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
	Card      string  `json:"card,omitempty"` // the card's name on its node (gpu0)
	Addr      string  `json:"addr,omitempty"` // its PCI address in sysfs form (0000:09:00.0)
	Index     int     `json:"index"`
	Name      string  `json:"name"`
	Scheduled bool    `json:"scheduled"`
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Free      uint64  `json:"free"`
	Baseline  uint64  `json:"baseline"`
	Age       float64 `json:"age_seconds"`
	// UnobservedPIDs are same-user processes whose DRM fdinfo is inaccessible.
	// Their unknown allocation remains in Baseline; absence is not a release.
	UnobservedPIDs []int `json:"unobserved_pids,omitempty"`
	// On APUs Total/Used/Free describe the reserved-UMA VRAM pool only.
	// GTT capacity is shared system RAM, never additional dedicated VRAM.
	APU         bool   `json:"apu,omitempty"`
	GTTTotal    uint64 `json:"gtt_total,omitempty"`
	GTTUsed     uint64 `json:"gtt_used,omitempty"`
	GTTBaseline uint64 `json:"gtt_baseline,omitempty"`
	// ProbeErr is this card's last probe failure, "" while it reads.
	ProbeErr string `json:"probe_error,omitempty"`
	// Passthrough is this card's VM state while it does not serve; nil
	// when the card is normal.
	Passthrough *Passthrough `json:"passthrough,omitempty"`

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
	Supported bool    `json:"supported"`
	CPU       float64 `json:"cpu_pct"`
	Cores     int     `json:"cores"`
	RAMTotal  uint64  `json:"ram_total"`
	RAMUsed   uint64  `json:"ram_used"`
	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	// Load is absent where the platform has no load averages (Windows).
	Load *[3]float64 `json:"load,omitempty"`
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
	Card     string `json:"card,omitempty"` // the card's name on its node (gpu0)
	ID       string `json:"id"`
	Engine   string `json:"engine"`
	Group    string `json:"group,omitempty"` // the engine family it counts for (llama-swap's models); "" = Engine
	Priority string `json:"priority"`
	State    string `json:"state"`
	Needs    uint64 `json:"needs"`
	PID      int    `json:"pid,omitempty"`
	PIDStart uint64 `json:"pid_start,omitempty"`
	// Containers are the containers the engine runs in, as the full IDs
	// berth run resolved them; empty for a host process. A Compose engine
	// lists the whole project.
	Containers   []string `json:"containers,omitempty"`
	GPUPIDs      []int    `json:"gpu_pids,omitempty"`
	Used         uint64   `json:"used"`
	GTTUsed      uint64   `json:"gtt_used,omitempty"`
	GTTContested uint64   `json:"gtt_contested,omitempty"`
	Contested    uint64   `json:"contested,omitempty"`
	Age          float64  `json:"age_seconds"`
	Pinned       bool     `json:"pinned,omitempty"` // never drained: a hold, a claim, berth run --pinned
	// ParkedSince is how long the engine has been parked, in seconds; 0
	// when it is not parked.
	ParkedSince float64 `json:"parked_since,omitempty"`
	// TurnS is the seconds of the lease's minimum turn left; 0 when the
	// lease has no turn or it has run out.
	TurnS float64 `json:"turn_s,omitempty"`
	// Position is the lease's place in the queue, 1 = next in line; 0 for
	// a lease that is not waiting.
	Position int `json:"position,omitempty"`
	// EstimateS is how many seconds after the card frees the broker
	// expects the waiting engine to start, from the measured starts in
	// its queue chain only; 0 = unknown (no book, or nothing measured).
	EstimateS float64 `json:"estimate_start_s,omitempty"`
}

type Squatter struct {
	Card     string `json:"card,omitempty"` // the card's name on its node (gpu0)
	PID      int    `json:"pid"`
	PIDStart uint64 `json:"pid_start,omitempty"`
	Name     string `json:"name"`
	Used     uint64 `json:"used"`
	GTTUsed  uint64 `json:"gtt_used,omitempty"`
	Run      string `json:"unprotected_run,omitempty"`
	// Container is the runtime container the process holds memory in,
	// shortened to the 12 characters people quote; empty for a host
	// process. ContainerName is the name a lease gave it, when known.
	Container     string  `json:"container,omitempty"`
	ContainerName string  `json:"container_name,omitempty"`
	Age           float64 `json:"age_seconds"`
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
	// MetricsReported is true when this read got real counters from the
	// engine. A zero rate beside it is an idle engine; without it the
	// engine never answered its metrics endpoint and the rate is unknown.
	MetricsReported bool `json:"metrics_reported"`
	// Window is the engine-level context budget: the /props n_ctx for the
	// llama.cpp family, the max model window for llama-swap. 0 = not reported.
	Window int `json:"window,omitempty"`
	// Slots are the live KV sessions the engine reports via /slots; an
	// engine without the endpoint has none.
	Slots []Slot    `json:"slots,omitempty"`
	At    time.Time `json:"at"`
	Age   float64   `json:"age_seconds"`
	// Kind is what the engine produces: llm, image, tts, stt or other.
	Kind string `json:"kind,omitempty"`
	// Voices is how many voices a text-to-speech engine offers; 0 = not one, or not reported.
	Voices int `json:"voices,omitempty"`
}

// Engine kinds.
const (
	KindLLM   = "llm"
	KindImage = "image"
	KindTTS   = "tts"
	KindSTT   = "stt"
	KindOther = "other"
)

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
	// File is the model file the engine's command loads, as the engine
	// spells it; "" when the engine does not say.
	File string `json:"file,omitempty"`
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

// Holder is one process on the card a reclaim is about. Lease names the
// lease that recorded it; empty means no lease recorded it, so it is never
// signalled. Stopped and Skipped are the confirm run's word per holder;
// a list-only reply sets neither.
type Holder struct {
	PID      int    `json:"pid"`
	PIDStart uint64 `json:"pid_start,omitempty"` // 0 = gone from the process table, never signalled
	Lease    string `json:"lease,omitempty"`
	Name     string `json:"name"`
	Used     uint64 `json:"used,omitempty"` // bytes on the card
	Stopped  bool   `json:"stopped,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
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

// SendWithDeadline writes one message, giving a slow reader d to take it.
// A writer that supports write deadlines (a net.Conn does) gets one per
// message; any other writer sends exactly like Send. NewConn stays the
// only constructor: callers that need the slow-reader policy use this
// method instead of Send.
func (c *Conn) SendWithDeadline(m Msg, d time.Duration) error {
	if wd, ok := c.w.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = wd.SetWriteDeadline(time.Now().Add(d))
	}
	return c.Send(m)
}

func (c *Conn) Recv() (Msg, error) {
	var m Msg
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(line, &m)
}

// GroupVFIO is the lease group of a VFIO hold: a pinned acquire at hold
// priority, Engine "vfio:<reason>", made by `berth vfio run`. While one is
// granted berthd does not open the card, so the driver switch can run.
const GroupVFIO = "vfio"

// ContainerIDs is the containers a message names, with the single
// Container of an older wrapper folded in.
func (m Msg) ContainerIDs() []string {
	if m.Container == "" || slices.Contains(m.Containers, m.Container) {
		return m.Containers
	}
	return append(slices.Clone(m.Containers), m.Container)
}
