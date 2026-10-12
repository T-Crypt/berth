// Package runner is `berth run`: acquire a berth, start the engine, hand
// the berth back by stopping the engine when berthd asks, and exit with the
// engine's status. If berthd is down the engine runs anyway (fail-open), so
// Berth never becomes the reason a model cannot load.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	Grace        time.Duration // per drain step before escalating
	FailClosed   bool          // refuse to start when berthd is unreachable
	Retry        time.Duration // how often a run that lost berthd dials it again; 0 = 2s
	Argv         []string
	Stderr       io.Writer
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

	var drainOnce sync.Once
	go func() {
		for {
			err := listen(conn, func(reason string) {
				drainOnce.Do(func() { go drain(o, cmd, done, reason, say) })
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

	err = cmd.Wait()
	close(done)
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

// drain stops the engine politely, then less so: the drain URL (llama-swap
// unloads the model and stops us itself), then a terminate to the process
// group, then a kill. Each step gets Grace to work.
func drain(o Options, cmd *exec.Cmd, done <-chan struct{}, reason string, say func(string, ...any)) {
	say("draining %s: %s", o.Engine, reason)
	if o.DrainComfyUI != "" && !jobBoundary(o, done, say) {
		return
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
