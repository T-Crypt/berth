// Engine cards plus the request analytics (token ledger, leaderboard, recent
// requests). Engine sparklines are browser-sampled, decoded and prefilled from
// each poll; the ledger/leaderboard/recent come from the node's insights.
import { el, fmtGiB, fmtNum, fmtAge, fmtClock, fmtTok, fmtPct, fmtMS, na, fmtClockShort, fmtDayShort } from "./util.js";
import { PAL } from "./palette.js";
import { sparkline, lineChart } from "./charts.js";
const HIST = 60;
// The stored pool chart reads the same range words and step notes the tide
// and throughput panels show, so the engines tab reads as one instrument.
const RANGE_WORD = { "1h": "last hour", "6h": "last 6h", "24h": "last 24h", "7d": "last 7d", "30d": "last 30d" };
function rangeWord(range) {
  return RANGE_WORD[range] || RANGE_WORD["1h"];
}
function stepNote(every) {
  if (!every) return "";
  if (every >= 3600) return ", 1h step";
  if (every >= 60) return ", 1m step";
  return ", " + Math.round(every) + " s step";
}

function makeCard(eng) {
  const card = el("div", "engine-card" + (eng.up ? "" : " offline"));
  const top = el("div", "engine-top");
  const name = el("span", "engine-name", eng.name || "?");
  const type = el("span", "engine-type", (eng.type || "").toUpperCase());
  const updown = el("span", "engine-state", eng.up ? "UP" : "OFFLINE");
  top.append(name, type, updown);
  card.append(top);
  const error = el("div", "engine-error", eng.error || "");
  card.append(error);
  const models = el("div", "engine-models");
  card.append(models);
  const tps = el("div", "engine-tps");
  const mkBlock = (label, color) => {
    const block = el("div", "tps-block");
    const value = el("div", "tps-value");
    const num = el("span");
    const unit = el("span", "unit", "t/s");
    value.append(num, unit);
    const sparkHolder = el("div", "tps-spark");
    const spark = sparkline([], { w: 100, h: 24, color, area: true, min: 0 });
    sparkHolder.append(spark.el);
    block.append(el("div", "tps-label", label), value, sparkHolder);
    return { block, num, spark, color };
  };
  const decode = mkBlock("DECODE", PAL.cyan);
  const prefill = mkBlock("PREFILL", PAL.amber);
  tps.append(decode.block, prefill.block);
  card.append(tps);
  const totals = el("div", "engine-totals");
  card.append(totals);
  const extra = el("div", "engine-extra");
  card.append(extra);
  return { card, name, type, updown, error, models, totals, extra, decode, prefill, hist: { decode: [], prefill: [] } };
}

export class EngineView {
  constructor(grid, ledger, leaderboard, recent, recentMeta, lbMeta, pool, poolChart, poolMeta) {
    this.grid = grid;
    this.ledger = ledger;
    this.leaderboard = leaderboard;
    this.recent = recent;
    this.recentMeta = recentMeta;
    this.lbMeta = lbMeta || null;
    this.pool = pool || null;
    this.poolChart = poolChart || null;
    this.poolMeta = poolMeta || null;
    this.colorOf = null; // assigned by the page: engine -> tide color
    this.cards = new Map(); // engine name -> card bundle
    this.sort = { key: "requests", dir: -1 };
    this.poolLine = null;
    this.poolSig = "";
    this.staleNote = "";
  }
  sample(name, decode, prefill) {
    const c = this.cards.get(name);
    if (!c) return;
    c.hist.decode.push(decode || 0);
    if (c.hist.decode.length > HIST) c.hist.decode.shift();
    c.hist.prefill.push(prefill || 0);
    if (c.hist.prefill.length > HIST) c.hist.prefill.shift();
  }
  render(status) {
    const engines = status && Array.isArray(status.engines) ? status.engines : [];
    this.renderCards(engines);
    const ins = (status && status.insights) || null;
    this.renderTables(ins);
    if (this.lbMeta) this.lbMeta.textContent = "24 h, click a column to sort";
  }
  renderTables(ins) {
    this.renderLedger(ins);
    this.renderLeaderboard(ins);
    this.renderRecent(ins);
  }

