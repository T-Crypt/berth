// The SCHEDULE tab: the card's scheduler. NOW is what holds each card, the
// waiters in grant order and the booking in force; 7 DAYS draws every
// booking, each engine's window and the live holder on a 24 h track per day,
// to the minute; BOOKINGS lists them. A paired device books, moves and
// cancels through POST /v1/board/schedule, straight to berthd. API strings
// only ever reach textContent.
import { $, el } from "./util.js";
import { showDialog, toast } from "./ui.js";

const POLL_MS = 5000;
const DAYS = 7;
const HOUR = 3600e3;
const SNAP_MIN = 15;
const PICK_KEY = "berth.schedule.card";
const KIND_WORD = { run: "RUN", priority: "PRIORITY", down: "DOWN" };

const pad2 = (n) => String(n).padStart(2, "0");
const hm = (d) => pad2(d.getHours()) + ":" + pad2(d.getMinutes());
const dayWord = (d) => d.toLocaleDateString(undefined, { weekday: "short" }).toUpperCase() + " " + d.getDate();
const when = (b) => {
  const s = new Date(b.start), e = new Date(b.end);
  const same = s.toDateString() === e.toDateString();
  return dayWord(s) + " " + hm(s) + "-" + (same ? "" : dayWord(e) + " ") + hm(e);
};
function span(ms) {
  const m = Math.max(0, Math.round(ms / 60e3));
  const h = Math.floor(m / 60), r = m % 60;
  if (h >= 48) return Math.round(h / 24) + "d";
  return h > 0 ? h + "h" + (r ? pad2(r) + "m" : "") : r + "m";
}
// nowStep is now, floored to the start field's 5-minute step, so a booking
// made "now" starts now and the field accepts it.
const nowStep = () => Math.floor(Date.now() / 300e3) * 300e3;
const localInput = (d) => d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) + "T" + hm(d);
const label = (b) => b.who + (b.engine && b.engine !== b.who ? " · " + b.engine : "") + (b.priority ? " · " + b.priority : "");

// What a booking will do, in the words the operator reads before saving.
function explain(b) {
  const from = hm(new Date(b.start)), to = hm(new Date(b.end));
  const eng = b.engine || "the engine";
  switch (b.kind) {
    case "run":
      return eng + " goes first in line from " + from + " and runs at " + (b.priority || "foreground") +
        "; until " + to + " nothing but a hold drains it." +
        (b.claim ? " When it asks, any holder but a hold drains for it, whatever its class." : "") +
        " If it does not ask for the card, the card serves everyone else.";
    case "priority":
      return eng + " runs at " + (b.priority || "?") + " from " + from + " to " + to + ", then back to its own class. It claims no time on the card.";
    case "down":
      return "From " + from + " no new work starts and holders drain at their next boundary; a hold and a training stay. Work queued resumes at " + to + ".";
  }
  return "";
}

// Demo bookings, relative to now, so the board has something to show.
function demoBookings() {
  const at = (day, h, m = 0) => { const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + day); d.setHours(h, m); return d; };
  const out = [];
  let n = 1;
  const add = (kind, who, engine, priority, s, hours, note) =>
    out.push({ id: "b" + n++, card: "gpu0", kind, who, engine, priority, start: s.toISOString(), end: new Date(s.getTime() + hours * HOUR).toISOString(), note });
  for (let d = 0; d < DAYS; d++) {
    add("run", "pi-pocket", "pocket-train", "batch", at(d, 23), 5, "omarchy nightly");
    if (d % 7 !== 6) add("run", "operator", "comfyui", "", at(d, 9, 30), 8, "day work");
  }
  add("run", "training", "sft-4b", "foreground", at(1, 18), 3, "sft run 4");
  add("priority", "operator", "llama-swap", "batch", at(1, 18), 3, "agents wait during training");
  add("down", "operator", "", "", at(3, 4), 1, "driver update");
  return out.sort((a, b) => a.start.localeCompare(b.start));
}

