// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import "time"

// LiveConfig is what a config reload may change on a running broker: the
// scheduling fields berth.toml names. Identity and wiring (card, events,
// files, readers) are start-up facts, and the flag values (reserve, waits,
// grace) are the process's own: neither moves.
type LiveConfig struct {
	Order           []string
	JuggleWindow    time.Duration
	Allow           []string
	Comfy           []ComfyEngine
	ParkFloor       uint64
	ParkMax         uint64
	ParkTTL         time.Duration
	ParkResidualMax uint64
	ParkPressure    float64
}

// SetLiveConfig applies a reloaded config's scheduling fields. A drain or a
// park that started under the old rules runs out under them; new decisions
// use the new ones.
func (b *Broker) SetLiveConfig(l LiveConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.Order = l.Order
	b.cfg.JuggleWindow = l.JuggleWindow
	b.cfg.Allow = l.Allow
	b.cfg.Comfy = l.Comfy
	b.cfg.ParkFloor = l.ParkFloor
	b.cfg.ParkMax = l.ParkMax
	b.cfg.ParkTTL = l.ParkTTL
	b.cfg.ParkResidualMax = l.ParkResidualMax
	b.cfg.ParkPressure = l.ParkPressure
}