  markStale(note) {
    this.staleNote = note;
    for (const m of [this.lbMeta, this.recentMeta, this.poolMeta]) {
      if (!m || m.textContent.includes("stale since")) continue;
      m.textContent = m.textContent ? m.textContent + ", " + note : note;
    }
  }
  clearStale() {
    this.staleNote = "";
  }
  restamp(m) {
    if (m && this.staleNote && !m.textContent.includes("stale since")) {
      m.textContent = m.textContent ? m.textContent + ", " + this.staleNote : this.staleNote;
    }
  }
  // The same three panels from the stored range's /v1/insights, when the
  // leaderboard's picker is off its 24 h default. The sort state carries
  // over, so a column sorted on 24 h stays sorted on 7 d.
  renderInsights(ins, range) {
    if (!ins) return;
    this.renderLedger(ins);
    this.renderLeaderboard(ins);
    this.renderRecent(ins);
    if (this.lbMeta) {
      const word = range === "7d" ? "7 d, stored" : range === "30d" ? "30 d, stored" : "24 h, stored";
      this.lbMeta.textContent = word + ", click a column to sort";
      this.restamp(this.lbMeta);
    }
  }
  renderCards(engines) {
    const skel = this.grid.querySelector(":scope > .skel-box");
    if (skel) skel.remove();
    const seen = new Set();
    for (const eng of engines) {
      const key = eng.name || "?" + engines.indexOf(eng);
      seen.add(key);
      let c = this.cards.get(key);
      if (!c) { c = makeCard(eng); this.cards.set(key, c); this.grid.append(c.card); }
      c.name.textContent = eng.name || "?";
      c.type.textContent = (eng.type || "").toUpperCase();
      // the node latches the state: up answers, latched was up and stopped
      // answering, offline was never confirmed up. Old nodes send no state:
      // derive it.
      const st = eng.state || (eng.up ? "up" : "down");
      c.updown.textContent = { up: "UP", latched: "UNRESPONSIVE", down: "OFFLINE", offline: "OFFLINE" }[st] || "OFFLINE";
      c.card.className = "engine-card" + (st === "up" ? "" : st === "latched" ? " latched" : " offline");
      c.card.title = st === "latched" ? "was up; readings stale" : "";
      c.error.textContent = st === "up" ? "" : eng.error || "";
      c.error.hidden = st === "up";
      c.models.textContent = "";
      if (Array.isArray(eng.models) && eng.models.length > 0) {
        for (const m of eng.models) {
          const vram = m.reported_vram ? " " + fmtGiB(m.reported_vram) + "G" : "";
          c.models.append(el("div", "engine-model", el("span", null, m.name || "?"), el("span", "ms", (m.state || "") + vram)));
        }
      } else {
        c.models.append(el("div", "engine-models-note", "no models reported"));
      }
      c.decode.num.textContent = (eng.decode_tps || 0).toFixed(1);
      c.prefill.num.textContent = (eng.prefill_tps || 0).toFixed(1);
      c.decode.spark.set(c.hist.decode);
      c.prefill.spark.set(c.hist.prefill);
      const m = eng.metrics || {};
      c.totals.textContent = "";
      c.totals.append(
        el("span", null, "in " + fmtNum(m.prompt_tokens) + " tok"),
        el("span", null, "out " + fmtNum(m.output_tokens) + " tok"),
        el("span", null, "queue " + (Math.round(m.processing || 0) + "+" + Math.round(m.waiting || 0)))
      );
      c.extra.textContent = "";
      if (m.draft_tokens > 0) {
        const pct = ((m.draft_accepted || 0) / m.draft_tokens) * 100;
        c.extra.append(el("span", null, "draft acceptance " + pct.toFixed(1) + "% (" + fmtNum(m.draft_accepted) + "/" + fmtNum(m.draft_tokens) + ")"));
      }
      // one KV line when the engine reports a window or live slots: the
      // deepest slot fill against the engine's window budget
      const slots = Array.isArray(eng.slots) ? eng.slots : [];
      if (slots.length > 0) {
        let fill = 0;
        for (const s of slots) {
          if (s.window > 0) fill = Math.max(fill, Math.min(1, s.used / s.window));
        }
        c.extra.append(el("span", null, eng.window > 0
          ? "KV " + Math.round(fill * 100) + "% of " + fmtTok(eng.window) + " window"
          : "KV " + Math.round(fill * 100) + "% of slot window"));
      } else if (eng.window > 0) {
        c.extra.append(el("span", null, "window " + fmtTok(eng.window) + " tok"));
      }
      c.extra.append(el("span", null, "report " + fmtAge(eng.age_seconds) + " old"));
    }
    for (const [key, c] of this.cards) {
      if (!seen.has(key)) { c.card.remove(); this.cards.delete(key); }
    }
  }

