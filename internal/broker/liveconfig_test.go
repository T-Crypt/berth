// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"testing"
	"time"
)

// SetLiveConfig moves the scheduling fields a decision reads; identity and
// the process's own flag values stay where they are.
func TestSetLiveConfigUpdatesSchedulingFieldsOnly(t *testing.T) {
	b := &Broker{cfg: Config{
		Card: "gpu0", Reserve: 42, DefaultWait: time.Minute,
		Order: []string{"a"}, JuggleWindow: 0,
	}}
	b.SetLiveConfig(LiveConfig{
		Order: []string{"b"}, JuggleWindow: 9 * time.Minute,
		Allow: []string{"xorg"}, ParkFloor: 1024, ParkMax: 2048,
		ParkTTL: time.Hour, ParkResidualMax: 512, ParkPressure: 0.5,
	})
	if b.cfg.Order[0] != "b" || b.cfg.JuggleWindow != 9*time.Minute || b.cfg.Allow[0] != "xorg" ||
		b.cfg.ParkFloor != 1024 || b.cfg.ParkMax != 2048 || b.cfg.ParkTTL != time.Hour ||
		b.cfg.ParkResidualMax != 512 || b.cfg.ParkPressure != 0.5 {
		t.Fatalf("scheduling fields = %+v", b.cfg)
	}
	if b.cfg.Card != "gpu0" || b.cfg.Reserve != 42 || b.cfg.DefaultWait != time.Minute {
		t.Fatalf("identity or flag values moved: %+v", b.cfg)
	}
}
