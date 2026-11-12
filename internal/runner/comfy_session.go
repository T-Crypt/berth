// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/T-Crypt/berth/internal/comfy"
	"github.com/T-Crypt/berth/internal/httpauth"
)

// comfySession retains the original grant and work evidence across refused
// attempts. Failure timers belong to each wait, not to the session lifetime.
type comfySession struct {
	granted     time.Time
	baseline    map[string]bool
	observed    bool
	initialized bool
}

func (session *comfySession) wait(o Options, done <-chan struct{}, say func(string, ...any)) bool {
	every := o.pollEvery()
	bound := 4 * o.Grace
	if bound <= 0 {
		bound = 2 * time.Minute
	}
	if !o.ExitComfyUIIdle {
		if o.DrainMax > 0 {
			bound = o.DrainMax
		} else if !o.FailClosed {
			bound = 0
		}
	}
	last := ""
	idleAfter := o.ExitComfyUIIdleAfter
	wait := &boundaryWait{engine: o.Engine, max: o.DrainMax, refuse: o.FailClosed}
	var queueBad, histBad time.Time
	var idleNow bool
	// Capture history before the first queue read, so work completed during
	// that read belongs to this grant. History is used only by session release.
	if session.granted.IsZero() {
		session.granted = time.Now()
	}
	if !session.initialized {
		session.initialized = true
		if o.ExitComfyUIIdle {
			if keys, err := comfyHistoryKeys(o.DrainComfyUI, o.APIKey, session.granted); err == nil {
				session.baseline = keys
			}
		}
	}
	for {
		if o.yielded != nil && o.yielded() {
			say("ComfyUI yielded the card from inside its job; handing off")
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		running, pending, err := comfy.Queue(httpauth.WithBearer(ctx, o.APIKey), o.DrainComfyUI)
		cancel()
		var now string
		switch {
		case err != nil:
			idleNow = false
			if queueBad.IsZero() {
				queueBad = time.Now()
			}
			now = fmt.Sprintf("cannot read the ComfyUI queue (%v); waiting", err)
			wait.note("queue unreadable", "queue unreadable")
		default:
			queueBad = time.Time{}
			idleNow = running == 0 && pending == 0
			switch {
			case running > 0 || pending > 0:
				session.observed = true
				now = fmt.Sprintf("waiting for ComfyUI to finish: %d running, %d queued", running, pending)
				desc := fmt.Sprintf("%d jobs running, %d queued", running, pending)
				if running == 1 {
					desc = fmt.Sprintf("1 job running, %d queued", pending)
				}
				wait.note(desc, desc)
			case !o.ExitComfyUIIdle:
				say("ComfyUI queue empty; handing off")
				return true
			case session.baseline == nil:
				session.baseline, err = comfyHistoryKeys(o.DrainComfyUI, o.APIKey, session.granted)
				if err != nil {
					if histBad.IsZero() {
						histBad = time.Now()
					}
					if idleAfter > 0 && !session.observed && time.Since(session.granted) >= idleAfter {
						say("no ComfyUI job within %s; handing off", idleAfter)
						return true
					}
					now = fmt.Sprintf("cannot read ComfyUI history (%v); waiting", err)
				} else if session.observed {
					// A baseline obtained after watched work contains that
					// job. Its queue drain is already a completed session.
					say("ComfyUI queue drained after watched work; handing off")
					return true
				} else {
					histBad = time.Time{}
				}
			default:
				newJob, herr := comfyFirstNewJob(o.DrainComfyUI, o.APIKey, session.baseline)
				if herr == nil {
					histBad = time.Time{}
				}
				switch {
				case herr != nil:
					if histBad.IsZero() {
						histBad = time.Now()
					}
					if idleAfter > 0 && !session.observed && time.Since(session.granted) >= idleAfter {
						say("no ComfyUI job within %s; handing off", idleAfter)
						return true
					}
					now = fmt.Sprintf("cannot read ComfyUI history (%v); waiting", herr)
				case newJob:
					return true
				case idleAfter > 0 && !session.observed && time.Since(session.granted) >= idleAfter:
					say("no ComfyUI job within %s; handing off", idleAfter)
					return true
				default:
					now = "waiting for the first ComfyUI job of this session"
				}
			}
		}
		// A history failure cannot prove that a readable busy queue is idle.
		if bound > 0 && ((!queueBad.IsZero() && time.Since(queueBad) >= bound) ||
			(!histBad.IsZero() && idleNow && time.Since(histBad) >= bound)) {
			if o.FailClosed {
				say("drain refused: no readable ComfyUI boundary within %s; %s keeps its lease", bound, o.Engine)
				return false
			}
			say("no readable ComfyUI boundary within %s; handing off", bound)
			return true
		}
		if now != last {
			say("%s", now)
			last = now
		}
		// DrainMax bounds a requested drain, not the first-job detector.
		if !o.ExitComfyUIIdle && wait.tick(say) {
			return !o.FailClosed
		}
		select {
		case <-done:
			return false
		case <-time.After(every):
		}
	}
}

// comfyHistoryKeys excludes timestamped terminal work from the baseline
// when it belongs to the current grant.
func comfyHistoryKeys(base, key string, since time.Time) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	history, err := comfy.History(httpauth.WithBearer(ctx, key), base)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(history))
	for k, record := range history {
		if since.IsZero() || !comfyCompletedAfter(record, since) {
			keys[k] = true
		}
	}
	return keys, nil
}

// comfyCompletedAfter accepts only terminal work recorded in this grant. Missing,
// malformed, or future timestamps cannot prove that lifetime history is new work.
func comfyCompletedAfter(raw json.RawMessage, since time.Time) bool {
	var record struct {
		Status struct {
			Messages [][]json.RawMessage `json:"messages"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &record) != nil {
		return false
	}
	for _, message := range record.Status.Messages {
		if len(message) != 2 {
			continue
		}
		var event string
		if json.Unmarshal(message[0], &event) != nil ||
			(event != "execution_success" && event != "execution_error" && event != "execution_interrupted") {
			continue
		}
		var detail struct {
			Timestamp int64 `json:"timestamp"`
		}
		if json.Unmarshal(message[1], &detail) == nil {
			completed := time.UnixMilli(detail.Timestamp)
			if completed.After(since) && !completed.After(time.Now()) {
				return true
			}
		}
	}
	return false
}

func comfyFirstNewJob(base, key string, baseline map[string]bool) (bool, error) {
	keys, err := comfyHistoryKeys(base, key, time.Time{})
	if err != nil {
		return false, err
	}
	for k := range keys {
		if !baseline[k] {
			return true, nil
		}
	}
	return false, nil
}
