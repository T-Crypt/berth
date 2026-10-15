// Context residency: the engine's own per-request token figures (cached,
// fresh prefill, generated) drawn against the model's window for the last 40
// requests - the same source the recent table reads - plus the engines' live
// KV sessions. One hand-rolled SVG surface, like the rest of the charts; no
// library.
import { el, fmtClock, fmtTok, fmtMS, na } from "./util.js";
import { armTapTip, armFocusTip, isPinned } from "./charts.js";

const SVGNS = "http://www.w3.org/2000/svg";
const W = 1200; // logical width; the surface scales to its container
const H = 120;
const PAD_L = 40; // y labels (tokens)
const PAD_R = 8;
const PAD_T = 10;
const PAD_B = 14;
const MAX_COLS = 40; // the column count the recent table feeds
const MAX_SESSIONS = 20; // the session list caps, with "…N more"

const CACHED = "#7ee0a3";
const FRESH = "#e0af68";
const GENERATED = "#7dcfff";

function S(tag, attrs) {
  const n = document.createElementNS(SVGNS, tag);
  if (attrs) for (const k in attrs) n.setAttribute(k, attrs[k]);
  return n;
}

export class ResidencyView {
  constructor(canvas, legend, sessions, meta) {
    this.canvas = canvas;
    this.legend = legend;
    this.sessions = sessions;
    this.meta = meta;
    this.colorOf = null; // engine -> tide color, assigned by the page
    this.lbrange = "24h"; // the leaderboard's picker: 24h = the live source
    this.status = null;
    this.cols = []; // the columns currently drawn; hover reads these
    this.svg = null;
    this.tip = null;
    this._bind();
  }

  _bind() {
    const wrap = this.canvas;
    this.tip = el("div", "res-tip");
    this.tip.hidden = true;
    wrap.append(this.tip);
    wrap.setAttribute("aria-label", "context residency: each request's tokens against its model's window");
    wrap.addEventListener("pointermove", (ev) => this._hover(ev));
    wrap.addEventListener("pointerleave", () => { if (!isPinned(this.tip)) this.tip.hidden = true; });
    armTapTip(wrap, this.tip, (ev) => this._hover(ev));
    armFocusTip(wrap, this.tip, (ev) => this._hover(ev), null);
  }

  _hover(ev) {
    if (!this.svg || this.cols.length === 0) return;
    const rect = this.svg.getBoundingClientRect();
    const x = (ev.clientX - rect.left) / rect.width * W;
    const plotW = W - PAD_L - PAD_R;
    const n = this.cols.length;
    if (x < PAD_L || x > W - PAD_R) { this.tip.hidden = true; return; }
    const i = Math.max(0, Math.min(n - 1, Math.floor((x - PAD_L) / (plotW / n))));
    const c = this.cols[i];
    this._showTip(ev, [
      ["model", (c.model || "?") + " " + (c.engine || "")],
      ["time", c.time],
      ["prompt", fmtTok(c.prompt) + " tokens"],
      ["cached", fmtTok(c.cached)],
      ["fresh", fmtTok(c.fresh)],
      ["generated", fmtTok(c.generated)],
      ["prefill", na(c.prefillMs, (v) => fmtMS(v))],
    ]);
  }

  _showTip(ev, rows) {
    const rect = this.canvas.getBoundingClientRect();
    this.tip.replaceChildren(...rows.map(([l, v]) => el("div", "res-tip-row", el("span", "l", l), el("span", "v", v))));
    this.tip.hidden = false;
    const x = ev.clientX - rect.left;
    const flip = x > rect.width * 0.6;
    this.tip.style.left = (flip ? x - this.tip.offsetWidth - 10 : x + 10) + "px";
    this.tip.style.top = Math.max(4, Math.min(ev.clientY - rect.top - 6, rect.height - this.tip.offsetHeight - 4)) + "px";
  }

  // The newest 40 requests the same source the recent table reads: the
  // node's live 24 h insights on the default range, the stored range's
  // insights once the picker leaves it.
  _source(status) {
    if (this.lbrange !== "24h") {
      const ins = this.storedInsights;
      return { reqs: (ins && Array.isArray(ins.recent) ? ins.recent : []), word: this.lbrange + ", stored" };
    }
    const ins = status && status.insights;
    return { reqs: ins && Array.isArray(ins.recent) ? ins.recent : [], word: "node 24h" };
  }