export function initSchedule({ demo }) {
  const nowRoot = $("sb-now");
  const nowMeta = $("sb-now-meta");
  const boardRoot = $("sb-board");
  const listRoot = $("sb-list");
  const pickRoot = $("sb-card-pick");
  const hint = $("sb-hint");
  const dlg = $("sched-dlg");
  const form = $("sched-form");
  const explainEl = $("sched-explain");
  const errEl = $("sched-error");
  let st = null;
  let paired = demo;
  let timer = 0;
  let pick = localStorage.getItem(PICK_KEY) || "";
  let editing = null; // the booking the dialog moves, or null for a new one

  async function read() {
    try {
      const res = await fetch(demo ? "demo.json" : "/v1/status", { headers: { accept: "application/json" } });
      const j = await res.json();
      if (!res.ok) throw new Error(j.message || res.statusText);
      if (demo && !j.bookings) j.bookings = demoBookings();
      st = j;
      paint();
    } catch (e) {
      st = null;
      nowRoot.replaceChildren(el("div", "empty-note error-note", "no status: " + e.message));
      nowMeta.textContent = "not read";
    }
  }

  function cards() {
    const names = (st.gpus || []).filter((g) => g.scheduled).map((g) => g.card || "gpu0");
    return names.length ? names : ["gpu0"];
  }

  function paint() {
    const cs = cards();
    if (!cs.includes(pick)) pick = cs[0];
    pickRoot.replaceChildren();
    if (cs.length > 1) {
      for (const c of cs) {
        const b = el("button", "btn sw-btn", c);
        b.type = "button";
        b.setAttribute("aria-pressed", String(c === pick));
        b.addEventListener("click", () => { pick = c; localStorage.setItem(PICK_KEY, c); paint(); });
        pickRoot.append(b);
      }
    }
    paintNow(cs.length > 1);
    paintBoard();
    paintList(cs.length > 1);
    $("sb-book").disabled = false;
    hint.textContent = paired ? "click an empty hour to book it, or a booking to move or cancel it" : "read only: pair this device (SWITCH tab) to book";
  }

  function onCard(card) { return (card || "gpu0") === pick; }

  function paintNow(many) {
    const now = Date.now();
    const rows = [];
    const bs = (st.bookings || []);
    for (const l of st.leases || []) {
      if (!onCard(l.card)) continue;
      const row = el("div", "sq sw-engine sw-up");
      const right = el("span", "sw-state", el("span", "sw-chip", (l.priority || "").toUpperCase()), el("span", "sw-chip", (l.state || "").toUpperCase()));
      if (l.booking) right.append(el("span", "sw-chip sb-chip-run", "BOOKED " + l.booking));
      row.append(el("div", "top", el("span", "name", "HOLDS " + (l.group && l.group !== l.engine ? l.group + " (" + l.engine + ")" : l.engine)), right));
      row.append(el("div", "meta", l.id + ", for " + span((l.age_seconds || 0) * 1000)));
      rows.push(row);
    }
    if (!rows.length) rows.push(el("div", "empty-note", "nothing holds " + pick));
    const waiting = (st.waiting || []).filter((l) => onCard(l.card));
    waiting.forEach((l, i) => {
      const row = el("div", "sq sw-engine sw-waiting");
      const right = el("span", "sw-state", el("span", "sw-chip", (l.priority || "").toUpperCase()));
      if (l.booking) right.append(el("span", "sw-chip sb-chip-run", "BOOKED " + l.booking));
      row.append(el("div", "top", el("span", "name", (i + 1) + ". " + l.engine), right));
      row.append(el("div", "meta", l.id + ", waiting " + span((l.age_seconds || 0) * 1000)));
      rows.push(row);
    });
    const mine = bs.filter((b) => onCard(b.card));
    const inForce = mine.filter((b) => new Date(b.start) <= now && now < new Date(b.end));
    const next = mine.find((b) => new Date(b.start) > now);
    for (const b of inForce) {
      rows.push(el("div", "sb-line sb-line-" + b.kind, KIND_WORD[b.kind] + " NOW: " + label(b) + ", ends in " + span(new Date(b.end) - now) + (b.note ? " (" + b.note + ")" : "")));
    }
    if (next) rows.push(el("div", "sb-line", "NEXT: " + KIND_WORD[next.kind] + " " + label(next) + " in " + span(new Date(next.start) - now) + ", " + when(next)));
    nowRoot.replaceChildren(...rows);
    nowMeta.textContent = (many ? pick + ": " : "") + waiting.length + " waiting, " + mine.length + " booked";
  }

  function dayStart(offset) {
    const d = new Date();
    d.setHours(0, 0, 0, 0);
    d.setDate(d.getDate() + offset);
    return d;
  }

  // The part of [s, e) inside the day [d0, d1), as left and width percent.
  function seg(s, e, d0, d1) {
    const a = Math.max(s, d0), b = Math.min(e, d1);
    if (b <= a) return null;
    const len = d1 - d0;
    return { left: ((a - d0) / len) * 100, width: ((b - a) / len) * 100, a, b };
  }

  function windowSegs(win, d0, d1) {
    const m = /^(\d\d):(\d\d)-(\d\d):(\d\d)$/.exec(win || "");
    if (!m) return [];
    const at = (h, mm) => { const d = new Date(d0); d.setHours(+h, +mm, 0, 0); return d.getTime(); };
    const s = at(m[1], m[2]), e = at(m[3], m[4]);
    return s < e ? [[s, e]] : [[d0, e], [s, d1]];
  }

  function paintBoard() {
    const now = Date.now();
    const head = el("div", "sb-row sb-hours");
    head.append(el("span", "sb-day", ""));
    const scale = el("div", "sb-track sb-scale");
    for (let h = 0; h < 24; h += 3) {
      const t = el("span", "sb-tick", pad2(h));
      t.style.left = (h / 24) * 100 + "%";
      scale.append(t);
    }
    head.append(scale);
    const rows = [head];
    const bs = (st.bookings || []).filter((b) => onCard(b.card));
    const live = (st.leases || []).filter((l) => onCard(l.card));
    for (let i = 0; i < DAYS; i++) {
      const d0 = dayStart(i).getTime(), d1 = dayStart(i + 1).getTime();
      const row = el("div", "sb-row");
      row.setAttribute("role", "row");
      row.append(el("span", "sb-day" + (i === 0 ? " sb-today" : ""), i === 0 ? "TODAY" : dayWord(new Date(d0))));
      const track = el("div", "sb-track");
      track.setAttribute("role", "gridcell");
      track.setAttribute("aria-label", dayWord(new Date(d0)));
      if (i === 0) {
        const past = el("div", "sb-past");
        past.style.width = ((now - d0) / (d1 - d0)) * 100 + "%";
        const line = el("div", "sb-nowline");
        line.style.left = past.style.width;
        line.title = "now " + hm(new Date(now));
        track.append(past, line);
      }
      for (const s of st.schedules || []) {
        for (const [a, b] of windowSegs(s.window, d0, d1)) {
          const g = seg(a, b, d0, d1);
          if (!g) continue;
          const w = el("div", "sb-window");
          w.style.left = g.left + "%";
          w.style.width = g.width + "%";
          w.title = "window " + s.window + ": " + s.engine;
          track.append(w);
        }
      }
      for (const l of live) {
        const g = seg(now - (l.age_seconds || 0) * 1000, now, d0, d1);
        if (!g) continue;
        const bar = el("div", "sb-live");
        bar.style.left = g.left + "%";
        bar.style.width = Math.max(g.width, 0.3) + "%";
        bar.title = "live: " + l.engine + " (" + l.priority + ") since " + hm(new Date(g.a));
        track.append(bar);
      }
      const lanes = { run: 0, down: 0, priority: 1 };
      for (const b of bs) {
        const g = seg(new Date(b.start).getTime(), new Date(b.end).getTime(), d0, d1);
        if (!g) continue;
        const bar = el("button", "sb-bar sb-" + b.kind + (lanes[b.kind] ? " sb-lane2" : ""));
        bar.type = "button";
        bar.style.left = g.left + "%";
        bar.style.width = g.width + "%";
        const text = hm(new Date(g.a)) + " " + label(b);
        bar.append(el("span", "sb-bar-text", text));
        bar.title = KIND_WORD[b.kind] + " " + b.id + ": " + label(b) + "\n" + when(b) + " (" + span(new Date(b.end) - new Date(b.start)) + ")" + (b.note ? "\n" + b.note : "");
        bar.setAttribute("aria-label", bar.title.replace(/\n/g, ", "));
        bar.addEventListener("click", (e) => { e.stopPropagation(); openDialog(b); });
        track.append(bar);
      }
      track.addEventListener("click", (e) => {
        if (!paired) { toast("pair this device to book: berth pair on the machine", "info"); return; }
        const r = track.getBoundingClientRect();
        const t = d0 + ((e.clientX - r.left) / r.width) * (d1 - d0);
        const snap = SNAP_MIN * 60e3;
        openDialog(null, new Date(Math.max(Math.floor(t / snap) * snap, nowStep())));
      });
      row.append(track);
      rows.push(row);
    }
    boardRoot.replaceChildren(...rows);
  }

  function paintList(many) {
    const bs = (st.bookings || []).filter((b) => onCard(b.card));
    if (!bs.length) { listRoot.replaceChildren(el("div", "empty-note", "nothing booked; every hour runs by priority, then arrival")); return; }
    const now = Date.now();
    listRoot.replaceChildren(...bs.map((b) => {
      const row = el("div", "sq sw-engine sb-item sb-item-" + b.kind);
      const right = el("span", "sw-state", el("span", "sw-chip", KIND_WORD[b.kind]));
      if (b.claim) right.append(el("span", "sw-chip", "CLAIM"));
      if (new Date(b.start) <= now) right.append(el("span", "sw-chip sw-pinned", "IN FORCE"));
      if (many) right.prepend(el("span", "sw-chip", b.card));
      row.append(el("div", "top", el("span", "name", b.id + " " + label(b)), right));
      row.append(el("div", "meta", when(b) + " (" + span(new Date(b.end) - new Date(b.start)) + ")" + (b.note ? ", " + b.note : "")));
      if (paired) {
        const edit = el("button", "sw-chip sb-edit", "EDIT");
        edit.type = "button";
        edit.setAttribute("aria-label", "move or cancel " + b.id);
        edit.addEventListener("click", () => openDialog(b));
        right.append(edit);
      }
      return row;
    }));
  }

  function engineNames() {
    const names = new Set();
    for (const e of st.engines || []) if (e.name) names.add(e.name);
    for (const f of st.footprints || []) if (f.engine) names.add(f.engine);
    for (const l of [...(st.leases || []), ...(st.waiting || [])]) { names.add(l.engine); if (l.group) names.add(l.group); }
    return [...names].sort();
  }

  function formBooking() {
    const f = form.elements;
    const start = new Date(f.start.value);
    const b = {
      kind: f.kind.value, who: f.who.value.trim(), engine: f.engine.value.trim(), priority: f.priority.value,
      start: start.toISOString(), end: new Date(start.getTime() + Number(f.hours.value) * HOUR).toISOString(),
      note: f.note.value.trim(), card: pick, claim: f.kind.value === "run" && f.claim.checked,
    };
    if (b.kind === "down") { b.engine = ""; b.priority = ""; }
    if (editing) b.id = editing.id;
    return b;
  }

  function syncForm() {
    const kind = form.elements.kind.value;
    form.querySelector(".sf-engine").hidden = kind === "down";
    form.querySelector(".sf-prio").hidden = kind === "down";
    form.querySelector(".sf-claim").hidden = kind !== "run";
    form.querySelector(".sf-new").hidden = !!editing;
    try { explainEl.textContent = explain(formBooking()); } catch (e) { explainEl.textContent = ""; }
  }

  function openDialog(b, at) {
    if (!paired && !b) { toast("pair this device to book: berth pair on the machine", "info"); return; }
    editing = b;
    const f = form.elements;
    $("sched-engines").replaceChildren(...engineNames().map((n) => { const o = el("option"); o.value = n; return o; }));
    $("sched-dlg-title").textContent = b ? "BOOKING " + b.id : "BOOK " + pick.toUpperCase();
    const start = b ? new Date(b.start) : at || new Date(nowStep());
    f.kind.value = b ? b.kind : "run";
    f.who.value = b ? b.who : "";
    f.engine.value = b ? b.engine || "" : "";
    f.priority.value = b ? b.priority || "" : "";
    f.start.value = localInput(start);
    f.hours.value = b ? String(Math.round(((new Date(b.end) - start) / HOUR) * 100) / 100) : "1";
    f.days.value = "1";
    f.earliest.checked = false;
    f.claim.checked = !!(b && b.claim);
    f.note.value = b ? b.note || "" : "";
    f.kind.disabled = !!b;
    for (const x of form.querySelectorAll("input, select")) if (x !== f.kind) x.disabled = !paired;
    $("sched-save").textContent = b ? "MOVE" : "BOOK";
    $("sched-save").hidden = !paired;
    $("sched-cancel").hidden = !b || !paired;
    errEl.textContent = "";
    syncForm();
    showDialog(dlg);
  }

  async function send(action, booking, extra) {
    if (demo) { toast("demo: " + action + " " + (booking.id || booking.who) + " is not sent", "info"); dlg.close(); return; }
    errEl.textContent = "";
    try {
      const res = await fetch("/v1/board/schedule", {
        method: "POST",
        headers: { "X-Berth": "1", "Content-Type": "application/json", accept: "application/json" },
        body: JSON.stringify({ action, booking, ...extra }),
      });
      const j = await res.json().catch(() => ({}));
      if (!res.ok) {
        if (res.status === 401) paired = false;
        errEl.textContent = j.message || res.statusText;
        return;
      }
      const made = j.bookings || [];
      toast(action === "book" ? "booked " + made.map((x) => x.id).join(", ") : (action === "move" ? "moved " : "cancelled ") + booking.id, "ok");
      dlg.close();
      await read();
    } catch (e) {
      errEl.textContent = e.message;
    }
  }

  form.addEventListener("input", syncForm);
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const b = formBooking();
    if (editing) send("move", b);
    else send("book", b, { days: Number(form.elements.days.value) || 1, earliest: form.elements.earliest.checked });
  });
  $("sched-cancel").addEventListener("click", () => editing && send("cancel", { id: editing.id }));
  dlg.querySelector(".sc-close").addEventListener("click", () => dlg.close());
  $("sb-book").addEventListener("click", () => openDialog(null));
  if (!demo) {
    fetch("/v1/me", { headers: { accept: "application/json" } })
      .then((r) => r.json()).then((j) => { paired = !!j.paired; if (st) paint(); }).catch(() => {});
  }

  return {
    show() {
      clearInterval(timer);
      read();
      timer = setInterval(() => { if (!document.hidden) read(); }, POLL_MS);
    },
    hide() { clearInterval(timer); timer = 0; },
  };
}
