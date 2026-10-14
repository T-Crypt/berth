# Harbor UI click/hover check (dev only). Needs python playwright + its chromium.
# Run against a live harbor: python3 test/ui/check.py [base-url]; screenshots go to $OUT or /tmp/berth-ui-check/.
import sys
from playwright.sync_api import sync_playwright
import os
BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:9798/"
out = os.environ.get("OUT", "/tmp/berth-ui-check/").rstrip("/") + "/"; os.makedirs(out, exist_ok=True)
res = []
def ok(name, cond, extra=""):
    res.append(("PASS" if cond else "FAIL", name, extra))
with sync_playwright() as p:
    b = p.chromium.launch()
    pg = b.new_page(viewport={"width": 1920, "height": 1200})
    errs = []
    demoreqs = []
    pg.on("request", lambda r: demoreqs.append(r.url))
    pg.on("console", lambda m: errs.append(m.text) if m.type == "error" else None)
    pg.on("pageerror", lambda e: errs.append(str(e)))
    pg.goto(BASE + "?demo", wait_until="networkidle"); pg.wait_for_timeout(1500)
    ok("demo never fetches /v1", not any("/v1/" in u for u in demoreqs), " | ".join(u for u in demoreqs if "/v1/" in u)[:160])
    # mode switch + persistence
    pg.click("#mode-advanced"); pg.wait_for_timeout(300)
    ok("advanced pressed", pg.get_attribute("#mode-advanced", "aria-pressed") == "true")
    adv_vis = pg.locator(".adv-only").first.is_visible()
    ok("adv-only visible in advanced", adv_vis)
    pg.reload(wait_until="networkidle"); pg.wait_for_timeout(1000)
    ok("mode persisted across reload", pg.get_attribute("#mode-advanced", "aria-pressed") == "true")
    pg.click("#mode-basic"); pg.wait_for_timeout(300)
    ok("adv-only hidden in basic", not pg.locator("#machine-charts").is_visible())
    ok("throughput advanced chart hidden in basic", not pg.locator("#throughput-rpm").is_visible())
    pg.click("#mode-advanced"); pg.wait_for_timeout(300)
    # tide hover
    tide = pg.locator("#tide svg").first
    bb = tide.bounding_box()
    ok("tide rendered", bb is not None and bb["width"] > 200, str(bb))
    if bb:
        pg.mouse.move(bb["x"] + bb["width"] * 0.5, bb["y"] + bb["height"] * 0.5); pg.wait_for_timeout(300)
        tip = pg.locator("#tide .chart-tip, #tide ~ .chart-tip, .tide-chart .chart-tip").first
        vis = tip.count() > 0 and tip.is_visible()
        ok("tide tooltip on hover", vis, tip.inner_text()[:160].replace("\n", " | ") if vis else "")
        pg.screenshot(path=out + "tide-hover.png", clip={"x": bb["x"] - 10, "y": bb["y"] - 40, "width": bb["width"] + 20, "height": bb["height"] + 120})
        pg.mouse.move(5, 5); pg.wait_for_timeout(200)
        ok("tide tooltip hides on leave", not (tip.count() > 0 and tip.is_visible()))
    # tide range picker: the picker moves aria-pressed and the tide repaints
    # from the range it names (demo synthesizes 6h-30d from the fixture)
    ok("tide range picker has five ranges", pg.locator("#tide-range .mode-btn").count() == 5, str(pg.locator("#tide-range .mode-btn").count()))
    pg.locator("#range-24h").click(); pg.wait_for_timeout(500)
    ok("tide range 24H pressed", pg.get_attribute("#range-24h", "aria-pressed") == "true")
    ok("tide range 1H unpressed", pg.get_attribute("#range-1h", "aria-pressed") == "false")
    meta24 = pg.inner_text("#tide-meta").lower()
    ok("tide meta names the 24h range", "24h" in meta24, meta24)
    bb = pg.locator("#tide svg").first.bounding_box()
    ok("tide repainted for 24h", bb is not None and bb["width"] > 200, meta24)
    # model timeline: demo mode synthesizes spans for the range without any
    # /v1 fetch (the request logger above still proves it), and the bars
    # share the tide's x scale, so the 24h first bar starts at the range's
    # left edge
    tlbars = pg.locator("#timeline .timeline-svg rect[fill-opacity]")
    ok("timeline renders in demo (24h)", tlbars.count() >= 3, str(tlbars.count()) + " bars")
    tb = tlbars.first.bounding_box() if tlbars.count() else None
    ok("timeline bar under the tide's plot", tb is not None and tb["width"] > 4, str(tb))
    if tb:
        pg.mouse.move(tb["x"] + max(tb["width"] / 2, 6), tb["y"] + tb["height"] / 2); pg.wait_for_timeout(300)
        tt = pg.locator("#timeline .chart-tip").first
        vis = tt.count() > 0 and tt.is_visible()
        ok("timeline tooltip on hover shows loaded", vis and "loaded" in tt.inner_text().lower(),
           tt.inner_text()[:160].replace("\n", " | ") if vis else "")
        pg.screenshot(path=out + "harbor-timeline-24h.png",
                     clip={"x": 0, "y": bb["y"] - 160, "width": 1920, "height": bb["height"] + 320})
        pg.mouse.move(5, 5); pg.wait_for_timeout(200)
    pg.screenshot(path=out + "harbor-24h.png", full_page=True)
    pg.locator("#range-1h").click(); pg.wait_for_timeout(500)
    ok("tide range back on 1H", pg.get_attribute("#range-1h", "aria-pressed") == "true")
    # the range switch repaints the timeline from the range it names
    def tlfirst():
        b = pg.locator("#timeline .timeline-svg rect[fill-opacity]").first.bounding_box()
        return (round(b["x"], 1), round(b["width"], 1)) if b else None
    t1 = tlfirst()
    ok("timeline range switch repaints", t1 is not None and tb is not None and (t1[0], t1[1]) != (round(tb["x"], 1), round(tb["width"], 1)),
       f"24h {(round(tb['x'], 1), round(tb['width'], 1)) if tb else None} -> 1h {t1}")
    ok("timeline meta names the 1h range", "last hour" in pg.inner_text("#timeline-meta").lower(), pg.inner_text("#timeline-meta"))
    pg.screenshot(path=out + "harbor-timeline-1h.png", full_page=True)
    # ticket dialog from the board
    row = pg.locator("#board [role=button], #board button, #board a, #board .row").first
    if row.count():
        row.click(); pg.wait_for_timeout(500)
        ok("ticket dialog opens from board", pg.locator("#ticket").evaluate("d => d.open"))
        pg.screenshot(path=out + "ticket.png")
        pg.keyboard.press("Escape"); pg.wait_for_timeout(200)
        ok("ticket closes on Escape", not pg.locator("#ticket").evaluate("d => d.open"))
    else:
        ok("ticket dialog opens from board", False, "no clickable board row found")
    # socket health, refusal tally, contested magnitude, juggle link
    ok("socket line shows timeouts", "timeout" in pg.inner_text("#socket-line").lower(), pg.inner_text("#socket-line"))
    ok("refusal tally lists the top reason", "no free berth" in pg.inner_text("#refusal-tally").lower(), pg.inner_text("#refusal-tally")[:160])
    titles = pg.locator("#board .board-row[title]").all()
    ok("contested row title names the held bytes", any("granted while" in (t.get_attribute("title") or "") for t in titles), str(len(titles)))
    ok("juggle log line links to the ranking", pg.locator("#log .log-why").count() >= 1, str(pg.locator("#log .log-why").count()))
    crow = pg.locator("#board .board-row[title]").first
    if crow.count():
        crow.click(); pg.wait_for_timeout(400)
        ok("ticket stamp names the contested bytes", "CONTESTED" in pg.inner_text("#t-stamp2"), pg.inner_text("#t-stamp2"))
        pg.keyboard.press("Escape"); pg.wait_for_timeout(200)
    else:
        ok("ticket stamp names the contested bytes", False, "no contested row")
    # log chips + pause
    pg.click("#chip-leases"); pg.wait_for_timeout(200)
    ok("chip toggles", pg.get_attribute("#chip-leases", "aria-pressed") == "false")
    pg.click("#log-pause"); pg.wait_for_timeout(200)
    ok("pause toggles", pg.get_attribute("#log-pause", "aria-pressed") == "true")
    # engines tab + leaderboard sort
    pg.click("#tab-engines"); pg.wait_for_timeout(500)
    ok("engines tab shows", pg.locator("#panel-engines").is_visible())
    ok("priority table ranks the engines", pg.locator("#priority .ptable tbody tr").count() >= 3, str(pg.locator("#priority .ptable tbody tr").count()))
    ok("priority meta names the source", "7 day" in pg.inner_text("#priority-meta"), pg.inner_text("#priority-meta"))
    ok("hash updated", "engines" in pg.url, pg.url)
    # leaderboard range picker: 24H is the live node's view; 7D and 30D read
    # the stored range, and the meta says so
    ok("leaderboard range picker present", pg.locator("#lb-range .mode-btn").count() == 3, str(pg.locator("#lb-range .mode-btn").count()))
    pg.locator("#lbrange-7d").click(); pg.wait_for_timeout(400)
    ok("leaderboard 7D pressed", pg.get_attribute("#lbrange-7d", "aria-pressed") == "true")
    lbmeta7 = pg.inner_text("#lb-meta")
    ok("leaderboard meta names the stored range", "7 d" in lbmeta7, lbmeta7)
    pg.locator("#lbrange-30d").click(); pg.wait_for_timeout(400)
    ok("leaderboard 30D pressed", pg.get_attribute("#lbrange-30d", "aria-pressed") == "true")
    pg.locator("#lbrange-24h").click(); pg.wait_for_timeout(400)
    ok("leaderboard back on 24H", pg.get_attribute("#lbrange-24h", "aria-pressed") == "true")
    ok("leaderboard meta back to live 24 h", "24 h" in pg.inner_text("#lb-meta"), pg.inner_text("#lb-meta"))
    hdrs = pg.locator("#leaderboard button.lth")
    ok("leaderboard has sort buttons", hdrs.count() > 2, str(hdrs.count()))
    def col(i):
        return [c.inner_text() for c in pg.locator(f"#leaderboard tbody tr td:nth-child({i})").all()]
    if hdrs.count() > 2:
        first = col(1)
        hdrs.nth(0).click(); pg.wait_for_timeout(200); a = col(1)
        hdrs.nth(0).click(); pg.wait_for_timeout(200); d = col(1)
        ok("sort by name toggles order", a != d and a == list(reversed(d)), f"{a} / {d}")
        for i in range(hdrs.count()):
            lab = hdrs.nth(i).inner_text()
            hdrs.nth(i).click(); pg.wait_for_timeout(150)
            th = pg.locator("#leaderboard thead th").nth(i)
            res.append(("INFO", f"sort {lab}", th.get_attribute("aria-sort")))
        pg.screenshot(path=out + "engines.png", full_page=True)
    # throughput panel: decode lines per engine over the tide's range, and in
    # advanced mode the smaller rpm/p95 chart
    ok("throughput decode chart rendered", pg.locator("#throughput-decode svg").count() >= 1)
    ok("throughput advanced chart rendered", pg.locator("#throughput-rpm svg").count() >= 1)
    tpmeta = pg.inner_text("#throughput-meta")
    ok("throughput meta names the range", "last" in tpmeta, tpmeta)
    # storage panel, advanced only, bottom of the engines tab
    ok("storage panel visible in advanced", pg.locator("#storage").is_visible())
    srows = pg.locator("#storage .storage-row").count()
    ok("storage rows rendered", srows >= 5, str(srows))
    spath = pg.inner_text("#storage-meta")
    ok("storage meta shows the store path", bool(spath), spath)
    # KV pool + context residency: both live in basic mode, and the demo
    # fixtures fill both; the screenshot below then covers the two panels
    ok("KV pool panel visible", pg.locator("#kv-pool").is_visible())
    ok("KV pool has cells", pg.locator("#kv-pool .pool-cell").count() >= 1, str(pg.locator("#kv-pool .pool-cell").count()))
    ok("residency canvas visible", pg.locator("#residency-canvas").is_visible())
    ok("residency sessions rendered", pg.locator("#residency-sessions .res-row").count() >= 1,
       str(pg.locator("#residency-sessions .res-row").count()))
    ok("residency drew the request columns", pg.locator("#residency-canvas svg").count() == 1)
    ok("KPI strip has QUEUE", pg.locator("#kpi-strip .kpi-label", has_text="QUEUE").count() == 1)
    # title tooltips exist on key tiles
    ok("ctile has title", bool(pg.get_attribute("#ctile-vram", "title")))
    pg.screenshot(path=out + "engines-adv-demo.png", full_page=True)
    pg.click("#tab-harbor"); pg.wait_for_timeout(300)
    pg.screenshot(path=out + "harbor-adv.png", full_page=True)
    # live page, not demo
    pg2 = b.new_page(viewport={"width": 1920, "height": 1200})
    pg2.on("pageerror", lambda e: errs.append("live: " + str(e)))
    livereqs = []
    pg2.on("request", lambda r: livereqs.append(r.url))
    pg2.goto(BASE, wait_until="domcontentloaded"); pg2.wait_for_timeout(4000)
    ok("live conn pill", "LIVE" in pg2.inner_text("#conn-pill").upper() or "CONNECTED" in pg2.inner_text("#conn-pill").upper(), pg2.inner_text("#conn-pill"))
    # the banner is the server mode's only consumer: it shows only when the
    # harbor is LAN-exposed (an explicit --listen opt-in)
    m = pg2.request.get(BASE + "v1/mode").json()["mode"]
    ok("lan banner matches server mode", pg2.locator("#lan-banner").is_visible() == (m == "lan"), f"mode={m}")
    ok("live storage fetched", any("/v1/storage" in u for u in livereqs), str(livereqs[-6:]))
    # the range picker on the live page must fetch the range it names; with
    # a fresh or young store the tide then says so instead of drawing a lie
    pg2.locator("#range-24h").click()
    pg2.wait_for_timeout(2500)
    ok("live 24H picker fetches the store range", any("/v1/history?range=24h" in u for u in livereqs), str(livereqs[-6:]))
    ok("live 24H pressed", pg2.get_attribute("#range-24h", "aria-pressed") == "true")
    lmeta = pg2.inner_text("#tide-meta").lower()
    ok("live long-range meta is honest", "24h" in lmeta or "no" in lmeta, lmeta)
    # stored-panel failures must say so: abort the window fetch and read the meta
    pg2.route(lambda url: "v1/counters" in str(url) and "range=5m" in str(url), lambda route, request: route.abort())
    pg2.locator("#counter-range [data-range='5m']").click(); pg2.wait_for_timeout(800)
    ok("counter window failure says stale since", "stale since" in pg2.inner_text("#counter-meta").lower(), pg2.inner_text("#counter-meta"))
    pg2.unroute_all()
    # the live page polls /v1/spans like the tide polls /v1/history; a young
    # store (or a node that predates Sample.models) yields the empty state,
    # never an error
    ok("live spans fetched", any("/v1/spans" in u for u in livereqs), str(livereqs[-6:]))
    ok("live status still polls at 3s", len([u for u in livereqs if "/v1/status" in u]) >= 2,
       str(len([u for u in livereqs if "/v1/status" in u])))
    pg2.locator("#tab-engines").click(); pg2.wait_for_timeout(300)
    pg2.route(lambda url: "v1/insights" in str(url) and "range=7d" in str(url), lambda route, request: route.abort())
    pg2.locator("#lbrange-7d").click(); pg2.wait_for_timeout(800)
    ok("stored ledger failure says stale since", "stale since" in pg2.inner_text("#lb-meta").lower(), pg2.inner_text("#lb-meta"))
    pg2.unroute_all()
    pg2.locator("#tab-harbor").click(); pg2.wait_for_timeout(300)
    ok("live timeline painted (data or empty state)",
       pg2.locator("#timeline .timeline-svg, #timeline .empty-note").count() >= 1,
       "svg" if pg2.locator("#timeline .timeline-svg").count() else "empty-note")
    pg2.screenshot(path=out + "live.png", full_page=True)
    ok("no JS errors", not errs, "; ".join(errs[:5]))
    # phone width: no page-level horizontal overflow; the board scrolls
    # inside its panel, the header and KPI strip wrap
    pg3 = b.new_page(viewport={"width": 390, "height": 844}, has_touch=True)
    pg3.on("pageerror", lambda e: errs.append("phone: " + str(e)))
    pg3.goto(BASE + "?demo", wait_until="networkidle"); pg3.wait_for_timeout(1500)
    ok("phone: no page-level horizontal overflow",
       pg3.evaluate("document.documentElement.scrollWidth") <= 390,
       str(pg3.evaluate("document.documentElement.scrollWidth")))
    ok("phone: board rendered", pg3.locator("#board").is_visible())
    # the odometer stops on whole digits at the phone's smaller digit size
    # (a fixed px step once left every reel between two numbers)
    pg3.wait_for_timeout(600)
    stops = pg3.evaluate("""() => [...document.querySelectorAll('#odometer .reel')].map(r =>
      (r.parentElement.getBoundingClientRect().top + 1 - r.getBoundingClientRect().top) / r.firstElementChild.offsetHeight)""")
    ok("phone: odometer reels stop on whole digits", all(abs(s - round(s)) < 0.02 for s in stops), str([round(s, 2) for s in stops]))
    # the banner element is in the DOM from the start; it stays hidden until
    # the server mode says lan (demo/loopback keeps it hidden)
    ok("phone: lan banner in the DOM, hidden outside lan mode",
       pg3.locator("#lan-banner").count() == 1 and not pg3.locator("#lan-banner").is_visible())
    ok("phone: chips meet 40px targets",
       pg3.evaluate("getComputedStyle(document.querySelector('#chip-leases')).minHeight") == "40px",
       pg3.evaluate("getComputedStyle(document.querySelector('#chip-leases')).minHeight"))
    ok("phone: tabs meet 44px targets",
       pg3.evaluate("getComputedStyle(document.querySelector('#tab-harbor')).minHeight") == "44px",
       pg3.evaluate("getComputedStyle(document.querySelector('#tab-harbor')).minHeight"))
    tide3 = pg3.locator("#tide svg").first
    tide3.scroll_into_view_if_needed(); pg3.wait_for_timeout(300)
    bb3 = tide3.bounding_box()
    if bb3:
        pg3.touchscreen.tap(bb3["x"] + bb3["width"] * 0.5, bb3["y"] + bb3["height"] * 0.5); pg3.wait_for_timeout(300)
        tip3 = pg3.locator(".tide-chart .chart-tip").first
        vis3 = tip3.count() > 0 and tip3.is_visible()
        ok("phone: tap pins the tide tooltip", vis3, tip3.inner_text()[:160].replace("\n", " | ") if vis3 else "")
        pg3.touchscreen.tap(10, 10); pg3.wait_for_timeout(300)
        ok("phone: tap away dismisses the tooltip", not (tip3.count() > 0 and tip3.is_visible()))
    else:
        ok("phone: tap pins the tide tooltip", False, "no tide svg")
    pg3.screenshot(path=out + "phone-390.png", full_page=True)
    ok("phone: no JS errors", not [e for e in errs if e.startswith("phone:")], "; ".join(e for e in errs if e.startswith("phone:"))[:160])
    b.close()
for r in res: print(*r, sep="\t")
sys.exit(1 if any(r[0] == "FAIL" for r in res) else 0)
