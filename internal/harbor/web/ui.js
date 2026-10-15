// Shared UI primitives: an icon set built with createElementNS, toasts,
// relative time, clipboard with a fallback, keyboard shortcuts, skeleton and
// empty-state nodes, and a one-shot flash animation. No innerHTML anywhere.
const SVGNS = "http://www.w3.org/2000/svg";
const ICON_PATHS = {
  search: ["M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14z", "M16.6 16.6 21 21"],
  close: ["M18 6 6 18", "M6 6l12 12"],
  copy: ["M9 9h11a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2v-9a2 2 0 0 1 2-2z", "M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"],
  link: ["M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.5 1.5", "M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.5-1.5"],
  command: ["M18 3a3 3 0 0 0-3 3v12a3 3 0 0 0 3 3 3 3 0 0 0 3-3 3 3 0 0 0-3-3H6a3 3 0 0 0-3 3 3 3 0 0 0 3 3 3 3 0 0 0 3-3V6a3 3 0 0 0-3-3 3 3 0 0 0-3 3 3 3 0 0 0 3 3h12a3 3 0 0 0 3-3 3 3 0 0 0-3-3z"],
  up: ["M12 19V5", "M5 12l7-7 7 7"],
  refresh: ["M21 12a9 9 0 1 1-2.6-6.4", "M21 3v5h-5"],
  down: ["M6 9l6 6 6-6"],
  alert: ["M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z", "M12 9v4", "M12 17h.01"],
  check: ["M20 6 9 17l-5-5"],
  swap: ["M7 20V4", "M4 7l3-3 3 3", "M17 4v16", "M14 17l3 3 3-3"],
  help: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M9.1 9a3 3 0 0 1 5.8 1c0 2-3 2.2-3 4", "M12 17h.01"],
  filter: ["M3 4h18l-7 8v6l-4 2v-8L3 4z"],
  clock: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M12 7v5l3 2"],
  pause: ["M8 5v14", "M16 5v14"],
  sort: ["M7 4v16", "M4 7l3-3 3 3", "M17 4v16", "M14 17l3 3 3-3"],
  ticket: ["M3 8a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v2a2 2 0 0 0 0 4v2a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-2a2 2 0 0 0 0-4V8z", "M13 6v2", "M13 11v2", "M13 16v2"],
};

// A stroke icon in the current text color. size is the square edge in px.
export function icon(name, size = 14) {
  const svg = document.createElementNS(SVGNS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.9");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  svg.classList.add("icon");
  for (const d of ICON_PATHS[name] || ICON_PATHS.help) {
    const p = document.createElementNS(SVGNS, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}

export function hasIcon(name) {
  return Object.prototype.hasOwnProperty.call(ICON_PATHS, name);
}

/* ---------------- toasts ---------------- */
let toastHost = null;
let toastSeq = 0;
function toastRoot() {
  if (!toastHost) {
    toastHost = document.createElement("div");
    toastHost.className = "toast-host";
    toastHost.id = "toast-host";
    toastHost.setAttribute("role", "status");
    toastHost.setAttribute("aria-live", "polite");
    document.body.append(toastHost);
  }
  return toastHost;
}
// A short-lived note bottom-right. tone: "info" | "good" | "warn" | "bad".
export function toast(text, tone = "info", ms = 2600) {
  const host = toastRoot();
  const id = ++toastSeq;
  const row = document.createElement("div");
  row.className = "toast tone-" + tone;
  row.dataset.toast = String(id);
  const body = document.createElement("span");
  body.className = "toast-text";
  body.textContent = text;
  const close = document.createElement("button");
  close.type = "button";
  close.className = "toast-close";
  close.setAttribute("aria-label", "dismiss: " + text);
  close.append(icon("close", 12));
  close.addEventListener("click", () => remove());
  row.append(body, close);
  host.append(row);
  let timer = 0;
  function remove() {
    if (!row.isConnected) return;
    window.clearTimeout(timer);
    if (row._removing) return;
    row._removing = true;
    row.classList.add("out");
    const done = () => row.remove();
    if (prefersReduced()) done();
    else row.addEventListener("animationend", done, { once: true });
  }
  timer = window.setTimeout(remove, ms);
  // keep at most four toasts on screen
  while (host.children.length > 4) host.firstElementChild.remove();
  return id;
}

/* ---------------- motion ---------------- */
let reducedQuery = null;
export function prefersReduced() {
  if (reducedQuery == null && typeof matchMedia === "function") {
    reducedQuery = matchMedia("(prefers-reduced-motion: reduce)");
  }
  return reducedQuery ? reducedQuery.matches : false;
}
// Watch the preference so the body class and JS decisions stay in step.
export function watchMotion(onChange) {
  if (reducedQuery == null && typeof matchMedia === "function") {
    reducedQuery = matchMedia("(prefers-reduced-motion: reduce)");
  }
  if (!reducedQuery || typeof reducedQuery.addEventListener !== "function") return;
  reducedQuery.addEventListener("change", () => {
    document.body.classList.toggle("reduced-motion", prefersReduced());
    if (typeof onChange === "function") onChange(prefersReduced());
  });
}

// Run cls once on el, then drop it when the animation ends.
export function flash(el, cls = "flash") {
  if (!el) return;
  el.classList.remove(cls);
  void el.offsetWidth;
  el.classList.add(cls);
  const done = () => el.classList.remove(cls);
  el.addEventListener("animationend", done, { once: true });
}

/* ---------------- relative time ---------------- */
const MIN = 60000, HOUR = 3600000, DAY = 86400000;
// "just now", "12s ago", "3m ago", "2h ago", then a date once it is days old.
export function relTime(d) {
  const ms = d instanceof Date ? d.getTime() : Number(d);
  if (!Number.isFinite(ms) || ms <= 0) return "";
  const diff = Date.now() - ms;
  if (diff < 2 * 1000) return "just now";
  if (diff < MIN) return Math.floor(diff / 1000) + "s ago";
  if (diff < HOUR) return Math.floor(diff / MIN) + "m ago";
  if (diff < DAY) return Math.floor(diff / HOUR) + "h ago";
  const day = Math.floor(diff / DAY);
  return day + (day === 1 ? " day ago" : " days ago");
}

/* ---------------- clipboard ---------------- */
export async function copyText(text) {
  let ok = false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      ok = true;
    }
  } catch (err) {
    ok = false;
  }
  if (!ok) {
    // Fallback for a plain http origin or a browser without the async API.
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.append(ta);
      ta.select();
      ok = document.execCommand("copy");
      ta.remove();
    } catch (err) {
      ok = false;
    }
  }
  return ok;
}

