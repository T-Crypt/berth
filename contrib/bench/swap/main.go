// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

// Command swap measures what a model switch through the Berth gateway
// costs. It asks the gateway for model A once, then per cycle for model B
// and A again, so each request forces the gateway to drain the running engine
// and take the other. For every request it records the time to the
// engine's first token, the total time, the card's used VRAM before the
// request and after the previous engine drained, the host's available RAM
// before and after, and the greedy output of one fixed prompt, so a
// parked resume can be checked against the fresh run for identical
// output.
//
// It runs against a live node: berthd, the gateway, and the engines the
// two models name. It takes the card only through the gateway, the
// supported path, and starts or stops nothing itself. Run it only when
// the card can be given to the bench.
//
//	go run ./contrib/bench/swap -a MODEL_A -b MODEL_B -cycles 5
//
// -comfy WORKFLOW.json swaps the B slot for a ComfyUI workflow posted to
// the gateway's /prompt, timed until ComfyUI's /history shows it done.
// -leak N runs N A-B cycles and prints the card's used VRAM after drain
// and the gateway's RSS, read from /proc at the PID of -gateway-pid, at
// cycle 1 and cycle N, the two points a leak check compares.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/host"
)

// prompt is the fixed greedy prompt of every request: short enough that
// a short answer finishes it, specific enough that a broken resume is obvious.
const prompt = "Reply with exactly one line: swap bench ok"

// requestTimeout bounds one request. A cold start may wait ten minutes
// for its berth and then load the model, so the client outlasts the
// gateway's own ready timeout.
const requestTimeout = 25 * time.Minute

type flags struct {
	gateway    string
	modelA     string
	modelB     string
	cycles     int
	comfy      string
	leak       int
	gwPID      int
	gpuIndex   int
	gpuBackend string
	keyFile    string
	jsonOut    bool
	maxTokens  int
	noThink    bool
}

func parseFlags() (flags, error) {
	fs := flag.NewFlagSet("swap", flag.ContinueOnError)
	gateway := fs.String("gateway", "http://127.0.0.1:9799", "the gateway base URL")
	a := fs.String("a", "", "model A")
	b := fs.String("b", "", "model B (replaced by the -comfy workflow when that is set)")
	cycles := fs.Int("cycles", 5, "how many B-then-A cycles to run after the opening A")
	comfy := fs.String("comfy", "", "path to a ComfyUI workflow JSON; its slot replaces model B")
	leak := fs.Int("leak", 0, "run N A-B cycles and print VRAM after drain and the gateway RSS at cycle 1 and N")
	gwPID := fs.Int("gateway-pid", 0, "the gateway's PID, for the RSS reading of -leak (Linux)")
	gpuIndex := fs.Int("gpu-index", 0, "the card the bench reads")
	gpuBackend := fs.String("gpu-backend", "auto", "GPU probe: auto, nvidia or amd (AMD index: DRM cardN)")
	key := fs.String("key", "", "the gateway's API key file (default: the berth api.key)")
	jsonOut := fs.Bool("json", false, "print the raw samples as JSON")
	maxTokens := fs.Int("max-tokens", 256, "the max_tokens of each request; a thinking model needs room to finish thinking")
	noThink := fs.Bool("no-think", false, "ask the model to answer without thinking (chat_template_kwargs enable_thinking=false, where the server honours it)")
	fs.Usage = func() {
		usage(fs)
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return flags{}, err
	}
	f := flags{*gateway, *a, *b, *cycles, *comfy, *leak, *gwPID, *gpuIndex, *gpuBackend, *key, *jsonOut, *maxTokens, *noThink}
	if err := gpu.ValidateSelection(f.gpuBackend, f.gpuIndex); err != nil {
		return f, err
	}
	if f.comfy != "" && f.leak > 0 {
		return f, fmt.Errorf("-comfy and -leak do not combine: the leak check reads A-B cycles")
	}
	if f.leak > 0 {
		f.cycles = f.leak
		if f.modelA == "" || f.modelB == "" {
			return f, fmt.Errorf("the leak check needs both models (-a and -b)")
		}
		if f.gwPID <= 0 {
			return f, fmt.Errorf("-leak needs -gateway-pid (the gateway's PID)")
		}
	} else if f.comfy == "" && (f.modelA == "" || f.modelB == "") {
		return f, fmt.Errorf("name both models (-a and -b), or use -comfy or -leak")
	}
	if f.cycles < 1 {
		return f, fmt.Errorf("run at least one cycle")
	}
	return f, nil
}

