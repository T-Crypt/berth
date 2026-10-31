// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package runner is `berth run`: acquire a berth, start the engine, hand
// the berth back by stopping the engine when berthd asks, and exit with the
// engine's status. If berthd is down the engine runs anyway (fail-open), so
// Berth never becomes the reason a model cannot load.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/comfy"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/engines"
	"github.com/T-Crypt/berth/internal/front"
	"github.com/T-Crypt/berth/internal/httpauth"
	"github.com/T-Crypt/berth/internal/lend"
	"github.com/T-Crypt/berth/internal/park"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
)

type Options struct {
	Socket string
	Engine string
	// Card is the card the run asked for (berth run --gpu, the entry's
	// gpu): its name on the node or its PCI address, or a comma list of
	// them for one lease on several cards. "" = the first card,
	// and the engine's environment is left as it is.
	Card     string
	Group    string // the engine family for juggles and usage (llama-swap's models: "llama-swap")
	Priority string
	Needs    uint64
	Pinned   bool // never drained: an app that must not be stopped (Blender, a capture)
	Switched bool // a person's switch (the wake page's Launch, /v1/berth/switch): wins a juggle
	// MinTurn is a minimum turn: while the lease is younger than it after
	// the grant, no request of equal or lower priority drains it, and
	// neither does a higher one; only a hold (a game, a call, berth hold)
	// takes the card out. When the turn ends the normal rules apply; a
	// re-attach starts a fresh turn. 0 = off.
	MinTurn time.Duration
	// ParkTTL is how long the lease may stay parked before the broker stops
	// the engine, in place of the node's [park] ttl. 0 = the node's.
	ParkTTL   time.Duration
	Wait      time.Duration
	DrainPost string // asked first on drain, e.g. llama-swap's unload URL
	// DrainSlots is an engine base URL with a llama.cpp-style /slots: on
	// drain, wait until no slot is processing, then post DrainPost or stop
	// the engine, so a drain never cuts off the request it is serving. For
	// a server with no unload URL (Strata), and for a llama-swap model,
	// whose slots are under /upstream/<model>, not at DrainPost's origin.
	// Without it, DrainPost's origin is the base.
	DrainSlots string
	// DrainComfyUI is a ComfyUI base URL: on drain, wait until its queue
	// has nothing running or pending before any stop step, so a job is
	// never cut off. There is no time limit; a job ends when it ends.
	DrainComfyUI string
	// Session release is opt-in and uses the same drain ticket as daemon requests.
	ExitComfyUIIdle      bool
	ExitComfyUIIdleAfter time.Duration
	// APIKeyEnv names the credential source; APIKey is resolved once by Run.
	APIKeyEnv  string
	APIKeyFile string
	APIKey     string
	// DrainMax is a ceiling on how long a drain may wait at a boundary (a
	// processing slot, a running ComfyUI job) before it runs the normal
	// stop ladder anyway: a wedged engine that keeps reporting busy holds
	// the card without it. 0 = unbounded, the default, so a long
	// generation is never cut off.
	DrainMax time.Duration
	// KV snapshots (config kv_snapshots). On a drain, after the slot
	// boundary and before the park step or anything stops the engine, the
	// runner saves every idle slot's prompt cache into KVDir through the
	// engine's own slot actions and asks the engine to trim nothing: the
	// runner itself removes the least recently saved snapshots past
	// KVBudget. Empty KVDir or KVBase disables the whole step.
	KVDir string
	// KVBudget is the snapshot directory's budget in bytes.
	KVBudget int64
	// KVBase is the engine base the slot calls go to: the entry's
	// drain_slots, else the origin of its drain_post, else the engine URL.
	KVBase string
	// KVFloor is the [library] free_floor fraction the save guard keeps on
	// the snapshot directory's root; 0 turns the guard off.
	KVFloor float64
	// Park, when set, is the engine's park adapter: on a drain, after the
	// boundary and before the stop ladder, the runner asks the broker to
	// admit the lease into RAM parking and the adapter moves the weights
	// out of VRAM. A refusal or a failed call falls back to the stop
	// ladder. The engine entry builds it from park = "hook".
	Park *park.Adapter
	// ParkBase is the base URL the park adapter and the resume probe go
	// to.
	ParkBase string
	// ParkProbe is the path the engine answers once its card memory is
	// back after a resume (/v1/models for LLMs, /system_stats for Comfy).
	ParkProbe string
	// Hook marks park = "hook" when the entry gave no adapter: the first
	// drain probes Berth's own park protocol once.
	Hook bool
	// Front is the address of the run's front (berth run --front): the
	// engine's only door, which holds requests while the engine sleeps.
	// The engine listens on a port the run picks, written into its
	// command at PortPlaceholder.
	Front string
	// Checkpoint parks the engine with cuda-checkpoint (park =
	// "checkpoint"): its device memory moves into its own RAM and back.
	// It needs Front, so no request reaches the engine while it sleeps.
	Checkpoint bool
	// Lend is the engine's lend calls, built from its entry's lend = "hook"
	// (internal/lend). The run declares the capability to berthd, and only a
	// run that declares it is ever asked to give part of the card to a waiter
	// or to take it back.
	Lend *lend.Adapter
	// WholeMachine is the entry's whole_machine: berthd stops what this
	// run drains rather than parking it, and grants nothing parked beside it.
	WholeMachine bool
	// front is the open front, set by Run.
	front *front.Front
	// Container is the one container the engine runs in (berth run
	// --container NAME, the name of `docker run --name`).
	Container string
	// Compose is the Compose file the engine's project runs from (berth run
	// --compose PATH): its containers are the engine, and the drain stops
	// the project through the runtime CLI instead of signalling this
	// process group, because containerd-shim, not this wrapper, parents the
	// engine. The containers are Berth's because this command started them;
	// nothing else is ever stopped. Container and Compose are mutually
	// exclusive, and config.Validate keeps them so.
	Compose string
	// Slice is the systemd slice (NAME.slice) the engine's scope is
	// created under (berth run --slice, the entry's slice): the slice
	// unit carries resource policy, CPU, memory and swap limits, so the
	// engine and everything it forks inherit them. Linux user manager
	// only; elsewhere the engine runs where the run leaves it.
	Slice string
	// CLI is the runtime CLI exec: every docker or podman inspect, list and
	// stop runs through it, under the timeout the caller names, and it
	// returns the command's combined output; nil = the real CLI, tests fake
	// it and assert the argv.
	CLI func(argv []string, timeout time.Duration) (string, error)
	// container carries the resolved containers (the runtime that answered
	// and the full IDs) from the resolver goroutine to the drain; Run sets
	// it, tests that fake the resolution set it directly.
	container  *containerRef
	Grace      time.Duration // per drain step before escalating
	FailClosed bool          // refuse to start when berthd is unreachable
	Retry      time.Duration // how often a run that lost berthd dials it again; 0 = 2s
	Argv       []string
	Stderr     io.Writer
	// Output, when set, gets a copy of the engine's own stdout and stderr
	// (the terminal still gets them too). The gateway keeps its tail, so a
	// start that dies says why in the engine's words, not berth's.
	Output io.Writer
	// Log is the run's debug log (nil = silent). It records the one
	// product decision the run makes without asking: whether the engine
	// child starts in its own systemd scope.
	Log *slog.Logger
}

