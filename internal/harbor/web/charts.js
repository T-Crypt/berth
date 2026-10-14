// SVG chart builders. Every shape is made with document.createElementNS; there
// is no canvas and no library. Charts use a fixed viewBox plus
// preserveAspectRatio so they scale to their container, and set() redraws only
// when the data signature changes. Strokes use vector-effect non-scaling-stroke
// so a stretched sparkline keeps a 1px line.

// The tide and the model timeline share one x geometry: the same viewBox
// (extended left by LABEL_GUTTER for row labels) and the same left/right
// plot pads, so both maps are the identity over the container width and a
// bar under a tide band sits exactly on it.
export const TL_W = 1400;
export const TL_PAD_L = 46; // plot left, viewBox units (the tide's y axis)
export const TL_PAD_R = 18;
export const LABEL_GUTTER = 140; // the label column left of the plot

const SVGNS = "http://www.w3.org/2000/svg";
function S(tag, attrs) {
  const n = document.createElementNS(SVGNS, tag);
  if (attrs) for (const k in attrs) n.setAttribute(k, attrs[k]);
  return n;
}
function fmtTick(v) {
  if (v === 0) return "0";
  if (v >= 1000) return (v / 1000).toFixed(v >= 10000 ? 0 : 1) + "k";
  return (v % 1 === 0 ? String(v) : v.toFixed(1));
}
// A few grid steps that read cleanly on a dark background.
function ticks(min, max, count) {
  if (!Number.isFinite(min) || !Number.isFinite(max) || max <= min) {
    const m = Math.max(max || 0, min || 0, 1);
    return [0, m / 2, m];
  }
  const span = max - min;
  const raw = span / count;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / mag;
  const step = (norm >= 5 ? 5 : norm >= 2 ? 2 : 1) * mag;
  const out = [];
  for (let v = Math.ceil(min / step) * step; v <= max + 1e-9; v += step) out.push(v);
  if (out[0] > min + step / 2) out.unshift(min);
  return out;
}
// One shared pointer-tooltip, absolutely positioned inside a chart wrapper.
function makeTip() {
  const t = document.createElement("div");
  t.className = "chart-tip";
  t.hidden = true;
  return t;
}
function tipRow(label, value, color) {
  const r = document.createElement("div");
  r.className = "chart-tip-row";
  const sw = document.createElement("span");
  sw.className = "chart-tip-sw";
  if (color) sw.style.background = color;
  const l = document.createElement("span");
  l.className = "chart-tip-l";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = "chart-tip-v";
  v.textContent = value;
  r.append(sw, l, v);
  return r;
}
// tipBox(rows...) -> one block of stacked rows. Rows must not nest: a row is a
// 3-column grid, so a row appended into another row lands in its cells.
function tipBox(...rows) {
  const b = document.createElement("div");
  b.append(...rows);
  return b;
}
function showTip(tip, wrap, ev, html) {
  const rect = wrap.getBoundingClientRect();
  tip.replaceChildren(html);
  tip.hidden = false;
  const x = ev.clientX - rect.left;
  const flip = x > rect.width * 0.6;
  tip.style.left = (flip ? x - tip.offsetWidth - 10 : x + 10) + "px";
  const y = Math.max(6, Math.min(ev.clientY - rect.top - 8, rect.height - tip.offsetHeight - 6));
  tip.style.top = y + "px";
}
function hideTip(tip) {
  tip.hidden = true;
}
let pinnedTip = null;
export function isPinned(tip) {
  return pinnedTip !== null && pinnedTip.tip === tip;
}
export function pinTip(tip, wrap) {
  if (pinnedTip && pinnedTip.tip !== tip) pinnedTip.tip.hidden = true;
  pinnedTip = { tip, wrap };
}
export function unpinTip(tip) {
  if (pinnedTip && (!tip || pinnedTip.tip === tip)) pinnedTip = null;
}
let tapAwayArmed = false;
export function armTapTip(wrap, tip, show) {
  wrap.addEventListener("click", (ev) => {
    if (show) show(ev);
    if (tip.hidden) unpinTip(tip);
    else pinTip(tip, wrap);
  });
  if (!tapAwayArmed) {
    tapAwayArmed = true;
    document.addEventListener("click", (ev) => {
      if (pinnedTip && !pinnedTip.wrap.contains(ev.target)) {
        pinnedTip.tip.hidden = true;
        pinnedTip = null;
      }
    });
  }
}