  // ---------------- token ledger ----------------
  renderLedger(ins) {
    this.ledger.replaceChildren();
    if (!ins || !Array.isArray(ins.ledger) || ins.ledger.length === 0) {
      this.ledger.append(el("div", "empty-note", "the node has not logged requests yet"));
      return;
    }
    const cols = el("div", "ledger-cols");
    for (const col of ins.ledger) {
      const box = el("div", "ledger-col");
      const sum = (col.generated || 0) + (col.fresh || 0) + (col.cached || 0);
      const head = el("div", "ledger-head", el("span", "ledger-label", col.label), el("span", "ledger-req", col.requests + " requests"));
      box.append(head);
      const bar = el("div", "ledger-bar");
      const seg = (v, cls) => {
        const d = el("div", "lseg " + cls);
        d.style.width = (sum ? (v / sum) * 100 : 0).toFixed(2) + "%";
        d.title = cls + " " + fmtTok(v) + " tokens";
        return d;
      };
      bar.append(seg(col.generated || 0, "gen"), seg(col.fresh || 0, "fresh"), seg(col.cached || 0, "cached"));
      box.append(bar);
      box.append(el("div", "ledger-nums",
        el("span", "ln gen", "generated " + fmtTok(col.generated || 0)),
        el("span", "ln fresh", "fresh " + fmtTok(col.fresh || 0)),
        el("span", "ln cached", "cached " + fmtTok(col.cached || 0))));
      cols.append(box);
    }
    const legend = el("div", "ledger-legend",
      el("span", null, el("span", "lg-sw gen", ""), "generated"),
      el("span", null, el("span", "lg-sw fresh", ""), "fresh (uncached prompt)"),
      el("span", null, el("span", "lg-sw cached", ""), "cached"));
    this.ledger.append(cols, legend);
  }

  // ---------------- leaderboard ----------------
  renderLeaderboard(ins) {
    const rows = (ins && Array.isArray(ins.leaderboard)) ? ins.leaderboard : [];
    this.leaderboard.replaceChildren();
    if (rows.length === 0) { this.leaderboard.append(el("div", "empty-note", "no leaderboard data yet")); return; }
    const cols = [
      { key: "model", label: "Model", cls: "" },
      { key: "engine", label: "Engine", cls: "" },
      { key: "requests", label: "Requests", cls: "num" },
      { key: "prompt_tokens", label: "Prompt", cls: "num", fmt: fmtTok },
      { key: "output_tokens", label: "Output", cls: "num", fmt: fmtTok },
      { key: "decode_tps_median", label: "Decode med", cls: "num", fmt: (v) => na(v, (v) => v.toFixed(0)) },
      { key: "cache_pct", label: "Cache", cls: "num adv-only", fmt: (v) => fmtPct(v, 0) },
      { key: "mtp_pct", label: "Draft acc", cls: "num adv-only", fmt: (v) => fmtPct(v, 0) },
    ];
    const table = el("table", "ltable");
    const thead = el("thead");
    const hrow = el("tr");
    for (const c of cols) {
      const th = el("th", c.cls + (this.sort.key === c.key ? " sorted" : ""));
      th.setAttribute("aria-sort", this.sort.key === c.key ? (this.sort.dir === -1 ? "descending" : "ascending") : "none");
      const btn = el("button", "lth", c.label);
      btn.type = "button";
      btn.setAttribute("aria-label", "sort by " + c.label);
      btn.addEventListener("click", () => {
        if (this.sort.key === c.key) this.sort.dir *= -1;
        else { this.sort.key = c.key; this.sort.dir = c.cls.startsWith("num") ? -1 : 1; }
        this.renderLeaderboard(ins);
      });
      th.append(btn);
      hrow.append(th);
    }
    thead.append(hrow);
    const body = el("tbody");
    const sorted = [...rows].sort((a, b) => {
      const x = a[this.sort.key], y = b[this.sort.key];
      if (typeof x === "string") return this.sort.dir * String(x).localeCompare(String(y));
      return this.sort.dir * ((x || 0) - (y || 0));
    });
    for (const r of sorted) {
      const tr = el("tr");
      for (const c of cols) {
        const td = el("td", c.cls);
        td.textContent = c.fmt ? c.fmt(r[c.key]) : String(r[c.key] != null ? r[c.key] : "");
        tr.append(td);
      }
      body.append(tr);
    }
    table.append(thead, body);
    this.leaderboard.append(table);
  }