func usage(fs *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `swap: measure model switch cost through the Berth gateway

usage: swap -a MODEL_A -b MODEL_B [flags]

The bench asks the gateway for model A once, then per cycle for model B
and A again; each request drains the running engine and takes the other. For
every request it records the time to first token, the total time, the
card's used VRAM before the request and after the previous engine
drained, the host's available RAM before and after, and the greedy
output of one fixed prompt (temperature 0, seed 1, 32 tokens).

Each engine's first request in the run is a cold switch; the rest are
warm. When park is on for an engine, its warm switches are the parked
numbers: the engine stays in RAM and the switch back is a resume.

-leak N runs N A-B cycles and prints the card's used VRAM after drain
and the gateway's RSS (from /proc, the PID of -gateway-pid) at cycle 1
and cycle N. -comfy WORKFLOW.json swaps model B for a ComfyUI workflow
posted to /prompt, timed until /history shows it done.

flags:
`)
	fs.PrintDefaults()
}

// leakPoint is one reading of the -leak check: the card's used VRAM once
// the cycle's engine holds it, and the gateway's own RSS.
type leakPoint struct {
	Cycle    int    `json:"cycle"`
	Vram     uint64 `json:"vram"`
	RSS      uint64 `json:"rss"`
	RSSKnown bool   `json:"rss_known"`
}

type run struct {
	f        flags
	key      string
	http     *http.Client
	probe    gpu.Probe
	baseline uint64 // card used VRAM at the start of the run, bytes
	card     string
	samples  []Sample
	leakA    leakPoint
	leakB    leakPoint
}

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintln(os.Stderr, "swap:", err)
		os.Exit(1)
	}
}

func realMain() error {
	f, err := parseFlags()
	if err != nil {
		return err
	}
	r, err := newRun(f)
	if err != nil {
		return err
	}
	if err := r.run(context.Background()); err != nil {
		r.report()
		return err
	}
	r.report()
	return nil
}

// newRun resolves the API key and takes the run's first card reading,
// which doubles as the pre-load baseline the park numbers are judged
// against.
func newRun(f flags) (*run, error) {
	keyFile := f.keyFile
	if keyFile == "" {
		keyFile = apikeyPath()
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the gateway key from %s: %v (pass -key to name another file)", keyFile, err)
	}
	key := strings.TrimSpace(string(b))
	if len(key) < 16 {
		return nil, fmt.Errorf("%s holds no usable key (under 16 characters)", keyFile)
	}
	probe, err := gpu.Open(gpu.Options{Backend: f.gpuBackend, Index: f.gpuIndex})
	if err != nil {
		return nil, err
	}
	r := &run{
		f:     f,
		key:   key,
		http:  &http.Client{},
		probe: probe,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snap, err := r.probe.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the card: %w", err)
	}
	r.baseline = snap.Used
	r.card = snap.Name
	return r, nil
}

// apikeyPath mirrors the gateway's key location: $BERTH_API_KEY_FILE,
// else api.key in the berth config directory.
func apikeyPath() string {
	if p := os.Getenv("BERTH_API_KEY_FILE"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "berth-api.key"
	}
	return dir + "/berth/api.key"
}

// run executes the switch cycles, or the -leak program.
func (r *run) run(ctx context.Context) error {
	if r.f.leak > 0 {
		return r.runLeak(ctx)
	}
	// One opening request for A, then B and A per cycle: every request
	// after the first is a real switch, never a second ask of the engine
	// that already holds the card.
	if err := r.request(ctx, r.f.modelA); err != nil {
		return fmt.Errorf("opening request, model A: %w", err)
	}
	for i := 0; i < r.f.cycles; i++ {
		if r.f.comfy != "" {
			if err := r.comfy(ctx); err != nil {
				return fmt.Errorf("cycle %d, comfyui: %w", i+1, err)
			}
		} else if err := r.request(ctx, r.f.modelB); err != nil {
			return fmt.Errorf("cycle %d, model B: %w", i+1, err)
		}
		if err := r.request(ctx, r.f.modelA); err != nil {
			return fmt.Errorf("cycle %d, model A: %w", i+1, err)
		}
	}
	return nil
}

