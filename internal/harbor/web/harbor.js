// berth harbor: one EventSource, status every 3 s, the live 1 h window every
// 10 s, longer ranges at most once a minute, and the panels. All API
// strings reach the page only through textContent.
import {
  $, el, fmtGiB, fmtMiB, fmtAge, fmtClock, fmtDate, fmtClockShort, pad,
  isNA, na, fmtPct, fmtMS, fmtTok, fmtBytes, fmtDayShort,
  kindWord, kindGroup, kindCss, berthLabel, cardTag, leaseCardLine, tideCapacity, kindSi, siEl,
} from "./util.js";
import { Board } from "./board.js";
import { PriorityView } from "./priority.js";
import { Ticket } from "./ticket.js";
import { EngineView } from "./engines.js";
import { LibraryView } from "./library.js";
import { ResidencyView } from "./residency.js";
import { initSwitch } from "./switch.js";
import { sparkline, lineChart, stackedArea, timelineChart } from "./charts.js";
import { PAL } from "./palette.js";
import { GIB } from "./util.js";
import {
  icon, toast, relTime, copyText, onKey, showDialog, skeleton, errorNote,
  syncKeyed, prefersReduced, watchMotion,
} from "./ui.js";
const POLL_MS = 3000;
const STALE_MS = 10000;
const DEBOUNCE_MS = 250;
const LOG_MAX = 200;
const MODE_KEY = "berth-harbor-mode";
// The range names are the server's own: 1h is the node's live window (5 s
// samples, gated at 10 s), 6h and 24h read the store's 1 m rows, 7d and
// 30d its 1 h rollups. Long ranges cost a store read, so they refresh at
// most once a minute.
const RANGE_MS = { "1h": 36e5, "6h": 216e5, "24h": 864e5, "7d": 6048e5, "30d": 2592e6 };
const RANGE_EVERY = { "1h": 5, "6h": 60, "24h": 60, "7d": 3600, "30d": 3600 };
const RANGE_KEY = "berth-harbor-range";
const LB_RANGES = ["24h", "7d", "30d"];
const LBRANGE_KEY = "berth-harbor-lbrange";
const RANGE_POLL_MS = 60000;
// The ticket counter's windows: "all" is the node's own board (kept across
// restarts); the rest read /v1/counters, the live totals minus the totals
// the store kept at the window's start (exact to the minute).
const COUNTER_RANGES = { "5m": 3e5, "15m": 9e5, "1h": 36e5, "24h": 864e5, "7d": 6048e5, all: 0 };
const COUNTER_WORDS = { "5m": "5 min", "15m": "15 min", "1h": "hour", "24h": "24 h", "7d": "7 days" };
const CRANGE_KEY = "berth-harbor-crange";
// the 1 h live window is re-fetched on this gate; the node samples every 5 s
const LIVE_POLL_MS = 10000;
// the library's scan stats every file in every root; /v1/models caches one
// walk for 30 s server-side, so the page asks no faster
const MODEL_POLL_MS = 30000;

// A fixed, muted palette for the tide's holder bands. Cyan leads so the main
// engine always reads as the dominant colour.
const HOLDER_PALETTE = PAL.holders;
const EVENT_TONE = { "lease.granted": "granted", "lease.draining": "draining", "lease.freed": "freed", "lease.stuck": "stuck" };

const params = new URLSearchParams(location.search);
const state = {
  demo: params.has("demo"),
  mode: initialMode(),
  range: initialRange(),
  lbrange: initialLbRange(),
  crange: initialCounterRange(),
  cwindow: null, // /v1/counters for crange, when it is not "all"
  insightsStaleSince: 0, // last stored-insights fetch failure, 0 = fresh
  counterStaleSince: 0, // last counter-window fetch failure, 0 = fresh
  lastCw: 0,
  status: null,
  alerts: null, // the last s.alerts from status: the node's threshold table + sticky levels
  history: null,
  insights: null, // the stored range's ledger/leaderboard/recent, when the leaderboard is off its 24 h default
  storage: null,  // /v1/storage, or the demo fixture
  spans: null,    // /v1/spans: { from, to, spans: [{engine, model, start, end (null = open)}] }
  models: null,   // /v1/models payload, or the demo fixture
  lastGood: 0,
  lastHeavy: 0, // the KPIs, ledger, recent and residency repaint on the 10 s live gate, not every 3 s poll
  lastHist: 0, lastIns: 0, lastStorage: 0, lastSpans: 0, // the long ranges refresh at most once a minute
  nodeDown: null,
  fetchFailing: false,
  events: [],
  filters: { leases: true, stowaways: true, probe: true, alerts: true },
  logPaused: false,
  logQuery: "", // the log toolbar's text filter, matched case-insensitively
  storageFailed: false, // the store read failed: the storage panel says so
  lastSeq: 0,
  vmFirstSeen: 0, // first poll that read the card as passthrough, for the gap the event ring did not keep
  demoHist: null, // ?demo: the recorded 1 h window the longer ranges are synthesized from
  boardSort: null, // departures board sort: null | "vessel" | "hold" | "status" | "time"
  alertSeen: {}, // key -> first-seen ms, so a sticky alert names its age
  alertDismissed: {}, // key -> level string the operator dismissed
  collapsed: [], // panel ids the operator folded, remembered in localStorage
};
const COLLAPSE_KEY = "berth-harbor-collapsed";

function initialMode() {
  const m = params.get("mode"); // ?mode= for tests/screenshot
  if (m === "advanced" || m === "basic") return m;
  try {
    const stored = localStorage.getItem(MODE_KEY);
    if (stored === "advanced" || stored === "basic") return stored;
  } catch (e) { /* private mode */ }
  return "basic";
}
function initialRange() {
  const r = params.get("range"); // ?range= for tests/screenshot
  if (r && RANGE_MS[r]) return r;
  try {
    const stored = localStorage.getItem(RANGE_KEY);
    if (stored && RANGE_MS[stored]) return stored;
  } catch (e) { /* private mode */ }
  return "1h";
}
function initialCounterRange() {
  try {
    const stored = localStorage.getItem(CRANGE_KEY);
    if (stored && stored in COUNTER_RANGES) return stored;
  } catch (e) { /* private mode */ }
  return "all";
}

function initialLbRange() {
  try {
    const stored = localStorage.getItem(LBRANGE_KEY);
    if (stored && LB_RANGES.includes(stored)) return stored;
  } catch (e) { /* private mode */ }
  return "24h";
}

let board, ticket, engineView, residency, priorityView, libraryView, switchView;
let connPill, statePill, vmPill, nodeVersion, lastUpdate, demoStrip, alertStripEl;
let odometerReels, counterSince, counterMeta;
let ctiles;
let logList, logNote, logMeta, refusalTallyEl, logCountEl, logWrapEl, logJumpEl, logSearchInput;
let shortcutsDlg, railMenuDlg;
let lastLogSig = null; // filter+query+pause signature: repaint only on change, append otherwise
let clearLogFilter = null; // set by buildLogSearch: the page's Esc reaches it (R-07)
let lastTick = 0; // the relative-time ticker's own 1 s cadence
let berthsRoot, claimsRoot, claimsMeta, stowRoot, notickRoot, machineRoot, machineCharts, machineMeta, socketLine;
let kpiStripEl, tideEl, tideLegend, tideMeta;
let boardMeta, berthsMeta, stowMeta, noticketMeta, enginesMeta;
let throughputMeta, throughputDecode, throughputRpm;
let storageEl, storageMeta, lbMeta, timelineEl, timelineMeta;
const berthCards = new Map();
let tideChart = null;
let tlChart = null;
const holderColors = new Map(); // engine -> tide palette slot, like the tide
const tlFallback = new Map();   // engine -> palette slot for non-holder engines
const kpiTiles = {}; // key -> { tile, value, spark, unit }
let histSeries = {}; // precomputed per-history sparkline series

function main() {
  connPill = $("conn-pill");
  statePill = $("state-pill");
  vmPill = $("vm-pill");
  alertStripEl = $("alerts");
  nodeVersion = $("node-version");
  lastUpdate = $("last-update");
  demoStrip = $("demo-strip");
  odometerReels = Array.from(document.querySelectorAll("#odometer .reel"));
  counterSince = $("counter-since");
  counterMeta = $("counter-meta");
  ctiles = { vram: $("ctile-vram"), drains: $("ctile-drains"), stuck: $("ctile-stuck"), stowaways: $("ctile-stowaways"), contested: $("ctile-contested"), refused: $("ctile-refused"), juggles: $("ctile-juggles") };
  logList = $("log");
  logNote = $("log-note");
  logMeta = $("log-meta");
  refusalTallyEl = $("refusal-tally");
  logCountEl = $("log-count");
  logWrapEl = $("log-wrap");
  logJumpEl = $("log-jump");
  shortcutsDlg = $("shortcuts");
  railMenuDlg = $("rail-menu");
  berthsRoot = $("berths");
  claimsRoot = $("claims");
  claimsMeta = $("claims-meta");
  stowRoot = $("stowaways");
  notickRoot = $("noticket");
  machineRoot = $("machine");
  machineCharts = $("machine-charts");
  machineMeta = $("machine-meta");
  socketLine = $("socket-line");
  kpiStripEl = $("kpi-strip");
  tideEl = $("tide");
  tideLegend = $("tide-legend");
  tideMeta = $("tide-meta");
  timelineEl = $("timeline");
  timelineMeta = $("timeline-meta");
  boardMeta = $("board-meta");
  berthsMeta = $("berths-meta");
  stowMeta = $("stow-meta");
  noticketMeta = $("noticket-meta");
  enginesMeta = $("engines-meta");
  throughputMeta = $("throughput-meta");
  throughputDecode = $("throughput-decode");
  throughputRpm = $("throughput-rpm");
  storageEl = $("storage");
  storageMeta = $("storage-meta");
  lbMeta = $("lb-meta");
  board = new Board($("board"), (key) => openTicket(key), (sort) => setBoardSort(sort));
  ticket = new Ticket($("ticket"), board);
  priorityView = new PriorityView($("priority"), $("priority-meta"));
  libraryView = new LibraryView($("library-root"), $("library-meta"), () => fetchModels(true));
  engineView = new EngineView($("engine-grid"), $("ledger"), $("leaderboard"), $("recent"), $("recent-meta"), lbMeta, $("kv-pool"), $("pool-chart"), $("pool-meta"));
  // the pool panel colors its cells like the tide colors its bands
  engineView.colorOf = (name) => engineColor(name);
  residency = new ResidencyView($("residency-canvas"), $("residency-legend"), $("residency-sessions"), $("residency-meta"));
  residency.colorOf = (name) => engineColor(name);
  switchView = initSwitch({ demo: state.demo });
  buildKpiStrip();
  wireMode();
  applyMode();
  wireTabs();
  wireLogControls();
  buildLogSearch();
  wireLogJump();
  wireRangePickers();
  wireChrome();
  paintSkeletons();
  paintRange();
  paintLbRange();
  wireDemo();
  if (!state.demo) {
    // The banner must not depend on the node: the mode is a fact of the
    // harbor, stable for the process lifetime, so the page fetches it once.
    fetch("/v1/mode", { headers: { accept: "application/json" } })
      .then((r) => r.json())
      .then((j) => { if (j && j.mode === "lan") $("lan-banner").hidden = false; })
      .catch(() => { /* mode unknown: the banner stays hidden, like loopback */ });
    fetchStatus();
    fetchHistory();
    fetchSpans();
    fetchModels();
    startEvents();
    setInterval(() => { if (!document.hidden) { tick(); pollHistory(); pollStorage(); pollInsights(); pollSpans(); pollModels(); } }, POLL_MS);
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden) {
        tick();
        pollHistory();
        pollStorage();
        pollInsights();
        pollSpans();
        pollModels();
        startEvents();
      }
    });
  } else {
    refreshDemoHistory();
  }
}


// ---------------- the data-source marker ----------------
// Each range picker says where its numbers come from: the live node or its
// store. The glyph inside the option, its accessible name and the chip in the
// panel head all read the pressed option's data-src, so the source is stated
// where a title cannot be read (R-06). "ALL" on the counter is the node's own
// board, not the store.
const SRC_WORDS = {
  "5m": "5 minutes", "15m": "15 minutes", "1h": "1 hour", "6h": "6 hours",
  "24h": "24 hours", "7d": "7 days", "30d": "30 days", all: "the whole board",
};
const SRC_CHIP = { live: "LIVE", stored: "STORED" };
// wireSourceMarkers gives every range option its glyph and its named source,
// once at startup: the source of a range never changes.
function wireSourceMarkers() {
  for (const b of document.querySelectorAll(".range-group .mode-btn")) {
    const src = b.dataset.src;
    if (!src) continue;
    b.prepend(siEl(src));
    const words = SRC_WORDS[b.dataset.range] || b.dataset.range;
    b.setAttribute("aria-label", words + ", " + src);
  }
}
// paintSourceChip names the pressed option's source in words in the panel head,
// beside the same glyph the option carries.
function paintSourceChip(groupId, range) {
  const group = $(groupId);
  if (!group) return;
  const btn = group.querySelector('[data-range="' + range + '"]');
  const src = btn ? btn.dataset.src : "";
  if (!src) return;
  let chip = group.nextElementSibling;
  if (!chip || !chip.classList.contains("src-chip")) {
    chip = el("span", "src-chip");
    group.insertAdjacentElement("afterend", chip);
  }
  chip.replaceChildren(siEl(src), el("span", null, SRC_CHIP[src] || src.toUpperCase()));
  chip.classList.toggle("is-live", src === "live");
}