  // ---------------- recent requests ----------------
  renderRecent(ins) {
    this.recent.replaceChildren();
    const rows = (ins && Array.isArray(ins.recent)) ? ins.recent : [];
    if (this.recentMeta) this.recentMeta.textContent = rows.length ? rows.length + " newest first" : "none";
    this.restamp(this.recentMeta);
    if (rows.length === 0) { this.recent.append(el("div", "empty-note", "no requests logged yet")); return; }
    const cols = [
      { key: "time", cls: "" },
      { key: "model", cls: "" },
      { key: "status", cls: "num adv-only" },
      { key: "prompt", cls: "num adv-only" },
      { key: "cached", cls: "num adv-only" },
      { key: "output", cls: "num" },
      { key: "prefill", cls: "num adv-only" },
      { key: "decode", cls: "num" },
      { key: "duration", cls: "num" },
      { key: "pfillms", cls: "num adv-only" },
    ];
    const table = el("table", "rtable");
    const thead = el("thead");
    const hrow = el("tr");
    for (const c of cols) {
      const th = el("th", c.cls);
      th.textContent = { time: "TIME", model: "MODEL", status: "STATUS", prompt: "PROMPT", cached: "CACHED", output: "OUTPUT", prefill: "PREFILL t/s", decode: "DECODE t/s", duration: "DURATION", pfillms: "PREFILL" }[c.key];
      hrow.append(th);
    }
    thead.append(hrow);
    const body = el("tbody");
    const limit = rows.length > 25 ? 25 : rows.length;
    for (let i = 0; i < limit; i++) {
      const r = rows[i];
      const d = new Date(r.t);
      const ok = r.status === 200;
      const tr = el("tr", ok ? "" : "bad");
      const cell = (key, text, cls) => { const td = el("td", cls); td.textContent = text; return td; };
      tr.append(
        cell("time", Number.isNaN(d.getTime()) ? String(r.t || "") : fmtClock(d), ""),
        cell("model", r.model || "?", ""),
        cell("status", String(r.status), "num adv-only"),
        cell("prompt", fmtTok(r.prompt_tokens), "num adv-only"),
        cell("cached", fmtTok(r.cache_tokens), "num adv-only"),
        cell("output", String(Math.round(r.output_tokens || 0)), "num"),
        cell("prefill", na(r.prefill_tps, (v) => v.toFixed(0)), "num adv-only"),
        cell("decode", na(r.decode_tps, (v) => v.toFixed(0)), "num"),
        cell("duration", (r.duration_ms >= 1000 ? (r.duration_ms / 1000).toFixed(1) + "s" : (r.duration_ms || 0).toFixed(0) + "ms"), "num"),
        cell("pfillms", fmtMS(r.prefill_ms), "num adv-only")
      );
      body.append(tr);
    }
    table.append(thead, body);
    const wrap = el("div", "recent-wrap");
    wrap.append(table);
    this.recent.append(wrap);
  }