// runLeak does N A-B cycles and keeps the VRAM and RSS readings of cycle
// 1 and cycle N. The reading goes out after the B of each cycle: B holds
// the card and A has drained.
func (r *run) runLeak(ctx context.Context) error {
	for i := 1; i <= r.f.cycles; i++ {
		if err := r.request(ctx, r.f.modelA); err != nil {
			return fmt.Errorf("cycle %d, model A: %w", i, err)
		}
		if err := r.request(ctx, r.f.modelB); err != nil {
			return fmt.Errorf("cycle %d, model B: %w", i, err)
		}
		last := r.samples[len(r.samples)-1]
		rss, ok := procRSS(r.f.gwPID)
		p := leakPoint{Cycle: i, Vram: last.VramAfter, RSS: rss, RSSKnown: ok}
		if i == 1 {
			r.leakA = p
		}
		if i == r.f.cycles {
			r.leakB = p
		}
	}
	return nil
}

// request sends one streaming chat completion to the gateway and records
// everything around it. The GPU and RAM readings go out before the
// request, so they see the card as the gateway hands it over; the "after"
// readings go out once the stream is closed, by which point the gateway
// has drained the previous engine.
func (r *run) request(ctx context.Context, model string) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	s := Sample{Engine: model, Kind: kindOf(r.samples, model)}
	before, err := r.probe.Read(ctx)
	if err != nil {
		return fmt.Errorf("gpu read: %w", err)
	}
	s.VramBefore = before.Used
	s.MemBefore = host.MemAvailable("")

	t0 := time.Now()
	res, err := r.post(ctx, "/v1/chat/completions", chatBody(model, r.f.maxTokens, r.f.noThink), t0)
	if err != nil {
		return err
	}
	s.TTFT = res.TTFT
	s.Total = res.Total
	s.Text, s.Reasoning = res.Text, res.Reasoning
	if res.Fail != "" {
		return fmt.Errorf("the gateway answered with an error: %s", res.Fail)
	}

	after, err := r.probe.Read(ctx)
	if err != nil {
		return fmt.Errorf("gpu read after the request: %w", err)
	}
	s.VramAfter = after.Used
	s.MemAfter = host.MemAvailable("")
	r.samples = append(r.samples, s)
	return nil
}