// ---------------- range pickers ----------------
// The tide's picker drives /v1/history for the tide and the throughput
// charts; the leaderboard's drives /v1/insights for the ledger and the
// leaderboard. Both remember their choice like the mode switch.
function wireRangePickers() {
  for (const b of document.querySelectorAll("#tide-range .mode-btn")) {
    b.addEventListener("click", () => setRange(b.dataset.range, true));
  }
  for (const b of document.querySelectorAll("#lb-range .mode-btn")) {
    b.addEventListener("click", () => setLbRange(b.dataset.range, true));
  }
  for (const b of document.querySelectorAll("#counter-range .mode-btn")) {
    b.addEventListener("click", () => setCounterRange(b.dataset.range, true));
  }
  wireSourceMarkers();
  paintCounterRange();
}
function setCounterRange(r, persist) {
  if (!(r in COUNTER_RANGES)) return;
  state.crange = r;
  state.cwindow = null;
  if (persist) {
    try { localStorage.setItem(CRANGE_KEY, r); } catch (e) { /* ignore */ }
  }
  paintCounterRange();
  if (state.status) renderCounters(state.status.counters);
  state.lastCw = 0;
  fetchCounterWindow();
}
function paintCounterRange() {
  for (const b of document.querySelectorAll("#counter-range .mode-btn")) {
    b.setAttribute("aria-pressed", String(b.dataset.range === state.crange));
  }
  paintSourceChip("counter-range", state.crange);
}
async function fetchCounterWindow() {
  if (state.crange === "all") return;
  const r = state.crange;
  if (state.demo) {
    state.cwindow = demoCounterWindow(r);
  } else {
    try {
      const res = await fetch("/v1/counters?range=" + r, { headers: { accept: "application/json" } });
      if (!res.ok) { noteCounterStale(); return; }
      const w = await res.json();
      if (r !== state.crange) return; // the picker moved on while this was in flight
      state.cwindow = w;
      state.counterStaleSince = 0;
    } catch (e) { noteCounterStale(); return; }
  }
  state.lastCw = Date.now();
  if (state.status) renderCounters(state.status.counters);
}
// ?demo: a window is the fixture board scaled to the share of its age the
// window covers, so the picker visibly moves without a store.
function demoCounterWindow(r) {
  const c = (state.status && state.status.counters) || {};
  const age = Math.max(1, Date.now() - new Date(c.since).getTime());
  const f = Math.min(1, COUNTER_RANGES[r] / age);
  const scale = (v) => Math.round((v || 0) * f);
  return {
    range: r, partial: false, from: new Date(Date.now() - COUNTER_RANGES[r]).toISOString(),
    counts: { collisions_avoided: scale(c.collisions_avoided), vram_handed_off: scale(c.vram_handed_off), drains: scale(c.drains), stuck_caught: scale(c.stuck_caught), squatters_caught: scale(c.squatters_caught), contested_grants: scale(c.contested_grants), refused: scale(c.refused), juggles_stopped: scale(c.juggles_stopped) },
  };
}
function setRange(r, persist) {
  if (!RANGE_MS[r]) return;
  state.range = r;
  if (persist) {
    try { localStorage.setItem(RANGE_KEY, r); } catch (e) { /* ignore */ }
  }
  paintRange();
  state.lastHist = 0; // the new range must arrive now, whatever the minute gate says
  state.lastSpans = 0;
  if (state.demo) { refreshDemoHistory(); return; }
  fetchHistory();
  fetchSpans();
}
function paintRange() {
  for (const b of document.querySelectorAll("#tide-range .mode-btn")) {
    b.setAttribute("aria-pressed", String(b.dataset.range === state.range));
  }
  paintSourceChip("tide-range", state.range);
}
function setLbRange(r, persist) {
  if (!LB_RANGES.includes(r)) return;
  state.lbrange = r;
  residency.lbrange = r;
  if (persist) {
    try { localStorage.setItem(LBRANGE_KEY, r); } catch (e) { /* ignore */ }
  }
  paintLbRange();
  if (r === "24h") {
    // back on the live node's own 24 h view; the stored view is stale, so
    // the ledger, leaderboard and recent list repaint from the status
    state.insights = null;
    state.insightsStaleSince = 0;
    engineView.clearStale();
    if (state.status) {
      engineView.render(state.status);
      residency.render(state.status);
    }
    return;
  }
  if (state.demo) {
    state.insights = demoInsights(r);
    state.lastIns = Date.now();
    state.insightsStaleSince = 0;
    engineView.clearStale();
    renderLbViews();
    return;
  }
  state.lastIns = 0; // fetch the stored range now
  fetchInsights();
}
function paintLbRange() {
  for (const b of document.querySelectorAll("#lb-range .mode-btn")) {
    b.setAttribute("aria-pressed", String(b.dataset.range === state.lbrange));
  }
  paintSourceChip("lb-range", state.lbrange);
}
// The ledger and the leaderboard read from one insights object: the node's
// live 24 h one by default, the stored range's once the picker leaves it.
function renderLbViews() {
  if (state.lbrange === "24h" || !state.insights) return;
  engineView.renderInsights(state.insights, state.lbrange);
  residency.refresh(state.history, state.insights);
}
async function fetchInsights() {
  if (state.demo || state.lbrange === "24h") return;
  try {
    const res = await fetch("/v1/insights?range=" + state.lbrange, { headers: { accept: "application/json" } });
    if (res.ok) {
      state.insights = await res.json();
      state.lastIns = Date.now();
      state.insightsStaleSince = 0;
      engineView.clearStale();
      renderLbViews();
    } else {
      noteInsightsStale();
    }
  } catch (e) { noteInsightsStale(); }
}
function noteCounterStale() {
  state.counterStaleSince = Date.now();
  if (state.status) renderCounters(state.status.counters);
}
function noteInsightsStale() {
  state.insightsStaleSince = Date.now();
  engineView.markStale("stale since " + fmtClock(new Date(state.insightsStaleSince)));
}
function pollInsights() {
  if (state.demo || state.lbrange === "24h" || !state.status) return;
  if (Date.now() - state.lastIns < RANGE_POLL_MS) return;
  fetchInsights();
}

// ---------------- modes ----------------
function wireMode() {
  $("mode-basic").addEventListener("click", () => setMode("basic", true));
  $("mode-advanced").addEventListener("click", () => setMode("advanced", true));
}
function setMode(m, persist) {
  if (m !== "basic" && m !== "advanced") m = "basic";
  state.mode = m;
  if (persist) {
    try { localStorage.setItem(MODE_KEY, m); } catch (e) { /* ignore */ }
  }
  applyMode();
  // advanced reveals charts that need sizing now
  refreshCharts();
}
function applyMode() {
  document.body.classList.toggle("mode-advanced", state.mode === "advanced");
  document.body.classList.toggle("mode-basic", state.mode === "basic");
  // one paint for the header's segment and the menu sheet's copy of it, so the
  // phone (where only the pressed one shows) can never read stale
  for (const btn of document.querySelectorAll("[data-mode]")) {
    btn.setAttribute("aria-pressed", String(btn.dataset.mode === state.mode));
  }
}

// ---------------- tabs ----------------
function wireTabs() {
  const tabs = { harbor: $("tab-harbor"), switch: $("tab-switch"), engines: $("tab-engines"), about: $("tab-about") };
  const panels = { harbor: $("panel-harbor"), switch: $("panel-switch"), engines: $("panel-engines"), about: $("panel-about") };
  const show = (key) => {
    if (!tabs[key]) key = "harbor";
    for (const k of Object.keys(tabs)) {
      panels[k].hidden = k !== key;
      if (k === key) tabs[k].setAttribute("aria-current", "page");
      else tabs[k].removeAttribute("aria-current");
    }
    if (key === "engines" && state.status) engineView.render(state.status);
    if (key === "switch") switchView.show(); else switchView.hide();
  };
  for (const key of Object.keys(tabs)) {
    tabs[key].addEventListener("click", () => {
      history.replaceState(null, "", key === "harbor" ? location.pathname + location.search : "#" + key);
      show(key);
    });
  }
  // A hash is a tab, a ticket, or a section the rail wrote: a section opens
  // the tab that holds it, so a shared or reloaded link lands where it says.
  const h = location.hash.slice(1);
  if (tabs[h] || h.startsWith("ticket=")) return show(h);
  const target = h ? document.getElementById(h) : null;
  const key = target ? Object.keys(panels).find((k) => panels[k].contains(target)) : undefined;
  show(key);
  if (key) pendingSection = target;
}
let pendingTicket = location.hash.startsWith("#ticket=") ? decodeURIComponent(location.hash.slice(8)) : null;
// A linked section waits for the first status, whose alerts and panels move
// it down the page.
let pendingSection = null;

// ---------------- page chrome ----------------
// The sub-navigation, the collapsible panels, the keyboard shortcuts, the
// relative-time ticker and the first-paint skeletons. None of it touches
// the node's data; it only organizes the page around it.
const HARBOR_SECTIONS = [
  ["sec-counter", "Counter"], ["sec-tide", "Tide"], ["sec-timeline", "Timeline"],
  ["sec-board", "Board"], ["sec-log", "Log"], ["sec-berths", "Berths"],
  ["sec-machine", "Machine"], ["sec-claims", "Claims"], ["sec-stow", "Stowaways"], ["sec-noticket", "No ticket"],
];
const ENGINES_SECTIONS = [
  ["sec-engines", "Engines"], ["sec-library", "Library"], ["priority-panel", "Ranking"], ["sec-pool", "KV pool"],
  ["sec-throughput", "Throughput"], ["sec-ledger", "Ledger"], ["sec-lb", "Leaderboard"],
  ["sec-recent", "Recent"], ["sec-residency", "Residency"], ["sec-storage", "Storage"],
];
const SWITCH_SECTIONS = [["sec-switch-cards", "Cards"], ["sec-switch-engines", "Engines"]];
const ABOUT_SECTIONS = [
  ["about-top", "Berth"], ["about-cards", "Cards"], ["about-modes", "Modes"], ["about-nav", "Getting around"],
  ["about-keys", "Keyboard"], ["about-states", "Status words"], ["about-kpis", "KPIs"], ["about-counters", "Counters"],
  ["about-kv", "Context and KV"], ["about-telemetry", "GPU and machine"], ["about-library", "Library"],
  ["about-holders", "Claims and stowaways"], ["about-next", "Terminal"],
];
function wireChrome() {
  try {
    const stored = localStorage.getItem(COLLAPSE_KEY);
    if (stored) state.collapsed = JSON.parse(stored) || [];
  } catch (e) { state.collapsed = []; }
  buildSubnav($("subnav-harbor"), HARBOR_SECTIONS);
  buildSubnav($("subnav-switch"), SWITCH_SECTIONS);
  buildSubnav($("subnav-engines"), ENGINES_SECTIONS);
  buildSubnav($("subnav-about"), ABOUT_SECTIONS);
  wireCollapse();
  wireShortcuts();
  wireRailMenu();
  // the header's clock ticks in relative time on its own second
  // and sleeps while the tab is hidden
  let clock = 0;
  const startClock = () => {
    clock = setInterval(() => {
      const now = Date.now();
      if (now - lastTick < 1000) return;
      lastTick = now;
      paintLastUpdate();
      paintConnAge();
    }, 500);
  };
  const syncClock = () => {
    clearInterval(clock);
    if (!document.hidden) startClock();
  };
  document.addEventListener("visibilitychange", syncClock);
  syncClock();
  document.body.classList.toggle("reduced-motion", prefersReduced());
  watchMotion(() => { /* the class above is the switch; CSS does the rest */ });
  requestAnimationFrame(() => requestAnimationFrame(() => document.body.classList.add("ready")));
}
function buildSubnav(nav, sections) {
  if (!nav) return;
  const links = [];
  for (const [id, label] of sections) {
    const target = document.getElementById(id);
    if (!target) continue;
    const a = el("a", null, label);
    a.href = "#" + id;
    a.dataset.sec = id;
    a.addEventListener("click", (e) => {
      e.preventDefault();
      markSection(links, target);
      target.scrollIntoView({ behavior: prefersReduced() ? "auto" : "smooth", block: "start" });
      history.replaceState(null, "", "#" + id);
    });
    nav.append(a);
    links.push([a, target]);
  }
  if (links.length === 0 || typeof IntersectionObserver !== "function") return;
  const obs = new IntersectionObserver((entries) => {
    for (const en of entries) {
      if (!en.isIntersecting) continue;
      markSection(links, en.target);
    }
  }, { rootMargin: "-30% 0px -60% 0px" });
  for (const [, t] of links) obs.observe(t);
}
// The rail's active pill carries the state for a screen reader too, not just
// a color: aria-current says which section is in view.
function markSection(links, target) {
  for (const [a, t] of links) {
    const on = t === target;
    a.classList.toggle("active", on);
    if (on) a.setAttribute("aria-current", "true");
    else a.removeAttribute("aria-current");
  }
}
// The library panel's retry lives on its own note when the ask fails; keep
// wireCollapse's loop untouched here.
function wireCollapse() {
  for (const panel of document.querySelectorAll(".panel")) {
    const head = panel.querySelector(":scope > .panel-head");
    if (!head || !panel.id) continue;
    const btn = el("button", "collapse-btn");
    btn.type = "button";
    btn.setAttribute("aria-expanded", String(!state.collapsed.includes(panel.id)));
    btn.setAttribute("aria-label", "fold the " + (panel.getAttribute("aria-label") || panel.id) + " panel");
    btn.append(icon("down", 14));
    btn.addEventListener("click", () => {
      const folded = panel.classList.toggle("collapsed");
      btn.setAttribute("aria-expanded", String(!folded));
      state.collapsed = state.collapsed.filter((id) => id !== panel.id);
      if (folded) state.collapsed.push(panel.id);
      try { localStorage.setItem(COLLAPSE_KEY, JSON.stringify(state.collapsed)); } catch (e) { /* ignore */ }
    });
    head.append(btn);
    if (state.collapsed.includes(panel.id)) panel.classList.add("collapsed");
  }
}
function wireShortcuts() {
  // a header button for the operators who never press ?
  const keys = el("button", "icon-btn");
  keys.type = "button";
  keys.id = "keys-btn";
  keys.setAttribute("aria-label", "keyboard shortcuts");
  keys.title = "keyboard shortcuts (?)";
  keys.append(icon("command", 15));
  keys.addEventListener("click", () => showDialog(shortcutsDlg));
  const mode = document.querySelector(".app-header .mode");
  if (mode) mode.before(keys);
  shortcutsDlg.querySelector(".sc-close").addEventListener("click", () => shortcutsDlg.close());
  const tabFor = { 1: "tab-harbor", 2: "tab-switch", 3: "tab-engines", 4: "tab-about" };
  for (const k of Object.keys(tabFor)) onKey(k, () => $(tabFor[k]).click());
  onKey("b", () => setMode("basic", true));
  onKey("a", () => setMode("advanced", true));
  onKey("p", () => $("log-pause").click());
  onKey("r", () => { fetchStatus(); fetchHistory(); });
  onKey("/", (e) => { e.preventDefault(); if (logSearchInput) logSearchInput.focus(); });
  onKey("?", () => showDialog(shortcutsDlg));
  // R-07: Esc means one thing everywhere. onKey already defers to an open dialog
  // and to a text field, so this is the layer under them: a typed log filter.
  onKey("Escape", () => {
    if (state.logQuery && clearLogFilter) clearLogFilter();
  });
}
// The phone's 56px rail has room for the wordmark, the two state pills, the
// density and one more control: the menu. Its sheet holds what the wide header
// says in text (the node's versions, the last successful read) plus the retry,
// the shortcut list and the detail level the rail can only show. It is a native
// dialog, so Esc closes it and focus returns to the button that opened it.
function wireRailMenu() {
  const btn = $("menu-btn");
  if (btn) {
    btn.append(icon("menu", 18));
    btn.addEventListener("click", () => showDialog(railMenuDlg));
  }
  if (!railMenuDlg) return;
  railMenuDlg.querySelector(".sheet-close").addEventListener("click", () => railMenuDlg.close());
  for (const item of railMenuDlg.querySelectorAll("[data-mode]")) {
    item.addEventListener("click", () => {
      setMode(item.dataset.mode, true);
      railMenuDlg.close();
    });
  }
  const retry = $("sheet-retry");
  if (retry) retry.addEventListener("click", () => {
    railMenuDlg.close();
    fetchStatus();
    fetchHistory();
  });
  const keys = $("sheet-keys");
  if (keys) keys.addEventListener("click", () => {
    railMenuDlg.close();
    showDialog(shortcutsDlg);
  });
  paintSheetMeta();
}
// The sheet's facts are the header's, painted where a phone can read them.
function paintSheetMeta() {
  if (!railMenuDlg) return;
  const s = state.status;
  let versions = "no status read yet";
  if (s) versions = "node " + (s.version || "?") + (s.harbor_version ? " / harbor " + s.harbor_version : "");
  else if (state.demo) versions = "the demo page reads no node";
  setText($("sheet-node"), versions);
  setText($("sheet-read"), state.lastGood
    ? relTime(state.lastGood) + " (" + fmtClock(new Date(state.lastGood)) + ")"
    : "no successful read yet");
  const c = connState();
  const why = { live: "reading the node live", stale: "no fresh status within 10 seconds",
    down: (state.nodeDown && state.nodeDown.message) || "node unreachable",
    demo: "fixtures from demo.json" }[c];
  setText($("sheet-conn"), CONN_WORD[c] + " - " + why);
}
// Loading shimmer in the data panels until the first status lands; every
// first render replaces its skeleton with real content or an honest note.
function paintSkeletons() {
  const skel = (root, n) => { if (root && root.childElementCount === 0) root.append(skeleton(n)); };
  skel($("board"), 4);
  skel(logList, 6);
  skel(berthsRoot, 3);
  skel(machineRoot, 3);
  skel(claimsRoot, 1);
  skel(stowRoot, 2);
  skel(notickRoot, 2);
  skel($("engine-grid"), 2);
  skel($("library-root"), 4);
  skel($("kv-pool"), 3);
  skel($("ledger"), 2);
  skel($("leaderboard"), 4);
  skel($("recent"), 4);
  skel($("residency-sessions"), 3);
  skel(storageEl, 5);
}
// The card tab's voyage count and the engines tab's engine count ride on
// the tabs, so a glance at the header says which tab wants attention.
function paintTabCounts(voyages, engines) {
  setTabCount($("tab-harbor"), voyages);
  setTabCount($("tab-engines"), engines);
}
function setTabCount(tab, n) {
  if (!tab) return;
  let badge = tab.querySelector(".count");
  if (n == null) { if (badge) badge.remove(); return; }
  if (!badge) { badge = el("span", "count"); tab.append(badge); }
  badge.textContent = String(n);
}
function setBoardSort(sort) {
  state.boardSort = sort;
  if (state.status) {
    const entries = boardEntries(state.status);
    board.render(entries, berthLabel(state.status), state.boardSort);
    boardMeta.textContent = entries.length + " voyage" + (entries.length === 1 ? "" : "s");
  }
}

