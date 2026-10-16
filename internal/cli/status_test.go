package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
	"github.com/T-Crypt/berth/internal/server"
)

// emptyCard is a machine whose card reports no processes, so the daemon below
// has nothing to report but its own state.
type emptyCard struct{}

func (emptyCard) Read(context.Context) (gpu.Snapshot, error) {
	return gpu.Snapshot{Total: 24 << 30, Free: 24 << 30, At: time.Now()}, nil
}

// startDaemon runs a real berthd on a temp socket: the CLI is then driven
// through the real wire, which is the only way to verify a document that has to
// match what the daemon sends.
func startDaemon(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "berth.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := broker.New(broker.Config{
		Poll: 20 * time.Millisecond, DefaultWait: 5 * time.Second, JuggleWindow: -1,
		Version: "test", Log: log,
	}, emptyCard{}, procs.System{})
	ln, err := server.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	ev := alerts.New(alerts.FromConfig(config.AlertsConfig{}), 3, nil)
	lastAlerts := &atomic.Pointer[proto.Alerts]{}
	lastAlerts.Store(ev.Snapshot())
	go server.Serve(ctx, ln, b, server.Extras{Alerts: lastAlerts.Load}, log)
	return sock
}

func TestStatusHumanAgainstADaemon(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "status", "--socket", sock)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	if !strings.Contains(out, "berthd test on "+sock) {
		t.Errorf("stdout %q lacks the daemon it read", out)
	}
	if !strings.Contains(out, "no leases, nobody waiting, no squatters") {
		t.Errorf("stdout %q lacks the empty state", out)
	}
}

func TestStatusJSONIsTheWireDocument(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, _ := runCLI(t, "status", "--socket", sock, "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var st proto.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if st.API != proto.APIVersion {
		t.Errorf("api = %d, want %d", st.API, proto.APIVersion)
	}
	if len(st.GPUs) != 1 || st.GPUs[0].Scheduled != true {
		t.Errorf("gpus = %+v", st.GPUs)
	}
}

func TestStatusYAML(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, _ := runCLI(t, "status", "--socket", sock, "--format", "yaml")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var st proto.Status
	if err := yaml.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("yaml %q: %v", out, err)
	}
	if st.API != proto.APIVersion {
		t.Errorf("api = %d, want %d", st.API, proto.APIVersion)
	}
}

// berthd unreachable is a system failure: exit 2, the reason on stderr, and
// nothing on stdout, in every format.
func TestStatusUnreachable(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.sock")
	for _, args := range [][]string{
		{"status", "--socket", missing},
		{"status", "--socket", missing, "--format", "json"},
		{"-q", "status", "--socket", missing},
	} {
		code, out, errOut := runCLI(t, args...)
		if code != ExitSystem {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitSystem)
		}
		if out != "" {
			t.Errorf("%v: stdout = %q, want empty", args, out)
		}
		if !strings.Contains(errOut, "[ERROR] berthd unreachable on "+missing) {
			t.Errorf("%v: stderr = %q", args, errOut)
		}
	}
}

// Without -f the backlog is printed and the command exits, so this terminates.
func TestEventsAgainstADaemon(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "events", "--socket", sock)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "  ") {
			t.Errorf("line %q is not an event line", line)
		}
	}
}

// --format json is one event per line, which is what a follower pipes into jq.
func TestEventsJSONIsLineDelimited(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, out, errOut := runCLI(t, "events", "--socket", sock, "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var e proto.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %q is not one event: %v", line, err)
		}
	}
}

func TestEventsBadSince(t *testing.T) {
	isolate(t)
	sock := startDaemon(t)
	code, _, errOut := runCLI(t, "events", "--socket", sock, "--since", "abc")
	if code != ExitSystem {
		t.Fatalf("exit = %d, want %d", code, ExitSystem)
	}
	if !strings.Contains(errOut, "invalid value") {
		t.Errorf("stderr = %q", errOut)
	}
}