// chatBody is one greedy request. noThink adds the chat-template switch
// that llama.cpp-style servers read to skip the thinking; without it the
// request stays plain OpenAI.
func chatBody(model string, maxTokens int, noThink bool) []byte {
	type kwargs struct {
		EnableThinking bool `json:"enable_thinking"`
	}
	body := struct {
		Model       string    `json:"model"`
		Stream      bool      `json:"stream"`
		Temperature float64   `json:"temperature"`
		MaxTokens   int       `json:"max_tokens"`
		Seed        int       `json:"seed"`
		Messages    []message `json:"messages"`
		Kwargs      *kwargs   `json:"chat_template_kwargs,omitempty"`
	}{model, true, 0, maxTokens, 1, []message{{Role: "user", Content: prompt}}, nil}
	if noThink {
		body.Kwargs = &kwargs{}
	}
	b, _ := json.Marshal(body)
	return b
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// post sends body to path on the gateway and, for a streaming answer,
// parses the SSE stream; both times run from start, the moment the
// request went out, so a cold start's drain and load are in them.
func (r *run) post(ctx context.Context, path string, body []byte, start time.Time) (streamResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.f.gateway, "/")+path, bytes.NewReader(body))
	if err != nil {
		return streamResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return streamResult{}, fmt.Errorf("gateway %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return streamResult{}, fmt.Errorf("gateway %s answered %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return parseSSE(resp.Body, time.Now, start)
}

// comfy posts a workflow to the gateway's /prompt slot, then waits until
// ComfyUI's /history entry shows the job done. The post itself holds
// until the gateway has ComfyUI up under a lease, so the total time
// includes the drain and the start, as the model switches do.
func (r *run) comfy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	wf, err := os.ReadFile(r.f.comfy)
	if err != nil {
		return fmt.Errorf("read the workflow: %w", err)
	}
	before, err := r.probe.Read(ctx)
	if err != nil {
		return fmt.Errorf("gpu read: %w", err)
	}
	s := Sample{Engine: "comfyui", Kind: kindOf(r.samples, "comfyui")}
	s.VramBefore = before.Used
	s.MemBefore = host.MemAvailable("")

	t0 := time.Now()
	var q struct {
		PromptID   string         `json:"prompt_id"`
		NodeErrors map[string]any `json:"node_errors"`
	}
	if err := r.postJSON(ctx, "/prompt", wf, &q); err != nil {
		return err
	}
	if len(q.NodeErrors) > 0 {
		return fmt.Errorf("comfyui rejected the workflow: %v", q.NodeErrors)
	}
	tEnd, err := r.comfyDone(ctx, q.PromptID, t0)
	if err != nil {
		return err
	}
	s.Total = tEnd.Sub(t0)

	after, err := r.probe.Read(ctx)
	if err != nil {
		return fmt.Errorf("gpu read after the request: %w", err)
	}
	s.VramAfter = after.Used
	s.MemAfter = host.MemAvailable("")
	r.samples = append(r.samples, s)
	return nil
}

// comfyDone polls the gateway's /history until the prompt's entry shows
// it done or failed.
func (r *run) comfyDone(ctx context.Context, id string, t0 time.Time) (time.Time, error) {
	type status struct {
		StatusStr string `json:"status_str"`
		Completed int    `json:"completed"`
	}
	for {
		var h map[string]struct {
			Status status `json:"status"`
		}
		if err := r.getJSON(ctx, "/history/"+id, &h); err != nil {
			return t0, err
		}
		entry, ok := h[id]
		if !ok {
			if time.Since(t0) > 5*time.Minute {
				return t0, fmt.Errorf("comfyui never recorded prompt %s", id)
			}
		} else {
			switch {
			case entry.Status.Completed > 0:
				return time.Now(), nil
			case entry.Status.StatusStr == "success":
				return time.Now(), nil
			case entry.Status.StatusStr == "error":
				return time.Now(), fmt.Errorf("the workflow failed in comfyui")
			}
		}
		select {
		case <-ctx.Done():
			return t0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (r *run) postJSON(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.f.gateway, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gateway %s answered %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("read the %s answer: %w", path, err)
	}
	return nil
}

func (r *run) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.f.gateway, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gateway %s answered %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("read the %s answer: %w", path, err)
	}
	return nil
}

// report prints the per-engine table, the raw samples, or the leak
// readings. It runs even after an error, so a partial run is still
// readable.
func (r *run) report() {
	if r.f.jsonOut {
		b, _ := json.MarshalIndent(r.jsonReport(), "", "  ")
		fmt.Println(string(b))
		return
	}
	if r.f.leak > 0 {
		r.reportLeak()
		return
	}
	r.reportTable()
}

func (r *run) reportLeak() {
	printPoint := func(p leakPoint) {
		rss := "unavailable"
		if p.RSSKnown {
			rss = human(p.RSS)
		}
		fmt.Printf("leak  cycle %d  vram after drain %s  gateway rss %s\n", p.Cycle, human(p.Vram), rss)
	}
	printPoint(r.leakA)
	printPoint(r.leakB)
	fmt.Printf("leak  growth over %d cycles: vram %s", r.f.cycles-1, humanSigned(int64(r.leakB.Vram)-int64(r.leakA.Vram)))
	if r.leakA.RSSKnown && r.leakB.RSSKnown {
		fmt.Printf(", gateway rss %s\n", humanSigned(int64(r.leakB.RSS)-int64(r.leakA.RSS)))
	} else {
		fmt.Println(" (rss unavailable off Linux)")
	}
}