// ---------------- connection ----------------
function connState() {
  if (state.demo) return "demo";
  if (state.nodeDown) return "down";
  if (state.fetchFailing) return "stale";
  if (state.lastGood === 0) return "stale";
  if (Date.now() - state.lastGood > STALE_MS) return "stale";
  return "live";
}
// The conn pill's four states, spelled in one place: the pill, and the menu
// sheet's copy of the same fact, never drift.
const CONN_WORD = { live: "LIVE", stale: "STALE", down: "NODE DOWN", demo: "DEMO" };
// The pill is a live region: it is rebuilt only when the connection state
// itself changes, never once a second because the relative age moved.
let connSig = null;
function setConn() {
  const s = connState();
  const label = CONN_WORD[s];
  const down = s === "down" && state.nodeDown;
  const text = down ? label + "  " + state.nodeDown.message : label;
  const title = down ? state.nodeDown.message
    : { live: "reading the node live", stale: "no fresh status within 10 seconds", down: "", demo: "fixtures from demo.json" }[s];
  const sig = s + "|" + text + "|" + title;
  if (sig === connSig) { paintConnAge(); return; }
  connSig = sig;
  connPill.className = "conn " + s;
  connPill.title = title;
  connPill.replaceChildren(pillSi(s), el("span", "word", text));
  if (s === "live" || s === "demo") {
    const ago = el("span", "ago");
    ago.id = "conn-ago";
    ago.setAttribute("aria-hidden", "true"); // decoration: the state word is the message
    connPill.append(ago);
    paintConnAge();
  } else {
    // stale or down: the retry reads the node now instead of waiting out the poll
    const retry = el("button", "retry");
    retry.type = "button";
    retry.setAttribute("aria-label", "read the node now");
    retry.title = "read the node now";
    retry.append(icon("refresh", 12));
    retry.addEventListener("click", () => { fetchStatus(); fetchHistory(); });
    connPill.append(retry);
  }
}
function paintConnAge() {
  const ago = $("conn-ago");
  if (ago) setText(ago, state.lastGood ? relTime(state.lastGood) : "");
}
function refreshConn() {
  setConn();
  paintOverall();
  paintVM();
  paintLastUpdate();
}
// The VM tag: the governed card is not on the node, it is bound to vfio-pci or
// switching to or from that. It reads beside the state pill on every tab, so
// the card's absence is visible without opening a panel. Hidden when the node
// reports no passthrough.
let vmSig = null;
// anyVM is the VM state of a card in this status: the status-wide field
// when every card is in a VM, else the first card that is.
function anyVM(s) {
  if (!s) return null;
  return s.passthrough || (s.gpus || []).map((g) => g.passthrough).find(Boolean) || null;
}
function paintVM() {
  const p = anyVM(state.status);
  const sig = p ? p.state + "|" + p.driver : "";
  if (sig === vmSig || !vmPill) return;
  vmSig = sig;
  if (!p) { vmPill.hidden = true; return; }
  vmPill.hidden = false;
  setText(vmPill, p.state === "detaching" ? "GPU TO VM" : "GPU IN VM");
  vmPill.title = p.state === "detaching"
    ? "a VFIO hold is granted and the card's driver is switching, driver " + (p.driver || "unbound")
    : "the governed card is bound to " + p.driver + " and serves a virtual machine";
}
function paintLastUpdate() {
  if (!lastUpdate) return;
  lastUpdate.textContent = state.lastGood ? "updated " + relTime(state.lastGood) : "never read";
  lastUpdate.title = state.lastGood ? "last successful status read: " + fmtClock(new Date(state.lastGood)) : "last successful status read";
  paintSheetMeta();
}
// overallState is the operator's state, reconciled against the conn pill's
// transport state: DEMO on the fixture page, offline when the node is down,
// degraded on any alert level, a latched engine, or a stale read, live
// otherwise.
function overallState() {
  if (state.demo) return "demo";
  const c = connState();
  if (c === "down") return "offline";
  const lv = alertLevels();
  // While the card is in a VM no engine can hold it, so an engine stopped for
  // the session is expected, not a fault.
  const inVM = !!anyVM(state.status);
  const latched = !inVM && (state.status && state.status.engines || []).some((e) => e.state === "latched");
  if (Object.keys(lv).length > 0 || latched) return "degraded";
  if (c === "stale") return "degraded"; // no fresh status within 10 s: not provably live
  return "live";
}
// alertLevels is the page's view of the node's sticky levels. While the card is
// in a VM (or switching to one) its probe failure is the known passthrough
// state, not a fault, so the probe level reaches neither pill nor strip.
function alertLevels() {
  const lv = (state.alerts && state.alerts.levels) || {};
  if (anyVM(state.status) && lv.probe) {
    const rest = { ...lv };
    delete rest.probe;
    return rest;
  }
  return lv;
}
// the state pill is a live region too: rebuild it only when the state or its
// explanation changes, not on every poll
let stateSig = null;
function paintOverall() {
  const s = overallState();
  const lv = alertLevels();
  const sorted = alertKeys(lv);
  const title = sorted.length ? alertMsg(sorted[0], lv[sorted[0]], state.status) : "";
  const sig = s + "|" + title;
  if (sig === stateSig) return;
  stateSig = sig;
  statePill.className = "conn state state-" + s;
  statePill.replaceChildren(pillSi(s), el("span", "word", { live: "LIVE", degraded: "DEGRADED", offline: "OFFLINE", demo: "DEMO" }[s]));
  statePill.title = title;
}

// The conn and state pills show the state's shape. The glyph keeps the pill's
// .dot class, so the existing .conn.<state> .dot colour rules fill it.
function pillSi(state) {
  const g = siEl(state);
  g.classList.add("dot");
  return g;
}

