// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// The gateway starts each engine as its own `berth run` process, not in
// process: the run keeps the lease, drains and reattaches on its own, and
// outlives the gateway that asked for it. This file builds that command
// and reads its output; adopt.go decides what a restart may reuse.
package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/runner"
	"github.com/T-Crypt/berth/internal/sysd"
)

// spawnRun starts the run process for one engine, records it, and waits
// for its end while a follower feeds its log to the tail and the
// journal. The run writes the log file, not gateway pipes, so a gateway
// that exits cannot break its output.
func (g *Gateway) spawnRun(name string, s *started, o runner.Options) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logPath, err := g.engineLogPath(name)
	if err != nil {
		return 0, fmt.Errorf("engine log: %w", err)
	}
	// O_APPEND lets the follower truncate the file under a live writer.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("engine log: %w", err)
	}
	full := append([]string{exe}, runArgv(o)...)
	if g.ConfigPath != "" {
		full = append(full, "--config", g.ConfigPath)
	}
	full = append(full, "--")
	full = append(full, o.Argv...)
	// The gateway's own run wrapper: the engine's scope comes from the
	// run's --slice, which runArgv carries from the entry.
	full = sysd.ScopeArgv("berth-engine", name, "", full)
	cmd := exec.Command(full[0], full[1:]...)
	detach(cmd)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	g.log().Info("gateway starting engine", "engine", name, "log", logPath)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return 0, err
	}
	logFile.Close() // the run and its engine own the file now
	g.putRecord(engineRec{Engine: name, PID: cmd.Process.Pid,
		Start: procs.System{}.StartTime(cmd.Process.Pid), Started: time.Now()})
	stop := make(chan struct{})
	done := make(chan struct{})
	go g.followEngine(name, logPath, 0, stop, done, s)
	code := exitCodeOf(cmd, cmd.Wait())
	close(stop)
	<-done // the tail has the run's last lines before anything reads it
	g.dropRecord(name)
	return code, nil
}

// runArgv renders the options as `berth run`'s own flags; the engine's
// command follows the caller's "--".
func runArgv(o runner.Options) []string {
	args := []string{"run", "--socket", o.Socket, "--as", o.Engine}
	if o.Group != "" {
		args = append(args, "--group", o.Group)
	}
	if o.Card != "" {
		args = append(args, "--gpu", o.Card)
	}
	if o.APIKeyFile != "" {
		args = append(args, "--api-key-file", o.APIKeyFile)
	}
	if o.APIKeyEnv != "" {
		args = append(args, "--api-key-env", o.APIKeyEnv)
	}
	if o.ExitComfyUIIdle {
		args = append(args, "--exit-comfyui-idle")
	}
	if o.ExitComfyUIIdleAfter != 0 {
		args = append(args, "--exit-comfyui-idle-after", o.ExitComfyUIIdleAfter.String())
	}
	if o.Priority != "" {
		args = append(args, "--priority", o.Priority)
	}
	if o.Needs != 0 {
		args = append(args, "--needs-mib", strconv.FormatUint(o.Needs>>20, 10))
	}
	if o.Wait != 0 {
		args = append(args, "--wait", o.Wait.String())
	}
	if o.Pinned {
		args = append(args, "--pinned")
	}
	if o.Switched {
		args = append(args, "--switched")
	}
	if o.DrainPost != "" {
		args = append(args, "--drain-post", o.DrainPost)
	}
	if o.DrainSlots != "" {
		args = append(args, "--drain-slots", o.DrainSlots)
	}
	if o.DrainComfyUI != "" {
		args = append(args, "--drain-comfyui", o.DrainComfyUI)
	}
	if o.DrainMax != 0 {
		args = append(args, "--drain-max", o.DrainMax.String())
	}
	if o.Container != "" {
		args = append(args, "--container", o.Container)
	}
	if o.Compose != "" {
		args = append(args, "--compose", o.Compose)
	}
	if o.Grace != 0 {
		args = append(args, "--grace", o.Grace.String())
	}
	if o.FailClosed {
		args = append(args, "--fail-closed")
	}
	if o.Slice != "" {
		args = append(args, "--slice", o.Slice)
	}
	return args
}

// exitCodeOf is the run process's exit code, the engine's status among
// them.
func exitCodeOf(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1
	}
	if code := ee.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

// grantedLease is the lease ID in a "granted" line of a run's output.
func grantedLease(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "berth: granted ")
	if !ok {
		return "", false
	}
	if i := strings.Index(rest, ","); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}

// watchWriter feeds w and watches the complete lines going through, so
// the record learns the lease the moment the run is granted one.
type watchWriter struct {
	w    io.Writer
	see  func(line string)
	rest []byte
}

func (t *watchWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.rest = append(t.rest, p...)
	for {
		i := bytes.IndexByte(t.rest, '\n')
		if i < 0 {
			break
		}
		t.see(string(t.rest[:i]))
		t.rest = t.rest[i+1:]
	}
	if len(t.rest) > 4<<10 { // no line ends in sight; drop the head
		t.rest = nil
	}
	return n, err
}