func (r *run) reportTable() {
	fmt.Printf("swap  gateway %s  card %s  baseline %s used\n", r.f.gateway, r.card, human(r.baseline))
	fmt.Printf("swap  opening A, then %d cycle(s) of B and A, fixed prompt, temperature 0, seed 1, max 32 tokens\n", r.f.cycles)

	engines := []string{}
	for _, s := range r.samples {
		if !contains(engines, s.Engine) {
			engines = append(engines, s.Engine)
		}
	}
	by := map[string][]Sample{}
	for _, s := range r.samples {
		by[s.Engine] = append(by[s.Engine], s)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nENGINE\tKIND\tN\tTTFT MEDIAN\tTTFT P90")
	for _, e := range engines {
		for _, k := range []Kind{KindCold, KindWarm} {
			var ss []Sample
			for _, s := range by[e] {
				if s.Kind == k {
					ss = append(ss, s)
				}
			}
			if len(ss) == 0 {
				continue
			}
			// A workflow has no tokens, so its row reads n/a.
			var xs []time.Duration
			for _, s := range ss {
				if s.TTFT > 0 {
					xs = append(xs, s.TTFT)
				}
			}
			if len(xs) == 0 {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", e, k, len(ss), "n/a", "n/a")
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", e, k, len(xs), humanDur(median(xs)), humanDur(p90(xs)))
		}
	}
	tw.Flush()

	tw = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nENGINE\tTOTAL MEDIAN\tPEAK RAM DROP\tVRAM HELD\tOUTPUT IDENTICAL")
	for _, e := range engines {
		ss := by[e]
		totals := make([]time.Duration, 0, len(ss))
		drop := uint64(0)
		var afters []uint64
		for _, s := range ss {
			totals = append(totals, s.Total)
			afters = append(afters, s.VramAfter)
			// Available RAM can rise across a request (a drained engine gave
			// its RAM back); only a fall is a drop.
			if s.MemBefore > s.MemAfter && s.MemBefore-s.MemAfter > drop {
				drop = s.MemBefore - s.MemAfter
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			e, humanDur(median(totals)), human(drop), human(median(afters)), outputsIdentical(ss))
	}
	tw.Flush()

	fmt.Printf("\ncold is the engine's first request in this run; warm is every switch back to it.\n")
	fmt.Printf("VRAM held is the card's use once the engine answered (the baseline is %s).\n", human(r.baseline))
	fmt.Printf("with park on for an engine, its warm numbers are the parked resume. Switching on every\n")
	fmt.Printf("request is the ping-pong Berth's juggle guard holds back: a switch inside the juggle\n")
	fmt.Printf("window waits for the holder, and that wait is part of the measured time.\n")
}

func (r *run) jsonReport() map[string]any {
	out := map[string]any{
		"gateway":      r.f.gateway,
		"card":         r.card,
		"baseline_mib": r.baseline / gpu.MiB,
		"cycles":       r.f.cycles,
		"prompt":       prompt,
		"samples":      r.samples,
	}
	if r.f.leak > 0 {
		out["leak"] = []leakPoint{r.leakA, r.leakB}
	}
	return out
}

// human renders bytes in the nearest useful unit: whole MiB under 1 GiB,
// one decimal GiB above it.
func human(b uint64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%d MiB", b/gpu.MiB)
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

func humanSigned(b int64) string {
	switch {
	case b < 0:
		return "-" + human(uint64(-b))
	case b > 0:
		return "+" + human(uint64(b))
	}
	return "0"
}

func humanDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "n/a"
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// procRSS reads VmRSS from /proc/<pid>/status, in bytes; off Linux or
// for a PID that is gone it reports ok=false.
func procRSS(pid int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	return parseVmRSS(string(b))
}

// parseVmRSS finds the VmRSS line of a /proc/*/status file.
func parseVmRSS(status string) (uint64, bool) {
	sc := bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "VmRSS:")
		if !ok {
			continue
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 || f[1] != "kB" {
			continue
		}
		var n uint64
		if _, err := fmt.Sscanf(f[0], "%d", &n); err != nil {
			continue
		}
		return n * 1024, true
	}
	return 0, false
}