// ---------------- data ----------------
async function fetchStatus() {
  if (state.demo) return;
  try {
    const res = await fetch("/v1/status", { headers: { accept: "application/json" } });
    if (res.status === 502) {
      const body = await res.json().catch(() => ({}));
      state.nodeDown = { message: body.message || "node unreachable" };
      state.fetchFailing = false;
      state.alerts = null;
    } else if (res.ok) {
      state.status = await res.json();
      state.alerts = state.status.alerts || null;
      state.vmFirstSeen = state.status.passthrough ? (state.vmFirstSeen || Date.now()) : 0;
      state.nodeDown = null;
      state.fetchFailing = false;
      state.lastGood = Date.now();
      onStatus(state.status);
    } else {
      state.fetchFailing = true;
    }
  } catch (e) {
    state.fetchFailing = true;
  }
  refreshConn();
}
async function fetchHistory() {
  if (state.demo) return;
  // 1h is the node's live window (the default); anything longer is a store
  // read. The live window refreshes on the 10 s gate, store ranges at most
  // once a minute, so a phone on the LAN never re-pulls the whole hour.
  const range = state.range === "1h" ? "" : "?range=" + state.range;
  if (Date.now() - state.lastHist < (state.range === "1h" ? LIVE_POLL_MS : RANGE_POLL_MS)) return;
  state.lastHist = Date.now();
  try {
    const res = await fetch("/v1/history" + range, { headers: { accept: "application/json" } });
    if (res.ok) {
      state.history = await res.json();
      onHistory();
    }
  } catch (e) { /* the node may predate /v1/history; history stays empty */ }
}
function pollHistory() {
  if (state.demo || document.hidden) return;
  fetchHistory();
}
// The timeline shares the tide's range and its cadence: 1h refreshes with
// the tide's live poll, longer ranges are store reads at most once a minute.
// Unlike history, spans exist for every range, 1h included.
async function fetchSpans() {
  if (state.demo) return;
  if (state.range !== "1h" && Date.now() - state.lastSpans < RANGE_POLL_MS) return;
  state.lastSpans = Date.now();
  try {
    const res = await fetch("/v1/spans?range=" + state.range, { headers: { accept: "application/json" } });
    if (res.ok) {
      state.spans = await res.json();
      paintTimeline();
    }
  } catch (e) { /* the harbor may predate /v1/spans; the panel keeps its last view */ }
}
function pollSpans() {
  if (state.demo || document.hidden) return;
  fetchSpans();
}
// The library is the models view's data: one walk of the roots, cached 30 s
// server-side, so the page's ask is bounded by the walk's cost, not the poll.
// A failed ask keeps the last listing, or shows its own retry note.
async function fetchModels(force) {
  if (state.demo) return;
  if (!force && Date.now() - state.lastModels < MODEL_POLL_MS) return;
  state.lastModels = Date.now();
  try {
    const res = await fetch("/v1/models", { headers: { accept: "application/json" } });
    if (res.ok) {
      state.models = await res.json();
      if (state.status) libraryView.render(state.models, state.status);
    } else {
      libraryView.failed();
    }
  } catch (e) {
    libraryView.failed(); // the harbor may predate /v1/models; the note says so
  }
}
function pollModels() {
  if (state.demo || document.hidden) return;
  fetchModels();
}
function pollStorage() {
  if (state.demo || document.hidden) return;
  if (Date.now() - state.lastStorage < RANGE_POLL_MS) return;
  fetchStorage();
}
async function fetchStorage() {
  if (state.demo) return;
  state.lastStorage = Date.now();
  try {
    const res = await fetch("/v1/storage", { headers: { accept: "application/json" } });
    if (res.ok) {
      state.storage = await res.json();
      state.storageFailed = false;
      paintStorage();
      return;
    }
    noteStorageFailed();
  } catch (e) {
    // the harbor may run without a store: say so in the panel rather than
    // leave the shimmer running forever
    noteStorageFailed();
  }
}
function noteStorageFailed() {
  state.storageFailed = true;
  paintStorage();
}
function tick() {
  refreshConn();
  fetchStatus();
}
let eventDebounce = 0;
function scheduleEventPoll() {
  clearTimeout(eventDebounce);
  // Event bursts refresh the status-driven panels (board, counters, berths,
  // stowaways, KPI values); the charts keep their own cadence (10 s live
  // window / one-minute store reads), so an event storm never triggers a
  // second full history or spans pull.
  eventDebounce = setTimeout(() => {
    if (!document.hidden) { fetchStatus(); }
  }, DEBOUNCE_MS);
}
function startEvents() {
  if (state.demo) return;
  if (state._es) { try { state._es.close(); } catch (e) { /* already closed */ } }
  const es = new EventSource("/v1/events");
  state._es = es;
  // The node restarted: its Seq began again at 1 and the server is replaying
  // the new generation's ring. Reset the dead generation's log and let the
  // replay repopulate it, and force a history fetch past the 10 s gate
  // because the node's in-memory window restarted empty.
  es.addEventListener("rebase", () => {
    state.lastSeq = 0;
    state.events.length = 0;
    state.lastHist = 0;
    lastLogSig = null; // the dead generation's rows belong to events that no longer exist
    fetchStatus();
    fetchHistory();
    paintLog();
  });
  es.addEventListener("berth", (m) => {
    let ev = null;
    try { ev = JSON.parse(m.data); } catch (e) { return; }
    if (!ev || typeof ev.kind !== "string") return;
    if (state.lastSeq > 0 && ev.seq != null && ev.seq <= state.lastSeq) return; // replay after a reconnect
    state.lastSeq = Math.max(state.lastSeq, ev.seq || 0);
    pushEvent(ev);
    scheduleEventPoll();
  });
  es.addEventListener("error", () => {
    state.fetchFailing = true;
    refreshConn();
  });
}
function wireDemo() {
  if (!state.demo) return;
  demoStrip.hidden = false;
  const set = (s) => { state.status = s; state.alerts = s.alerts || null; state.lastGood = Date.now(); onStatus(s); };
  refreshConn();
  fetch("demo.json")
    .then((r) => r.json())
    .then((j) => {
      if (Array.isArray(j.events)) for (const ev of j.events) pushEvent(ev);
      state.storage = j.storage || null; // the demo's storage panel fixture
      state.models = j.models || null;   // the demo's library fixture, in /v1/models shape
      paintStorage();
      set(j);
      refreshConn();
      if (state.logPaused === false) paintLog();
    })
    .catch(() => {
      demoStrip.textContent = "DEMO DATA - NOT LIVE. demo.json could not be loaded: " + location.href;
      refreshConn();
    });
}
// ---------------- demo history ----------------
// ?demo never calls /v1/*: the 1h range plays back the recorded demo-history
// window, and every longer range is synthesized from its last samples, so
// the picker works on fixtures for 6h through 30d alike.
async function refreshDemoHistory() {
  if (!state.demoHist) {
    try {
      const r = await fetch("demo-history.json");
      if (r.ok) state.demoHist = await r.json();
    } catch (e) { /* synthesis falls back on its built-in base sample */ }
  }
  state.history = state.range === "1h" ? state.demoHist : synthHistory(state.range, state.demoHist);
  state.spans = demoSpans(); // the demo timeline, in /v1/spans shape
  onHistory();
}
// ?demo synthesizes the model timeline from the same samples the tide draws:
// the holder runs of each tide engine become their models' spans (llama-swap
// swaps between two models while its band stays up), plus the engines the
// demo status reports but the tide never holds on the scheduled card.
function demoSpans() {
  const samples = state.history && state.history.samples ? state.history.samples : [];
  if (samples.length < 2) return { from: "", to: "", spans: [] };
  const t0 = new Date(samples[0].t).getTime();
  const t1 = new Date(samples[samples.length - 1].t).getTime();
  const range = t1 - t0;
  const span = (engine, model, a, b, open) => ({ engine, model, start: a, end: open ? null : b });
  const runsOf = (eng) => {
    const rs = [];
    let cur = null;
    for (const sm of samples) {
      const t = new Date(sm.t).getTime();
      if (((sm.holders || {})[eng] || 0) > 0) {
        if (!cur) cur = { a: t, b: t };
        cur.b = t;
      } else if (cur) {
        rs.push(cur);
        cur = null;
      }
    }
    if (cur) rs.push(cur);
    return rs.map((r) => ({ ...r, open: r.b === t1 }));
  };
  const out = [];
  for (const r of runsOf("llama-swap")) {
    if (r.b - r.a > 0.4 * range) {
      // a long holding: the main model, a swap, then the main model back
      const m1 = r.a + (r.b - r.a) * 0.5;
      const m2 = r.a + (r.b - r.a) * 0.85;
      out.push(span("llama-swap", "NInfer-Qwen3.8-27B", r.a, m1, false));
      out.push(span("llama-swap", "agent-fast", m1, m2, false));
      out.push(span("llama-swap", "NInfer-Qwen3.8-27B", m2, r.b, r.open));
    } else {
      out.push(span("llama-swap", "NInfer-Qwen3.8-27B", r.a, r.b, r.open));
    }
  }
  for (const r of runsOf("comfyui")) out.push(span("comfyui", "flux-krea", r.a, r.b, r.open));
  for (const r of runsOf("batch-embed")) out.push(span("batch-embed", "bge-m3", r.a, r.b, r.open));
  const F = (f) => t0 + f * range;
  out.push(span("ollama", "qwen3:8b", F(0.1), F(0.35), false));
  out.push(span("ollama", "qwen3:8b", F(0.8), t1, true)); // loaded now, like the demo status
  out.push(span("strata", "strata", F(0.55), F(0.75), false)); // ran earlier, offline now
  return {
    from: new Date(t0).toISOString(),
    to: new Date(t1).toISOString(),
    spans: out,
  };
}
const DEMO_BASE_SAMPLE = {
  gpus: [
    { index: 0, used: 17179869184, util_pct: 60, temp_c: 61, power_w: 320 },
    { index: 1, used: 271577600, util_pct: 3, temp_c: 34, power_w: 18 },
  ],
  holders: { "llama-swap": 17179869184, comfyui: 3221225472 },
  decode_tps: { "llama-swap": 118 },
  cpu_pct: 25, ram_used: 32212254720, rpm: 2, p95_ms: 6400,
};
function synthHistory(range, base) {
  const every = RANGE_EVERY[range] || 60;
  const n = Math.max(8, Math.round(RANGE_MS[range] / 1000 / every));
  const end = Date.now();
  const last = base && Array.isArray(base.samples) && base.samples.length ? base.samples[base.samples.length - 1] : null;
  const g0 = (last && last.gpus && last.gpus[0]) || DEMO_BASE_SAMPLE.gpus[0];
  const g1 = (last && last.gpus && last.gpus[1]) || DEMO_BASE_SAMPLE.gpus[1];
  const holders = (last && last.holders) || DEMO_BASE_SAMPLE.holders;
  const dec = (last && last.decode_tps) || DEMO_BASE_SAMPLE.decode_tps;
  const cpu = last ? last.cpu_pct : DEMO_BASE_SAMPLE.cpu_pct;
  const ram = last ? last.ram_used : DEMO_BASE_SAMPLE.ram_used;
  const kv = last ? last.kv : null; // the demo's KV state rides along with the synthesized ranges
  const samples = [];
  for (let i = 0; i < n; i++) {
    const ph = (i / n) * Math.PI * 2 * (range === "30d" ? 4 : range === "7d" ? 2 : 1);
    const scale = (e, lo, hi) => lo + (hi - lo) * (0.5 + 0.5 * Math.sin(ph + e.length * 0.7));
    const wave = 0.5 + 0.5 * Math.sin(ph);
    const busy = 0.72 + 0.28 * wave; // the card swings from mostly free to pinned
    const wig = Math.sin(ph * 2.7);
    const holdersOut = {};
    for (const e in holders) holdersOut[e] = Math.round((holders[e] || 0) * scale(e, 0.85, 1.0));
    const decOut = {};
    for (const e in dec) decOut[e] = Math.max(0, (dec[e] || 0) * scale(e, 0.7, 1.0));
    samples.push({
      t: new Date(end - RANGE_MS[range] + i * every * 1000).toISOString(),
      gpus: [
        { index: g0.index, used: Math.round(g0.used * busy), util_pct: Math.max(0, Math.round(g0.util_pct * busy + wig * 10)), temp_c: Math.max(30, Math.round(g0.temp_c + wig * 6)), power_w: Math.round(g0.power_w * busy) },
        { index: g1.index, used: g1.used, util_pct: g1.util_pct, temp_c: g1.temp_c, power_w: g1.power_w },
      ],
      holders: holdersOut,
      cpu_pct: Math.max(2, Math.round(cpu + wig * 8)),
      ram_used: Math.round(ram * (0.96 + 0.04 * wave)),
      decode_tps: decOut,
      rpm: Math.max(0, ((last ? last.rpm : 2) || 2) * (0.5 + wave)),
      p95_ms: Math.max(200, (last ? last.p95_ms : 6400) * (1 + wig * 0.2)),
      kv,
    });
  }
  return { every_seconds: every, samples };
}
// Plausible stored-range insights for the demo: the 24 h columns of the
// fixture scaled to the range, same shape as /v1/insights.
function demoInsights(range) {
  const days = RANGE_MS[range] / 864e5;
  const ins = state.status && state.status.insights;
  const base = ins || { ledger: [], leaderboard: [], recent: [], since: new Date(Date.now() - 30 * 864e5).toISOString(), rpm: 0, p50_ms: 0, p95_ms: 0 };
  const col = (l) => (Array.isArray(base.ledger) ? base.ledger.find((c) => c.label === l) : null) || { label: l, requests: 0, generated: 0, fresh: 0, cached: 0 };
  const one = col("1h"), day = col("24h");
  const round = (x) => Math.round(x * days);
  const roundF = (x) => x * days;
  return {
    requests_5m: ins ? base.requests_5m : 0,
    rpm: base.rpm || 0,
    p50_ms: base.p50_ms || 0,
    p95_ms: base.p95_ms || 0,
    prefill_ms: base.prefill_ms || 0,
    tpot_ms: base.tpot_ms || 0,
    decode_tps: base.decode_tps || 0,
    prefill_tps: base.prefill_tps || 0,
    cache_pct: base.cache_pct || 0,
    reingest_pct: base.reingest_pct || 0,
    mtp_pct: base.mtp_pct || 0,
    ledger: [
      { label: "1h", requests: one.requests, generated: one.generated, fresh: one.fresh, cached: one.cached },
      { label: range === "24h" ? "24h" : range, requests: round(day.requests), generated: roundF(day.generated), fresh: roundF(day.fresh), cached: roundF(day.cached) },
    ],
    leaderboard: (base.leaderboard || []).map((r) => ({
      engine: r.engine, model: r.model,
      requests: round(r.requests),
      prompt_tokens: roundF(r.prompt_tokens),
      output_tokens: roundF(r.output_tokens),
      decode_tps_median: r.decode_tps_median,
      cache_pct: r.cache_pct, mtp_pct: r.mtp_pct,
    })),
    recent: base.recent || [],
    since: base.since || "",
  };
}
function onStatus(s) {
  nodeVersion.textContent = "node " + (s.version || "") + (s.harbor_version ? " / harbor " + s.harbor_version : "");
  setText($("about-version"), nodeVersion.textContent);
  renderAll(s);
  paintOverall();
  if (pendingSection) {
    const target = pendingSection;
    pendingSection = null;
    requestAnimationFrame(() => target.scrollIntoView({ block: "start" }));
  }
  if (pendingTicket) {
    const key = pendingTicket;
    pendingTicket = null;
    openTicket(key);
  }
  for (const e of s.engines || []) engineView.sample(e.name, e.decode_tps, e.prefill_tps);
  if (state.crange !== "all" && Date.now() - state.lastCw >= LIVE_POLL_MS) fetchCounterWindow();
  if (state.lbrange !== "24h") {
    // the stored range's view needs the status only as a "the store exists"
    // signal; it then refreshes on the one-minute gate, not every poll
    if (!state.insights || Date.now() - state.lastIns >= RANGE_POLL_MS) fetchInsights();
  } else {
    state.insights = null;
  }
  renderLbViews();
  refreshCharts();
}
function onHistory() {
  computeHistSeries();
  if (state.status) updateKpiStrip(state.status);
  residency.refresh(state.history, state.insights);
  refreshCharts();
}

// ---------------- KPI strip ----------------
const KPIS = [
  { key: "decode_tps", label: "DECODE", unit: "t/s", fmt: (v) => na(v, (v) => v.toFixed(1)), spark: true, title: "tokens per second generated, summed across the running engines; sparkline over the last hour" },
  { key: "tpot_ms", label: "TPOT", unit: "ms", fmt: (v) => na(v, (v) => v.toFixed(1)), spark: true, alertKey: "tpot", title: "time per output token, the inverse of decode rate, last 15 min; the sparkline is derived from the decode series" },
  { key: "p95_ms", label: "P95", unit: "", fmt: (v) => fmtMS(v), spark: true, alertKey: "p95", title: "request duration that 95% of recent requests finished within, last 15 min; sparkline over the last hour" },
  { key: "rpm", label: "REQ/MIN", unit: "", fmt: (v) => na(v, (v) => v.toFixed(1)), spark: true, title: "requests completed per minute over the last 5 min; sparkline over the last hour" },
  { key: "queue", label: "QUEUE", unit: "", fmt: (v) => na(v, (v) => String(Math.round(v))), value: (s) => (s.waiting || []).length, spark: true, alertKey: "queue", title: "engines queued on the card (the berth queue); each engine's own queue is on its card; sparkline over the selected range" },
  { key: "cache_pct", label: "CACHE", unit: "%", fmt: (v) => fmtPct(v), alertKey: "cache", title: "share of prompt tokens served from the KV cache instead of recomputed, last hour" },
  { key: "mtp_pct", label: "DRAFT ACCEPT", unit: "%", fmt: (v) => fmtPct(v), title: "of the tokens the draft model proposed, the share the main model accepted; accepted over drafted, last hour" },
  { key: "prefill_tps", label: "PREFILL", unit: "t/s", advOnly: true, fmt: (v) => na(v, (v) => v.toFixed(1)), title: "prompt tokens processed per second (the engine's intake rate), median last 15 min" },
  { key: "prefill_ms", label: "PREFILL", unit: "", advOnly: true, fmt: (v) => fmtMS(v), alertKey: "ttft", title: "derived time-to-first-token: uncached prompt tokens over the prefill rate, last 15 min" },
  { key: "p50_ms", label: "P50", unit: "", advOnly: true, fmt: (v) => fmtMS(v), title: "median request duration, last 15 min" },
  { key: "reingest_pct", label: "REINGEST", unit: "%", advOnly: true, fmt: (v) => fmtPct(v), alertKey: "reingest", title: "share of prompt tokens that had to be computed from scratch (the complement of cache), last hour" },
];
function buildKpiStrip() {
  for (const k of KPIS) {
    const tile = el("div", "kpi" + (k.advOnly ? " adv-only" : ""));
    tile.title = k.title;
    const top = el("div", "kpi-top");
    const label = el("span", "kpi-label", k.label);
    top.append(label);
    const unit = el("span", "kpi-unit", k.unit);
    top.append(unit);
    const value = el("div", "kpi-value");
    const spark = null;
    tile.append(top, value);
    let sparkCtl = null;
    if (k.spark) {
      const holder = el("div", "kpi-spark");
      sparkCtl = sparkline([], { w: 100, h: 26, color: k.unit === "ms" ? PAL.amber : PAL.cyan, area: true });
      holder.append(sparkCtl.el);
      tile.append(holder);
    }
    kpiTiles[k.key] = { tile, value, spark: sparkCtl, unit, label };
    kpiStripEl.append(tile);
  }
}
function setKpi(key, valueText) {
  const t = kpiTiles[key];
  if (t) t.value.textContent = valueText;
}
// Derives the per-sample series a KPI sparkline is built from, and the
// tide's holder palette assignment, which the timeline reuses.
function computeHistSeries() {
  const samples = state.history && state.history.samples ? state.history.samples : [];
  const decode = new Array(samples.length).fill(0);
  const byName = new Map();
  for (let i = 0; i < samples.length; i++) {
    const sm = samples[i];
    const d = sm.decode_tps || {};
    let sum = 0;
    for (const k in d) sum += d[k] || 0;
    decode[i] = sum;
    for (const eng in sm.holders || {}) byName.set(eng, (byName.get(eng) || 0) + (sm.holders[eng] || 0));
  }
  holderColors.clear();
  [...byName.keys()].sort((a, b) => byName.get(b) - byName.get(a))
    .forEach((eng, i) => holderColors.set(eng, HOLDER_PALETTE[i % HOLDER_PALETTE.length]));
  const tpot = decode.map((v) => (v > 0 ? 1000 / v : 0));
  histSeries = {
    decode_tps: decode,
    tpot_ms: tpot,
    p95_ms: samples.map((s) => s.p95_ms || 0),
    rpm: samples.map((s) => s.rpm || 0),
    // stored samples carry it from the row's waiting_max; live samples from
    // the status's waiting leases
    waiting: samples.map((s) => s.waiting || 0),
  };
}
function updateKpiStrip(s) {
  const ins = s && s.insights;
  const lv = (state.alerts && state.alerts.levels) || {};
  for (const k of KPIS) {
    const t = kpiTiles[k.key];
    setKpi(k.key, k.value ? k.value(s) : (ins ? k.fmt(ins[k.key]) : "not reported"));
    if (t.spark) t.spark.set(histSeries[k.key] || []);
    // the tile's level comes only from the node's sticky levels; the page
    // never re-derives one from a raw value
    const l = k.alertKey ? lv[k.alertKey] : "";
    t.tile.classList.toggle("lvl-warn", l === "warn");
    t.tile.classList.toggle("lvl-crit", l === "crit");
    setKpiLevel(t, l);
  }
}
// A KPI's level reads as the attention shape beside its word, so warn and crit
// do not depend on the tile's colour alone.
function setKpiLevel(t, l) {
  const want = l === "warn" || l === "crit" ? l : "";
  let g = t.label._si;
  if (!want) {
    if (g) g.hidden = true;
    return;
  }
  if (!g) {
    g = siEl(want);
    t.label.prepend(g);
    t.label._si = g;
  }
  g.hidden = false;
  const cls = "si si-" + want;
  if (g.className !== cls) g.className = cls;
}

