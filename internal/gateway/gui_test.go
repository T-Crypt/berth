// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/T-Crypt/berth/internal/config"
	"github.com/T-Crypt/berth/internal/proto"
)

// noFollow is a client that shows a redirect instead of following it.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func guiReq(t *testing.T, method, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func guiState(t *testing.T, base, name string) guiStatus {
	t.Helper()
	resp := guiReq(t, "GET", base+"/gui/"+name+"/state", nil)
	defer resp.Body.Close()
	var s guiStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A visit to a stopped engine's wake page shows the page and takes nothing;
// the page's Start takes a lease, the state turns up, and the next visit goes straight to the
// engine's web UI. No API key is involved.
func TestWakePageStartsAndRedirects(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_COMFY", addr)
	const gui = "https://box.example.ts.net:8188/"
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://" + addr,
			Start: []string{os.Args[0]}, Priority: "interactive", GUI: gui}}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	resp := guiReq(t, "GET", srv.URL+"/gui/comfyui", nil)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), `data-engine="comfyui"`) {
		t.Fatalf("wake page %d: %s", resp.StatusCode, page)
	}
	if s := guiState(t, srv.URL, "comfyui"); s.State != "offline" || !strings.Contains(s.Message, "the card is free") {
		t.Fatalf("state before a start = %+v", s)
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("opening the page took a lease: %+v", st.Leases)
	}

	resp = guiReq(t, "POST", srv.URL+"/gui/comfyui/wake", map[string]string{"Sec-Fetch-Site": "same-origin"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("wake = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		s := guiState(t, srv.URL, "comfyui")
		if s.State == "up" {
			if s.URL != gui {
				t.Fatalf("up state url = %q", s.URL)
			}
			break
		}
		if s.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("engine never came up: %+v", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "comfyui" {
		t.Fatalf("leases = %+v", st.Leases)
	}

	resp = guiReq(t, "GET", srv.URL+"/gui/comfyui", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != gui {
		t.Fatalf("visit when up = %d to %q, want 302 to %s", resp.StatusCode, resp.Header.Get("Location"), gui)
	}
}

// Another site cannot make a visitor's browser start an engine, a rebinding
// name is refused, and only engines with both gui and start are reachable.
func TestWakePageGuard(t *testing.T) {
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Ready: time.Second, Key: testKey, AllowHosts: []string{"box.example.ts.net"},
		Engines: []config.Engine{
			{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:1", Start: []string{"false"}, GUI: "http://127.0.0.1:1/"},
			{Name: "nogui", Type: "llamacpp", URL: "http://127.0.0.1:1", Start: []string{"false"}, Serves: []string{"m"}},
		}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	for _, c := range []struct {
		name   string
		method string
		path   string
		hdr    map[string]string
		want   int
	}{
		{"cross-site fetch", "POST", "/gui/comfyui/wake", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin", "POST", "/gui/comfyui/wake", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"rebinding host", "GET", "/gui/comfyui", map[string]string{"Host": "evil.example"}, http.StatusForbidden},
		{"no gui", "GET", "/gui/nogui", nil, http.StatusNotFound},
		{"unknown", "POST", "/gui/nope/wake", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNotFound},
		{"allowed proxy host", "GET", "/gui/", map[string]string{"Host": "box.example.ts.net:9796"}, http.StatusOK},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		for k, v := range c.hdr {
			if k == "Host" {
				req.Host = v
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("a refused request took a lease: %+v", st.Leases)
	}

	// A start that fails reports its reason to the page.
	resp := guiReq(t, "POST", srv.URL+"/gui/comfyui/wake", map[string]string{"Origin": srv.URL})
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := guiState(t, srv.URL, "comfyui")
		if s.State == "failed" {
			if s.Code == "" || s.Detail == "" {
				t.Fatalf("failed state carries no reason: %+v", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed start never reported: %+v", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The page names who holds the card and what a start waits for, with
// llama-swap's models under their family.
func TestCardView(t *testing.T) {
	ninfer := proto.Lease{Engine: "NInfer-27B", Group: "llama-swap", State: "active"}
	starting := &wake{starting: true}
	for _, c := range []struct {
		name  string
		node  *proto.Status
		wk    *wake
		state string
		want  string
	}{
		{"free", &proto.Status{}, nil, "offline", "the card is free"},
		{"in use", &proto.Status{Leases: []proto.Lease{ninfer}}, nil, "offline", "in use by llama-swap (NInfer-27B). Start asks it to release"},
		{"pinned", &proto.Status{Leases: []proto.Lease{{Engine: "blender", State: "active", Pinned: true}}}, nil, "offline", "Berth never stops it"},
		{"no berthd", nil, nil, "offline", "berthd is not answering"},
		{"draining", &proto.Status{Leases: []proto.Lease{{Engine: "strata", State: "draining"}}}, starting, "waiting", "Waiting for strata release: finishing its current request"},
		{"releasing", &proto.Status{Leases: []proto.Lease{{Engine: "NInfer-27B", Group: "llama-swap", State: "releasing"}}}, starting, "waiting", "Waiting for llama-swap (NInfer-27B) release: freeing its memory"},
		{"granted", &proto.Status{Leases: []proto.Lease{{Engine: "comfyui", State: "active"}}}, starting, "starting", "loading comfyui"},
		{"failed", &proto.Status{}, &wake{err: &startError{code: "no_berth", msg: "comfyui got no berth"}}, "failed", "did not start"},
	} {
		got := cardView("comfyui", c.node, c.wk)
		if got.State != c.state || !strings.Contains(got.Message, c.want) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, got.State, got.Message, c.state, c.want)
		}
	}
}

// guiPlaneRows reads the plane's state endpoint the way the index does.
func guiPlaneRows(t *testing.T, base string) []guiRow {
	t.Helper()
	resp := guiReq(t, "GET", base+"/gui/state", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/gui/state = %d", resp.StatusCode)
	}
	var rows []guiRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func planeRow(rows []guiRow, name string) guiRow {
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	return guiRow{}
}

// rowFragment is the page's markup for one engine's row.
func rowFragment(body, name string) string {
	i := strings.Index(body, "data-name=\""+name+"\"")
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</li>")
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}

// The plane's index lists every configured engine as one row: the
// launchable one with a button and a deep link, the gui-only one with a
// link, the api-only one with its address. /gui/state says what each is
// doing on the card, and reading it takes nothing.
func TestGUIPlaneIndex(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_COMFY", addr)
	const gui = "https://box.example.ts.net:8188/"
	const apiUrl = "http://127.0.0.1:1"
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "comfyui", Type: "comfyui", URL: "http://" + addr,
				Start: []string{os.Args[0]}, Priority: "interactive", GUI: gui},
			{Name: "noinfer", Type: "llamacpp", URL: "http://127.0.0.1:2", GUI: "http://127.0.0.1:2/"},
			{Name: "mcp", Type: "custom", URL: apiUrl, Serves: []string{"m"}},
		}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	resp := guiReq(t, "GET", srv.URL+"/gui/", nil)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index = %d", resp.StatusCode)
	}
	body := string(page)
	comfy := rowFragment(body, "comfyui")
	if !strings.Contains(comfy, "data-launch") {
		t.Errorf("launchable row carries no data-launch: %s", comfy)
	}
	if !strings.Contains(comfy, `href="/gui/comfyui"`) {
		t.Errorf("launchable gui row has no deep link: %s", comfy)
	}
	noinfer := rowFragment(body, "noinfer")
	if !strings.Contains(noinfer, `data-gui="http://127.0.0.1:2/"`) {
		t.Errorf("gui-only row carries no gui: %s", noinfer)
	}
	if strings.Contains(noinfer, "data-launch") {
		t.Errorf("gui-only row is launchable: %s", noinfer)
	}
	mcp := rowFragment(body, "mcp")
	if !strings.Contains(mcp, `data-api="`+apiUrl+`"`) {
		t.Errorf("no-ui row carries no api address: %s", mcp)
	}
	if !strings.Contains(mcp, "No UI") {
		t.Errorf("no-ui row has no No UI marker: %s", mcp)
	}

	rows := guiPlaneRows(t, srv.URL)
	want := []struct {
		name   string
		launch bool
		gui    string
		api    string
	}{
		{"comfyui", true, gui, ""},
		{"noinfer", false, "http://127.0.0.1:2/", ""},
		{"mcp", false, "", apiUrl},
	}
	if len(rows) != len(want) {
		t.Fatalf("state rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i].Name != w.name {
			t.Fatalf("row %d = %s, want %s (config order)", i, rows[i].Name, w.name)
		}
		if rows[i].Launch != w.launch || rows[i].GUI != w.gui || rows[i].API != w.api {
			t.Errorf("%s row = %+v, want launch=%v gui=%q api=%q", w.name, rows[i], w.launch, w.gui, w.api)
		}
	}
	if st := planeRow(rows, "comfyui"); st.State != "offline" || !strings.Contains(st.Message, "the card is free") {
		t.Fatalf("stopped launchable row = %+v", st)
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("reading the plane took a lease: %+v", st.Leases)
	}

	// The launchable engine answers on its own once it comes up; the row
	// turns up without anyone asking the card. The child is the test binary
	// serving the ComfyUI stand-in, as the wake page's start would.
	proc := exec.Command(os.Args[0])
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Process.Kill(); _ = proc.Wait() })
	deadline := time.Now().Add(8 * time.Second)
	for {
		r := planeRow(guiPlaneRows(t, srv.URL), "comfyui")
		if r.State == "up" {
			if !strings.Contains(r.Message, "is ready") {
				t.Fatalf("up row = %+v", r)
			}
			break
		}
		if r.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("engine never came up: %+v", r)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The plane's Launch button does what the wake page's did: a same-origin
// POST takes one lease, the row follows the start to up, and the card holds
// exactly that engine.
func TestGUIPlaneLaunchFlow(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_COMFY", addr)
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "comfyui", Type: "comfyui", URL: "http://" + addr,
				Start: []string{os.Args[0]}, Priority: "interactive", GUI: "https://box.example.ts.net:8188/"},
			{Name: "mcp", Type: "custom", URL: "http://127.0.0.1:1", Serves: []string{"m"}},
		}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	resp := guiReq(t, "POST", srv.URL+"/gui/comfyui/wake", map[string]string{"Sec-Fetch-Site": "same-origin"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("wake = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		r := planeRow(guiPlaneRows(t, srv.URL), "comfyui")
		if r.State == "up" {
			break
		}
		if r.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("row never came up: %+v", r)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "comfyui" {
		t.Fatalf("leases = %+v", st.Leases)
	}
}

// An engine with a start command and no web UI (Strata, a llama-server)
// launches from the plane like one with a UI: the board is the phone's way to
// switch the card. Its wake page does not exist, since there is no UI to send
// the browser to.
func TestGUIPlaneLaunchAPIOnly(t *testing.T) {
	sock, b := daemon(t)
	addr := freeAddr(t)
	t.Setenv("BERTH_GW_ENGINE", addr+",strata-model,300ms")
	g := &Gateway{Socket: sock, Ready: 10 * time.Second, Key: testKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{
			{Name: "strata", Type: "llamacpp", URL: "http://" + addr, Start: []string{os.Args[0]}, Serves: []string{"strata-model"}},
		}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	resp := guiReq(t, "POST", srv.URL+"/gui/strata/wake", map[string]string{"Sec-Fetch-Site": "same-origin"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("wake = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		r := planeRow(guiPlaneRows(t, srv.URL), "strata")
		if r.State == "up" {
			break
		}
		if r.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("row never came up: %+v", r)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st := b.Status(); len(st.Leases) != 1 || st.Leases[0].Engine != "strata" {
		t.Fatalf("leases = %+v", st.Leases)
	}
	resp = guiReq(t, "GET", srv.URL+"/gui/strata", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wake page for an engine with no UI = %d, want 404", resp.StatusCode)
	}
}

// The harbor's SWITCH tab is the one board: with --harbor the plane's index
// redirects an old bookmark there, and the wake page and the plane's state
// answer as before. Without it both index URLs are the index page.
func TestGUIPlaneIndexToSwitch(t *testing.T) {
	const harbor = "https://box.tailnet.ts.net:9798"
	// / with no harbor keeps the relative Location it has always sent.
	for _, c := range []struct {
		harbor string
		path   string
		want   int
		loc    string // the Location header, empty for the index page
	}{{harbor, "/gui/", http.StatusFound, harbor + "/#switch"},
		{harbor, "/", http.StatusFound, harbor + "/#switch"},
		{"", "/gui/", http.StatusOK, ""},
		{"", "/", http.StatusFound, "/gui/"},
	} {
		sock, b := daemon(t)
		g := &Gateway{Socket: sock, Ready: time.Second, Key: testKey, Harbor: c.harbor,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:1",
				Start: []string{"false"}, GUI: "http://127.0.0.1:1/"}}}
		srv := httptest.NewServer(g.GUIHandler())
		resp := guiReq(t, "GET", srv.URL+c.path, nil)
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		loc := resp.Header.Get("Location")
		if c.loc == "" {
			if resp.StatusCode != c.want || !strings.Contains(string(page), `data-name="comfyui"`) {
				t.Errorf("%s with no harbor = %d: %s", c.path, resp.StatusCode, page)
			}
		} else if resp.StatusCode != c.want || loc != c.loc {
			t.Errorf("%s with harbor %q = %d to %q, want %d to %q", c.path, c.harbor, resp.StatusCode, loc, c.want, c.loc)
		}
		if st := b.Status(); len(st.Leases) != 0 {
			t.Fatalf("opening the index took a lease: %+v", st.Leases)
		}
	}
}

// The harbor does not take the wake page or the plane's state away: an
// engine's own bookmark still lands on its page, and the state endpoint
// still answers JSON.
func TestGUIPlaneWakeAndStateWithHarbor(t *testing.T) {
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Ready: time.Second, Key: testKey, Harbor: "https://box.tailnet.ts.net:9798",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:1",
			Start: []string{"false"}, GUI: "http://127.0.0.1:1/"}}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	resp := guiReq(t, "GET", srv.URL+"/gui/comfyui", nil)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), `data-engine="comfyui"`) {
		t.Fatalf("wake page with a harbor = %d: %s", resp.StatusCode, page)
	}
	if rows := guiPlaneRows(t, srv.URL); len(rows) != 1 || rows[0].Name != "comfyui" {
		t.Fatalf("/gui/state with a harbor = %+v", rows)
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("reading the plane took a lease: %+v", st.Leases)
	}
}

// A rebinding name cannot read the plane's state.
func TestGUIPlaneGuard(t *testing.T) {
	sock, b := daemon(t)
	g := &Gateway{Socket: sock, Ready: time.Second, Key: testKey,
		Engines: []config.Engine{{Name: "comfyui", Type: "comfyui", URL: "http://127.0.0.1:1", Start: []string{"false"}, GUI: "http://127.0.0.1:1/"}}}
	srv := httptest.NewServer(g.GUIHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/gui/state", nil)
	req.Host = "evil.example"
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host read the plane: %d", resp.StatusCode)
	}
	if st := b.Status(); len(st.Leases) != 0 {
		t.Fatalf("a refused request took a lease: %+v", st.Leases)
	}
}
