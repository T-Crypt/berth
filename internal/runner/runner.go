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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/runs"
	"github.com/T-Crypt/berth/internal/server"
)

type Options struct {
	Socket    string
	Engine    string
	Group     string // the engine family for juggles and usage (llama-swap's models: "llama-swap")
	Priority  string
	Needs     uint64
	Wait      time.Duration
	DrainPost string // asked first on drain, e.g. llama-swap's unload URL
	// DrainComfyUI is a ComfyUI base URL: on drain, wait until its queue
	// has nothing running or pending before any stop step, so a job is
	// never cut off. There is no time limit; a job ends when it ends.
	DrainComfyUI string
	// KeepComfyUI leaves a drained ComfyUI running: after the queue empties
	// its models are unloaded via POST /free (VRAM frees while the server
	// and port stay up for OpenWebUI) and the lease is handed back without
	// stopping the process. Without it the server is stopped after the
	// unload, which is also the fallback when the idle footprint still
	// blocks the next engine. No-op without DrainComfyUI.
	KeepComfyUI bool
	Grace       time.Duration // per drain step before escalating
	FailClosed  bool          // refuse to start when berthd is unreachable
	Retry       time.Duration // how often a run that lost berthd dials it again; 0 = 2s
	Argv        []string
	Stderr      io.Writer
}

// ExitRefused is EX_TEMPFAIL: no berth was granted.
const ExitRefused = 75

func Run(o Options) int {
	say := func(format string, a ...any) { fmt.Fprintf(o.Stderr, "berth: "+format+"\n", a...) }

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

	if err := conn.Send(proto.Msg{Op: proto.OpAcquire, Engine: o.Engine, Group: o.Group, Priority: o.Priority, Needs: o.Needs, Wait: o.Wait.Seconds()}); err != nil {
		say("acquire: %v", err)
		return ExitRefused
	}
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
		}
	}

	cmd := command(o.Argv)
	if err := cmd.Start(); err != nil {
		say("start %s: %v", o.Argv[0], err)
		return 127
	}
	_ = conn.Send(proto.Msg{Op: proto.OpAttach, PID: cmd.Process.Pid})

	done := make(chan struct{})
	go forwardSignals(cmd, done)

	// released closes when a drain hands the lease back while the engine
	// keeps running (ComfyUI keep-up after unload); Run then returns clean
	// although the engine never exited.
	released := make(chan struct{})

	var drainOnce sync.Once
	go func() {
		for {
			err := listen(conn, func(reason string) {
				drainOnce.Do(func() { go drain(o, cmd, done, released, reason, say) })
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
			var lease string
			conn, raw, lease = reattach(o, cmd, done)
			remove()
			if conn == nil {
				return
			}
			// Hang up when the engine exits, as the first connection does,
			// so the daemon frees the lease even if this process lives on.
			go func(c net.Conn) { <-done; c.Close() }(raw)
			say("re-attached %s as %s", o.Engine, lease)
		}
	}()

	err = func() error {
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }()
		select {
		case err := <-waitCh:
			close(done)
			return err
		case <-released:
			// Keep-up: the engine stays up for its other clients; the
			// deferred conn close releases the lease with a clean status.
			close(done)
			return nil
		}
	}()
	return exitCode(cmd, err)
}

// listen hears the daemon until the connection drops, passing each drain
// request on, and returns why it dropped.
func listen(conn *proto.Conn, onDrain func(string)) error {
	for {
		m, err := conn.Recv()
		if err != nil {
			return err
		}
		if m.Op == proto.OpDrain {
			onDrain(m.Reason)
		}
	}
}

