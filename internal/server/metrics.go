package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
)

// ServeMetrics serves Prometheus text on a loopback address only. It is
// read-only, and requests whose Host header is not a loopback name are
// refused, so a web page cannot read it through DNS rebinding.
func ServeMetrics(ctx context.Context, addr string, b *broker.Broker, engines func() []proto.Engine, log *slog.Logger) error {
	if err := config.Loopback(addr); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		st := b.Status()
		st.Engines = engines()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		WriteProm(w, st)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("metrics up", "addr", "http://"+addr+"/metrics")
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func loopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// WriteProm renders status as Prometheus text. Names are berth_*; memory
// is bytes; engine figures carry source="engine" because they are the
// engine's own claims, not driver measurements.
func WriteProm(w io.Writer, st proto.Status) {
	p := func(help, typ, name string, series ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, s := range series {
			fmt.Fprintln(w, s)
		}
	}
	var total, used, free []string
	for _, g := range st.GPUs {
		l := fmt.Sprintf(`{gpu="%d",name=%q,scheduled="%t"}`, g.Index, g.Name, g.Scheduled)
		total = append(total, fmt.Sprintf("berth_gpu_memory_total_bytes%s %d", l, g.Total))
		used = append(used, fmt.Sprintf("berth_gpu_memory_used_bytes%s %d", l, g.Used))
		free = append(free, fmt.Sprintf("berth_gpu_memory_free_bytes%s %d", l, g.Free))
	}
	p("Card memory, measured by the driver.", "gauge", "berth_gpu_memory_total_bytes", total...)
	p("Card memory in use, measured by the driver.", "gauge", "berth_gpu_memory_used_bytes", used...)
	p("Card memory free, measured by the driver.", "gauge", "berth_gpu_memory_free_bytes", free...)

	states := map[string]int{"active": 0, "draining": 0, "releasing": 0, "stuck": 0}
	for _, l := range st.Leases {
		states[l.State]++
	}
	var ls []string
	for _, s := range []string{"active", "draining", "releasing", "stuck"} {
		ls = append(ls, fmt.Sprintf(`berth_leases{state=%q} %d`, s, states[s]))
	}
	p("Leases by state.", "gauge", "berth_leases", ls...)
	p("Requests waiting for a berth.", "gauge", "berth_waiting", fmt.Sprintf("berth_waiting %d", len(st.Waiting)))
	var sb uint64
	for _, s := range st.Squatters {
		sb += s.Used
	}
	p("Processes holding GPU memory without a lease.", "gauge", "berth_squatters", fmt.Sprintf("berth_squatters %d", len(st.Squatters)))
	p("GPU memory held without a lease.", "gauge", "berth_squatter_bytes", fmt.Sprintf("berth_squatter_bytes %d", sb))
	p("Engines running without a lease.", "gauge", "berth_unprotected_runs", fmt.Sprintf("berth_unprotected_runs %d", len(st.Unprotected)))
	probe := 1
	if st.ProbeErr != "" {
		probe = 0
	}
	p("1 when the GPU probe is reading.", "gauge", "berth_probe_up", fmt.Sprintf("berth_probe_up %d", probe))

	c := st.Counters
	p("Grants that waited on a held card instead of colliding.", "counter", "berth_collisions_avoided_total", fmt.Sprintf("berth_collisions_avoided_total %d", c.CollisionsAvoided))
	p("Peak measured bytes of leases whose memory was freed.", "counter", "berth_vram_handed_off_bytes_total", fmt.Sprintf("berth_vram_handed_off_bytes_total %d", c.VRAMHandedOff))
	p("Drain requests sent.", "counter", "berth_drains_total", fmt.Sprintf("berth_drains_total %d", c.Drains))
	p("Released leases whose memory did not free in time.", "counter", "berth_stuck_total", fmt.Sprintf("berth_stuck_total %d", c.StuckCaught))
	p("Squatters seen.", "counter", "berth_squatters_caught_total", fmt.Sprintf("berth_squatters_caught_total %d", c.SquattersCaught))
	p("Grants made while unleased processes held memory.", "counter", "berth_contested_grants_total", fmt.Sprintf("berth_contested_grants_total %d", c.ContestedGrants))
	p("Requests refused.", "counter", "berth_refused_total", fmt.Sprintf("berth_refused_total %d", c.Refused))
	p("Equal-priority handoffs the engine ranking decided instead of flipping the card.", "counter", "berth_juggles_stopped_total", fmt.Sprintf("berth_juggles_stopped_total %d", c.JugglesStopped))

	var up, models, dec, pre, out, in []string
	for _, e := range st.Engines {
		l := fmt.Sprintf(`{engine=%q,type=%q,source="engine"}`, e.Name, e.Type)
		b := 0
		if e.Up {
			b = 1
		}
		up = append(up, fmt.Sprintf("berth_engine_up%s %d", l, b))
		models = append(models, fmt.Sprintf("berth_engine_models%s %d", l, len(e.Models)))
		dec = append(dec, fmt.Sprintf("berth_engine_decode_tokens_per_second%s %g", l, e.DecodeTPS))
		pre = append(pre, fmt.Sprintf("berth_engine_prefill_tokens_per_second%s %g", l, e.PrefillTPS))
		out = append(out, fmt.Sprintf("berth_engine_output_tokens_total%s %g", l, e.Metrics.OutputTokens))
		in = append(in, fmt.Sprintf("berth_engine_prompt_tokens_total%s %g", l, e.Metrics.PromptTokens))
	}
	if len(st.Engines) > 0 {
		p("1 when the engine answered its last read.", "gauge", "berth_engine_up", up...)
		p("Models the engine reports loaded.", "gauge", "berth_engine_models", models...)
		p("Decode rate over the last read interval, from the engine's counters.", "gauge", "berth_engine_decode_tokens_per_second", dec...)
		p("Prefill rate over the last read interval, from the engine's counters.", "gauge", "berth_engine_prefill_tokens_per_second", pre...)
		p("Output tokens the engine reports since it started.", "counter", "berth_engine_output_tokens_total", out...)
		p("Prompt tokens the engine reports since it started.", "counter", "berth_engine_prompt_tokens_total", in...)
	}
}
