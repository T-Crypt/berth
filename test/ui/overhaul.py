# Harbor UI overhaul gate (dev only, not CI). Asserts the new behavior and the
# accessibility floor on demo fixtures and the live node, desktop and phone.
# Run against a harbor serving the new UI: python3 test/ui/overhaul.py [base-url]
import sys
from playwright.sync_api import sync_playwright
import os
BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:9790/"
out = os.environ.get("OUT", "/tmp/berth-ui-overhaul/").rstrip("/") + "/"; os.makedirs(out, exist_ok=True)
res = []
def ok(name, cond, extra=""):
    res.append(("PASS" if cond else "FAIL", name, extra))

CONTRAST_JS = """([sel, prop]) => {
  const n = document.querySelector(sel);
  if (!n || !n.isConnected || n.getBoundingClientRect().width === 0) return 'missing:' + sel;
  const nums = (c) => c.match(/[\d.]+/g).map(Number);
  const lum = ([r, g, b]) => {
    const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
  };
  // composite the translucent row tints over the ancestor stack
  let layers = [];
  for (let e = n; e && e !== document.documentElement; e = e.parentElement) {
    layers.push(getComputedStyle(e).backgroundColor);
  }
  layers.push('rgb(11, 13, 18)');
  let bg = [11, 13, 18];
  for (const c of layers.reverse()) {
    const m = nums(c); const a = m.length > 3 ? m[3] : 1;
    bg = [0, 1, 2].map((i) => Math.round(m[i] * a + bg[i] * (1 - a)));
  }
  const fg = nums(getComputedStyle(n).getPropertyValue(prop || 'color')).slice(0, 3);
  const L1 = lum(fg), L2 = lum(bg);
  const hi = Math.max(L1, L2), lo = Math.min(L1, L2);
  return ((hi + 0.05) / (lo + 0.05)).toFixed(2);
}"""

