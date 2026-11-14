// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// The timing and classification logic of the swap bench, kept free of HTTP
// and /proc so it is testable with fixtures.

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
)

// Kind is how a switch got to its engine: cold is the engine's first
// request in the run, warm is any switch back to it.
type Kind string

const (
	KindCold Kind = "cold"
	KindWarm Kind = "warm"
)

// Sample is one request through the gateway: which engine answered, how
// long to its first token and to the end, what it said, and the card and
// host readings taken before the request and after the previous engine
// had drained. VramBefore is the card as the gateway handed it over;
// VramAfter is the card once this engine holds it.
type Sample struct {
	Engine     string        `json:"engine"`
	Kind       Kind          `json:"kind"`
	TTFT       time.Duration `json:"ttft"`
	Total      time.Duration `json:"total"`
	Text       string        `json:"text"`
	Reasoning  string        `json:"reasoning,omitempty"`
	VramBefore uint64        `json:"vram_before"`
	VramAfter  uint64        `json:"vram_after"`
	MemBefore  uint64        `json:"mem_before"`
	MemAfter   uint64        `json:"mem_after"`
}

// kindOf is the engine's place in the run: cold on its first request,
// warm on every one after.
func kindOf(samples []Sample, engine string) Kind {
	for _, s := range samples {
		if s.Engine == engine {
			return KindWarm
		}
	}
	return KindCold
}

// streamResult is what one streamed request produced. TTFT runs from the
// moment the request was sent to the first token, so a cold start's wait
// for the berth and its load are in the number; Total runs to the end of
// the stream. Fail carries an error the stream itself reported.
type streamResult struct {
	TTFT  time.Duration
	Total time.Duration
	Text  string
	// Reasoning is what a thinking model streamed before its answer.
	Reasoning string
	Fail      string
}

// parseSSE reads an SSE stream into the first-token time and the text it
// carries. Lines starting with a colon are SSE comments, the gateway's
// keep-alive while an engine loads, and never count. Data lines join
// within one event per the SSE spec; [DONE] ends the stream; an event
// carrying an "error" key is the gateway's failure, not a token. The text
// is the delta content of every token event, which for a fixed prompt at
// temperature 0 is the model's whole answer.
func parseSSE(r io.Reader, at func() time.Time, start time.Time) (streamResult, error) {
	res := streamResult{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var pending []string
	var firstToken time.Time
	done := false

	dispatch := func() {
		if len(pending) == 0 {
			return
		}
		payload := strings.Join(pending, "\n")
		pending = nil
		if payload == "[DONE]" {
			done = true
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &fields) != nil {
			return // not an OpenAI-shaped event; nothing to measure
		}
		if _, failed := fields["error"]; failed {
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(payload), &e) == nil && e.Error.Message != "" {
				res.Fail = e.Error.Message
			} else {
				res.Fail = payload
			}
			return
		}
		if _, ok := fields["choices"]; !ok {
			return
		}
		if firstToken.IsZero() {
			firstToken = at()
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &ev) == nil && len(ev.Choices) > 0 {
			d := ev.Choices[0].Delta
			res.Text += d.Content
			res.Reasoning += d.ReasoningContent + d.Reasoning
		}
	}

	for !done && sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, ":"):
			// keep-alive comment
		case strings.HasPrefix(line, "data:"):
			pending = append(pending, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// event:, id: and retry: lines carry no body
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if !done {
		dispatch() // an event the stream ended without its blank line
	}
	if !firstToken.IsZero() {
		res.TTFT = firstToken.Sub(start)
	}
	res.Total = at().Sub(start)
	return res, nil
}

// median is the middle of xs, the mean of the two middles when it is even;
// 0 when xs is empty.
func median[T time.Duration | uint64](xs []T) T {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(xs))
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// p90 is the nearest-rank 90th percentile: the value at rank ceil(0.9n)
// of the sorted xs, which for n of one, two or three is the top value; 0
// when xs is empty.
func p90(xs []time.Duration) time.Duration {
	n := len(xs)
	if n == 0 {
		return 0
	}
	return slices.Sorted(slices.Values(xs))[(9*n+9)/10-1] // ceil(0.9n) without floats
}

// outputsIdentical says whether every sample of one engine carries the
// same greedy output, the check a parked resume passes or fails: a resume
// that is not bit-identical to the fresh run is not the same model state.
// One sample is identical to itself.
func outputsIdentical(ss []Sample) string {
	if len(ss) == 0 {
		return "n/a"
	}
	first := ss[0]
	empty := true
	for _, s := range ss {
		if s.Text != first.Text || s.Reasoning != first.Reasoning {
			return "no"
		}
		empty = empty && s.Text == "" && s.Reasoning == ""
	}
	if empty {
		return "n/a (empty)" // equal empty strings say nothing about a resume
	}
	return "yes"
}