// ---------------- sparkline ----------------
// sparkline(series, opts) -> { el, set(series) }
// Fills its box (preserveAspectRatio none). series is numbers, oldest first.
export function sparkline(series, opts = {}) {
  const w = opts.w || 100;
  const h = opts.h || 28;
  const o = { color: "#7dcfff", area: true, min: 0, max: null, pad: 2, ...opts };
  const el = S("svg", { class: "chart chart-spark", viewBox: `0 0 ${w} ${h}`, preserveAspectRatio: "none" });
  el.setAttribute("aria-hidden", "true");
  const area = S("path", { fill: o.color, opacity: "0.12", stroke: "none" });
  const line = S("path", { fill: "none", stroke: o.color, "stroke-width": "1.4", "vector-effect": "non-scaling-stroke" });
  el.append(area, line);
  const ctl = {
    el,
    _sig: null,
    set(data) {
      if (!Array.isArray(data) || data.length < 2) { line.setAttribute("d", ""); area.setAttribute("d", ""); return; }
      const n = data.length;
      const sig = n + ":" + (data[0] || 0).toFixed(1) + ":" + (data[n - 1] || 0).toFixed(1) + ":" + (data[n >> 1] || 0).toFixed(1);
      if (sig === ctl._sig) return;
      ctl._sig = sig;
      let lo = o.min;
      let hi = o.max == null ? Math.max(...data, o.min) : o.max;
      if (hi <= lo) hi = lo + 1;
      const step = (w - o.pad * 2) / (n - 1);
      const y = (v) => h - o.pad - ((v - lo) / (hi - lo)) * (h - o.pad * 2);
      let d = "";
      for (let i = 0; i < n; i++) {
        const x = o.pad + i * step;
        d += (i === 0 ? "M" : "L") + x.toFixed(2) + " " + y(data[i]).toFixed(2);
      }
      line.setAttribute("d", d);
      if (o.area) area.setAttribute("d", d + "L" + (o.pad + (n - 1) * step).toFixed(2) + " " + (h - o.pad) + "L" + o.pad + " " + (h - o.pad) + "Z");
      else area.setAttribute("d", "");
    },
  };
  ctl.set(series);
  return ctl;
}

