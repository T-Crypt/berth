package cli

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/alerts"
	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/gpu"
	"github.com/T-Crypt/berth/internal/history"
	"github.com/T-Crypt/berth/internal/host"
	"github.com/T-Crypt/berth/internal/procs"
	"github.com/T-Crypt/berth/internal/proto"
)

// fixedProbe is a driver that always reports the same card: 92% of the
// VRAM used and a 92 C temperature, so with temp {warn 80, crit 90} one
// sample crosses straight to crit and every later sample stays there.
type fixedProbe struct{}

func (fixedProbe) Read(context.Context) (gpu.Snapshot, error) {
	total := uint64(1 << 30)
	return gpu.Snapshot{
		Index:   0,
		Devices: []gpu.Device{{Index: 0, Name: "fake", Total: total, Used: total * 92 / 100, Free: total * 8 / 100, Temp: 92}},
		At:      time.Now(),
	}, nil
}

// alertRecorder is a fake broker.Reporter: it records every alert the
// channels are asked to deliver.
type alertRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *alertRecorder) Squatter(appeared bool, p gpu.Proc) {}

func (r *alertRecorder) Alert(level, key, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, level+":"+key+":"+message)
}

func (r *alertRecorder) alerts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// TestAlertCrossingFiresEventAndNotificationTogether is the proof that
// the event-log line and the notification fire together on one crossing:
// the daemon's sampler loop evaluates the table on its sample, and one
// crossing produces exactly one alert.raised event and exactly one
// notification, both with the same key and level.
func TestAlertCrossingFiresEventAndNotificationTogether(t *testing.T) {
	rep := &alertRecorder{}
	b := broker.New(broker.Config{
		Poll:     10 * time.Millisecond,
		Reporter: rep,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fixedProbe{}, procs.System{})
	b.Tick(context.Background()) // one manual tick to populate the snapshot

	table := alerts.FromConfig(config.AlertsConfig{Temp: proto.Threshold{Warn: 80, Crit: 90}})
	ev := alerts.New(table, 3, func(key alerts.Key, lv alerts.Level, raised bool, msg string) {
		if raised {
			b.Note(proto.Event{Kind: proto.EvAlertRaised, Detail: lv.Name() + ": " + msg})
			rep.Alert(lv.Name(), string(key), msg)
		} else {
			b.Note(proto.Event{Kind: proto.EvAlertCleared, Detail: msg})
		}
	})

	rec := &history.Recorder{Every: 10 * time.Millisecond}
	var hostOut atomic.Pointer[proto.Host]
	var lastAlerts atomic.Pointer[proto.Alerts]
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	t.Cleanup(cancel)
	go samplerLoop(ctx, rec, &host.Reader{}, b, nil, ev, &hostOut, &lastAlerts)
	<-ctx.Done()

	// One crossing: exactly one alert.raised event in the ring, named
	// after the level and key the node evaluated.
	raised, cleared := 0, 0
	events, _, _ := b.Events(0, false)
	for _, e := range events {
		switch e.Kind {
		case proto.EvAlertRaised:
			raised++
			if !strings.HasPrefix(e.Detail, "crit: temp") {
				t.Errorf("raised detail = %q, want prefix %q", e.Detail, "crit: temp")
			}
		case proto.EvAlertCleared:
			cleared++
		}
	}
	if raised != 1 {
		t.Errorf("alert.raised events = %d, want 1", raised)
	}
	if cleared != 0 {
		t.Errorf("alert.cleared events = %d, want 0 (the level never dropped)", cleared)
	}

	// And the one notification through the channels, same level and key.
	got := rep.alerts()
	if len(got) != 1 {
		t.Fatalf("notifications = %v, want exactly one", got)
	}
	if !strings.HasPrefix(got[0], "crit:temp:") {
		t.Errorf("notification = %q, want prefix %q", got[0], "crit:temp:")
	}

	// The snapshot the harbor paints: temp held at crit, the built-in
	// danger keys in the table.
	snap := lastAlerts.Load()
	if snap == nil {
		t.Fatal("no alert snapshot stored")
	}
	if len(snap.Levels) != 1 || snap.Levels["temp"] != "crit" {
		t.Errorf("levels = %v, want {temp: crit}", snap.Levels)
	}
	for _, k := range []string{"probe", "stuck", "temp"} {
		if _, ok := snap.Table[k]; !ok {
			t.Errorf("table lacks %q: %v", k, snap.Table)
		}
	}
}
