package engines

import (
	"strings"
	"time"
)

// Busy says whether the engine a lease belongs to is serving right now: a
// request in flight (a llama.cpp slot processing, vLLM/SGLang requests
// running), queued work (ComfyUI's queue), or a model still loading. name is
// a lease's group or engine name: an engine's own name, or a model one of the
// engines lists (llama-swap leases each model under its own name). known is
// false when no fresh reading answers that, and the caller should assume busy.
func (w *Watcher) Busy(name string) (busy, known bool) {
	if w == nil || name == "" {
		return false, false
	}
	every := w.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	stale := 3*every + 5*time.Second
	for _, e := range w.Snapshot() {
		match := strings.EqualFold(e.Name, name)
		if !match {
			for _, m := range e.Models {
				if strings.EqualFold(m.Name, name) {
					match = true
					break
				}
			}
		}
		if !match {
			continue
		}
		if !e.Up || time.Duration(e.Age*float64(time.Second)) > stale {
			return false, false // down, loading, or an old reading: nobody can say it is idle
		}
		if e.Metrics.Processing > 0 || e.Metrics.Waiting > 0 {
			return true, true
		}
		for _, s := range e.Slots {
			if s.Processing {
				return true, true
			}
		}
		for _, m := range e.Models {
			if m.State != "" && m.State != "ready" && m.State != "loaded" {
				return true, true // starting or stopping
			}
		}
		return false, true
	}
	return false, false
}