// ---------------- lineChart ----------------
// lineChart(series[], opts) -> { el, set(series2) }
// series: [{ name, color, data, unit? }] sharing one x (length n); a
// series' own unit wins over opts.unit in the tooltip, for mixed charts.
// opts: w, h, unit, xLabels (n strings, optional), interactive (crosshair
// + tooltip), yFmt (y tick label formatter, default fmtTick).
export function lineChart(series, opts = {}) {
  const w = opts.w || 320;
  const h = opts.h || 120;
  const o = { unit: "", xLabels: null, yMin: 0, yMax: null, yTicks: 3, area: false, yFmt: null, ...opts };
  const padL = 34, padR = 8, padT = 8, padB = o.xLabels ? 18 : 6;
  const plotW = w - padL - padR;
  const plotH = h - padT - padB;
  const wrap = document.createElement("div");
  wrap.className = "chart-wrap";
  const el = S("svg", { class: "chart chart-line", viewBox: `0 0 ${w} ${h}`, preserveAspectRatio: "none" });
  el.setAttribute("aria-hidden", "true");
  const gGrid = S("g", { class: "axis-grid" });
  const gLines = S("g", { class: "lines" });
  const gLabel = S("g", { class: "axis-label" });
  const gX = S("g", { class: "axis-label" });
  el.append(gGrid, gLines, gLabel, gX);
  let cross = null;
  let tip = null;
  if (o.interactive) {
    cross = S("line", { class: "crosshair", y1: padT, y2: padT + plotH, stroke: "rgba(201,212,227,0.4)", "stroke-width": "1", "vector-effect": "non-scaling-stroke" });
    cross.setAttribute("visibility", "hidden");
    el.append(cross);
    tip = makeTip();
    wrap.append(el, tip);
  } else {
    wrap.append(el);
  }
  const xScale = (i, n) => padL + (n <= 1 ? 0 : (i / (n - 1)) * plotW);
  const yScale = (v, lo, hi) => padT + plotH - ((v - lo) / (hi - lo)) * plotH;

  function paint(data) {
    let n = 0;
    for (const s of data) n = Math.max(n, (s.data || []).length);
    if (n < 2) { clear(); return; }
    let lo = o.yMin;
    let hi = o.yMax == null ? 0 : o.yMax;
    for (const s of data) for (const v of s.data || []) { if (hi !== null && v > hi) hi = v; if (v < lo) lo = v; }
    if (o.yMax == null) { for (const s of data) for (const v of s.data || []) if (v > hi) hi = v; if (hi <= lo) hi = lo + 1; }
    const y = (v) => yScale(v, lo, hi);
    // grid + y labels
    gGrid.replaceChildren();
    gLabel.replaceChildren();
    for (const tv of ticks(lo, hi, o.yTicks)) {
      const gy = y(tv);
      gGrid.append(S("line", { x1: padL, x2: padL + plotW, y1: gy, y2: gy, stroke: "#1d2939", "stroke-width": "1", "vector-effect": "non-scaling-stroke" }));
      const t = S("text", { x: padL - 5, y: gy + 3, "text-anchor": "end", class: "tick" });
      t.textContent = o.yFmt ? o.yFmt(tv) : fmtTick(tv);
      gLabel.append(t);
    }
    gLines.replaceChildren();
    for (const s of data) {
      const pts = s.data || [];
      let d = "";
      for (let i = 0; i < n; i++) {
        const v = pts[i] != null ? pts[i] : lo;
        d += (i === 0 ? "M" : "L") + xScale(i, n).toFixed(1) + " " + y(v).toFixed(1);
      }
      gLines.append(S("path", { d, fill: "none", stroke: s.color, "stroke-width": s.width || "1.5", "vector-effect": "non-scaling-stroke", opacity: s.area ? "1" : "0.95" }));
      if (s.area) {
        const a = S("path", { d: d + "L" + xScale(n - 1, n).toFixed(1) + " " + (padT + plotH) + "L" + xScale(0, n).toFixed(1) + " " + (padT + plotH) + "Z", fill: s.color, opacity: "0.1" });
        gLines.append(a);
      }
    }
    gX.replaceChildren();
    if (o.xLabels && o.xLabels.length === n) {
      const every = Math.max(1, Math.ceil(n / 6));
      for (let i = 0; i < n; i += every) {
        const t = S("text", { x: xScale(i, n), y: h - 4, "text-anchor": "middle", class: "tick" });
        t.textContent = o.xLabels[i] || "";
        gX.append(t);
      }
    }
    _range = { lo, hi, n };
  }
  function clear() { gGrid.replaceChildren(); gLines.replaceChildren(); gLabel.replaceChildren(); gX.replaceChildren(); }
  let _range = null;
  const ctl = {
    el: wrap,
    set(data) { paint(data); },
  };
  if (o.interactive) {
    const inspect = (ev) => {
      const rect = el.getBoundingClientRect();
      const vx = (ev.clientX - rect.left) / rect.width * w;
      if (vx < padL || vx > padL + plotW) return hideTip(tip);
      const frac = (vx - padL) / plotW;
      const idx = Math.max(0, Math.min(_range ? _range.n - 1 : 1, Math.round(frac * (_range ? _range.n - 1 : 1))));
      const px = xScale(idx, _range.n);
      cross.setAttribute("x1", px);
      cross.setAttribute("x2", px);
      cross.setAttribute("visibility", "visible");
      const html = tipBox(tipRow("", xLabelAt(idx), null));
      for (const s of (ctl._series || [])) {
        const v = (s.data || [])[idx];
        html.append(tipRow(s.name, v == null ? "n/a" : (Math.round(v * 10) / 10) + (s.unit != null ? s.unit : o.unit), s.color));
      }
      showTip(tip, wrap, ev, html);
    };
    el.addEventListener("pointermove", inspect);
    el.addEventListener("pointerleave", () => { if (!isPinned(tip)) hideTip(tip); if (cross) cross.setAttribute("visibility", "hidden"); });
    armTapTip(wrap, tip, inspect);
  }
  ctl.set = (data) => { ctl._series = data; paint(data); };
  ctl.set(series);
  function xLabelAt(i) { return o.xLabels && o.xLabels[i] ? o.xLabels[i] : ""; }
  return ctl;
}