// ---------------- alerts ----------------
// The page paints exactly what the node's evaluator held: the levels come
// only from s.alerts.levels, never recomputed from a raw value. The strip's
// message text is composed from the same values the node evaluated, with
// the same guards alerts.FromStatus uses, so it reads like the node's own
// notification; a missing value renders "not reported".
function schedGpu(s) {
  const gs = (s && s.gpus) || [];
  for (const g of gs) if (g.scheduled) return g;
  return gs.length ? gs[0] : null;
}
function alertValue(key, s) {
  const ins = s.insights;
  const g = schedGpu(s);
  switch (key) {
    case "queue":
      return (s.waiting || []).length;
    case "ttft":
      return ins ? ins.prefill_ms : null;
    case "tpot":
      return ins ? ins.tpot_ms : null;
    case "p95":
      return ins ? ins.p95_ms : null;
    case "reingest":
      return ins ? ins.reingest_pct : null;
    case "cache": // only while there was request traffic
      return ins && ins.requests_5m > 0 ? ins.cache_pct : null;
    case "vram":
      return g && g.total > 0 ? g.used / g.total : null;
    case "temp":
      return g && !isNA(g.temp_c) ? g.temp_c : null;
    case "power":
      return g && g.power_w > 0 && g.power_limit_w > 0 ? g.power_w / g.power_limit_w : null;
    case "kv": {
      let best = null;
      for (const e of s.engines || [])
        for (const sl of e.slots || [])
          if (sl.window > 0) {
            const p = sl.used / sl.window;
            if (best == null || p > best) best = p;
          }
      return best;
    }
  }
  return null;
}
// crit first, then warn, then alphabetical: the strip's line order.
function alertKeys(lv) {
  return Object.keys(lv).sort((a, b) =>
    (lv[a] === "crit" ? 0 : 1) - (lv[b] === "crit" ? 0 : 1) || a.localeCompare(b));
}
// The boundary the message names: the level's own threshold, falling back
// to warn when the level's is not set, like the node's own messages.
function alertBound(key, level, s) {
  const t = ((s.alerts && s.alerts.table) || {})[key] || {};
  const word = level === "crit" && t.crit ? "crit" : "warn";
  const v = word === "crit" ? t.crit : t.warn;
  if (v == null || !Number.isFinite(v)) return "";
  let show;
  if (key === "vram" || key === "power" || key === "kv") show = Math.round(v * 100) + "%";
  else if (key === "temp") show = Math.round(v) + "°C";
  else if (key === "ttft" || key === "tpot" || key === "p95") show = Math.round(v) + " ms";
  else if (key === "cache" || key === "reingest") show = Math.round(v) + " %";
  else show = String(Math.round(v));
  return word + " at " + show;
}
function alertMsg(key, level, s) {
  const bound = alertBound(key, level, s);
  const note = bound ? " (" + bound + ")" : "";
  const v = alertValue(key, s);
  const miss = (t) => (t == null ? "not reported" : t);
  const g = schedGpu(s);
  switch (key) {
    case "queue":
      return v + " waiting" + note;
    case "ttft":
    case "tpot":
    case "p95":
      return miss(v == null ? null : fmtMS(v)) + note;
    case "cache":
    case "reingest":
      return miss(v == null ? null : fmtPct(v)) + note;
    case "vram":
      return miss(v == null ? null : Math.round(v * 100) + "% of card") + note;
    case "temp":
      return (g ? "gpu" + g.index + " " : "gpu ") + miss(v == null ? null : v + "°C") + note;
    case "power":
      return (g && !isNA(g.power_w) && !isNA(g.power_limit_w)
        ? g.power_w.toFixed(0) + "W of " + g.power_limit_w.toFixed(0) + "W"
        : "not reported") + note;
    case "kv":
      return miss(v == null ? null : "KV " + Math.round(v * 100) + "% of window") + note;
    case "probe":
      return "GPU probe failing: " + (s.probe_error || "driver probe failing");
    case "stuck": {
      let n = 0;
      for (const l of [...(s.leases || []), ...(s.waiting || [])]) if (l.state === "stuck") n++;
      return n + " leases aground";
    }
  }
  return key;
}
// Alert cards: one per level the node holds, with a severity badge, the
// node's own message, when it first showed, and a dismiss that lasts until
// the level clears or changes. The strip only rebuilds when the set of
// levels changes, so a screen reader is not re-told the same alert.
let lastAlertSig = null;
function paintAlerts(s) {
  const lv = (s.alerts && s.alerts.levels) || {};
  const now = Date.now();
  for (const k of Object.keys(lv)) if (!state.alertSeen[k]) state.alertSeen[k] = now;
  for (const k of Object.keys(state.alertSeen)) if (!lv[k]) { delete state.alertSeen[k]; delete state.alertDismissed[k]; }
  const keys = alertKeys(lv).filter((k) =>
    state.alertDismissed[k] !== lv[k] && !(anyVM(s) && k === "probe"));
  const sig = keys.map((k) => k + ":" + lv[k]).join("|") + "#" + keys.map((k) => alertMsg(k, lv[k], s)).join("|");
  if (sig === lastAlertSig) return;
  lastAlertSig = sig;
  alertStripEl.replaceChildren();
  if (keys.length === 0) { alertStripEl.hidden = true; return; }
  alertStripEl.hidden = false;
  for (const k of keys) {
    const msg = alertMsg(k, lv[k], s);
    const card = el("div", "alert-card " + lv[k]);
    const badge = el("span", "alert-badge", lv[k].toUpperCase());
    const text = el("span", "alert-msg", msg);
    const when = el("span", "alert-when", "since " + fmtClock(new Date(state.alertSeen[k] || now)));
    const dismiss = el("button", "alert-dismiss");
    dismiss.type = "button";
    dismiss.setAttribute("aria-label", "dismiss the " + k + " alert");
    dismiss.title = "dismiss until it clears or changes";
    dismiss.append(icon("close", 12));
    dismiss.addEventListener("click", () => {
      state.alertDismissed[k] = lv[k];
      lastAlertSig = null;
      paintAlerts(state.status || s);
    });
    card.append(siEl(lv[k] === "crit" ? "crit" : "warn"), badge, text, when, dismiss);
    alertStripEl.append(card);
  }
}

// ---------------- the tide ----------------
// The card's windows in a VM: [start, end] pairs in epoch ms, recovered from
// the node's card events, so the tide draws the session as a gap in the data
// and never as the frozen last reading. An open window is one with no end:
// the card is still in the VM right now, or the page saw a passthrough status
// whose start event has fallen out of the event ring.
function vmWindows(now) {
  const out = [];
  let open = null;
  for (const ev of state.events) {
    if (ev.kind === "card.passthrough") {
      if (open == null) open = new Date(ev.at).getTime();
    } else if (ev.kind === "card.returned" && open != null) {
      out.push([open, new Date(ev.at).getTime()]);
      open = null;
    }
  }
  if (open != null) {
    out.push([open, now]);
    return out;
  }
  return state.status && state.status.passthrough && state.vmFirstSeen
    ? [[state.vmFirstSeen, now]]
    : out;
}
function inVmWindow(t, win) {
  for (const [a, b] of win) if (t >= a && t <= b) return true;
  return false;
}
// The card events' plain words in the log and on the tide's event ticks: the
// node's own detail is the driver, the word is the state.
function eventCardText(e) {
  const g = schedGpu(state.status);
  const card = g && g.index != null ? "gpu" + g.index : "the card";
  const bare = e.kind === "card.passthrough";
  return bare ? card + " passed through to a VM" : card + " back from the VM";
}
// The axis reads the current range when it paints, so one chart object
// serves every range: clock times within a day, short dates for 7d and 30d.
function xAxisLabel(d) {
  return state.range === "7d" || state.range === "30d" ? fmtDayShort(d) : fmtClockShort(d);
}
function tideTipLabel(d) {
  return state.range === "7d" || state.range === "30d" ? fmtDayShort(d) + " " + fmtClockShort(d) : fmtClock(d);
}
function buildTide() {
  if (!tideChart) {
    tideChart = stackedArea({ t: [], total: 1, series: [], events: [] }, {
      xLabels: 5,
      fmtX: (ts) => xAxisLabel(new Date(ts)),
      fmtTip: (ts) => tideTipLabel(new Date(ts)),
    });
    tideEl.append(tideChart.el);
  }
}
// How the selected range reads in panel meta, and the honest note for when
// the store covers less of it than asked for.
const RANGE_WORD = { "1h": "last hour", "6h": "last 6h", "24h": "last 24h", "7d": "last 7d", "30d": "last 30d" };
function rangeSinceNote(samples) {
  if (state.range === "1h" || !samples || samples.length < 2) return "";
  const first = new Date(samples[0].t);
  const last = new Date(samples[samples.length - 1].t);
  if (last - first >= RANGE_MS[state.range] * 0.9) return ""; // covers the range
  const since = state.range === "7d" || state.range === "30d"
    ? fmtDate(first)
    : fmtDate(first) + " " + fmtClockShort(first);
  return ", history since " + since;
}
function rangeStepNote() {
  const e = state.history && state.history.every_seconds;
  if (!e) return "";
  if (e >= 3600) return ", 1h step";
  if (e >= 60) return ", 1m step";
  return ", " + Math.round(e) + " s step";
}
function tideSeries(s) {
  const gpus = (s && s.gpus) || [];
  const sched = gpus.find((g) => g.scheduled);
  const samples = state.history && state.history.samples ? state.history.samples : [];
  const n = samples.length;
  if (!sched || n < 2) return null;
  // samples inside a VM passthrough window carry the frozen pre-hold reading:
  // they are holes, not data
  const win = vmWindows(Date.now());
  const holes = samples.map((sm) => inVmWindow(new Date(sm.t).getTime(), win));
  const nHoles = holes.filter(Boolean).length;
  const cap = tideCapacity(s);
  const total = cap.total / GIB;
  const base = cap.baseline / GIB;
  const byName = new Map();
  for (let i = 0; i < n; i++) {
    const h = samples[i].holders || {};
    for (const eng in h) {
      if (!byName.has(eng)) byName.set(eng, { total: 0, data: new Array(n).fill(0) });
      byName.get(eng).data[i] = (h[eng] || 0) / GIB;
      byName.get(eng).total += h[eng] || 0;
    }
  }
  const names = [...byName.keys()].sort((a, b) => byName.get(b).total - byName.get(a).total);
  const series = [{ label: "baseline", kind: "baseline", color: PAL.baseline, data: new Array(n).fill(base) }];
  names.forEach((eng, i) => series.push({ label: eng, kind: "holder", color: HOLDER_PALETTE[i % HOLDER_PALETTE.length], data: byName.get(eng).data }));
  series.push({ label: "squatter", kind: "squat", color: PAL.rose, data: samples.map((sm) => (sm.squat || 0) / GIB) });
  if (nHoles) for (const band of series) band.data = band.data.map((v, i) => (holes[i] ? null : v));
  const events = state.events
    .filter((ev) => EVENT_TONE[ev.kind])
    .map((ev) => ({ t: new Date(ev.at).getTime(), tone: EVENT_TONE[ev.kind], kind: ev.kind, engine: ev.engine || "", detail: ev.detail || "" }));
  const t = samples.map((sm) => new Date(sm.t).getTime());
  // the card events ride the tide too: a hole sticks out, its boundary says why
  for (const ev of state.events) {
    if (ev.kind !== "card.passthrough" && ev.kind !== "card.returned") continue;
    events.push({ t: new Date(ev.at).getTime(), tone: "stuck", kind: kindWord(ev.kind), engine: "", detail: eventCardText(ev) });
  }
  return { total, base, t, series, events, holes, nHoles };
}
function updateTide(s) {
  buildTide();
  const data = tideSeries(s);
  if (!data) {
    tideMeta.textContent = state.range === "1h"
      ? "no card scheduled or no history yet"
      : "no card scheduled or no stored history for " + RANGE_WORD[state.range];
    tideChart.set({ t: [], total: 1, series: [], events: [] });
    paintTideLegend([]);
    return;
  }
  // 1h keeps its measured window; the stored ranges name their span and the
  // store's own step, and say when the store holds less than the range asked
  const meta = state.range === "1h"
    ? tideCapacity(s).title + " memory, last " + Math.round((data.t[data.t.length - 1] - data.t[0]) / 60000) + " min, " + (state.history && state.history.every_seconds ? Math.round(state.history.every_seconds) : "?") + " s"
    : tideCapacity(s).title + " memory, " + RANGE_WORD[state.range] + rangeStepNote() + rangeSinceNote(state.history.samples);
  tideMeta.textContent = meta;
  setAttr(tideChart.el, "aria-label", "scheduled card memory by holder, " + RANGE_WORD[state.range] + ". Focus shows a data point.");
  const lastHolder = data.series[1] ? (data.series[1].data[data.t.length - 1] || 0).toFixed(1) : "0";
  const sig = data.total + "|" + data.t[0] + "|" + data.t[data.t.length - 1] + "|" + data.series.map((b) => b.label).join(",") + "|" + lastHolder + "|" + data.events.length + "|" + data.nHoles;
  if (sig === tideChart._sig) { paintTideLegend(data.series); return; }
  tideChart._sig = sig;
  tideChart.set({ t: data.t, total: data.total, series: data.series, events: data.events, holes: data.holes });
  paintTideLegend(data.series);
}
function paintTideLegend(series) {
  tideLegend.replaceChildren();
  const item = (label, cls) => el("span", "legend-item " + cls, el("span", "legend-sw", ""), label);
  const mk = (label, color, hatch) => {
    const sw = el("span", "legend-sw" + (hatch ? " hatch" : ""));
    sw.style.background = hatch ? "" : color;
    return el("span", "legend-item", sw, label);
  };
  for (const band of series) {
    if (band.kind === "baseline") tideLegend.append(mk(band.label, band.color));
    else if (band.kind === "holder") tideLegend.append(mk(band.label, band.color));
  }
  tideLegend.append(mk("squatter", "", true));
  const free = mk("free (to total)", PAL.free);
  free.classList.add("legend-dim");
  tideLegend.append(free);
}

