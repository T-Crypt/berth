package engines

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
)

// One read, no daemon and no timer: what `berth engine check` runs. A server
// that answers is up with its model, a closed port is down with the reason.
func TestPollOnceReadsEveryEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"qwen3-30b"}]}`)
	}))
	defer srv.Close()

	w := &Watcher{Engines: []config.Engine{
		{Name: "up", Type: "openai", URL: srv.URL},
		{Name: "down", Type: "openai", URL: "http://127.0.0.1:1"},
	}, Client: &http.Client{Timeout: 2 * time.Second}}
	w.PollOnce(context.Background())

	got := w.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot has %d engines, want 2", len(got))
	}
	if !got[0].Up || len(got[0].Models) != 1 || got[0].Models[0].Name != "qwen3-30b" {
		t.Errorf("up engine = %+v", got[0])
	}
	if got[1].Up || got[1].Error == "" {
		t.Errorf("down engine = %+v, want up false with an error", got[1])
	}
}

func TestPollOnceWithNoEngines(t *testing.T) {
	w := &Watcher{}
	w.PollOnce(context.Background())
	if got := w.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want nothing", got)
	}
}
