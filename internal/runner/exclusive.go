// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// exclusiveTimeout bounds each nvidia-smi call the exclusive mode makes.
const exclusiveTimeout = 30 * time.Second

// exclusive puts the granted cards (their UUIDs, comma separated) in the
// driver's exclusive-process compute mode, so the run's process is the only
// one that can open a CUDA context there, and returns the func that puts
// them back to the default mode. It refuses when another compute process is
// already on the cards: in exclusive mode that process would lock the run
// itself out. Setting the mode needs root, through `sudo -n`, so a missing
// sudo rule refuses the run instead of starting it without the guard.
func exclusive(o Options, uuids string) (func(func(string, ...any)), error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("exclusive compute mode is Linux only")
	}
	if uuids == "" {
		return nil, errors.New("the granted card has no NVIDIA UUID")
	}
	out, err := o.CLI([]string{"nvidia-smi", "-i", uuids, "--query-compute-apps=pid,process_name", "--format=csv,noheader"}, exclusiveTimeout)
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi could not list the compute processes: %v", err)
	}
	if apps := strings.TrimSpace(out); apps != "" {
		return nil, fmt.Errorf("another compute process is on the card (%s); exclusive mode would lock the training out", strings.ReplaceAll(apps, "\n", "; "))
	}
	set := func(mode string) error {
		_, err := o.CLI([]string{"sudo", "-n", "nvidia-smi", "-i", uuids, "-c", mode}, exclusiveTimeout)
		return err
	}
	if err := set("EXCLUSIVE_PROCESS"); err != nil {
		return nil, fmt.Errorf("could not set exclusive-process mode (it needs a sudo rule that runs nvidia-smi -c without a password): %v", err)
	}
	return func(say func(string, ...any)) {
		if err := set("DEFAULT"); err != nil {
			say("could not put %s back to the default compute mode: %v; run: sudo nvidia-smi -i %s -c DEFAULT", uuids, err, uuids)
		}
	}, nil
}