  // ---------------- KV pool ----------------
  // Live (the 1 h range): one cell per slot the engines report via /slots,
  // fill = used against the slot's window, 75/90 % ticks as the only early
  // warning. Stored (6 h - 30 d): the per-engine peak slot fill the store
  // folded from the samples' kv columns.
  renderPool(status, history, range) {
    if (!this.pool || !this.poolChart) return;
    if (range === "1h") this.paintPoolLive(status);
    else {
      this.paintPoolStored(history, range);
      // the kv tint belongs to the live view; a stored range shows none
      const panel = this.pool.parentElement;
      if (panel) { panel.classList.remove("lvl-warn"); panel.classList.remove("lvl-crit"); }
    }
  }
  paintPoolLive(status) {
    this.poolLine = null;
    this.poolChart.replaceChildren();
    // the kv level tints the panel: the node's sticky level, not a value
    // this page recomputed
    const lv = (status && status.alerts && status.alerts.levels) || {};
    const panel = this.pool.parentElement;
    if (panel) {
      panel.classList.toggle("lvl-warn", lv.kv === "warn");
      panel.classList.toggle("lvl-crit", lv.kv === "crit");
    }
    const engines = (status && Array.isArray(status.engines)) ? status.engines : [];
    this.pool.replaceChildren();
    let n = 0;
    let reporting = 0;
    for (const eng of engines) {
      if (!eng.up) continue; // a down engine's read failed: no live slots
      const slots = Array.isArray(eng.slots) ? eng.slots : [];
      if (slots.length === 0) continue;
      reporting++;
      const color = this.colorOf ? this.colorOf(eng.name) : PAL.cyan;
      for (const s of slots) {
        const frac = s.window > 0 ? Math.max(0, Math.min(1, s.used / s.window)) : 0;
        const tag = s.model ? " " + s.model : "";
        const cell = el("div", "pool-cell");
        const bar = el("div", "pool-bar");
        const fill = el("div", "pool-fill");
        fill.style.height = (frac * 100).toFixed(1) + "%";
        fill.style.background = color;
        bar.append(fill, el("div", "pool-tick tick75"), el("div", "pool-tick tick90"));
        cell.append(
          bar,
          el("span", "pool-pct", Math.round(frac * 100) + "%"),
          el("span", "pool-id", (s.id || "?") + tag)
        );
        cell.title = (eng.name || "?") + " " + (s.id || "?") + tag + ": " + fmtTok(s.used)
          + " of " + fmtTok(s.window) + " tokens"
          + (s.cached ? ", " + fmtTok(s.cached) + " cached" : "")
          + ", " + (s.processing ? "processing" : "idle");
        this.pool.append(cell);
        n++;
      }
    }
    if (n === 0) {
      this.pool.append(el("div", "empty-note", "no slot data: no engine on this node reports /slots"));
      this.poolMeta.textContent = "no slot data";
      this.restamp(this.poolMeta);
      return;
    }
    this.poolMeta.textContent = n + " live slot" + (n === 1 ? "" : "s") + ", " + reporting
      + (reporting === 1 ? " engine reports" : " engines report") + " /slots";
    this.restamp(this.poolMeta);
  }
  paintPoolStored(history, range) {
    const samples = history && Array.isArray(history.samples) ? history.samples : [];
    const names = new Set();
    for (const s of samples) {
      for (const e in s.kv || {}) names.add(e);
    }
    if (samples.length < 2 || names.size === 0) {
      if (this.poolLine) { this.poolLine.el.remove(); this.poolLine = null; this.poolSig = ""; }
      this.poolChart.replaceChildren(el("div", "empty-note", "no stored KV data for this range yet"));
      this.poolMeta.textContent = "peak slot fill per engine, " + rangeWord(range);
      this.restamp(this.poolMeta);
      return;
    }
    const labels = samples.map((sm) => (range === "7d" || range === "30d" ? fmtDayShort(new Date(sm.t)) : fmtClockShort(new Date(sm.t))));
    const series = [];
    for (const e of [...names].sort()) {
      series.push({
        name: e,
        color: this.colorOf ? this.colorOf(e) : PAL.cyan,
        unit: "%",
        data: samples.map((s) => (((s.kv || {})[e] || {}).pressure || 0) * 100),
      });
    }
    const sig = samples.length + ":" + range + ":" + [...names].sort().join(",");
    if (sig !== this.poolSig) {
      this.poolChart.replaceChildren();
      if (this.poolLine) { this.poolLine.el.remove(); this.poolLine = null; }
      this.poolLine = lineChart(series, { w: 1200, h: 160, xLabels: labels, interactive: true, yMin: 0, yMax: 100, yFmt: (v) => Math.round(v) + "%" });
      // focusable, so it must say what it is
      this.poolLine.el.setAttribute("aria-label", "peak KV slot fill per engine, " + rangeWord(range) + ". Focus shows a data point.");
      this.poolChart.append(this.poolLine.el);
      this.poolSig = sig;
    } else {
      this.poolLine.set(series);
    }
    this.poolMeta.textContent = "peak slot fill per engine, " + rangeWord(range) + stepNote(history.every_seconds);
    this.restamp(this.poolMeta);
  }
}
