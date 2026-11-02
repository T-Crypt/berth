// The SWITCH tab: who holds each card, and for a paired device the
// switchboard's actions: switch the card to an engine, stop the one that
// holds it, or load one llama-swap model. The board and the actions are the gateway's; the harbor
// forwards them. API strings only ever reach textContent.
import { $, el, fmtGiB, siEl } from "./util.js";
import { showDialog, toast } from "./ui.js";

const POLL_MS = 3000;
const PICK_KEY = "berth.switch.card";
const ENGINE_SI = { up: "up", offline: "offline", waiting: "boarding", starting: "half", failed: "crit" };
const CARD_SI = { free: "adrift", held: "docked", draining: "casting-off", passthrough: "aground", "probe-failed": "crit" };
const CARD_WORD = { free: "FREE", held: "HELD", draining: "DRAINING", passthrough: "IN A VM", "probe-failed": "NO READING" };

export function initSwitch({ demo }) {
  const cardsRoot = $("sw-cards");
  const pickRoot = $("sw-picker");
  const enginesRoot = $("sw-engines");
  const noteRoot = $("sw-note");
  const meta = $("sw-meta");
  const pairDlg = $("pair-dlg");
  let board = null;
  let paired = demo;
  let timer = 0;
  let pick = localStorage.getItem(PICK_KEY) || "";
  const busy = new Set();
  const errors = new Map(); // engine -> the last action's error, until the next read
  const open = new Set();   // engines whose model list is unfolded

  async function read() {
    try {
      const res = await fetch(demo ? "demo-board.json" : "/v1/board", { headers: { accept: "application/json" } });
      const j = await res.json();
      if (!res.ok) throw new Error(j.message || (j.error && j.error.message) || res.statusText);
      board = j;
      paint();
    } catch (e) {
      board = null;
      enginesRoot.replaceChildren(el("div", "empty-note error-note", "no board: " + e.message));
      cardsRoot.replaceChildren();
      pickRoot.replaceChildren();
      meta.textContent = "not read";
    }
  }

  async function act(action, engine, model) {
    if (demo) { toast("demo: " + action + " " + (model || engine) + " is not sent", "info"); return; }
    const key = model ? engine + "\u0000" + model : engine;
    busy.add(key);
    errors.delete(key);
    paint();
    try {
      const res = await fetch("/v1/board/" + action, {
        method: "POST",
        headers: { "X-Berth": "1", "Content-Type": "application/json", accept: "application/json" },
        body: JSON.stringify(model ? { engine, model } : { engine }),
      });
      if (!res.ok) {
        const j = await res.json().catch(() => ({}));
        errors.set(key, j.message || (j.error && j.error.message) || res.statusText);
        if (res.status === 401) paired = false;
      }
    } catch (e) {
      errors.set(key, e.message);
    }
    busy.delete(key);
    await read();
  }

  function pinnedHolders() {
    const names = new Set();
    for (const c of (board && board.cards) || []) for (const h of c.holders || []) if (h.pinned) names.add(h.name);
    return names;
  }

  function paint() {
    if (!board) return;
    const cards = board.cards || [];
    const engines = board.engines || [];
    if (pick && !cards.some((c) => c.card === pick)) pick = "";
    meta.textContent = board.reachable ? (board.name || "this node") : "berthd is not answering; cards unknown";
    cardsRoot.replaceChildren(...cards.map(cardItem));
    paintPicker(cards);
    noteRoot.replaceChildren(paired ? pairButton() : el("div", "empty-note", "Read only. Run berth pair on the machine to switch from this device."));
    const shown = pick ? engines.filter((e) => (e.cards || []).includes(pick)) : engines;
    const pinned = pinnedHolders();
    enginesRoot.replaceChildren(...shown.map((e) => engineRow(e, cards.length > 1, pinned.has(e.name))));
    if (shown.length === 0) enginesRoot.append(el("div", "empty-note", pick ? "no engine runs on " + pick : "berth.toml has no engines"));
  }

  function cardItem(c) {
    const word = CARD_WORD[c.state] || String(c.state || "").toUpperCase();
    const head = el("div", "top", el("span", "name", (c.card ? c.card + " " : "") + c.name),
      el("span", "sw-state", siEl(CARD_SI[c.state] || "square"), word));
    const row = el("div", "sq sw-card");
    row.append(head);
    if (c.cordoned) head.append(el("span", "sw-chip sw-cordon", "CORDONED"));
    if (c.total) row.append(el("div", "meta", fmtGiB(c.used) + " of " + fmtGiB(c.total) + " GiB in use"));
    const holders = (c.holders || []).map((h) => h.name + (h.pinned ? " (pinned)" : "") + (h.state && h.state !== "active" ? ", " + h.state : ""));
    row.append(el("div", "meta", holders.length ? "held by " + holders.join(", ") : "no holder"));
    return row;
  }

  function paintPicker(cards) {
    if (cards.length < 2) { pickRoot.replaceChildren(); return; }
    const chip = (value, label) => {
      const b = el("button", "chip", label);
      b.type = "button";
      b.setAttribute("aria-pressed", String(pick === value));
      b.addEventListener("click", () => {
        pick = value;
        if (value) localStorage.setItem(PICK_KEY, value); else localStorage.removeItem(PICK_KEY);
        paint();
      });
      return b;
    };
    const g = el("div", "mode sw-picker");
    g.setAttribute("role", "group");
    g.setAttribute("aria-label", "the card to switch");
    g.append(chip("", "ALL"), ...cards.filter((c) => c.card).map((c) => chip(c.card, c.card.toUpperCase())));
    pickRoot.replaceChildren(g);
  }

  function engineRow(e, many, pinned) {
    const state = e.state || "offline";
    const name = el("span", "name", e.name);
    const right = el("span", "sw-state", siEl(ENGINE_SI[state] || "square"), state.toUpperCase());
    if (many && e.cards && e.cards.length) right.prepend(el("span", "sw-chip", e.cards.join(",")));
    const row = el("div", "sq sw-engine sw-" + state);
    row.append(el("div", "top", name, right));
    if (e.message) row.append(el("div", "meta", e.message));
    if (state === "failed" && e.detail) row.append(el("div", "meta sw-detail", e.detail));
    const err = errors.get(e.name);
    if (err) row.append(el("div", "meta sw-error", err));
    const actions = el("div", "sw-actions");
    if (pinned) actions.append(el("span", "sw-chip sw-pinned", "PINNED"));
    if (paired) {
      if (e.launch && state !== "up" && state !== "starting" && state !== "waiting") actions.append(button("SWITCH", "switch", e.name));
      if (e.stoppable && !pinned) actions.append(button("STOP", "stop", e.name));
    }
    const models = e.models || [];
    if (models.length) actions.append(modelsToggle(e.name, models.length));
    if (actions.childElementCount) row.append(actions);
    if (models.length && open.has(e.name)) row.append(modelList(e, models));
    return row;
  }

  function button(label, action, engine) {
    const b = el("button", "btn sw-btn" + (action === "stop" ? " sw-stop" : ""), busy.has(engine) ? "…" : label);
    b.type = "button";
    b.disabled = busy.has(engine);
    b.setAttribute("aria-label", label.toLowerCase() + " " + engine);
    b.addEventListener("click", () => act(action, engine));
    return b;
  }

  function modelsToggle(engine, n) {
    const b = el("button", "btn sw-btn sw-models", (open.has(engine) ? "HIDE " : "") + n + (n === 1 ? " MODEL" : " MODELS"));
    b.type = "button";
    b.setAttribute("aria-expanded", String(open.has(engine)));
    b.addEventListener("click", () => { if (open.has(engine)) open.delete(engine); else open.add(engine); paint(); });
    return b;
  }

  // A llama-swap row loads one model on request; the board marks it loading
  // until llama-swap reports it ready.
  function modelList(e, models) {
    const ul = el("ul", "sw-model-list");
    for (const m of models) {
      const li = el("li", m.loaded ? "loaded" : null, siEl(m.loaded ? "dot" : m.loading ? "half" : "ring"), el("span", "sw-model-id", m.id));
      if (m.loaded) li.append(el("span", "sw-loaded", "loaded"));
      else if (m.loading) li.append(el("span", "sw-loading", "loading"));
      else if (paired && e.type === "llama-swap") li.append(loadButton(e.name, m.id));
      const err = errors.get(e.name + "\u0000" + m.id);
      if (err) li.append(el("span", "sw-error", err));
      ul.append(li);
    }
    return ul;
  }

  function loadButton(engine, model) {
    const key = engine + "\u0000" + model;
    const b = el("button", "btn sw-btn sw-load", busy.has(key) ? "…" : "LOAD");
    b.type = "button";
    b.disabled = busy.has(key);
    b.setAttribute("aria-label", "load " + model);
    b.addEventListener("click", () => act("load", engine, model));
    return b;
  }

  function pairButton() {
    const b = el("button", "btn sw-btn", "PAIR A PHONE");
    b.type = "button";
    b.addEventListener("click", pairPhone);
    return el("div", "sw-paired", el("span", "meta", "This device is paired and can switch the card."), b);
  }

  async function pairPhone() {
    const body = pairDlg.querySelector(".pair-body");
    body.replaceChildren(el("div", "empty-note", "asking berthd for a code"));
    showDialog(pairDlg);
    if (demo) { body.replaceChildren(el("div", "empty-note", "demo: no code is minted")); return; }
    try {
      const res = await fetch("/v1/pair/code", { method: "POST", headers: { "X-Berth": "1", accept: "application/json" } });
      const j = await res.json();
      if (!res.ok) throw new Error(j.message || res.statusText);
      const img = el("img", "pair-qr");
      img.src = j.qr;
      img.alt = "QR code for the pairing link";
      body.replaceChildren(img, el("div", "meta", "Scan it on the other phone within 15 minutes. It works once."), el("div", "pair-link", j.link));
    } catch (e) {
      body.replaceChildren(el("div", "empty-note error-note", "no code: " + e.message));
    }
  }

  pairDlg.querySelector(".sc-close").addEventListener("click", () => pairDlg.close());
  if (!demo) {
    fetch("/v1/me", { headers: { accept: "application/json" } })
      .then((r) => r.json()).then((j) => { paired = !!j.paired; paint(); }).catch(() => {});
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