// reattach dials berthd every Retry until one leases the running engine the
// card again, or the engine exits. A daemon that refuses (an older one that
// does not know reattach, or a PID that is no longer this engine) ends the
// attempts: the engine stays on record as unprotected until it exits.
func reattach(o Options, cmd *exec.Cmd, done <-chan struct{}) (*proto.Conn, net.Conn, string) {
	every := o.Retry
	if every <= 0 {
		every = 2 * time.Second
	}
	pid := cmd.Process.Pid
	start := procs.System{}.StartTime(pid)
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
		err = conn.Send(proto.Msg{Op: proto.OpReattach, Engine: o.Engine, Group: o.Group, Priority: o.Priority, Needs: o.Needs, PID: pid, PIDStart: start})
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

// jobBoundary waits for ComfyUI's queue to empty. It reports false when
// the engine exited meanwhile, true at the boundary. An unreadable queue
// (ComfyUI still starting, or wedged) is reported and waited out the same
// way: Berth has no proof a job is not running.
func jobBoundary(o Options, done <-chan struct{}, say func(string, ...any)) bool {
	every := o.Retry
	if every <= 0 {
		every = 2 * time.Second
	}
	url := strings.TrimRight(o.DrainComfyUI, "/") + "/queue"
	last := ""
	for {
		running, pending, err := comfyQueue(url)
		var now string
		switch {
		case err != nil:
			now = fmt.Sprintf("cannot read the ComfyUI queue (%v); waiting", err)
		case running == 0 && pending == 0:
			say("ComfyUI queue empty; handing off")
			return true
		default:
			now = fmt.Sprintf("waiting for ComfyUI to finish: %d running, %d queued", running, pending)
		}
		if now != last {
			say("%s", now)
			last = now
		}
		select {
		case <-done:
			return false
		case <-time.After(every):
		}
	}
}

func comfyQueue(url string) (running, pending int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("GET /queue: %s", resp.Status)
	}
	var q struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return 0, 0, fmt.Errorf("/queue: %w", err)
	}
	return len(q.Running), len(q.Pending), nil
}

// comfyFree asks a drained ComfyUI to unload its models and free VRAM while
// staying up, so OpenWebUI keeps answering on its port. The /free shape is
// ComfyUI's own: unload_models plus free_memory.
func comfyFree(base string) error {
	body := strings.NewReader(`{"unload_models":true,"free_memory":true}`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/free", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST /free: %s", resp.Status)
	}
	return nil
}

// comfyIdleFootprint is what an unloaded ComfyUI server is allowed to keep
// on the card: its own process plus driver and desktop baseline. A keep-up
// release only happens once the free memory reaches total minus this.
const comfyIdleFootprint = 2 << 30