// ---------------- stackedArea ----------------
// The tide chart. data = { t: [ms], total: GiB, series: [{label, color, kind,
// data: [GiB per sample]}] (series[0] is the dim baseline), events: [{t, kind,
// engine, detail}] }. Returns { el, set(data) }.
export function stackedArea(data, opts = {}) {
  const w = opts.w || 1400;
  const h = opts.h || 340;
  const p2 = (x) => String(x).padStart(2, "0");
  const o = {
    xLabels: 5,
    // axis and tooltip time labels; callers pass date-aware ones for day
    // ranges, otherwise the plain clock readings
    fmtX: (ts) => { const d = new Date(ts); return p2(d.getHours()) + ":" + p2(d.getMinutes()); },
    fmtTip: (ts) => { const d = new Date(ts); return p2(d.getHours()) + ":" + p2(d.getMinutes()) + ":" + p2(d.getSeconds()); },
    ...opts,
  };
  const padL = 46, padR = 18, padT = 12, padB = 22;
  const plotW = w - padL - padR;
  const plotH = h - padT - padB;
  const wrap = document.createElement("div");
  wrap.className = "chart-wrap chart-tide";
  const defs = S("defs");
  const pid = "hatch-" + Math.random().toString(36).slice(2, 8);
  const pat = S("pattern", { id: pid, width: "7", height: "7", patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
  pat.append(S("line", { x1: "0", y1: "0", x2: "0", y2: "7", stroke: "#f7768e", "stroke-width": "3.5" }));
  defs.append(pat);
  const el = S("svg", { class: "chart chart-area", viewBox: `-${LABEL_GUTTER} 0 ${w + LABEL_GUTTER} ${h}`, preserveAspectRatio: "none" });
  el.setAttribute("role", "img");
  el.setAttribute("aria-label", "scheduled card memory over the last hour");
  const gGrid = S("g", {});
  const gBands = S("g", {});
  const gEvents = S("g", {});
  const gAxis = S("g", { class: "axis-label" });
  const gX = S("g", { class: "axis-label" });
  const totalLine = S("line", { class: "total-line", stroke: "rgba(201,212,227,0.5)", "stroke-width": "1", "stroke-dasharray": "5 4", "vector-effect": "non-scaling-stroke" });
  const totalLabel = S("text", { class: "total-label" });
  const cross = S("line", { class: "crosshair", y1: padT, y2: padT + plotH, stroke: "rgba(201,212,227,0.5)", "stroke-width": "1", "vector-effect": "non-scaling-stroke" });
  cross.setAttribute("visibility", "hidden");
  el.append(defs, gGrid, gBands, totalLine, totalLabel, gEvents, gAxis, gX, cross);
  const hit = S("rect", { x: padL, y: padT, width: plotW, height: plotH, fill: "transparent" });
  el.append(hit);
  const tip = makeTip();
  wrap.append(el, tip);

  let state = null; // { t, total, series, events, x, y, n }
  const xOf = (t) => padL + ((t - state.t0) / (state.t1 - state.t0)) * plotW;

  function paint(data) {
    const t = data.t || [];
    const n = t.length;
    if (n < 2) { gBands.replaceChildren(); gGrid.replaceChildren(); gAxis.replaceChildren(); gX.replaceChildren(); gEvents.replaceChildren(); state = null; return; }
    const total = data.total || 0;
    state = { t, t0: t[0], t1: t[n - 1], total, series: data.series || [], events: data.events || [] };
    // y grid + labels
    gGrid.replaceChildren();
    gAxis.replaceChildren();
    const yTicks = ticks(0, total || 1, 4);
    for (const tv of yTicks) {
      const gy = padT + plotH - (tv / (total || 1)) * plotH;
      gGrid.append(S("line", { x1: padL, x2: padL + plotW, y1: gy, y2: gy, stroke: "#16202f", "stroke-width": "1", "vector-effect": "non-scaling-stroke" }));
      const tx = S("text", { x: padL - 6, y: gy + 3, "text-anchor": "end", class: "tick" });
      tx.textContent = (Math.round(tv * 10) / 10) + "G";
      gAxis.append(tx);
    }
    // x labels
    gX.replaceChildren();
    const every = Math.max(1, Math.ceil(n / o.xLabels));
    for (let i = 0; i < n; i += every) {
      const tx = S("text", { x: xOf(t[i]), y: h - 5, "text-anchor": "middle", class: "tick" });
      tx.textContent = o.fmtX(t[i]);
      gX.append(tx);
    }
    // total line
    const gyTotal = padT + plotH - (total / (total || 1)) * plotH;
    totalLine.setAttribute("x1", padL);
    totalLine.setAttribute("x2", padL + plotW);
    totalLine.setAttribute("y1", gyTotal);
    totalLine.setAttribute("y2", gyTotal);
    totalLabel.setAttribute("x", padL + 4);
    totalLabel.setAttribute("y", gyTotal - 4);
    totalLabel.textContent = "TOTAL " + (Math.round(total * 10) / 10) + "G";
    // stacked bands, bottom to top
    gBands.replaceChildren();
    const series = state.series;
    const cum = new Array(n).fill(0);
    const lower = new Array(n).fill(0);
    for (const band of series) {
      const dataArr = band.data || [];
      const lo = new Array(n).fill(0);
      for (let i = 0; i < n; i++) {
        lo[i] = cum[i];
        cum[i] += dataArr[i] || 0;
      }
      let d = "";
      for (let i = 0; i < n; i++) {
        const yTop = padT + plotH - (cum[i] / (total || 1)) * plotH;
        d += (i === 0 ? "M" : "L") + xOf(t[i]).toFixed(1) + " " + yTop.toFixed(1);
      }
      for (let i = n - 1; i >= 0; i--) {
        const yBot = padT + plotH - (lo[i] / (total || 1)) * plotH;
        d += "L" + xOf(t[i]).toFixed(1) + " " + yBot.toFixed(1);
      }
      d += "Z";
      const isSquat = band.kind === "squat";
      const isBase = band.kind === "baseline";
      const path = S("path", { d, "vector-effect": "non-scaling-stroke" });
      path.style.fill = isSquat ? "url(#" + pid + ")" : band.color || "#2b394d";
      // translucent bands with a crisp edge read as layers, not slabs of paint
      if (!isSquat) {
        path.style.fillOpacity = isBase ? "0.5" : "0.28";
        path.style.stroke = band.color || "#2b394d";
        path.style.strokeWidth = "1.25";
        path.style.strokeOpacity = "0.9";
      }
      if (isSquat) path.style.stroke = "#f7768e";
      path.style.fillRule = "evenodd";
      gBands.append(path);
    }
    // event ticks
    gEvents.replaceChildren();
    for (const ev of state.events) {
      if (ev.t < state.t0 || ev.t > state.t1) continue;
      const ex = xOf(ev.t);
      const color = { granted: "#7dcfff", draining: "#e0af68", freed: "#42566e", stuck: "#f7768e" }[ev.tone] || "#42566e";
      gEvents.append(S("line", { x1: ex, x2: ex, y1: padT, y2: padT + plotH, stroke: color, "stroke-width": "1", "vector-effect": "non-scaling-stroke", opacity: "0.8" }));
      const dot = S("circle", { cx: ex, cy: padT + 4, r: "2.5", fill: color });
      gEvents.append(dot);
      const hitline = S("line", { x1: ex - 4, x2: ex + 4, y1: padT, y2: padT + plotH, stroke: "transparent", "stroke-width": "8" });
      hitline.style.cursor = "pointer";
      const showEv = (e) => {
        const html = tipBox(tipRow(new Date(ev.t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }), ev.kind + (ev.engine ? " " + ev.engine : ""), color));
        if (ev.detail) html.append(tipRow("", ev.detail, null));
        showTip(tip, wrap, e, html);
      };
      hitline.addEventListener("pointerenter", showEv);
      hitline.addEventListener("pointerleave", () => { if (!isPinned(tip)) hideTip(tip); });
      hitline.addEventListener("click", (e) => { e.stopPropagation(); showEv(e); pinTip(tip, wrap); });
      gEvents.append(hitline);
    }
    // crosshair over the whole plot
    const inspect = (ev) => {
      if (!state) return;
      const rect = el.getBoundingClientRect();
      const vx = (ev.clientX - rect.left) / rect.width * w;
      if (vx < padL || vx > padL + plotW) return hideTip(tip);
      const frac = (vx - padL) / plotW;
      const idx = Math.max(0, Math.min(n - 1, Math.round(frac * (n - 1))));
      const px = xOf(state.t[idx]);
      cross.setAttribute("x1", px);
      cross.setAttribute("x2", px);
      cross.setAttribute("visibility", "visible");
      let used = 0;
      const html = tipBox(tipRow(o.fmtTip(state.t[idx]), "", null));
      for (const band of state.series) {
        const v = (band.data || [])[idx] || 0;
        used += v;
        if (v < 0.05) continue; // a holder with nothing on the card at this moment
        html.append(tipRow(band.label, v.toFixed(1) + "G", band.kind === "squat" ? "#f7768e" : band.color));
      }
      html.append(tipRow("used", used.toFixed(1) + "G", "#c9d4e3"));
      html.append(tipRow("free", Math.max(0, total - used).toFixed(1) + "G", "#42566e"));
      showTip(tip, wrap, ev, html);
    };
    hit.onpointermove = inspect;
    hit.onpointerleave = () => { if (!isPinned(tip)) hideTip(tip); cross.setAttribute("visibility", "hidden"); };
    wrap._inspect = inspect;
    if (!wrap._tapArmed) {
      wrap._tapArmed = true;
      armTapTip(wrap, tip, (ev) => { if (wrap._inspect) wrap._inspect(ev); });
    }
  }
  const ctl = { el: wrap, set: paint };
  ctl.set(data);
  return ctl;
}

// ---------------- timeline ----------------
// Gantt bars for the model timeline, drawn with the tide's own x geometry
// (same viewBox width 1400 and left/right pads 46/18, full container width)
// so the bars line up under the tide's x axis. data = { t0, t1 (ms), t (the
// tide's sample times, for the shared grid lines), rows: [{ label, color,
// spans: [{ start, end (null = open) }] }] }. Returns { el, set(data) }.
export function timelineChart(data, opts = {}) {
  const p2 = (x) => String(x).padStart(2, "0");
  const o = {
    rowH: 28, padT: 8, padB: 6, gridLines: 5,
    fmtX: (ts) => { const d = new Date(ts); return p2(d.getHours()) + ":" + p2(d.getMinutes()); },
    fmtTip: null, // defaults to fmtX; the page passes the tide's richer one
    ...opts,
  };
  if (!o.fmtTip) o.fmtTip = o.fmtX;
  const w = TL_W; // the tide's viewBox width; fixed, do not override
  const padL = TL_PAD_L, padR = TL_PAD_R; // the tide's pads; fixed, do not override
  const wrap = document.createElement("div");
  wrap.className = "chart-wrap timeline-wrap";
  const el = S("svg", { class: "chart timeline-svg", viewBox: `-${LABEL_GUTTER} 0 ${w + LABEL_GUTTER} 80`, preserveAspectRatio: "none" });
  el.setAttribute("aria-hidden", "true");
  const gGrid = S("g", { class: "axis-grid" });
  const gLabels = S("g", { class: "axis-label" });
  const gRows = S("g", {});
  el.append(gGrid, gLabels, gRows);
  const tip = makeTip();
  wrap.append(el, tip);
  armTapTip(wrap, tip, null);
  const xOf = (t, t0, t1) => padL + ((t1 <= t0 ? 0 : (t - t0) / (t1 - t0)) * (w - padL - padR));
  const trunc = (s, n) => (s.length > n ? s.slice(0, n - 1) + "…" : s);

  function paint(data) {
    const rows = data.rows || [];
    const t0 = data.t0, t1 = data.t1;
    const h = o.padT + Math.max(rows.length, 1) * o.rowH + o.padB;
    el.setAttribute("viewBox", `-${LABEL_GUTTER} 0 ${w + LABEL_GUTTER} ${h}`);
    gGrid.replaceChildren();
    gLabels.replaceChildren();
    gRows.replaceChildren();
    // the tide's x labels sit at every ceil(n/5) sample; the grid lines
    // reuse those positions so the two charts read as one axis
    const tt = data.t || [];
    if (tt.length >= 2) {
      const every = Math.max(1, Math.ceil(tt.length / o.gridLines));
      for (let i = 0; i < tt.length; i += every) {
        const gx = xOf(tt[i], t0, t1);
        gGrid.append(S("line", { x1: gx, x2: gx, y1: o.padT, y2: h - o.padB, stroke: "#16202f", "stroke-width": "1", "vector-effect": "non-scaling-stroke" }));
      }
    }
    rows.forEach((row, i) => {
      const top = o.padT + i * o.rowH;
      const y = top + (o.rowH - 12) / 2;
      // row label in the shared gutter: engine dim above, model bright below;
      // the hover tooltip carries the untruncated names
      const eng = S("text", { x: padL - 8, y: top + 12, "text-anchor": "end", class: "tl-eng" });
      eng.textContent = trunc(row.engine, 12);
      const mod = S("text", { x: padL - 8, y: top + 25, "text-anchor": "end", class: "tl-model" });
      mod.textContent = trunc(row.model, 20);
      gLabels.append(eng, mod);
      for (const sp of row.spans || []) {
        const s = Math.max(sp.start, t0);
        const eRaw = sp.end == null ? t1 : sp.end;
        const e = Math.min(eRaw, t1);
        if (e < t0 || s > t1) continue; // outside this range
        const x = xOf(s, t0, t1);
        const bw = Math.max(xOf(e, t0, t1) - x, 1.5);
        const open = sp.end == null;
        const bar = S("rect", { x, y, width: bw, height: 12, rx: "2", fill: row.color, "fill-opacity": open ? "0.95" : "0.72" });
        gRows.append(bar);
        if (open) {
          // a brighter cap where the span is still running
          gRows.append(S("rect", { x: x + bw - 3.5, y, width: 3.5, height: 12, fill: row.color, class: "tl-live" }));
        }
        // a wider invisible run makes short spans hoverable
        const hit = S("rect", { x: x - 4, y: o.padT + i * o.rowH, width: Math.max(bw + 8, 12), height: o.rowH, fill: "transparent" });
        hit.style.cursor = "pointer";
        const show = (ev) => {
          const dur = Math.round((e - s) / 1000);
          const html = tipBox(
            tipRow(row.model, row.engine, row.color),
            tipRow("loaded at", o.fmtTip(s), null),
            tipRow("unloaded at", open ? "still loaded" : o.fmtTip(e), null),
            tipRow("duration", fmtAgeW(dur), null)
          );
          showTip(tip, wrap, ev, html);
        };
        hit.addEventListener("pointerenter", show);
        hit.addEventListener("pointermove", show);
        hit.addEventListener("pointerleave", () => { if (!isPinned(tip)) hideTip(tip); });
        hit.addEventListener("click", (ev) => { ev.stopPropagation(); show(ev); pinTip(tip, wrap); });
        gRows.append(hit);
      }
    });
  }
  const ctl = { el: wrap, set: paint };
  ctl.set(data);
  return ctl;
}
// fmtAgeW is a tiny local age formatter: whole units, "2h 13m" style.
function fmtAgeW(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + "s";
  const m = Math.floor(sec / 60);
  if (m < 60) return m + "m";
  const hh = Math.floor(m / 60);
  if (hh < 48) return hh + "h" + (m % 60 ? " " + (m % 60) + "m" : "");
  return Math.floor(hh / 24) + "d" + (hh % 24 ? " " + (hh % 24) + "h" : "");
}
