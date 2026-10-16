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
)

// Record is one unprotected run, identified by PID plus start time.
type Record struct {
	Engine  string    `json:"engine"`
	PID     int       `json:"pid"`
	Start   uint64    `json:"pid_start"`
	Started time.Time `json:"started"`
	Reason  string    `json:"reason"`
}

// ReasonLost is the reason berth run records when its daemon goes away
// mid-run. A daemon that starts while such a run is alive gives it a
// window to re-attach before granting anything new.
const ReasonLost = "berthd went away mid-run"

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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return func() {}, err
	}
	p := path(dir, r.PID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return func() {}, err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return func() {}, err
	}
	return func() { os.Remove(p) }, nil
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
