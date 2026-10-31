// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package runner

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// stopSignals: Ctrl-C and Ctrl-Break reach berth run as os.Interrupt. Closing
// the console window, logging off and shutting down arrive as SIGTERM. Windows
// gives the process about 5 s after that, so the engine's drain must be quick.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// jobs holds each engine's job object, so a drain reaches every process the
// engine starts (a server that spawns its runner, like Strata's), as the
// process group does on Unix. No kill-on-close: an engine outlives a dead
// berth run, as on Unix, and is recorded as an unprotected run.
var jobs sync.Map // pid -> windows.Handle

// isolate starts the engine in its own console process group, so a
// Ctrl-Break meant for it does not also stop berth run.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// scopeToChild has no user manager to answer to on this platform; Job
// objects and the attribution they allow come in a later ticket.
func scopeToChild(as, slice string, argv []string, log *slog.Logger) []string { return argv }

// afterStart puts the engine in a job object. A child it spawns in the first
// instant before this lands escapes the job; the drain still reaches the
// engine itself, and its memory still shows as a stowaway, never hidden.
func afterStart(cmd *exec.Cmd) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return
	}
	jobs.Store(cmd.Process.Pid, job)
}

// afterWait drops the engine's job once it has exited, so the handle does not
// leak and a later process with the same pid never finds a stale job.
func afterWait(cmd *exec.Cmd) {
	if j, ok := jobs.LoadAndDelete(cmd.Process.Pid); ok {
		_ = windows.CloseHandle(j.(windows.Handle))
	}
}

// signalGroup forwards a stop to the engine's process group as Ctrl-Break,
// the one console signal another process group can receive.
func signalGroup(cmd *exec.Cmd, _ os.Signal) { terminate(cmd) }

func terminate(cmd *exec.Cmd) {
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)); err != nil {
		kill(cmd) // no shared console (a service, a detached start): nothing polite left to try
	}
}

func kill(cmd *exec.Cmd) {
	if j, ok := jobs.LoadAndDelete(cmd.Process.Pid); ok {
		job := j.(windows.Handle)
		err := windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		if err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

func exitCode(_ *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}
