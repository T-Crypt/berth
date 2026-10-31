// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gateway

// Deferred ComfyUI jobs. A workflow posted while the card is out of reach
// (passed through to a VM, or held by a game, a hold or a pinned app) on an
// engine with on_busy = "queue" is kept on disk and answered at once with a
// berth job id. The background loop submits it once the card is free, so a
// render asked for during a VM session runs when the card comes back, and
// /history/<berth id> reads ComfyUI's entry under that id.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/fsx"
	"github.com/T-Crypt/berth/internal/proto"
)

const deferredPrefix = "berth-"

// deferredJob is one queued workflow: the request body as the client sent
// it, and ComfyUI's prompt_id once submitted.
type deferredJob struct {
	ID     string          `json:"id"`
	Engine string          `json:"engine"`
	Body   json.RawMessage `json:"body"`
	At     time.Time       `json:"at"`
	Prompt string          `json:"prompt_id,omitempty"`
}

// deferredBook is DeferredFile's content. Submitted jobs stay, so their
// history reads back by the berth id, until the book trims them.
type deferredBook struct {
	Jobs []deferredJob `json:"jobs"`
}

// keepSubmitted is how many submitted jobs the book remembers for
// /history; older ones drop off.
const keepSubmitted = 200

// outOfReach says why a ComfyUI job cannot run now, "" when it can: the
// card is in a VM, or a pinned lease (a game, a hold, a pinned app) has it.
func outOfReach(st *proto.Status) string {
	if st == nil {
		return ""
	}
	if st.Passthrough != nil {
		return "the card is passed through to a VM"
	}
	if cordoned(st) {
		return "the card is cordoned"
	}
	for _, l := range st.Leases {
		if l.Pinned {
			return fmt.Sprintf("%s holds the card", holderName(l))
		}
	}
	return ""
}

// queueingComfy is the ComfyUI engine whose jobs queue while the card is
// out of reach, nil when none does.
func (g *Gateway) queueingComfy() *config.Engine {
	for i := range g.Engines {
		if e := &g.Engines[i]; e.Type == "comfyui" && e.OnBusy == "queue" {
			return e
		}
	}
	return nil
}

// book loads DeferredFile once. Called with g.mu held.
func (g *Gateway) book() *deferredBook {
	if g.deferred != nil {
		return g.deferred
	}
	g.deferred = &deferredBook{}
	if g.DeferredFile == "" {
		return g.deferred
	}
	b, err := os.ReadFile(g.DeferredFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			g.log().Warn("comfyui queue unreadable; starting empty", "file", g.DeferredFile, "err", err)
		}
		return g.deferred
	}
	if err := json.Unmarshal(b, g.deferred); err != nil {
		g.log().Warn("comfyui queue unreadable; starting empty", "file", g.DeferredFile, "err", err)
	}
	return g.deferred
}

// saveBook writes the book. Called with g.mu held.
func (g *Gateway) saveBook() {
	if g.DeferredFile == "" {
		return
	}
	b, err := json.Marshal(g.deferred)
	if err == nil {
		err = fsx.WriteFile(g.DeferredFile, b, 0o600)
	}
	if err != nil {
		g.log().Warn("comfyui queue not saved", "file", g.DeferredFile, "err", err)
	}
}

// deferPrompt keeps the workflow and answers the way ComfyUI does, with
// the berth job id as the prompt_id and the reason in X-Berth-Deferred.
func (g *Gateway) deferPrompt(w http.ResponseWriter, r *http.Request, e *config.Engine, why string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil || !json.Valid(body) {
		apiError(w, http.StatusBadRequest, "the workflow is not JSON")
		return
	}
	now := time.Now()
	job := deferredJob{ID: deferredPrefix + strconv.FormatInt(now.UnixNano(), 36), Engine: e.Name, Body: body, At: now}
	g.mu.Lock()
	book := g.book()
	book.Jobs = append(book.Jobs, job)
	pending := 0
	for _, j := range book.Jobs {
		if j.Prompt == "" {
			pending++
		}
	}
	g.saveBook()
	g.mu.Unlock()
	g.log().Info("comfyui job queued until the card is free", "id", job.ID, "why", why, "queued", pending)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Berth-Deferred", why)
	_ = json.NewEncoder(w).Encode(map[string]any{"prompt_id": job.ID, "number": pending, "node_errors": map[string]any{}})
}