// ---------------- the model timeline ----------------
// One row per engine/model, drawn under the tide with its exact x geometry
// (the shared chart constants), so a loaded model reads against the band it
// held. Colors follow the tide palette where the engine is a tide holder.
function engineColor(engine) {
  if (holderColors.has(engine)) return holderColors.get(engine);
  if (!tlFallback.has(engine)) tlFallback.set(engine, HOLDER_PALETTE[(holderColors.size + tlFallback.size) % HOLDER_PALETTE.length]);
  return tlFallback.get(engine);
}
// One row per engine/model; every span of the pair for the range sits on it.
function timelineRows() {
  const toMs = (v) => {
    const t = typeof v === "number" ? v : new Date(v).getTime();
    return Number.isNaN(t) ? 0 : t;
  };
  const byPair = new Map();
  for (const sp of (state.spans && state.spans.spans) || []) {
    const k = sp.engine + "\u0000" + sp.model;
    if (!byPair.has(k)) byPair.set(k, { engine: sp.engine, model: sp.model, color: engineColor(sp.engine), spans: [] });
    byPair.get(k).spans.push({ start: toMs(sp.start), end: sp.end ? toMs(sp.end) : null });
  }
  return [...byPair.values()].sort((a, b) => (a.engine + a.model).localeCompare(b.engine + b.model));
}
function paintTimeline() {
  const samples = state.history && state.history.samples ? state.history.samples : [];
  const meta = "when each model was loaded on each engine, " + (state.range === "1h" ? "last hour" : RANGE_WORD[state.range]);
  const rows = timelineRows();
  if (samples.length < 2 || rows.length === 0) {
    if (tlChart) { tlChart.el.remove(); tlChart = null; }
    timelineEl.replaceChildren(el("div", "empty-note", state.demo
      ? "no model timeline in the demo fixtures"
      : "no model history for this range yet; it builds as the harbor stores the node's history"));
    timelineMeta.textContent = meta;
    return;
  }
  const t0 = new Date(samples[0].t).getTime();
  const t1 = new Date(samples[samples.length - 1].t).getTime();
  if (!tlChart) {
    tlChart = timelineChart({ t0, t1, t: [], rows: [] }, {
      fmtX: (ts) => xAxisLabel(new Date(ts)),
      fmtTip: (ts) => tideTipLabel(new Date(ts)),
    });
    timelineEl.replaceChildren(tlChart.el);
    tlChart._sig = null;
  }
  // the spans endpoint repaints at 3 s in the 1 h range; skip the rebuild
  // when no span moved
  const sig = t0 + "|" + t1 + "|" + rows.map((r) => r.engine + "\u0000" + r.model + ":" + r.spans.map((sp) => sp.start + "-" + (sp.end || "open")).join(",")).join("|");
  timelineMeta.textContent = meta + ", " + rows.length + (rows.length === 1 ? " model" : " models");
  setAttr(tlChart.el, "aria-label", "model load timeline, " + meta + ". Focus reads the first row.");
  if (sig === tlChart._sig) return;
  tlChart._sig = sig;
  tlChart.set({ t0, t1, t: samples.map((sm) => new Date(sm.t).getTime()), rows });
}

// ---------------- throughput ----------------
// Decode tokens per second per engine over the tide's selected range, from
// the same history fetch. Advanced adds a smaller rpm/p95 chart whose y
// axis is ms/s-aware (fmtMS), since p95 request durations cross the
// second mark.
const tpColor = new Map();
function tpColorOf(name) {
  if (!tpColor.has(name)) tpColor.set(name, HOLDER_PALETTE[tpColor.size % HOLDER_PALETTE.length]);
  return tpColor.get(name);
}
function decodeSeries(samples) {
  const names = new Set();
  for (const s of samples) for (const e in s.decode_tps || {}) names.add(e);
  const n = samples.length;
  const out = [];
  for (const e of names) {
    out.push({ name: e, color: tpColorOf(e), unit: " t/s", data: samples.map((s) => ((s.decode_tps || {})[e]) || 0) });
  }
  if (out.length === 0) out.push({ name: "no decode reported", color: PAL.free, unit: "", data: new Array(n).fill(0) });
  return out;
}
function miniSeries(samples) {
  return [
    { name: "RPM", color: PAL.amber, unit: "", data: samples.map((s) => s.rpm || 0) },
    { name: "P95", color: PAL.green, unit: " ms", data: samples.map((s) => s.p95_ms || 0) },
  ];
}
let tpDecodeChart = null, tpMiniChart = null, tpSig = "";
function paintThroughput() {
  const samples = state.history && state.history.samples ? state.history.samples : [];
  const n = samples.length;
  const sig = n + ":" + state.range;
  if (n < 2) {
    throughputDecode.replaceChildren(el("div", "empty-note", "no history yet for this range"));
    throughputRpm.replaceChildren();
    tpDecodeChart = null;
    tpMiniChart = null;
    tpSig = sig;
    throughputMeta.textContent = "decode tokens per second, per engine";
    return;
  }
  // the chart is rebuilt when the range (or the sample count) changes,
  // because its x labels are fixed at build time; same-range refreshes
  // only repaint the lines
  if (tpSig !== sig) {
    throughputDecode.replaceChildren();
    throughputRpm.replaceChildren();
    const labels = samples.map((sm) => xAxisLabel(new Date(sm.t)));
    tpDecodeChart = lineChart(decodeSeries(samples), { w: 1200, h: 240, xLabels: labels, interactive: true });
    setAttr(tpDecodeChart.el, "aria-label", "decode tokens per second per engine, " + RANGE_WORD[state.range] + ". Focus shows a data point.");
    throughputDecode.append(tpDecodeChart.el);
    tpMiniChart = lineChart(miniSeries(samples), { w: 1200, h: 140, xLabels: labels, interactive: true, yFmt: fmtMS });
    setAttr(tpMiniChart.el, "aria-label", "requests per minute and p95 duration, " + RANGE_WORD[state.range] + ". Focus shows a data point.");
    throughputRpm.append(tpMiniChart.el);
    tpSig = sig;
    throughputMeta.textContent = "decode t/s per engine, " + RANGE_WORD[state.range] + rangeSinceNote(samples);
  } else {
    tpDecodeChart.set(decodeSeries(samples));
    if (tpMiniChart) tpMiniChart.set(miniSeries(samples));
  }
}

// ---------------- storage ----------------
// The store's disk and row accounting (advanced only, bottom of the
// engines tab). Retentions come to the browser as whole seconds.
function paintStorage() {
  if (!storageEl) return;
  const skel = storageEl.querySelector(":scope > .skel-box");
  const st = state.storage;
  if (!st) {
    if (skel) skel.remove();
    storageEl.replaceChildren(state.storageFailed
      ? errorNote("the harbor could not read its store", () => fetchStorage())
      : el("div", "empty-note", state.demo ? "no storage fixture in the demo" : "the harbor is running without a history store"));
    storageMeta.textContent = "harbor history store";
    return;
  }
  storageMeta.textContent = st.path;
  const row = (l, v) => el("div", "storage-row", el("span", "sl", l), el("span", "sv", v));
  const days = (sec) => (sec ? (sec / 86400).toFixed(sec % 86400 ? 1 : 0) + " days" : "n/a");
  storageEl.replaceChildren(
    row("FILE", st.path || "n/a"),
    row("SIZE", fmtBytes(st.bytes)),
    row("SAMPLES 1M", String((st.rows && st.rows.samples_1m) || 0)),
    row("SAMPLES 1H", String((st.rows && st.rows.samples_1h) || 0)),
    row("REQUESTS", String((st.rows && st.rows.requests) || 0)),
    row("MODEL SPANS", String((st.rows && st.rows.model_spans) || 0)),
    row("RETENTION", days(st.retention_seconds)),
    row("1H ROLLUP RETENTION", days(st.hour_retention_seconds)),
    row("OLDEST SAMPLE", st.oldest_sample ? fmtDate(st.oldest_sample) : "none stored yet"),
    row("STORE CREATED", st.created ? fmtDate(st.created) : "n/a")
  );
}

// ---------------- berths ----------------
function makeBerth(g) {
  const root = el("div", "berth" + (g.scheduled ? " is-scheduled" : " inventory"));
  const head = el("div", "berth-head");
  head.append(el("span", "berth-id", "gpu" + g.index), el("span", "berth-name", g.name || ""), el("span", "berth-note", g.scheduled ? "SCHEDULED" : "INVENTORY ONLY - NOT LEASED YET"));
  const bar = el("div", "membar");
  const memnums = el("div", "memnums");
  const telemo = el("div", "telemetry");
  const charts = el("div", "berth-charts adv-only");
  const occ = el("div", "berth-occupants");
  root.append(head, bar, memnums, telemo, charts, occ);
  return { root, head, bar, memnums, telemo, charts, occ, g };
}
function renderBerths(s) {
  const gpus = s.gpus || [];
  const skel = berthsRoot.querySelector(":scope > .skel-box");
  if (skel) skel.remove();
  const seen = new Set();
  for (const g of gpus) {
    seen.add(g.index);
    let card = berthCards.get(g.index);
    if (!card) { card = makeBerth(g); berthCards.set(g.index, card); berthsRoot.append(card.root); }
    updateBerth(card, g, s);
  }
  for (const [idx, card] of berthCards) if (!seen.has(idx)) { card.root.remove(); berthCards.delete(idx); }
}
function updateBerth(card, g, s) {
  // a lease or stowaway belongs on its own card's panel; one without a card
  // (a berthd older than per-card status) shows on every card, as before
  const onCard = (x) => !x.card || !g.card || x.card === g.card;
  card.root.classList.toggle("is-scheduled", !!g.scheduled);
  card.root.classList.toggle("inventory", !g.scheduled);
  // the card in a VM: its status names the state, its figures are the last
  // probe read before the hold, which the card does not serve now
  const pt = g.scheduled ? g.passthrough || s.passthrough || null : null;
  card.root.classList.toggle("is-vfio", !!pt);
  // the vram level tints the whole card; it comes from the node's sticky
  // levels, never re-derived here
  const alv = (s.alerts && s.alerts.levels) || {};
  card.root.classList.toggle("lvl-warn", alv.vram === "warn");
  card.root.classList.toggle("lvl-crit", alv.vram === "crit");
  setText(card.head.children[1], g.name || "");
  setText(card.head.children[2], g.scheduled ? "SCHEDULED" : "INVENTORY ONLY - NOT LEASED YET");
  // memory bar: keyed segments, so a segment that only changes width keeps
  // its node and eases to the new size instead of being replaced
  const total = Math.max(g.total, 1);
  const bands = [];
  const push = (key, bytes, cls, title) => {
    if (!bytes || bytes <= 0) return;
    bands.push({ key, width: Math.max(0.2, bytes / total * 100), cls, title });
  };
  if (!g.scheduled) {
    push("used", g.used, "seg-baseline", fmtGiB(g.used) + " GiB in use, not attributed (inventory only)");
  } else {
    push("baseline", g.baseline, "seg-baseline", "baseline " + fmtGiB(g.baseline) + " GiB, driver reserved");
    const occupied = new Set();
    let seat = 0;
    for (const l of [...(s.leases || []), ...(s.waiting || [])].filter(onCard)) {
      if (l.used > 0 && !occupied.has(l.id)) {
        occupied.add(l.id);
        seat += 1;
        push("lease:" + (l.id || "seat" + seat), l.used, "seg-lease" + (l.state === "stuck" ? " stuck" : " active"), l.engine + " " + fmtGiB(l.used) + " GiB" + (l.state === "stuck" ? " (aground)" : ""));
      }
    }
    for (const q of (s.squatters || []).filter(onCard)) push("squat:" + (q.pid || q.name), q.used, "seg-squatter", "stowaway " + q.name + " " + fmtGiB(q.used) + " GiB, no lease");
  }
  bands.push({ key: "free", width: 0, cls: "seg-free", title: "" });
  syncKeyed(card.bar, bands, (b) => b.key, () => el("div", "seg"), (node, b) => {
    const cls = "seg " + b.cls;
    if (node.className !== cls) node.className = cls;
    const w = b.width.toFixed(3) + "%";
    if (node.style.width !== w) node.style.width = w;
    if (b.title && node.title !== b.title) node.title = b.title;
  });
  if (pt) {
    // the passthrough word reads as the card's state in place of the figures;
    // the last reading keeps its cell, greyed by .is-vfio, labelled as its age
    const word = pt.state === "detaching"
      ? "being passed through to a VM" + (pt.driver ? " (driver " + pt.driver + ")" : "")
      : "passed through to a VM (" + pt.driver + ")";
    setNums(card.memnums, [
      [word, "CARD STATE"],
      [fmtGiB(g.used) + " / " + fmtGiB(g.total) + " GiB", "LAST READING, READ " + fmtAge(g.age_seconds) + " AGO"],
    ]);
  } else {
    setNums(card.memnums, [
      [fmtGiB(g.used) + " / " + fmtGiB(g.total) + " GiB", "USED / TOTAL"],
      [fmtGiB(g.free) + " GiB", "FREE"],
      [fmtGiB(g.baseline) + " GiB", "BASELINE"],
      [fmtAge(g.age_seconds), "READING AGE"],
    ]);
  }
  paintTelemetry(card, g, s);
  // occupants
  card.occ.replaceChildren();
  if (g.scheduled) {
    const fp = {};
    for (const f of (s && s.footprints) || []) if (f.engine) fp[f.engine] = f.estimated_vram_bytes || 0;
    for (const l of (s.leases || []).filter(onCard)) {
      const tier = l.state === "stuck" ? "AGROUND" : l.state === "parking" ? "PARKING" : l.state === "parked" ? "WARM" : "DOCKED";
      let ram = "";
      if (l.state === "parked") {
        if (fp[l.engine] > 0) ram = " " + fmtGiB(fp[l.engine]) + " GiB in RAM (est.)";
        if (l.parked_since != null) ram += ", parked " + fmtAge(l.parked_since);
      }
      const cls = "occ" + (l.state === "stuck" ? " stuck" : l.state === "parked" ? " warm" : "");
      const sits = l.group === "vfio" ? "in the VM" : l.state === "parked" ? ram.trim() : l.used > 0 ? fmtGiB(l.used) + " GiB" : "draining";
      const serves = leaseCardLine(s, l);
      const what = serves.model === l.engine ? l.engine : l.engine + " " + serves.model;
      const also = serves.cards ? " on " + serves.cards : "";
      card.occ.append(el("div", cls, el("span", "who", tier + " " + what + also + " " + (l.id || "")), el("span", null, sits)));
    }
    for (const q of (s.squatters || []).filter(onCard)) card.occ.append(el("div", "occ squatter", el("span", "who", "STOWAWAY " + q.name), el("span", null, fmtGiB(q.used) + " GiB")));
    if (card.occ.childElementCount === 0) card.occ.append(el("div", "occ", el("span", "who", "all berths free")));
  }
}
function setText(node, text) {
  if (node && node.textContent !== text) node.textContent = text;
}
function setAttr(node, name, value) {
  if (node && node.getAttribute(name) !== value) node.setAttribute(name, value);
}
// The four memory cells keep their nodes: only their numbers move.
function setNums(root, cells) {
  cells.forEach(([value, label], i) => {
    let cell = root.children[i];
    if (!cell) {
      cell = el("div", "n", el("span", "v"), el("span", "l"));
      root.append(cell);
    }
    setText(cell.firstChild, value);
    setText(cell.lastChild, label);
  });
  while (root.childElementCount > cells.length) root.lastElementChild.remove();
}
function paintTelemetry(card, g, s) {
  const lv = (s.alerts && s.alerts.levels) || {};
  const lvl = (k) => (lv[k] ? " lvl-" + lv[k] : "");
  const total = Math.max(g.power_limit_w || 0, 1);
  // built once per card, then only the numbers and the fill widths move: a
  // rebuilt gauge would restart its own transition and jump
  const cells = card.telemo._cells || (card.telemo._cells = {
    power: powerCell(), temp: tcell("TEMP"), util: tcell("UTIL"), adv: advCell(),
  });
  if (card.telemo.childElementCount === 0) {
    card.telemo.append(cells.power.root, cells.temp.root, cells.util.root, cells.adv.root);
  }
  const pct = isNA(g.power_w) || isNA(g.power_limit_w) ? 0 : Math.max(0, Math.min(100, (g.power_w / total) * 100));
  cells.power.root.className = "tcell t-power" + lvl("power");
  const w = pct.toFixed(1) + "%";
  if (cells.power.fill.style.width !== w) cells.power.fill.style.width = w;
  setText(cells.power.val, na(g.power_w, (v) => v.toFixed(0)) + "W" + (isNA(g.power_limit_w) ? "" : " / " + g.power_limit_w.toFixed(0)));
  cells.temp.root.className = "tcell" + lvl("temp");
  setText(cells.temp.val, na(g.temp_c, (v) => v.toFixed(0) + "°C"));
  setText(cells.util.val, na(g.util_pct, (v) => v.toFixed(0) + "%"));
  setText(cells.adv.lines[0], "SM " + na(g.clock_sm_mhz, (v) => v.toFixed(0)) + "  MEM " + na(g.clock_mem_mhz, (v) => (v / 1000).toFixed(1) + "G"));
  setText(cells.adv.lines[1], "FAN " + na(g.fan_pct, (v) => v.toFixed(0) + "%"));
  setText(cells.adv.lines[2], "PCIe gen " + na(g.pcie_gen, (v) => v.toFixed(0)) + " x" + na(g.pcie_width, (v) => v.toFixed(0)));
}
function tcell(label) {
  const val = el("span", "tv");
  return { root: el("div", "tcell", el("span", "tl", label), val), val };
}
function powerCell() {
  const fill = el("div", "powerfill");
  const val = el("span", "tv");
  return { root: el("div", "tcell t-power", el("span", "tl", "POWER"), el("div", "powerbar", fill), val), fill, val };
}
function advCell() {
  const lines = [el("span", "tline"), el("span", "tline"), el("span", "tline")];
  return { root: el("div", "tcell t-adv adv-only", ...lines), lines };
}