// ExitRefused is EX_TEMPFAIL: no berth was granted.
const ExitRefused = 75

// pollEvery is how often a background poll retries: the configured Retry,
// or 2s when unset.
func (o Options) pollEvery() time.Duration {
	if o.Retry <= 0 {
		return 2 * time.Second
	}
	return o.Retry
}

func Run(o Options) int {
	say := func(format string, a ...any) { fmt.Fprintf(o.Stderr, "berth: "+format+"\n", a...) }
	if o.APIKeyEnv != "" || o.APIKeyFile != "" {
		key, err := (config.Engine{APIKeyEnv: o.APIKeyEnv, APIKeyFile: o.APIKeyFile}).APIKey()
		if err != nil {
			say("credential: %v; not starting %s", err, o.Engine)
			return ExitRefused
		}
		o.APIKey = key
	}

	if o.Front != "" {
		var stop func()
		var err error
		if o, stop, err = openFront(o); err != nil {
			say("%v; not starting %s", err, o.Engine)
			return ExitRefused
		}
		defer stop()
	}

	conn, raw, err := server.Dial(o.Socket)
	if err != nil {
		if o.FailClosed {
			say("berthd unreachable (%v); not starting %s", err, o.Engine)
			return ExitRefused
		}
		say("UNPROTECTED: berthd unreachable (%v); starting %s without a lease", err, o.Engine)
		return runUnleased(o, "berthd unreachable at start")
	}
	defer raw.Close()

	if err := conn.Send(proto.Msg{Op: proto.OpAcquire, Card: o.Card, Engine: o.Engine, Group: o.Group, Priority: o.Priority, Needs: o.Needs, Wait: o.Wait.Seconds(), Pinned: o.Pinned, Switched: o.Switched, Lend: o.Lend != nil, WholeMachine: o.WholeMachine, MinTurn: o.MinTurn.Seconds(), ParkTTL: o.ParkTTL.Seconds()}); err != nil {
		say("acquire: %v", err)
		return ExitRefused
	}
	var cardEnv string
	for granted := false; !granted; {
		m, err := conn.Recv()
		if err != nil {
			say("berthd went away while %s was waiting: %v", o.Engine, err)
			return ExitRefused
		}
		switch m.Op {
		case proto.OpWaiting:
			say("waiting: %s", m.Reason)
		case proto.OpRefused:
			say("refused: %s", m.Reason)
			return ExitRefused
		case proto.OpGranted:
			if m.Reason != "" {
				say("granted %s, WARNING: %s", m.Lease, m.Reason)
			} else {
				say("granted %s", m.Lease)
			}
			granted = true
			cardEnv = visibleDevices(o.Card, m, os.LookupEnv, say)
		}
	}

	cmd := command(o, o.Output)
	if cardEnv != "" {
		cmd.Env = append(os.Environ(), cardEnv)
	}
	session := comfySession{granted: time.Now()}
	if err := cmd.Start(); err != nil {
		say("start %s: %v", o.Argv[0], err)
		return 127
	}
	afterStart(cmd)
	_ = conn.Send(proto.Msg{Op: proto.OpAttach, PID: cmd.Process.Pid})
	if o.Checkpoint {
		o.Park = checkpointPark(o, cmd.Process.Pid, say)
	}

	done := make(chan struct{})

	if o.CLI == nil {
		o.CLI = execCLI
	}
	var cid containerRef
	o.container = &cid

	// line is the lease's live conn and park state; the drain, the resume
	// and the container resolver ride it across reattach loops.
	line := newLine(conn)
	go forwardSignals(cmd, done, func() bool { return o.Checkpoint && line.isParked() })

	// A container engine resolves its containers to their full IDs and names
	// them on the lease, so the broker attributes the engine's GPU processes
	// (containerd-shim parents them, not this wrapper). The IDs also ride on
	// any re-attach, in case the daemon changes before the next poll.
	if o.Container != "" || o.Compose != "" {
		go resolveContainers(o, line, cmd, done, &cid, say)
	}

	// One drain at a time: a drain parks the engine or stops it, and the
	// daemon's next drain waits for that answer. A token leaves the slot
	// without a drain under way and comes back when it ends.
	drainTicket := make(chan struct{}, 1)
	drainTicket <- struct{}{}
	// A drain that arrives while one runs is kept, the newest only, and
	// runs when the current one ends: a drain that parked the engine must
	// still hear "berth stop" or "park did not free the card" sent during
	// its park call.
	var pendingMu sync.Mutex
	var pending *queuedDrain
	startDrain := func(opts Options, reason string) {
		select {
		case <-drainTicket:
		default:
			pendingMu.Lock()
			pending = &queuedDrain{opts, reason}
			pendingMu.Unlock()
			return
		}
		go func() {
			defer func() { drainTicket <- struct{}{} }()
			for next := (&queuedDrain{opts, reason}); next != nil; {
				drain(next.opts, cmd, line, done, next.reason, say)
				pendingMu.Lock()
				next, pending = pending, nil
				pendingMu.Unlock()
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	if o.ExitComfyUIIdle {
		go func() {
			complete := false
			for {
				if !complete {
					complete = session.wait(o, done, say)
				}
				if complete {
					opts := o
					// Completing a session ends it rather than parking its idle process.
					opts.Park, opts.Hook = nil, false
					startDrain(opts, "ComfyUI session complete")
				}
				select {
				case <-done:
					return
				case <-time.After(o.pollEvery()):
				}
			}
		}()
	}

	// Run returns only after the listener does, so an unprotected-run record
	// it wrote is gone by then.
	listening := make(chan struct{})
	go func(c net.Conn) { <-done; c.Close() }(raw)
	go func() {
		defer close(listening)
		for {
			err := listen(conn, func(m proto.Msg) {
				switch {
				case m.Op == proto.OpDrain:
					startDrain(o, m.Reason)
				case m.Op == proto.OpParkOK || m.Op == proto.OpRefused:
					line.note(m)
				case m.Op == proto.OpLend:
					go line.lendStep(o, m, say)
				case m.Op == proto.OpReturn:
					go line.returnStep(o, m.LendID, say)
				case m.Op == proto.OpGranted && m.Resume:
					go line.resume(o, cmd, done, say)
				}
			})
			select {
			case <-done:
				return
			default:
			}
			// The engine keeps running (fail-open) and is on record as
			// unprotected until a daemon answers again and re-attaches it.
			say("UNPROTECTED: lost berthd (%v); %s keeps running without a lease, retrying", err, o.Engine)
			remove := record(o, cmd, runs.ReasonLost)
			line.adoptLends(o, say)
			var lease string
			conn, raw, lease = reattach(o, cmd, done, &cid, line)
			remove()
			if conn == nil {
				return
			}
			// Hang up when the engine exits, as the first connection does,
			// so the daemon frees the lease even if this process lives on.
			go func(c net.Conn) { <-done; c.Close() }(raw)
			line.setConn(conn)
			if ids := cid.list(); len(ids) > 0 {
				_ = conn.Send(proto.Msg{Op: proto.OpAttach, PID: cmd.Process.Pid, Containers: ids})
			}
			say("re-attached %s as %s", o.Engine, lease)
		}
	}()

	err = cmd.Wait()
	afterWait(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // exited cleanly; a grandchild still held the output copy
	}
	code := exitCode(cmd, err)
	if code == 0 {
		_ = line.send(proto.Msg{Op: proto.OpFinished})
	}
	close(done)
	<-listening
	return code
}

// listen hears the daemon until the connection drops, passing each
// message to the runner's router, and returns why it dropped.
func listen(conn *proto.Conn, onMsg func(proto.Msg)) error {
	for {
		m, err := conn.Recv()
		if err != nil {
			return err
		}
		onMsg(m)
	}
}

// reattach dials berthd every Retry until one leases the running engine the
// card again, or the engine exits. A daemon that refuses (an older one that
// does not know reattach, or a PID that is no longer this engine) ends the
// attempts: the engine stays on record as unprotected until it exits. The
// line carries what the engine is in the middle of: parked weights, and the
// one lend still open if the run holds one.
func reattach(o Options, cmd *exec.Cmd, done <-chan struct{}, cid *containerRef, line *leaseLine) (*proto.Conn, net.Conn, string) {
	every := o.pollEvery()
	pid := cmd.Process.Pid
	start := procs.System{}.StartTime(pid)
	parked := line.isParked()
	for {
		select {
		case <-done:
			return nil, nil, ""
		case <-time.After(every):
		}
		conn, raw, err := server.Dial(o.Socket)
		if err != nil {
			continue
		}
		err = conn.Send(reattachMsg(o, pid, start, cid, parked, line))
		var m proto.Msg
		if err == nil {
			m, err = conn.Recv()
		}
		switch {
		case err != nil:
			raw.Close()
		case m.Op == proto.OpGranted:
			return conn, raw, m.Lease
		default:
			raw.Close()
			fmt.Fprintf(o.Stderr, "berth: berthd would not re-attach %s (%s); it stays unprotected\n", o.Engine, m.Reason)
			<-done
			return nil, nil, ""
		}
	}
}

// reattachMsg is what a re-attach tells the new daemon: the lease as it was,
// plus what the engine is in the middle of. A lend the run holds open is
// named by ticket with the bytes the engine gave, so the new daemon restores
// the holder as `lent` instead of counting its full budget and blocking
// waiters behind memory that is already gone.
func reattachMsg(o Options, pid int, start uint64, cid *containerRef, parked bool, line *leaseLine) proto.Msg {
	m := proto.Msg{Op: proto.OpReattach, Card: o.Card, Engine: o.Engine, Group: o.Group, Priority: o.Priority,
		Needs: o.Needs, Pinned: o.Pinned, Lend: o.Lend != nil, WholeMachine: o.WholeMachine,
		MinTurn: o.MinTurn.Seconds(), ParkTTL: o.ParkTTL.Seconds(),
		PID: pid, PIDStart: start, Containers: cid.list(), Parked: parked}
	if t, ok := line.openLend(); ok {
		m.LendID, m.Lent = t.ID, t.MiB<<20
	}
	return m
}

// jobBoundary waits for ComfyUI's queue to empty. It reports false when
// the engine exited meanwhile, true at the boundary. An unreadable queue
// (ComfyUI still starting, or wedged) is reported and waited out the same
// way: Berth has no proof a job is not running. A DrainMax ends the wait
// and the drain goes on to the stop ladder.
func jobBoundary(o Options, done <-chan struct{}, say func(string, ...any)) bool {
	var session comfySession
	return session.wait(o, done, say)
}

// drainProgressEvery is how often a drain boundary that is still busy
// says so, so the journal shows why the drain is taking.
var drainProgressEvery = 60 * time.Second

// boundaryWait tracks a drain boundary that has to wait (a processing
// slot, a running ComfyUI job, an unreadable queue): when the wait
// started and what its reads showed. It says so once a minute and ends
// the wait when a max has elapsed.
type boundaryWait struct {
	refuse   bool
	engine   string
	max      time.Duration // 0 = wait as long as it takes
	start    time.Time     // zero until the first note
	last     time.Time     // when the last progress line was printed
	progress string        // what the last read showed, in a progress line
	stop     string        // ...and in the line that ends the wait
}

// note remembers what the boundary shows and starts the wait clock on
// the first note.
func (w *boundaryWait) note(progress, stop string) {
	if w.start.IsZero() {
		w.start = time.Now()
		w.last = w.start
	}
	w.progress, w.stop = progress, stop
}

// tick says why the drain is still waiting, once a minute, and says the
// wait is over when the max has elapsed; it returns true then, and the
// drain continues as for an idle engine.
func (w *boundaryWait) tick(say func(string, ...any)) bool {
	if w.start.IsZero() {
		return false
	}
	if w.max > 0 && time.Since(w.start) >= w.max {
		if w.refuse {
			say("drain refused: %s still busy after %s (%s); keeps its lease", w.engine, w.max, w.stop)
		} else {
			say("%s still busy after %s (%s); draining anyway", w.engine, w.max, w.stop)
		}
		return true
	}
	if time.Since(w.last) >= drainProgressEvery {
		say("draining %s: %s for %s", w.engine, w.progress, time.Since(w.start).Round(time.Second))
		w.last = time.Now()
	}
	return false
}

// slotSummary names how many slots a read showed busy, in the words a
// drain line uses: "1 slot busy", "3 slots processing".
func slotSummary(n int, state string) string {
	if n == 1 {
		return "1 slot " + state
	}
	return fmt.Sprintf("%d slots %s", n, state)
}

// slotBoundaryAt waits until the engine at base reports no processing
// slot, so an unload never cuts an in-flight request off. A host with no
// boundary at all (404, an unreachable host, a body that is not a slot
// list) unloads at once and says so. A sick boundary (any other
// status, a read timeout) waits like a busy one: Berth then has proof work
// was running and none that it stopped. An engine that exits meanwhile
// ends the drain. A DrainMax ends it too: after that long the drain goes
// on to the stop ladder, so a wedged engine that keeps reporting busy
// holds the card no longer.
func slotBoundaryAt(o Options, base string, done <-chan struct{}, say func(string, ...any)) bool {
	if base == "" {
		return true
	}
	every := o.pollEvery()
	url := base + "/slots"
	max := o.DrainMax
	if max == 0 && o.FailClosed {
		max = 4 * o.Grace
		if max <= 0 {
			max = 2 * time.Minute
		}
	}
	wait := &boundaryWait{engine: o.Engine, max: max, refuse: o.FailClosed}
	n, err := slotsBusy(url, o.APIKey)
	if err != nil {
		if failOpen(err) {
			if o.FailClosed {
				say("drain refused: no readable slot boundary; %s keeps its lease", o.Engine)
				return false
			}
			say("drain: no slot boundary at %s (%v); unloading anyway", url, err)
			return true
		}
		say("drain: slot boundary unreadable (%v); waiting for it to answer", err)
		wait.note("boundary unreadable", "boundary unreadable")
	} else if n == 0 {
		return true
	} else {
		say("waiting for the engine to finish its requests before unloading")
		wait.note(slotSummary(n, "busy"), slotSummary(n, "processing"))
	}
	for {
		select {
		case <-done:
			return false
		case <-time.After(every):
		}
		n, err := slotsBusy(url, o.APIKey)
		switch {
		case err != nil:
			if o.FailClosed && failOpen(err) {
				say("drain refused: the slot boundary vanished; %s keeps its lease", o.Engine)
				return false
			}
			say("drain: lost the slot boundary (%v); waiting", err)
			wait.note("boundary unreadable", "boundary unreadable")
		case n == 0:
			say("engine idle; unloading")
			return true
		default:
			wait.note(slotSummary(n, "busy"), slotSummary(n, "processing"))
		}
		if wait.tick(say) {
			return !o.FailClosed
		}
	}
}

// drainPostBase is the scheme://host the drain-post URL lives on, where its
// /slots boundary would answer; empty when the URL has no usable origin.
func drainPostBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// slotsErr carries enough of a failed /slots read for slotBoundary to
// tell "no boundary here" from "boundary sick". A status means the host
// answered; badBody means the body arrived but is not a slot list;
// timeout means the read hit its deadline without an answer.
type slotsErr struct {
	status  int // HTTP status, 0 when no response arrived
	badBody bool
	timeout bool
	err     error
}

func (e *slotsErr) Error() string { return e.err.Error() }

// failOpen reports whether a /slots failure means no boundary exists, so
// the drain may unload at once. 404, an unreachable host, or a body that
// is not a slot list fail open; any other status or a timeout waits.
func failOpen(err error) bool {
	var se *slotsErr
	if !errors.As(err, &se) {
		return false
	}
	if se.badBody {
		return true
	}
	if se.status == http.StatusNotFound {
		return true
	}
	if se.status != 0 {
		return false
	}
	return !se.timeout
}

func slotsTimeout(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
}

// slotsBusy counts the llama.cpp-style slots that are processing. The
// counting rule is engines.SlotBusy, the same one the engine reader uses.
// Failures come back as a *slotsErr so the caller can tell "no boundary"
// from "sick boundary" (see failOpen).
func slotsBusy(url, key string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, &slotsErr{err: err}
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, &slotsErr{timeout: slotsTimeout(err), err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, &slotsErr{status: resp.StatusCode, err: fmt.Errorf("GET /slots: %s", resp.Status)}
	}
	var slots []struct {
		State        string `json:"state"`
		IsProcessing *bool  `json:"is_processing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return 0, &slotsErr{badBody: true, err: fmt.Errorf("/slots: %w", err)}
	}
	if slots == nil {
		return 0, &slotsErr{badBody: true, err: errors.New("/slots: expected an array")}
	}
	n := 0
	for _, s := range slots {
		if s.IsProcessing == nil && s.State != "idle" && s.State != "processing" {
			return 0, &slotsErr{badBody: true, err: errors.New("/slots: missing processing state")}
		}
		if engines.SlotBusy(s.State, s.IsProcessing != nil && *s.IsProcessing) {
			n++
		}
	}
	return n, nil
}

func runUnleased(o Options, reason string) int {
	cmd := command(o, o.Output)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(o.Stderr, "berth: start %s: %v\n", o.Argv[0], err)
		return 127
	}
	afterStart(cmd)
	defer record(o, cmd, reason)()
	done := make(chan struct{})
	go forwardSignals(cmd, done, func() bool { return false })
	err := cmd.Wait()
	afterWait(cmd)
	close(done)
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	return exitCode(cmd, err)
}

// record leaves a note beside the socket that this engine runs without a
// lease, so berthd lists it instead of it being an anonymous squatter. It
// returns the func that removes the note.
func record(o Options, cmd *exec.Cmd, reason string) func() {
	pid := cmd.Process.Pid
	remove, err := runs.Write(runs.Dir(o.Socket), runs.Record{
		Engine: o.Engine, PID: pid, Start: procs.System{}.StartTime(pid), Started: time.Now(), Reason: reason,
	})
	if err != nil {
		fmt.Fprintf(o.Stderr, "berth: could not record the unprotected run: %v\n", err)
	}
	return remove
}

// visibleDevices is the CUDA_VISIBLE_DEVICES=UUID entry that keeps the
// engine on the cards the run named, "" when the run named none, when the
// environment already picks the devices, or when a card has no UUID (an
// AMD card, or one in a VM when berthd started).
func visibleDevices(card string, granted proto.Msg, env func(string) (string, bool), say func(string, ...any)) string {
	if card == "" {
		return ""
	}
	if v, ok := env("CUDA_VISIBLE_DEVICES"); ok {
		say("CUDA_VISIBLE_DEVICES=%s is set; %s keeps it", v, granted.Card)
		return ""
	}
	if granted.CardUUID == "" {
		say("%s has no CUDA UUID; the engine must pick the card itself", granted.Card)
		return ""
	}
	return "CUDA_VISIBLE_DEVICES=" + granted.CardUUID
}

func command(o Options, out io.Writer) *exec.Cmd {
	argv := scopeToChild(o.Engine, o.Slice, o.Argv, o.Log)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if out != nil {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, out), io.MultiWriter(os.Stderr, out)
		// The copy runs through pipes. A grandchild that keeps them open
		// after the engine exits must not hold Wait, and with it the lease.
		cmd.WaitDelay = 2 * time.Second
	}
	isolate(cmd)
	return cmd
}

// ExitCode is a wrapped command's exit status, 128 plus the signal number
// when it was signalled. berth vfio run exits with its script's code.
func ExitCode(cmd *exec.Cmd, err error) int { return exitCode(cmd, err) }

// forwardSignals passes stop signals (llama-swap stopping a model, Ctrl-C)
// to the engine's whole process group. A checkpointed engine is killed
// instead: its CUDA calls block until a restore, so its own shutdown never
// finishes, and the supervisor kills this wrapper before a grace would end.
func forwardSignals(cmd *exec.Cmd, done <-chan struct{}, locked func() bool) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, stopSignals...)
	defer signal.Stop(ch)
	for {
		select {
		case <-done:
			return
		case s := <-ch:
			if locked() {
				kill(cmd)
				continue
			}
			signalGroup(cmd, s)
		}
	}
}

// drain stops the engine politely, then less so: the ComfyUI unload (models
// out of VRAM once an empty queue proves no job is running), the drain URL
// (llama-swap unloads the model and stops us itself, past its slot
// boundary), then a terminate to the process group, then a kill. Each step
// gets Grace to work.
// queuedDrain is a drain that arrived while another ran.
type queuedDrain struct {
	opts   Options
	reason string
}

// drainWait is how long a drain waits at its boundary: --drain-max, or
// for as long as a request takes when it is unset.
func drainWait(o Options) time.Duration {
	if o.DrainMax > 0 {
		return o.DrainMax
	}
	return time.Duration(math.MaxInt64)
}

func drain(o Options, cmd *exec.Cmd, line *leaseLine, done <-chan struct{}, reason string, say func(string, ...any)) {
	say("draining %s: %s", o.Engine, reason)
	if o.DrainComfyUI != "" {
		boundary := o
		boundary.ExitComfyUIIdle = false
		if !jobBoundary(boundary, done, say) {
			return
		}
		// A park hook does its own unload (with the RAM copy kept); only
		// an engine without one unloads-and-stops here.
		if o.Park == nil && !o.Hook {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := comfy.Free(httpauth.WithBearer(ctx, o.APIKey), o.DrainComfyUI, true)
			cancel()
			if err != nil {
				say("ComfyUI unload (%v); stopping the server instead", err)
			} else {
				say("ComfyUI models unloaded; stopping the server")
			}
		}
	}
	wait := func() bool {
		select {
		case <-done:
			return true
		case <-time.After(o.Grace):
			return false
		}
	}
	switch {
	case o.front != nil:
		// A front is its engine's boundary: it holds new requests and
		// waits for the ones in flight, as long as --drain-max allows. An
		// engine whose request outlived that wait is never parked
		// mid-request (docs/design.md, rule 3): this drain stops it.
		if !o.front.Sleep(drainWait(o)) {
			say("a request to %s outlived --drain-max %s; stopping it instead of parking it", o.Engine, o.DrainMax)
			o.Park, o.Hook = nil, false
		}
	case o.DrainSlots != "":
		// The base as given: llama-swap serves a model's slots only under
		// /upstream/<model>, beside an unload URL on the same host.
		if !slotBoundaryAt(o, strings.TrimRight(o.DrainSlots, "/"), done, say) {
			return
		}
	case o.DrainPost != "":
		if !slotBoundaryAt(o, drainPostBase(o.DrainPost), done, say) {
			return
		}
	}
	// KV snapshots ride the boundary: with nothing generating, the idle
	// slots save their prompt caches, before the park step or anything
	// stops the engine. A failure here never changes the stop ladder.
	kvSave(o, done, say)
	// The park step sits after the boundary, before anything stops the
	// engine: on admission the weights move to RAM, the lease is held
	// parked, and the stop ladder never runs.
	if o.Park != nil || o.Hook {
		if line.isParked() && o.Checkpoint {
			say("killing checkpointed %s: it cannot shut down while its CUDA calls are locked", o.Engine)
			kill(cmd)
			return
		}
		if line.isParked() {
			say("stopping parked %s instead of parking it again: %s", o.Engine, reason)
		} else if line.park(o, say) {
			return
		}
	}
	if o.DrainPost != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.DrainPost, strings.NewReader(""))
		if o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.APIKey)
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			say("drain url: %v", err)
		} else {
			resp.Body.Close()
			if wait() {
				return
			}
			say("drain url answered %s but %s is still running", resp.Status, o.Engine)
		}
	}
	if o.Container != "" || o.Compose != "" {
		// The engine lives in containers of ours: stop them through the
		// runtime CLI (their own SIGTERM, then SIGKILL inside the image)
		// instead of signalling the CLI wrapper's process group. A lone
		// container is stopped by the full ID the resolver answered, never
		// the bare name; a Compose project is stopped as a project.
		if stopContainers(o, say) {
			return
		}
	}
	terminate(cmd)
	if wait() {
		return
	}
	say("%s ignored terminate for %s; killing", o.Engine, o.Grace)
	kill(cmd)
}