  // windowFor is engine\x00model -> window: the newest history sample with a
  // kv table (stored or live - the same field), falling back to what the
  // engines report resident now.
  _windows() {
    const w = new Map();
    const samples = this.history && Array.isArray(this.history.samples) ? this.history.samples : [];
    for (let i = samples.length - 1; i >= 0; i--) {
      const kv = samples[i].kv;
      if (!kv || Object.keys(kv).length === 0) continue;
      for (const engine in kv) {
        const models = kv[engine].models;
        if (!models) continue;
        for (const model in models) w.set(engine + "\x00" + model, models[model]);
      }
      break;
    }
    const st = this.status;
    if (st && Array.isArray(st.engines)) {
      for (const e of st.engines) {
        for (const m of e.models || []) {
          if (m.window > 0 && !w.has(e.name + "\x00" + m.name)) w.set(e.name + "\x00" + m.name, m.window);
        }
      }
    }
    return w;
  }

  // render runs on the status cadence: the sessions are always live, and on
  // the 24 h range the columns come from the node's own insights too.
  render(status) {
    this.status = status;
    this._paintSessions(status);
    if (this.lbrange !== "24h") return; // the stored view owns the columns
    const src = this._source(status);
    this._paintColumns(src.reqs, src.word);
  }

  // refresh runs when the history or the stored range's insights arrive: the
  // columns move to the stored source and the window map to the samples.
  refresh(history, insights) {
    this.history = history;
    this.storedInsights = insights;
    if (this.lbrange === "24h") return;
    this._paintColumns((insights && Array.isArray(insights.recent) ? insights.recent : []), this.lbrange + ", stored");
  }

  _paintColumns(reqs, word) {
    const wins = this._windows();    // newest first from the source; the panel is the last 40, oldest left
    const src = reqs.slice(0, MAX_COLS).reverse();
    this.cols = src.map((r) => {
      const prompt = Math.max(0, r.prompt_tokens || 0);
      const cached = Math.min(prompt, r.cache_tokens || 0);
      const fresh = Math.max(0, prompt - cached);
      const generated = Math.max(0, r.output_tokens || 0);
      const win = wins.get((r.engine || "") + "\x00" + (r.model || "")) || 0;
      const d = new Date(r.t);
      return {
        engine: r.engine || "", model: r.model || "?", t: r.t,
        time: Number.isNaN(d.getTime()) ? String(r.t || "") : fmtClock(d),
        prompt, cached, fresh, generated, window: win,
        prefillMs: r.prefill_ms,
      };
    });
    const haveWindow = this.cols.some((c) => c.window > 0);
    // the window line is the set's max window; with none known, the columns
    // scale to their own max total and the line hides itself
    let scale = this.cols.reduce((m, c) => Math.max(m, c.window), 0);
    if (scale === 0) scale = this.cols.reduce((m, c) => Math.max(m, c.cached + c.fresh + c.generated), 0);
    if (scale <= 0) scale = 1;
    const noWindows = !haveWindow;
    this._paintSurface(scale, noWindows);
    this._paintLegend(noWindows);
    this.meta.textContent = "last " + this.cols.length + " requests, " + word
      + "; sessions live now" + (noWindows ? " - no model window known, columns scaled to their own size" : "");
  }