// Advanced per-GPU trend charts, driven by history only. The whole grid
// rebuilds only when the sample window moves, not on every status poll.
let berthChartsSig = null;
function updateBerthCharts(s) {
  const samples = state.history && state.history.samples ? state.history.samples : [];
  const sig = samples.length + "|" + (samples.length ? samples[0].t + "|" + samples[samples.length - 1].t : "");
  if (sig === berthChartsSig) return;
  berthChartsSig = sig;
  for (const [idx, card] of berthCards) {
    if (!card.g || !card.g.scheduled) continue;
    card.charts.replaceChildren();
    if (samples.length < 2) { card.charts.append(el("div", "empty-note", "no history yet")); continue; }
    // samples inside a VM passthrough window are no data, never a frozen
    // reading drawn as if the card answered
    const win = vmWindows(Date.now());
    const pick = (fn) => samples.map((sm) => {
      if (inVmWindow(new Date(sm.t).getTime(), win)) return null;
      const gs = (sm.gpus || []).find((x) => x.index === idx);
      return gs ? gs[fn] || 0 : 0;
    });
    const mk = (label, data, color, unit) => {
      const box = el("div", "mini-chart");
      box.append(el("div", "mc-label", label));
      const c = sparkline(data, { w: 100, h: 22, color, area: true, min: 0, max: maxOf(data) || 1 });
      box.append(c.el);
      box.append(el("div", "mc-val", lastOf(data, unit || "")));
      return box;
    };
    const maxOf = (a) => a.reduce((m, v) => Math.max(m, v || 0), 0);
    const lastOf = (a, unit) => {
      const v = a[a.length - 1];
      if (v == null) return "no data";
      return (v >= 1000 ? (v / 1000).toFixed(1) + "k" : v.toFixed(0)) + unit;
    };
    card.charts.append(mk("UTIL %", pick("util_pct"), PAL.cyan, "%"));
    card.charts.append(mk("TEMP °C", pick("temp_c"), PAL.amber, "°"));
    card.charts.append(mk("POWER W", pick("power_w"), PAL.green, "W"));
  }
}

// ---------------- the machine ----------------
function socketHealth(c) {
  const t = c ? c.send_timeouts : null;
  const d = c ? c.dropped_consumers : null;
  return { timeouts: t == null ? null : t, dropped: d == null ? null : d };
}
// The machine gauges keep their nodes between polls: a rebuilt bar restarts
// its own width transition every three seconds, so the fill jumps instead of
// easing to the new number.
let machineRows = null;
function meterRow(label, cls) {
  const fill = el("div", "mfill");
  const val = el("span", "mval");
  const root = el("div", "mrow" + (cls ? " " + cls : ""), el("span", "mlabel", label), el("div", "mbar", fill), val);
  return { root, fill, val };
}
function setMeter(m, used, total, text) {
  const w = (total ? Math.min(100, (used / total) * 100) : 0).toFixed(1) + "%";
  if (m.fill.style.width !== w) m.fill.style.width = w;
  setText(m.val, text);
}
function renderMachine(s) {
  const h = s && s.host;
  machineMeta.textContent = h && h.cores ? h.cores + " cores" : "host";
  const sh = socketHealth(s && s.counters);
  setText(socketLine, (sh.timeouts == null || sh.dropped == null)
    ? "socket timeouts/dropped not reported by this node"
    : "socket " + sh.timeouts + " timeouts / " + sh.dropped + " dropped");
  socketLine.title = "event socket write timeouts and slow readers dropped; when the log stalls but the node reads live, look here first";
  if (!h) {
    machineRows = null;
    // "not reported" replaces the shimmer once, and then stays put
    if (!machineRoot.querySelector(".empty-note")) machineRoot.replaceChildren(el("div", "empty-note", "host telemetry not reported by this node"));
    return;
  }
  const hasSwap = h.swap_total > 0;
  if (!machineRows || machineRows.hasSwap !== hasSwap) {
    const load = el("div", "mrow mload", el("span", "mlabel", "LOAD"), el("span", "mval"), el("span", "mlabel msub", "1m  5m  15m"));
    machineRows = {
      hasSwap,
      cpu: meterRow("CPU", "mcpu"),
      ram: meterRow("RAM"),
      swap: meterRow("SWAP"),
      load: { root: load, val: load.children[1] },
    };
    machineRoot.replaceChildren(machineRows.cpu.root, machineRows.ram.root);
    if (hasSwap) machineRoot.append(machineRows.swap.root);
    machineRoot.append(machineRows.load.root);
  }
  setMeter(machineRows.cpu, h.cpu_pct, 100, fmtPct(h.cpu_pct, 1));
  setMeter(machineRows.ram, h.ram_used, h.ram_total, fmtGiB(h.ram_used) + " / " + fmtGiB(h.ram_total) + " GiB");
  if (hasSwap) setMeter(machineRows.swap, h.swap_used, h.swap_total, fmtGiB(h.swap_used) + " / " + fmtGiB(h.swap_total) + " GiB");
  // Windows reads no load averages: the node leaves them out, and the row says so.
  setText(machineRows.load.val, na(h.load, (l) => l.map((v) => v.toFixed(2)).join("  ")));
}
// The CPU/RAM lines are history-driven and repaint on the heavy gate from
// renderHeavy, so a status poll never rebuilds them; a poll that reads the
// same window builds nothing at all.
let machineChartsSig = null;
function paintMachineCharts() {
  const samples = state.history && state.history.samples ? state.history.samples : [];
  const sig = state.mode + "|" + (samples.length ? samples[0].t + "|" + samples[samples.length - 1].t + "|" + samples.length : "0");
  if (sig === machineChartsSig) return;
  machineChartsSig = sig;
  machineCharts.replaceChildren();
  if (state.mode !== "advanced") return;
  if (!state.status || !state.status.host) return;
  if (samples.length < 2) return;
  const xlabels = samples.map((sm) => fmtClockShort(new Date(sm.t)));
  const cpu = { name: "CPU %", color: PAL.cyan, data: samples.map((sm) => sm.cpu_pct || 0) };
  const ram = { name: "RAM GiB", color: PAL.amber, data: samples.map((sm) => (sm.ram_used || 0) / GIB) };
  const cpuChart = lineChart([cpu], { w: 300, h: 90, xLabels: xlabels, yMax: 100, interactive: true, area: true });
  const ramChart = lineChart([ram], { w: 300, h: 90, xLabels: xlabels, interactive: true, area: true });
  const wrap = el("div", "machine-charts-inner");
  wrap.append(chartCard("CPU %", cpuChart), chartCard("RAM GiB", ramChart));
  machineCharts.append(wrap);
}
function chartCard(label, ctl) {
  // the wrap is focusable, so it needs a name of its own
  setAttr(ctl.el, "aria-label", label + " trend, " + RANGE_WORD[state.range] + ". Focus shows a data point.");
  return el("div", "machine-chart", el("div", "mc-label", label), ctl.el);
}

// ---------------- claims ----------------
// The claim rules matching right now, as berth claims lists them: a hold
// keeps its cards for a game, a track names an app's card memory. Idle
// rules are only counted.
function claimLine(cl) {
  if (cl.action === "track") return (cl.used || cl.gtt_used) ? memoryWord(cl) + " named, not a stowaway" : "named; holds no card memory now";
  return cl.lease ? "holds the card as " + cl.lease + "; engines wait" : "waiting for the card";
}
function memoryWord(cl) {
  return fmtGiB(cl.used || 0) + " GiB" + (cl.gtt_used ? " + " + fmtGiB(cl.gtt_used) + " GiB GTT" : "");
}
function renderClaims(s) {
  const list = el("div", "squatter-list");
  const all = s.claims || [];
  const active = all.filter((cl) => cl.active);
  claimsMeta.textContent = active.length ? active.length + " active of " + all.length + " rules" : all.length + " rules, none active";
  if (all.length === 0) list.append(el("div", "empty-note", "no claim rules: the built-in pack is off and berth.toml has no [[claim]]"));
  else if (active.length === 0) list.append(el("div", "empty-note", "no game or app is claiming a card"));
  for (const cl of active) {
    const on = cardTag(s, (cl.cards || []).join(", "));
    const name = el("span", "name", cl.name);
    if (cl.builtin) name.append(el("span", "claim-builtin", " built-in"));
    const row = el("div", "sq claim " + (cl.action === "track" ? "claim-track" : "claim-hold"));
    row.append(
      el("div", "top", name, el("span", null, (on ? on + ", " : "") + (cl.action || "hold").toUpperCase())),
      el("div", "meta", claimLine(cl)),
    );
    if (cl.procs) row.append(el("div", "meta claim-procs", cl.procs));
    list.append(row);
  }
  claimsRoot.replaceChildren(list);
}

// ---------------- stowaways and no ticket ----------------
function renderStowaways(s) {
  const list = el("div", "squatter-list");
  const sq = s.squatters || [];
  stowMeta.textContent = sq.length ? sq.length + " on the card" : "none";
  if (sq.length === 0) list.append(el("div", "empty-note", "no stowaways on the card"));
  for (const q of sq) {
    const row = el("div", "sq");
    row.append(el("div", "top", el("span", "name", q.name || "unknown"), el("span", null, (cardTag(s, q.card) ? cardTag(s, q.card) + ", " : "") + "pid " + q.pid)), el("div", "meta", fmtGiB(q.used) + " GiB held, seen " + fmtAge(q.age_seconds) + " ago"));
    if (q.unprotected_run) row.append(el("div", "noticket", "no ticket: " + q.unprotected_run));
    list.append(row);
  }
  stowRoot.replaceChildren(list);
}
function renderNoTicket(s) {
  const list = el("div", "noticket-list");
  const runs = s.unprotected || [];
  noticketMeta.textContent = runs.length ? runs.length + " unprotected" : "all ticketed";
  if (runs.length === 0) list.append(el("div", "empty-note", "every engine holds a ticket"));
  for (const r of runs) {
    const row = el("div", "nr");
    row.append(el("div", "top", el("span", "name", r.engine || "unknown"), el("span", null, "pid " + r.pid)), el("div", "meta", "since " + fmtAge(r.age_seconds) + " ago"), el("div", "meta", r.reason || "started while the node was down"));
    list.append(row);
  }
  notickRoot.replaceChildren(list);
}

