// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// clockOf is a fake time source: the calls take the given stamps in
// order, and any call past the list keeps the last one.
func clockOf(ts ...time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		if i < len(ts) {
			t := ts[i]
			i++
			return t
		}
		return ts[len(ts)-1]
	}
}

func TestParseSSE(t *testing.T) {
	t0 := time.Now()
	tests := []struct {
		name  string
		in    string
		clock func() time.Time
		want  streamResult
	}{
		{
			// The real shape of a gateway stream: keep-alive comments
			// while the engine loads, the role chunk, the content
			// chunks, [DONE].
			name: "stream with keep-alive comments",
			in: `: berth: loading strata, 10 s

data: {"id":"1","choices":[{"delta":{"role":"assistant"},"index":0}]}

: berth: loading strata, 20 s

data: {"id":"1","choices":[{"delta":{"content":"swap"},"index":0}]}

data: {"id":"1","choices":[{"delta":{"content":" bench"},"index":0}]}

data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop","index":0}]}

data: [DONE]
`,
			clock: clockOf(t0.Add(25*time.Second), t0.Add(40*time.Second)),
			want:  streamResult{TTFT: 25 * time.Second, Total: 40 * time.Second, Text: "swap bench"},
		},
		{
			name: "comments only, no token",
			in: `: berth: loading comfyui, 10 s

: berth: loading comfyui, 20 s

`,
			clock: clockOf(t0.Add(20 * time.Second)),
			want:  streamResult{Total: 20 * time.Second},
		},
		{
			// The gateway's failure for a streaming request: one error
			// event, then [DONE]. The error is not a token.
			name: "gateway failure event",
			in: `data: {"error":{"message":"no engine on this node serves \"x\"","type":"berth"}}

data: [DONE]
`,
			clock: clockOf(t0.Add(5 * time.Second)),
			want:  streamResult{Total: 5 * time.Second, Fail: `no engine on this node serves "x"`},
		},
		{
			// SSE allows one data field split over lines of the same
			// event; the parser joins them before reading JSON. The
			// split sits between JSON tokens, where the newline the join
			// inserts is legal whitespace.
			name: "event split over data lines",
			in: `data: {"choices":[{"delta":
data: {"content":"swap"}}]}

data: [DONE]
`,
			clock: clockOf(t0.Add(3*time.Second), t0.Add(6*time.Second)),
			want:  streamResult{TTFT: 3 * time.Second, Total: 6 * time.Second, Text: "swap"},
		},
		{
			// A thinking model streams its reasoning before its answer:
			// llama.cpp, NInfer and Strata as reasoning_content, others as
			// reasoning. It is a token, and it is kept apart from the answer.
			name: "reasoning then answer",
			in: `data: {"choices":[{"delta":{"reasoning_content":"one "}}]}

data: {"choices":[{"delta":{"reasoning":"line"}}]}

data: {"choices":[{"delta":{"content":"ok"}}]}

data: [DONE]
`,
			clock: clockOf(t0.Add(2*time.Second), t0.Add(4*time.Second)),
			want:  streamResult{TTFT: 2 * time.Second, Total: 4 * time.Second, Text: "ok", Reasoning: "one line"},
		},
		{
			name:  "trailing event without blank line or DONE",
			in:    "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}",
			clock: clockOf(t0.Add(time.Second), t0.Add(time.Second)),
			want:  streamResult{TTFT: time.Second, Total: time.Second, Text: "hi"},
		},
		{
			name: "data that is not OpenAI JSON counts nothing",
			in: `data: <html>gateway error</html>

data: [DONE]
`,
			clock: clockOf(t0.Add(2 * time.Second)),
			want:  streamResult{Total: 2 * time.Second},
		},
		{
			name:  "CRLF line endings",
			in:    "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\ndata: [DONE]\r\n",
			clock: clockOf(t0.Add(4*time.Second), t0.Add(8*time.Second)),
			want:  streamResult{TTFT: 4 * time.Second, Total: 8 * time.Second, Text: "a"},
		},
		{
			// A stream that ends on [DONE] stops reading: anything
			// after must not reach the parser's bookkeeping.
			name: "junk after DONE is ignored",
			in: `data: {"choices":[{"delta":{"content":"ok"}}]}

data: [DONE]
data: {"choices":[{"delta":{"content":" more"}}]}

`,
			clock: clockOf(t0.Add(time.Second), t0.Add(time.Second)),
			want:  streamResult{TTFT: time.Second, Total: time.Second, Text: "ok"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSSE(strings.NewReader(tt.in), tt.clock, t0)
			if err != nil {
				t.Fatalf("parseSSE: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseSSE = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestKindOf(t *testing.T) {
	samples := []Sample{
		{Engine: "a"},
		{Engine: "b"},
		{Engine: "a"},
		{Engine: "comfyui"},
		{Engine: "b"},
	}
	for i, s := range samples {
		got := kindOf(samples[:i], s.Engine)
		want := KindCold
		for _, p := range samples[:i] {
			if p.Engine == s.Engine {
				want = KindWarm
				break
			}
		}
		if got != want {
			t.Errorf("kind of sample %d (%s) = %s, want %s", i, s.Engine, got, want)
		}
	}
}

func TestMedian(t *testing.T) {
	s := func(x ...int) []time.Duration {
		out := make([]time.Duration, len(x))
		for i, v := range x {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	tests := []struct {
		name string
		xs   []time.Duration
		want time.Duration
	}{
		{"empty", nil, 0},
		{"one", s(7), 7 * time.Millisecond},
		{"odd", s(30, 10, 20), 20 * time.Millisecond},
		{"even", s(10, 20, 30, 40), 25 * time.Millisecond},
		{"unsorted even", s(40, 10, 30, 20), 25 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := median(tt.xs); got != tt.want {
			t.Errorf("%s: median = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestP90(t *testing.T) {
	s := func(x ...int) []time.Duration {
		out := make([]time.Duration, len(x))
		for i, v := range x {
			out[i] = time.Duration(v) * time.Millisecond
		}
		return out
	}
	tests := []struct {
		name string
		xs   []time.Duration
		want time.Duration
	}{
		{"empty", nil, 0},
		{"one", s(5), 5 * time.Millisecond},
		{"three takes the top", s(10, 20, 30), 30 * time.Millisecond},
		{"ten takes the ninth", s(10, 20, 30, 40, 50, 60, 70, 80, 90, 100), 90 * time.Millisecond},
		{"twelve takes the eleventh", s(10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120), 110 * time.Millisecond},
		{"unsorted", s(90, 10, 30, 50, 70, 20, 40, 60, 80, 100), 90 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := p90(tt.xs); got != tt.want {
			t.Errorf("%s: p90 = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestMedianBytes(t *testing.T) {
	if got := median([]uint64(nil)); got != 0 {
		t.Errorf("median(nil) = %d", got)
	}
	if got := median([]uint64{300, 100, 200}); got != 200 {
		t.Errorf("median = %d, want 200", got)
	}
	if got := median([]uint64{100, 200, 300, 400}); got != 250 {
		t.Errorf("medianU64 even = %d, want 250", got)
	}
}

func TestOutputsIdentical(t *testing.T) {
	tests := []struct {
		name    string
		samples []Sample
		want    string
	}{
		{"none", nil, "n/a"},
		{"one", []Sample{{Text: "swap bench ok"}}, "yes"},
		{"all the same", []Sample{{Text: "swap bench ok"}, {Text: "swap bench ok"}, {Text: "swap bench ok"}}, "yes"},
		{"a resume that drifts", []Sample{{Text: "swap bench ok"}, {Text: "swap bench ok "}}, "no"},
		{"thinking that drifts", []Sample{{Reasoning: "ok, one line", Text: ""}, {Reasoning: "ok one line", Text: ""}}, "no"},
		{"the same thinking, no answer yet", []Sample{{Reasoning: "ok, one line"}, {Reasoning: "ok, one line"}}, "yes"},
		{"empty answers prove nothing", []Sample{{}, {}}, "n/a (empty)"},
	}
	for _, tt := range tests {
		if got := outputsIdentical(tt.samples); got != tt.want {
			t.Errorf("%s: outputsIdentical = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestParseVmRSS(t *testing.T) {
	status := "Name:\tswap\nUmask:\t0022\nState:\tS (sleeping)\nVmPeak:\t  120000 kB\nVmSize:\t  110000 kB\nVmRSS:\t  98765 kB\nThreads:\t1\n"
	if got, ok := parseVmRSS(status); !ok || got != 98765*1024 {
		t.Errorf("parseVmRSS = %d, %v, want %d and true", got, ok, 98765*1024)
	}
	if _, ok := parseVmRSS("Name:\tswap\nVmSize:\t 100 kB\n"); ok {
		t.Error("a status file without VmRSS must read as unknown")
	}
	if _, ok := parseVmRSS(""); ok {
		t.Error("an empty status file must read as unknown")
	}
}

func TestChatBody(t *testing.T) {
	var body struct {
		Model       string    `json:"model"`
		Stream      bool      `json:"stream"`
		Temperature float64   `json:"temperature"`
		MaxTokens   int       `json:"max_tokens"`
		Seed        int       `json:"seed"`
		Messages    []message `json:"messages"`
	}
	raw := chatBody("model-x", 256, false)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("chatBody is not JSON: %v", err)
	}
	if body.Model != "model-x" || !body.Stream || body.Temperature != 0 || body.MaxTokens != 256 || body.Seed != 1 {
		t.Errorf("chatBody fields = %+v", body)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != prompt {
		t.Errorf("chatBody message = %+v", body.Messages)
	}
	if strings.Contains(string(raw), "chat_template_kwargs") {
		t.Errorf("a plain request carries chat_template_kwargs: %s", raw)
	}
	if got := string(chatBody("model-x", 64, true)); !strings.Contains(got, `"chat_template_kwargs":{"enable_thinking":false}`) || !strings.Contains(got, `"max_tokens":64`) {
		t.Errorf("-no-think body = %s", got)
	}
}