// comfyUnloaded polls ComfyUI's stats until the device shows all but the
// idle footprint free. Unreadable stats fail open: older servers answer no
// /system_stats and a failed unload already fell back to stopping.
func comfyUnloaded(base string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	every := 500 * time.Millisecond
	if within/4 < every {
		every = within / 4
	}
	if every < 20*time.Millisecond {
		every = 20 * time.Millisecond
	}
	for {
		free, total, err := comfyVRAM(base)
		if err != nil || free+comfyIdleFootprint >= total {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(every)
	}
}

// comfyVRAM reads ComfyUI's device 0 (vram_free, vram_total) in bytes.
func comfyVRAM(base string) (free, total uint64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/system_stats", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("GET /system_stats: %s", resp.Status)
	}
	var stats struct {
		Devices []struct {
			VRAMFree  uint64 `json:"vram_free"`
			VRAMTotal uint64 `json:"vram_total"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, 0, fmt.Errorf("/system_stats: %w", err)
	}
	if len(stats.Devices) == 0 {
		return 0, 0, fmt.Errorf("/system_stats: no devices")
	}
	return stats.Devices[0].VRAMFree, stats.Devices[0].VRAMTotal, nil
}

// slotBoundary waits until the engine behind the drain-post URL reports no
// processing slot, so an unload never cuts an in-flight request off. A host
// with no boundary at all (404, an unreachable host, a body that is not a
// slot list) unloads at once and says so. A sick boundary (any other
// status, a read timeout) waits like a busy one: Berth then has proof work
// was running and none that it stopped. An engine that exits meanwhile
// ends the drain.
func slotBoundary(o Options, done <-chan struct{}, say func(string, ...any)) bool {
	base := drainPostBase(o.DrainPost)
	if base == "" {
		return true
	}
	every := o.Retry
	if every <= 0 {
		every = 2 * time.Second
	}
	url := base + "/slots"
	if busy, err := slotsBusy(url); err != nil {
		if failOpen(err) {
			say("drain: no slot boundary at %s (%v); unloading anyway", url, err)
			return true
		}
		say("drain: slot boundary unreadable (%v); waiting for it to answer", err)
	} else if !busy {
		return true
	} else {
		say("waiting for the engine to finish its requests before unloading")
	}
	for {
		select {
		case <-done:
			return false
		case <-time.After(every):
		}
		busy, err := slotsBusy(url)
		if err != nil {
			say("drain: lost the slot boundary (%v); waiting", err)
			continue
		}
		if !busy {
			say("engine idle; unloading")
			return true
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

// slotsBusy reports whether any llama.cpp-style slot is processing. A slot
// counts on either shape engines serve: a "processing" state or an
// is_processing bool, the same rule the engine reader uses. Failures come
// back as a *slotsErr so the caller can tell "no boundary" from "sick
// boundary" (see failOpen).
func slotsBusy(url string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, &slotsErr{err: err}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, &slotsErr{timeout: slotsTimeout(err), err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &slotsErr{status: resp.StatusCode, err: fmt.Errorf("GET /slots: %s", resp.Status)}
	}
	var slots []struct {
		State        string `json:"state"`
		IsProcessing bool   `json:"is_processing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return false, &slotsErr{badBody: true, err: fmt.Errorf("/slots: %w", err)}
	}
	for _, s := range slots {
		if s.State == "processing" || s.IsProcessing {
			return true, nil
		}
	}
	return false, nil
}

func runUnleased(o Options, reason string) int {
	cmd := command(o.Argv)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(o.Stderr, "berth: start %s: %v\n", o.Argv[0], err)
		return 127
	}
	defer record(o, cmd, reason)()
	done := make(chan struct{})
	go forwardSignals(cmd, done)
	err := cmd.Wait()
	close(done)
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

func command(argv []string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	isolate(cmd)
	return cmd
}

// forwardSignals passes stop signals (llama-swap stopping a model, Ctrl-C)
// to the engine's whole process group.
func forwardSignals(cmd *exec.Cmd, done <-chan struct{}) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, stopSignals...)
	defer signal.Stop(ch)
	for {
		select {
		case <-done:
			return
		case s := <-ch:
			signalGroup(cmd, s)
		}
	}
}

// drain stops the engine politely, then less so: the ComfyUI unload (models
// out of VRAM once an empty queue proves no job is running), the drain URL
// (llama-swap unloads the model and stops us itself, past its slot
// boundary), then a terminate to the process group, then a kill. Each step
// gets Grace to work.
func drain(o Options, cmd *exec.Cmd, done <-chan struct{}, released chan<- struct{}, reason string, say func(string, ...any)) {
	say("draining %s: %s", o.Engine, reason)
	if o.DrainComfyUI != "" {
		if !jobBoundary(o, done, say) {
			return
		}
		// Unload first: models leave VRAM the moment the queue is known
		// empty. A failed unload falls through to stopping the server; a
		// keep-up leaves it on its port for OpenWebUI instead, but only
		// once the driver shows the memory actually left: the run record
		// written below tells the broker the lingering server no longer
		// holds the card.
		if err := comfyFree(o.DrainComfyUI); err != nil {
			say("ComfyUI unload (%v); stopping the server instead", err)
		} else if o.KeepComfyUI {
			if unloadWait := 2 * o.Grace; !comfyUnloaded(o.DrainComfyUI, unloadWait) {
				say("ComfyUI memory did not free within %s; stopping the server instead", unloadWait)
			} else {
				say("ComfyUI models unloaded; leaving the server up")
				// The record must outlive this run: the server stays up and
				// the broker lists it as an unprotected run, so the released
				// lease frees instead of turning stuck on the idle server.
				// runs.List clears the record once the process is gone.
				_ = record(o, cmd, runs.ReasonKeepUp)
				close(released)
				return
			}
		} else {
			say("ComfyUI models unloaded; stopping the server")
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
	if o.DrainPost != "" {
		if !slotBoundary(o, done, say) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.DrainPost, strings.NewReader(""))
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
	terminate(cmd)
	if wait() {
		return
	}
	say("%s ignored terminate for %s; killing", o.Engine, o.Grace)
	kill(cmd)
}
