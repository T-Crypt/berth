// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"testing"
	"time"
)

// Training finds a live training record and passes over the rest: a plain
// run, and a training whose process is gone.
func TestTrainingIsALiveTrainingRecord(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []Record{
		{Engine: "chat", PID: 10, Start: 1, Started: time.Now(), Reason: ReasonLost},
		{Engine: "train-old", PID: 11, Start: 1, Started: time.Now(), Reason: ReasonLost, Training: true},
		{Engine: "train-a", PID: 12, Start: 5, Started: time.Now(), Reason: ReasonLost, Training: true},
	} {
		if _, err := Write(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	alive := func(pid int) uint64 {
		return map[int]uint64{10: 1, 12: 5}[pid]
	}
	r, ok := Training(dir, alive)
	if !ok || r.Engine != "train-a" {
		t.Fatalf("Training = %+v, %v; want train-a", r, ok)
	}
	if _, ok := Training(t.TempDir(), alive); ok {
		t.Error("an empty directory has a training")
	}
}