with sync_playwright() as p:
    b = p.chromium.launch()
    errs = []
    # ---------- desktop demo ----------
    pg = b.new_page(viewport={"width": 1440, "height": 900})
    pg.on("console", lambda m: errs.append(m.text) if m.type == "error" else None)
    pg.on("pageerror", lambda e: errs.append(str(e)))
    pg.goto(BASE + "?demo", wait_until="networkidle"); pg.wait_for_timeout(1800)
    ok("demo loads clean", not errs, "; ".join(errs[:3]))
    # skeletons are replaced by content on first paint
    ok("no skeleton left after load", pg.locator(".skel-box").count() == 0, str(pg.locator(".skel-box").count()))
    # page chrome
    ok("subnav has nine harbor links", pg.locator("#subnav-harbor a").count() == 9)
    ok("collapse buttons on panels", pg.locator("#panel-harbor .collapse-btn").count() >= 9,
       str(pg.locator("#panel-harbor .collapse-btn").count()))
    ok("keys button in header", pg.locator("#keys-btn").count() == 1)
    ok("tab counts present", pg.locator("#tab-harbor .count").count() == 1, pg.inner_text("#tab-harbor .count"))
    # relative time
    ok("relative update time", "ago" in pg.inner_text("#last-update") or "just" in pg.inner_text("#last-update"),
       pg.inner_text("#last-update"))
    ok("conn pill keeps its label", "DEMO" in pg.inner_text("#conn-pill").upper(), pg.inner_text("#conn-pill"))
    # contrast floor: every text token clears 4.5 on its surface
    pairs = [
        (".panel-title", "color"), (".panel-meta", "color"), (".kpi-value", "color"),
        (".kpi-label", "color"), (".ctile .cvalue", "color"), (".ctile .clabel", "color"),
        (".log-row .lt", "color"), (".log-row .ld", "color"),
        (".tcell .tl", "color"), (".node-version", "color"),
        (".app-footer", "color"), (".memnums .l", "color"), (".board-head", "color"),
        (".tab", "color"), (".chip", "color"), (".log-note", "color"),
    ]
    adv_pairs = [(".machine-chart .mc-label", "color"), (".t-adv .tline", "color")]  # adv-only surfaces
    bad = []
    for sel, prop in pairs:
        try:
            r = float(pg.evaluate(CONTRAST_JS, [sel, prop]))
            if r < 4.5: bad.append(f"{sel}={r}")
        except Exception as e:
            bad.append(f"{sel} err {str(e)[:60]}")
    ok("text contrast >= 4.5", not bad, "; ".join(bad[:8]))
    # ticket paper contrast: open a ticket, then measure the inks on cream
    pg.locator("#board .board-row").first.click(); pg.wait_for_timeout(400)
    ok("ticket opens", pg.locator("#ticket").evaluate("d => d.open"))
    paper = []
    for sel in ["#t-no", ".tlabel", "#t-stamp", ".stub-note"]:
        try:
            r = float(pg.evaluate(CONTRAST_JS, [sel, "color"]))
            if r < 4.5: paper.append(f"{sel}={r}")
        except Exception as e:
            paper.append(f"{sel} err {str(e)[:60]}")
    ok("ticket paper contrast >= 4.5", not paper, "; ".join(paper))
    # copy + link buttons toast
    pg.context.grant_permissions(["clipboard-read", "clipboard-write"])
    pg.click("#t-copy"); pg.wait_for_timeout(300)
    clip = pg.evaluate("navigator.clipboard.readText()")
    ok("ticket COPY copies JSON", '"id"' in clip or '"engine"' in clip, clip[:80])
    ok("copy toasts", pg.locator("#toast-host .toast").count() >= 1)
    pg.click("#t-link"); pg.wait_for_timeout(300)
    link = pg.evaluate("navigator.clipboard.readText()")
    ok("ticket LINK copies a deep link", "#ticket=" in link, link[-60:])
    pg.keyboard.press("Escape"); pg.wait_for_timeout(300)
    ok("ticket closes on Escape", not pg.locator("#ticket").evaluate("d => d.open"))
    # focus returns to the voyage row
    ok("focus returns to the row", (pg.evaluate("document.activeElement.className") or "").find("board-row") >= 0,
       pg.evaluate("document.activeElement.className"))
    # shortcuts
    pg.keyboard.press("?"); pg.wait_for_timeout(300)
    ok("shortcuts open on ?", pg.locator("#shortcuts").evaluate("d => d.open"))
    pg.screenshot(path=out + "shortcuts.png")
    pg.keyboard.press("Escape"); pg.wait_for_timeout(200)
    ok("shortcuts close on Escape", not pg.locator("#shortcuts").evaluate("d => d.open"))
    pg.keyboard.press("2"); pg.wait_for_timeout(400)
    ok("key 2 shows engines", pg.locator("#panel-engines").is_visible())
    bad_tables = []
    for sel, prop in [(".ltable th", "color"), (".rtable th", "color"), (".ledger-label", "color")]:
        try:
            r = float(pg.evaluate(CONTRAST_JS, [sel, prop]))
            if r < 4.5: bad_tables.append(f"{sel}={r}")
        except Exception as e:
            bad_tables.append(f"{sel} err {str(e)[:60]}")
    ok("table text contrast >= 4.5", not bad_tables, "; ".join(bad_tables[:8]))
    pg.keyboard.press("1"); pg.wait_for_timeout(400)
    ok("key 1 shows harbor", pg.locator("#panel-harbor").is_visible())
    pg.keyboard.press("a"); pg.wait_for_timeout(400)
    ok("key A enables advanced", pg.get_attribute("#mode-advanced", "aria-pressed") == "true")
    bad_adv = []
    for sel, prop in adv_pairs:
        try:
            r = float(pg.evaluate(CONTRAST_JS, [sel, prop]))
            if r < 4.5: bad_adv.append(f"{sel}={r}")
        except Exception as e:
            bad_adv.append(f"{sel} err {str(e)[:60]}")
    ok("advanced text contrast >= 4.5", not bad_adv, "; ".join(bad_adv[:8]))
    pg.keyboard.press("b"); pg.wait_for_timeout(400)
    ok("key B back to basic", pg.get_attribute("#mode-basic", "aria-pressed") == "true")
    pg.keyboard.press("p"); pg.wait_for_timeout(200)
    ok("key P pauses the log", pg.get_attribute("#log-pause", "aria-pressed") == "true")
    pg.keyboard.press("p"); pg.wait_for_timeout(200)
    pg.keyboard.press("/"); pg.wait_for_timeout(200)
    ok("key / focuses the filter", pg.evaluate("document.activeElement.id") == "log-search")
    # log search
    pg.keyboard.type("granted"); pg.wait_for_timeout(300)
    rows = pg.locator("#log .log-row").all()
    ok("filter narrows rows", len(rows) > 0 and len(rows) < 21, str(len(rows)))
    ok("filtered rows match", all("granted" in r.inner_text().lower() for r in rows),
       (rows[0].inner_text()[:80] if rows else "none"))
    ok("count names the filter", "of" in pg.inner_text("#log-count"), pg.inner_text("#log-count"))
    pg.keyboard.press("Escape"); pg.wait_for_timeout(200)
    pg.keyboard.press("Escape"); pg.wait_for_timeout(300)
    ok("Escape clears the filter", pg.locator("#log .log-row").count() >= 15,
       str(pg.locator("#log .log-row").count()))
    # chips carry counts and still toggle
    ok("chip counts rendered", pg.locator("#chip-leases .n").count() == 1)
    pg.click("#chip-leases"); pg.wait_for_timeout(200)
    ok("chip toggles off", pg.get_attribute("#chip-leases", "aria-pressed") == "false")
    pg.click("#chip-leases"); pg.wait_for_timeout(200)
    # board sort
    pg.locator("#board .bhead-btn[data-sort='vessel']").click(); pg.wait_for_timeout(300)
    ok("board sorts by vessel", "ordered by vessel" in pg.inner_text("#sec-board"), pg.inner_text("#sec-board .board-foot"))
    ok("sort button marked", "sorted" in (pg.get_attribute("#board .bhead-btn[data-sort='vessel']", "class") or ""))
    pg.locator("#board .bhead-btn[data-sort='time']").click(); pg.wait_for_timeout(300)
    ok("board sorts by time", "ordered by time" in pg.inner_text("#sec-board"))
    # alert dismiss
    n0 = pg.locator("#alerts .alert-card").count()
    if n0:
        pg.locator("#alerts .alert-dismiss").first.click(); pg.wait_for_timeout(300)
        ok("alert dismisses", pg.locator("#alerts .alert-card").count() == n0 - 1,
           f"{n0} -> {pg.locator('#alerts .alert-card').count()}")
    else:
        ok("alert dismisses", True, "no alerts in fixture")
    # chart keyboard parity: focus the tide, the tip shows; blur hides
    tide = pg.locator("#tide .chart-wrap").first
    tide.scroll_into_view_if_needed(); pg.wait_for_timeout(300)
    tide.focus(); pg.wait_for_timeout(300)
    tip = pg.locator("#tide .chart-tip").first
    ok("tide tip on focus", tip.count() > 0 and tip.is_visible(), tip.inner_text()[:120].replace("\n", " | ") if tip.count() else "")
    pg.locator("#tab-harbor").focus(); pg.wait_for_timeout(200)
    ok("tide tip hides on blur", not (tip.count() > 0 and tip.is_visible()))
    # residency focus (it lives on the engines tab)
    pg.click("#tab-engines"); pg.wait_for_timeout(400)
    rescanvas = pg.locator("#residency-canvas").first
    rescanvas.scroll_into_view_if_needed(); pg.wait_for_timeout(200)
    rescanvas.focus(); pg.wait_for_timeout(300)
    rtip = pg.locator("#residency-canvas .res-tip").first
    ok("residency tip on focus", rtip.count() > 0 and rtip.is_visible(),
       rtip.inner_text()[:120].replace("\n", " | ") if rtip.count() else "")
    # collapse persists
    pg.click("#tab-harbor"); pg.wait_for_timeout(300)
    pg.locator("#sec-tide .collapse-btn").click(); pg.wait_for_timeout(200)
    ok("tide collapses", pg.get_attribute("#sec-tide", "class").find("collapsed") >= 0)
    pg.reload(wait_until="networkidle"); pg.wait_for_timeout(1500)
    ok("collapse persists", pg.get_attribute("#sec-tide", "class").find("collapsed") >= 0)
    pg.locator("#sec-tide .collapse-btn").click(); pg.wait_for_timeout(200)
    # subnav scroll-spy
    pg.locator("#subnav-harbor a[data-sec='sec-log']").click(); pg.wait_for_timeout(600)
    ok("subnav jumps to the log", "sec-log" in pg.url and pg.locator("#sec-log").is_visible(), pg.url[-40:])
    # tap targets at desktop
    small = pg.evaluate("""() => [...document.querySelectorAll('#panel-harbor .chip,#panel-harbor .mode-btn,#panel-harbor .log-pause,#panel-harbor .collapse-btn')].filter(e => e.offsetParent && e.getBoundingClientRect().height < 28).length""")
    ok("desktop controls >= 28px", small == 0, str(small))
    # no placeholder text leaks
    body = pg.inner_text("body")
    ok("no undefined/NaN text", "undefined" not in body and "NaN" not in body)
    pg.screenshot(path=out + "harbor-new.png", full_page=True)
    ok("demo: no JS errors", not errs, "; ".join(errs[:5]))

    # ---------- reduced motion ----------
    ctx2 = b.new_context(viewport={"width": 1440, "height": 900}, reduced_motion="reduce")
    pm = ctx2.new_page()
    merrs = []
    pm.on("pageerror", lambda e: merrs.append(str(e)))
    pm.goto(BASE + "?demo", wait_until="networkidle"); pm.wait_for_timeout(1500)
    ok("reduced-motion class set", pm.evaluate("document.body.classList.contains('reduced-motion')"))
    anims = pm.evaluate("document.getAnimations().length")
    ok("no running animations reduced", anims == 0, str(anims))
    ok("reduced: no JS errors", not merrs, "; ".join(merrs[:3]))
    ctx2.close()

    # ---------- phone ----------
    ph = b.new_page(viewport={"width": 390, "height": 844}, has_touch=True)
    ph.on("pageerror", lambda e: errs.append("phone: " + str(e)))
    ph.goto(BASE + "?demo", wait_until="networkidle"); ph.wait_for_timeout(1500)
    ok("phone: no page-level overflow", ph.evaluate("document.documentElement.scrollWidth") <= 390,
       str(ph.evaluate("document.documentElement.scrollWidth")))
    ok("phone: subnav scrolls inside itself", ph.evaluate("document.querySelector('#subnav-harbor').scrollWidth") > 390)
    ok("phone: collapse targets >= 40px",
       ph.evaluate("getComputedStyle(document.querySelector('#sec-tide .collapse-btn')).minHeight") in ("40px",) or
       ph.evaluate("document.querySelector('#sec-tide .collapse-btn').getBoundingClientRect().height") >= 40)
    ph.screenshot(path=out + "phone-new.png", full_page=True)

    # ---------- live ----------
    lv = b.new_page(viewport={"width": 1440, "height": 900})
    lv.on("pageerror", lambda e: errs.append("live: " + str(e)))
    lv.goto(BASE, wait_until="domcontentloaded"); lv.wait_for_timeout(4000)
    ok("live conn pill", "LIVE" in lv.inner_text("#conn-pill").upper(), lv.inner_text("#conn-pill"))
    ok("live tab counts", lv.locator("#tab-harbor .count").count() == 1)
    # force a stale read: abort status, wait past the 10 s gate, retry
    lv.route(lambda url: "v1/status" in str(url), lambda route, request: route.abort())
    stale = False
    for _ in range(40):
        lv.wait_for_timeout(500)
        if "STALE" in lv.inner_text("#conn-pill").upper():
            stale = True
            break
    ok("stale pill names STALE", stale, lv.inner_text("#conn-pill"))
    ok("retry button appears when stale", lv.locator("#conn-pill .retry").count() == 1)
    lv.unroute_all()
    lv.locator("#conn-pill .retry").click(); lv.wait_for_timeout(4000)
    ok("retry reads the node again", "LIVE" in lv.inner_text("#conn-pill").upper(), lv.inner_text("#conn-pill"))
    lv.screenshot(path=out + "live-new.png", full_page=True)
    ok("live: no JS errors", not [e for e in errs if e.startswith("live:")], "; ".join(e for e in errs if e.startswith("live:"))[:200])
    b.close()
for r in res: print(*r, sep="\t")
sys.exit(1 if any(r[0] == "FAIL" for r in res) else 0)
