// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Package runs records engines that started without a lease: berthd was
// down (fail-open), or went away mid-run. The wrapper writes one small file
// per run beside the socket, and berthd lists the live ones, so an
// unprotected engine is never invisible.
package runs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/fsx"
)

// Record is one unprotected run, identified by PID plus start time.
type Record struct {
	Engine  string    `json:"engine"`
	PID     int       `json:"pid"`
	Start   uint64    `json:"pid_start"`
	Started time.Time `json:"started"`
	Reason  string    `json:"reason"`
	// Wrapper is the berth run that wrote the record, the one process that
	// can re-attach the engine. 0 in a record from before v0.5.1.
	Wrapper      int    `json:"wrapper_pid,omitempty"`
	WrapperStart uint64 `json:"wrapper_start,omitempty"`
	// Training marks a berth train run. While it lives without a lease, it
	// still locks the node: berth run will not start an engine without
	// berthd, and a new berthd refuses other requests until it re-attaches.
	Training bool `json:"training,omitempty"`
}

// WrapperState is "alive" while the berth run that wrote r still runs,
// "exited" once it is gone (or its PID is someone else's), and "" when the
// record does not say which berth run wrote it.
func (r Record) WrapperState(start func(pid int) uint64) string {
	if r.Wrapper <= 0 || r.WrapperStart == 0 {
		return ""
	}
	if start(r.Wrapper) == r.WrapperStart {
		return "alive"
	}
	return "exited"
}

// ReasonLost is the reason berth run records when its daemon goes away
// mid-run. A daemon that starts while such a run is alive gives it a
// window to re-attach before granting anything new.
const ReasonLost = "berthd went away mid-run"

// ReasonStart is the reason berth run records when no daemon answered as
// it started the engine (a service that starts beside berthd at boot can
// dial first). The run keeps dialling, so it re-attaches like a lost one.
const ReasonStart = "berthd unreachable at start"

// ReasonKeepUp is the reason berth run records when a keep-up drain leaves
// the engine running with its models unloaded: the process stays on its
// port, but it no longer holds the card, so the released lease must not
// keep blocking waiters on its idle footprint.
const ReasonKeepUp = "keep-up drain: models unloaded, server left up"

// Dir is where records live for a given socket path.
func Dir(socket string) string {
	return filepath.Join(filepath.Dir(socket), "berth-unprotected")
}

func path(dir string, pid int) string {
	return filepath.Join(dir, strconv.Itoa(pid)+".json")
}

// Write stores r, readable only by its owner, and returns a func that removes it.
func Write(dir string, r Record) (func(), error) {
	b, err := json.Marshal(r)
	if err != nil {
		return func() {}, err
	}
	p := path(dir, r.PID)
	if err := fsx.WriteFile(p, b, 0o600); err != nil {
		return func() {}, err
	}
	return func() { os.Remove(p) }, nil
}

// Training is a live training record in dir, and whether there is one.
func Training(dir string, alive func(pid int) uint64) (Record, bool) {
	for _, r := range List(dir, alive) {
		if r.Training {
			return r, true
		}
	}
	return Record{}, false
}

// List returns the records whose process is still the same process
// (alive reports its current start time, 0 if gone), and deletes the rest.
func List(dir string, alive func(pid int) uint64) []Record {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		var r Record
		if err != nil || json.Unmarshal(b, &r) != nil {
			os.Remove(p)
			continue
		}
		if st := alive(r.PID); st == 0 || st != r.Start {
			os.Remove(p)
			continue
		}
		out = append(out, r)
	}
	return out
}