// ---------------- harbor log ----------------
function pushEvent(ev) {
  state.events.unshift(ev);
  if (state.events.length > LOG_MAX) {
    // the ring drops the oldest: drop its row too, so the board never grows
    const dropped = state.events.pop();
    if (dropped && dropped._row) { dropped._row.remove(); dropped._row = null; }
  }
  if (!state.logPaused) paintLog();
}
function buildLogSearch() {
  const wrap = $("log-search-wrap");
  if (!wrap) return;
  wrap.append(icon("search", 13));
  const input = document.createElement("input");
  input.type = "search";
  input.id = "log-search";
  input.placeholder = "filter events";
  input.setAttribute("aria-label", "filter the harbor log");
  input.setAttribute("autocomplete", "off");
  input.addEventListener("input", () => {
    state.logQuery = input.value.trim().toLowerCase();
    wrap.classList.toggle("has-text", input.value.length > 0);
    paintLog();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && input.value) clearLogFilter();
    else if (e.key === "Escape") input.blur();
  });
  const clear = el("button", "clear-btn");
  clear.type = "button";
  clear.setAttribute("aria-label", "clear the log filter");
  clear.append(icon("close", 12));
  // one clear for the button, the field's own Esc and the page's Esc (R-07)
  clearLogFilter = () => {
    input.value = "";
    state.logQuery = "";
    wrap.classList.remove("has-text");
    paintLog();
  };
  clear.addEventListener("click", () => {
    clearLogFilter();
    input.focus();
  });
  wrap.append(input, clear);
  logSearchInput = input;
}
function wireLogJump() {
  if (!logWrapEl || !logJumpEl) return;
  logList.addEventListener("scroll", () => {
    const up = logList.scrollTop > 40;
    logWrapEl.classList.toggle("scrolled-up", up);
  });
  logJumpEl.append(icon("up", 12));
  logJumpEl.addEventListener("click", () => logList.scrollTo({ top: 0, behavior: prefersReduced() ? "auto" : "smooth" }));
}
// The text filter reads what the row shows: the kind word, the engine or
// lease, and the detail line.
function matchQuery(e) {
  const q = state.logQuery;
  if (!q) return true;
  const hay = (kindWord(e.kind) + " " + (e.engine || "") + " " + (e.lease || "") + " " + (e.detail || "")).toLowerCase();
  return q.split(/\s+/).every((w) => hay.includes(w));
}
function logVisible(e) {
  return state.filters[kindGroup(e.kind)] && matchQuery(e);
}
function logSig() {
  return JSON.stringify(state.filters) + "|" + state.logQuery;
}
function allFiltersOn() {
  return state.filters.leases && state.filters.stowaways && state.filters.probe && state.filters.alerts;
}
// ALL reads the four chips: with one of them off it is no longer holding.
function syncChipAll() {
  const b = $("chip-all");
  if (b) b.setAttribute("aria-pressed", String(allFiltersOn()));
}
function updateChipCounts() {
  const counts = { leases: 0, stowaways: 0, probe: 0, alerts: 0 };
  for (const e of state.events) {
    if (!matchQuery(e)) continue;
    const g = kindGroup(e.kind);
    if (counts[g] != null) counts[g] += 1;
  }
  for (const [id, group] of [["chip-leases", "leases"], ["chip-stowaways", "stowaways"], ["chip-probe", "probe"], ["chip-alerts", "alerts"]]) {
    const b = $(id);
    if (!b) continue;
    let n = b.querySelector(".n");
    if (!n) { n = el("span", "n"); b.append(n); }
    n.textContent = String(counts[group]);
  }
}
function wireLogControls() {
  const chip = (id, group) => {
    const b = $(id);
    b.addEventListener("click", () => {
      state.filters[group] = !state.filters[group];
      b.setAttribute("aria-pressed", String(state.filters[group]));
      syncChipAll();
      paintLog();
    });
  };
  chip("chip-leases", "leases");
  chip("chip-stowaways", "stowaways");
  chip("chip-probe", "probe");
  chip("chip-alerts", "alerts");
  $("chip-all").addEventListener("click", () => {
    const want = !allFiltersOn();
    for (const id of ["chip-leases", "chip-stowaways", "chip-probe", "chip-alerts"]) {
      state.filters[$(id).id.replace("chip-", "")] = want;
      $(id).setAttribute("aria-pressed", String(want));
    }
    syncChipAll();
    // the rows the filters hid have to come back now, not at the next event
    paintLog();
  });
  const pause = $("log-pause");
  pause.addEventListener("click", () => {
    state.logPaused = !state.logPaused;
    pause.setAttribute("aria-pressed", String(state.logPaused));
    pause.textContent = state.logPaused ? "RESUME" : "PAUSE";
    logNote.textContent = state.logPaused ? "log paused; events keep streaming to the node, the board just holds" : "events stream in as they happen; the page resumes where it left off";
    logMeta.textContent = state.logPaused ? "paused" : "streaming";
    if (!state.logPaused) paintLog();
  });
}
function refusalTally(events) {
  const counts = new Map();
  for (const e of events) {
    if (e.kind !== "lease.refused") continue;
    const d = ((e.detail || "").trim() || "no reason given");
    counts.set(d, (counts.get(d) || 0) + 1);
  }
  return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 3);
}
// The tally is a live region over the log; it only changes when a refusal
// reason or its count does, so it is not rewritten on every poll.
let refusalSig = null;
function paintRefusals() {
  const top = refusalTally(state.events);
  const sig = top.map(([detail, n]) => n + "× " + detail).join("·");
  if (sig === refusalSig) return;
  refusalSig = sig;
  refusalTallyEl.replaceChildren();
  if (top.length === 0) {
    refusalTallyEl.hidden = true;
    return;
  }
  refusalTallyEl.hidden = false;
  refusalTallyEl.append(el("span", "rt-label", "REFUSED"));
  top.forEach(([detail, n], i) => {
    if (i > 0) refusalTallyEl.append(el("span", "rt-sep", "·"));
    refusalTallyEl.append(el("span", "rt-item", n + "× " + detail));
  });
}
function makeLogRow(e) {
  const at = new Date(e.at);
  const row = el("div", "log-row" + (kindCss(e.kind) ? " " + kindCss(e.kind) : ""));
  const detail = el("span", "ld", e.detail || "");
  if (e.kind === "card.passthrough" || e.kind === "card.returned") {
    // the card events speak in plain words; the driver rides in the title
    detail.textContent = eventCardText(e);
    detail.title = eventCardText(e) + ", device driver " + (e.detail || "unknown");
  }
  // the phone's 2-line row ellipsises both words, so the whole of each rides on title
  detail.title = detail.title || e.detail || "";
  if (e.kind === "lease.juggle") {
    const why = el("button", "log-why", "why");
    why.type = "button";
    why.setAttribute("aria-label", "show the card ranking that decided this juggle");
    why.addEventListener("click", showPriority);
    detail.append(" ", why);
  }
  const lk = el("span", "lk");
  // the kind word leads with the shape of the state it names
  const ks = kindSi(e.kind);
  if (ks) lk.append(siEl(ks));
  lk.append(kindWord(e.kind));
  const engineText = e.engine || e.lease || "";
  const engine = el("span", "le", engineText);
  engine.title = engineText;
  row.append(el("span", "lt", Number.isNaN(at.getTime()) ? String(e.at || "") : fmtClock(at)), lk, engine,
    el("span", "lb", fmtMiB(e.bytes)), detail);
  e._row = row;
  return row;
}
// The log appends new rows and only rebuilds on a filter, search, pause or
// rebase change: a screen reader hears the new arrivals, not the whole
// 200-row board every poll. Arrivals are prepended in place, so the rows
// already on the board keep their nodes and their scroll position.
function paintLog() {
  paintRefusals();
  const visible = state.events.filter(logVisible);
  logMeta.textContent = state.logPaused ? "paused" : visible.length + " shown";
  if (logCountEl) {
    setText(logCountEl, state.logQuery
      ? visible.length + " of " + state.events.length + " shown"
      : state.events.length ? state.events.length + " events" : "");
  }
  updateChipCounts();
  // every visible event already has a row on the board: the poll only has to
  // prepend what arrived since the last paint
  const complete = visible.every((e) => e._row && e._row.isConnected);
  const sig = logSig() + (state.logPaused ? "|paused" : "|live");
  if (sig !== lastLogSig || !complete) {
    lastLogSig = sig;
    const frag = document.createDocumentFragment();
    if (visible.length === 0) {
      frag.append(el("div", "log-empty", state.events.length === 0
        ? "quiet at the harbor; events appear as they happen"
        : state.logQuery ? "no events match the filter" : "nothing in the selected groups"));
    }
    for (const e of visible) frag.append(makeLogRow(e));
    logList.replaceChildren(frag);
    return;
  }
  const pending = visible.filter((e) => !e._row);
  if (pending.length === 0) return;
  const empty = logList.querySelector(".log-empty");
  if (empty) empty.remove();
  for (let i = pending.length - 1; i >= 0; i -= 1) logList.prepend(makeLogRow(pending[i]));
}

// ---------------- counters ----------------
let prevOdo = -1;
function renderCounters(c) {
  if (!c) {
    for (const k of Object.keys(ctiles)) setCtile(ctiles[k], "—", { vram: "VRAM HANDED OFF", drains: "DRAINS", stuck: "AGROUND CAUGHT", stowaways: "STOWAWAYS CAUGHT", contested: "CONTESTED", refused: "REFUSED", juggles: "JUGGLES STOPPED" }[k]);
    return;
  }
  const all = c;
  if (state.crange !== "all") {
    const w = state.cwindow;
    if (!w || w.range !== state.crange) {
      if (state.counterStaleSince > 0) {
        counterMeta.textContent = "stale since " + fmtClock(new Date(state.counterStaleSince)) + ", showing last kept numbers";
      } else {
        // the window has not arrived yet: say so rather than show the all-time board under a window's name
        counterMeta.textContent = "reading the last " + COUNTER_WORDS[state.crange] + "...";
      }
      return;
    }
    c = w.counts;
  }
  const target = Math.min(999999, Math.max(0, Math.floor(c.collisions_avoided || 0)));
  const str = pad(target, 6);
  if (target !== prevOdo) {
    // A reel holds ten equal digits, so each step is 10% of its own height:
    // the stop follows whatever size the CSS gives the digit at this width.
    odometerReels.forEach((reel, i) => { reel.style.transform = "translateY(-" + (parseInt(str[i], 10) * 10) + "%)"; });
    prevOdo = target;
    // role=img hides the reels from a screen reader, so the name carries the
    // number: focusable it is not, so there is no announcement storm here
    setAttr($("odometer"), "aria-label", "collisions avoided: " + target.toLocaleString("en-US"));
  }
  if (state.crange === "all") {
    counterSince.textContent = "since " + fmtDate(all.since);
    counterMeta.textContent = "counts the collisions the broker has caught since " + fmtDate(all.since);
  } else {
    const w = state.cwindow;
    const words = "the last " + COUNTER_WORDS[state.crange];
    counterSince.textContent = w.partial ? "since " + fmtClockOrDate(w.from) : words;
    counterMeta.textContent = "caught in " + words + (w.partial ? " (the harbor has kept readings since " + fmtClockOrDate(w.from) + ")" : "");
  }
  setCtile(ctiles.vram, fmtGiB(c.vram_handed_off) + " GiB", "VRAM HANDED OFF");
  setCtile(ctiles.drains, String(c.drains || 0), "DRAINS");
  setCtile(ctiles.stuck, String(c.stuck_caught || 0), "AGROUND CAUGHT");
  setCtile(ctiles.stowaways, String(c.squatters_caught || 0), "STOWAWAYS CAUGHT");
  setCtile(ctiles.contested, String(c.contested_grants || 0), "CONTESTED");
  setCtile(ctiles.refused, String(c.refused || 0), "REFUSED");
  setCtile(ctiles.juggles, String(c.juggles_stopped || 0), "JUGGLES STOPPED");
}
function fmtClockOrDate(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  return Date.now() - d.getTime() < 864e5 ? fmtClock(d) : fmtDate(iso);
}
function setCtile(tile, value, label) {
  if (!tile) return;
  let v = tile.querySelector(".cvalue");
  if (!v) { tile.append(el("div", "clabel", label), (v = el("div", "cvalue"))); }
  v.textContent = value;
}

// ---------------- ticket ----------------
function openTicket(key) {
  const s = state.status;
  if (!s) return;
  const entry = findVoyage(s, key);
  if (!entry) return;
  ticket.open(entry.lease, s, key);
}
function findVoyage(s, key) {
  for (const l of s.leases || []) if (l.id === key || key === l.id) return { lease: l, isWaiting: false };
  for (const l of s.waiting || []) if (l.id === key) return { lease: l, isWaiting: true };
  return null;
}

// ---------------- render all ----------------
function showPriority() {
  const tab = $("tab-engines");
  if (tab) tab.click();
  const panel = $("priority-panel");
  if (panel && panel.scrollIntoView) panel.scrollIntoView();
}
function renderAll(s) {
  renderLight(s);
  const now = Date.now();
  if (!state.lastHeavy || now - state.lastHeavy >= LIVE_POLL_MS) {
    state.lastHeavy = now;
    renderHeavy(s);
  }
}
function renderLight(s) {
  renderCounters(s.counters);
  priorityView.render(s.priority);
  const entries = boardEntries(s);
  board.render(entries, berthLabel(s), state.boardSort);
  boardMeta.textContent = entries.length + " voyage" + (entries.length === 1 ? "" : "s");
  paintTabCounts(entries.length, (s.engines || []).length);
  renderBerths(s);
  paintAlerts(s);
  renderClaims(s);
  renderStowaways(s);
  renderNoTicket(s);
  renderMachine(s);
  enginesMeta.textContent = (s.engines || []).length + " engine" + ((s.engines || []).length === 1 ? "" : "s");
  engineView.renderCards((s && Array.isArray(s.engines)) ? s.engines : [], s && s.footprints);
  libraryView.render(state.models, s);
  engineView.renderPool(s, state.history, state.range);
  if (ticket.current) ticket.refresh();
}
function renderHeavy(s) {
  updateKpiStrip(s);
  engineView.renderTables(s.insights || null);
  if (state.lbrange === "24h" && engineView.lbMeta) engineView.lbMeta.textContent = "24 h, click a column to sort";
  residency.render(s);
  // the CPU/RAM lines rebuild from history on the 10 s heavy gate, never
  // on the 3 s status poll
  paintMachineCharts();
}
function boardEntries(s) {
  const fp = {};
  for (const f of (s && s.footprints) || []) if (f.engine) fp[f.engine] = f.estimated_vram_bytes || 0;
  const out = [];
  for (const l of s.leases || []) out.push({ key: l.id || l.engine + "#" + out.length, lease: l, isWaiting: false, fp: fp[l.engine] || 0, berth: cardTag(s, l.card) });
  for (const l of s.waiting || []) out.push({ key: l.id || "wait-" + l.engine, lease: l, isWaiting: true, fp: fp[l.engine] || 0, berth: cardTag(s, l.card) });
  return out;
}
// History-driven panels refresh together: tide, throughput, KPI sparklines,
// machine lines, the per-GPU advanced trend charts, and the KV pool's
// stored-range chart.
function refreshCharts() {
  if (!state.status) return;
  engineView.renderPool(state.status, state.history, state.range);
  if (state.mode !== "advanced") {
    updateTide(state.status);
    paintThroughput();
    paintTimeline();
    return;
  }
  updateTide(state.status);
  paintThroughput();
  updateBerthCharts(state.status);
  paintMachineCharts();
  paintTimeline();
}

document.readyState === "loading" ? document.addEventListener("DOMContentLoaded", main) : main();