/* ---------------- keyboard shortcuts ---------------- */
const TYPING = new Set(["INPUT", "TEXTAREA", "SELECT"]);
function isTyping(el) {
  return !!el && (TYPING.has(el.tagName) || el.isContentEditable);
}
// Bind a key such as "1" or "?" (no modifiers, ignored while typing).
export function onKey(key, fn) {
  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (isTyping(e.target)) return;
    if (e.key === key) fn(e);
  });
}

/* ---------------- dialog helpers ---------------- */
// Show a dialog, focus the first control, and return it closed or on Escape.
export function showDialog(dialog, selector = "[data-autofocus]") {
  if (typeof dialog.showModal === "function") dialog.showModal();
  else dialog.setAttribute("open", "");
  const target = dialog.querySelector(selector) || dialog.querySelector("button");
  if (target) target.focus();
}
export function closeDialog(dialog) {
  if (dialog && dialog.open) dialog.close();
}

/* ---------------- states: skeleton, empty, error ---------------- */
export function skeleton(lines = 3, cls = "skel") {
  const box = document.createElement("div");
  box.className = "skel-box";
  box.setAttribute("aria-hidden", "true");
  for (let i = 0; i < lines; i += 1) {
    const line = document.createElement("div");
    line.className = cls + (i === lines - 1 ? " skel-short" : "");
    box.append(line);
  }
  return box;
}
export function emptyNote(text, cls = "empty-note") {
  const box = document.createElement("div");
  box.className = cls;
  const ico = document.createElement("span");
  ico.className = "empty-ico";
  ico.append(icon("clock", 13));
  const label = document.createElement("span");
  label.textContent = text;
  box.append(ico, label);
  return box;
}
// A note with a retry button, for a fetch that failed.
export function errorNote(text, onRetry, cls = "empty-note error-note") {
  const box = document.createElement("div");
  box.className = cls;
  const ico = document.createElement("span");
  ico.className = "empty-ico";
  ico.append(icon("alert", 13));
  const label = document.createElement("span");
  label.textContent = text;
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "retry-btn";
  btn.textContent = "RETRY";
  btn.addEventListener("click", () => onRetry());
  box.append(ico, label, btn);
  return box;
}

/* ---------------- misc ---------------- */
// Toggle a class on el for ms. Used for "just changed" row highlights.
export function pulseClass(el, cls, ms = 900) {
  if (!el) return;
  el.classList.add(cls);
  window.setTimeout(() => el.classList.remove(cls), ms);
}
// Escape text for a data attribute value or a CSV field.
export function csvCell(v) {
  const s = String(v == null ? "" : v);
  return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}