// submitDeferred starts the ComfyUI engine and hands it every queued job in
// order, recording ComfyUI's prompt_id for each. A job ComfyUI refuses or
// does not answer stays queued for the next free card. It reports whether
// there was anything to submit.
func (g *Gateway) submitDeferred(ctx context.Context) bool {
	g.mu.Lock()
	var todo []deferredJob
	for _, j := range g.book().Jobs {
		if j.Prompt == "" {
			todo = append(todo, j)
		}
	}
	g.mu.Unlock()
	if len(todo) == 0 {
		return false
	}
	e := g.queueingComfy()
	if e == nil {
		return false
	}
	sctx, cancel := context.WithTimeout(ctx, g.ready())
	defer cancel()
	if err := g.ensure(sctx, e, false); err != nil {
		g.log().Warn("comfyui did not start for the queued jobs; they stay queued", "engine", e.Name, "err", err)
		return true
	}
	for _, j := range todo {
		id, err := g.postPrompt(sctx, e, j.Body)
		if err != nil {
			g.log().Warn("queued comfyui job refused; it stays queued", "id", j.ID, "err", err)
			continue
		}
		g.mu.Lock()
		book := g.book()
		for i := range book.Jobs {
			if book.Jobs[i].ID == j.ID {
				book.Jobs[i].Prompt = id
			}
		}
		if n := len(book.Jobs); n > keepSubmitted {
			book.Jobs = book.Jobs[n-keepSubmitted:]
		}
		g.saveBook()
		g.mu.Unlock()
		g.log().Info("queued comfyui job submitted", "id", j.ID, "prompt_id", id, "waited", time.Since(j.At).Round(time.Second))
	}
	return true
}

// postPrompt sends one workflow to ComfyUI and returns its prompt_id.
func (g *Gateway) postPrompt(ctx context.Context, e *config.Engine, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.URL, "/")+"/prompt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var got struct {
		PromptID string `json:"prompt_id"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &got) != nil || got.PromptID == "" {
		return "", fmt.Errorf("ComfyUI answered %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return got.PromptID, nil
}

// deferredHistory answers /history/<berth id>: {} while the job waits, as
// ComfyUI answers for a prompt it has not finished, then ComfyUI's entry
// for the submitted prompt under the berth id.
func (g *Gateway) deferredHistory(w http.ResponseWriter, r *http.Request, id string) {
	g.mu.Lock()
	var job *deferredJob
	for _, j := range g.book().Jobs {
		if j.ID == id {
			job = &j
			break
		}
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if job == nil || job.Prompt == "" {
		_, _ = io.WriteString(w, "{}")
		return
	}
	var e *config.Engine
	for i := range g.Engines {
		if g.Engines[i].Name == job.Engine {
			e = &g.Engines[i]
		}
	}
	if e == nil {
		_, _ = io.WriteString(w, "{}")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(e.URL, "/")+"/history/"+job.Prompt, nil)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := g.client().Do(req)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "ComfyUI is not answering: "+err.Error())
		return
	}
	defer resp.Body.Close()
	var entries map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&entries); err != nil {
		apiError(w, http.StatusBadGateway, "ComfyUI's history is not JSON")
		return
	}
	out := map[string]json.RawMessage{}
	if v, ok := entries[job.Prompt]; ok {
		out[job.ID] = v
	}
	_ = json.NewEncoder(w).Encode(out)
}

// cordoned is whether berth cordon closed a card the status covers.
func cordoned(st *proto.Status) bool {
	return slices.ContainsFunc(st.GPUs, func(g proto.GPU) bool { return g.Scheduled && g.Cordoned })
}