  _paintSurface(scale, noWindows) {
    if (this.svg) this.svg.remove();
    if (this.cols.length === 0) {
      this.canvas.replaceChildren(this.tip);
      this.canvas.append(el("div", "empty-note", "no requests logged yet"));
      return;
    }
    const plotW = W - PAD_L - PAD_R;
    const plotH = H - PAD_T - PAD_B;
    const n = this.cols.length;
    const cw = plotW / n;
    const svg = S("svg", { class: "chart residency-svg", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none" });
    svg.setAttribute("aria-hidden", "true");
    // y ticks (tokens)
    for (const f of [0, 0.5, 1]) {
      const y = PAD_T + plotH - f * plotH;
      svg.append(S("line", { x1: PAD_L, x2: W - PAD_R, y1: y, y2: y, stroke: "#16202f", "stroke-width": "1", "vector-effect": "non-scaling-stroke" }));
      const t = S("text", { x: PAD_L - 4, y: y + 3, "text-anchor": "end", class: "tick" });
      t.textContent = f === 0 ? "0" : fmtTok(scale * f);
      svg.append(t);
    }
    const yOf = (tok) => PAD_T + plotH - (Math.min(tok, scale) / scale) * plotH;
    // the columns: stacked cached + fresh + generated
    this.cols.forEach((c, i) => {
      const x = PAD_L + i * cw;
      const w = Math.max(1, cw - 2);
      const bx = x + 1;
      const yTop = yOf(c.cached + c.fresh + c.generated);
      const yCached = yOf(c.cached);
      const yFresh = yOf(c.cached + c.fresh);
      if (c.cached > 0) svg.append(S("rect", { x: bx, y: yCached, width: w, height: Math.max(0, PAD_T + plotH - yCached), fill: CACHED }));
      if (c.fresh > 0) svg.append(S("rect", { x: bx, y: yFresh, width: w, height: Math.max(0, yCached - yFresh), fill: FRESH }));
      if (c.generated > 0) svg.append(S("rect", { x: bx, y: yTop, width: w, height: Math.max(0, yFresh - yTop), fill: GENERATED }));
    });
    if (!noWindows) {
      // the window line (dashed) and a faint one at 75 % of it
      const yWin = yOf(scale);
      svg.append(S("line", { x1: PAD_L, x2: W - PAD_R, y1: yWin, y2: yWin, stroke: "rgba(201,212,227,0.6)", "stroke-width": "1", "stroke-dasharray": "5 4", "vector-effect": "non-scaling-stroke" }));
      const y75 = yOf(scale * 0.75);
      svg.append(S("line", { x1: PAD_L, x2: W - PAD_R, y1: y75, y2: y75, stroke: "rgba(201,212,227,0.2)", "stroke-width": "1", "vector-effect": "non-scaling-stroke" }));
    }
    this.svg = svg;
    this.canvas.append(svg);
    // re-append the tip last so it stacks above the surface
    if (this.tip.parentNode !== this.canvas) this.canvas.append(this.tip);
  }

  _paintLegend(noWindows) {
    this.legend.replaceChildren();
    if (this.cols.length === 0) return; // both sources empty: the legend hides
    const mk = (color, label) => {
      const sw = el("span", "res-sw");
      sw.style.background = color;
      return el("span", "res-legend-item", sw, label);
    };
    this.legend.append(mk(CACHED, "cached"), mk(FRESH, "fresh prefill"), mk(GENERATED, "generated"));
    if (!noWindows) this.legend.append(el("span", "res-legend-item res-dim", "window (dashed)"));
  }

  // The live sessions are always live, whatever the picker says: one row per
  // slot the engines report, capped at 20 with the rest counted.
  _paintSessions(status) {
    this.sessions.replaceChildren();
    const rows = [];
    for (const eng of (status && Array.isArray(status.engines)) ? status.engines : []) {
      for (const s of Array.isArray(eng.slots) ? eng.slots : []) {
        rows.push({ eng: eng.name || "?", slot: s });
      }
    }
    if (rows.length === 0) {
      this.sessions.append(el("div", "empty-note", "no active sessions"));
      return;
    }
    const shown = rows.slice(0, MAX_SESSIONS);
    for (const { eng, slot } of shown) {
      const frac = slot.window > 0 ? Math.max(0, Math.min(1, slot.used / slot.window)) : 0;
      const color = this.colorOf ? this.colorOf(eng) : "#7dcfff";
      const fill = el("i", "res-fill");
      fill.style.width = (frac * 100).toFixed(1) + "%";
      fill.style.background = color;
      const bar = el("div", "res-meter", fill, el("i", "res-tick t75"), el("i", "res-tick t90"));
      const row = el("div", "res-row");
      const dot = el("span", "res-dot");
      dot.style.background = color;
      const tag = slot.model ? " " + slot.model : "";
      row.append(
        dot,
        el("span", "res-id", (slot.id || "?") + tag),
        bar,
        el("span", "res-used", fmtTok(slot.used) + " / " + (slot.window > 0 ? fmtTok(slot.window) : "n/a"))
      );
      this.sessions.append(row);
    }
    const more = rows.length - shown.length;
    if (more > 0) this.sessions.append(el("div", "res-more", "…" + more + " more"));
  }
}
