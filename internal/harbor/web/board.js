// The departures board: split-flap tiles, one tile per character.
// Rows are real buttons so Tab reaches every voyage and Enter opens its ticket.
import { el, fmtBytes, isReducedMotion } from "./util.js";
const COLS = ["ticket", "vessel", "berth", "cls", "hold", "status", "time"];

// Characters per column. Every cell always shows exactly this many tiles
// (padded with blank tiles, trimmed with "~"), so the board never wraps.
const WIDTH = { ticket: 4, vessel: 14, berth: 4, cls: 8, hold: 6, status: 11, time: 6 };
const CLASS = { foreground: "FIRST", interactive: "CABIN", batch: "STEERAGE" };

function fit(text, n) {
  const t = String(text).toUpperCase();
  return t.length > n ? t.slice(0, n - 1) + "~" : t.padEnd(n, " ");
}

// The ticket number is the lease's sequence; the full id is on the ticket.
function ticketNo(id) {
  const m = /(\d+)$/.exec(id || "");
  return m ? m[1].slice(-4).padStart(4, "0") : "----";
}

function gib(bytes) {
  return (Math.round(bytes / 1073741824 * 10) / 10).toFixed(1) + "G";
}
const HEADS = {
  ticket: "TICKET",
  vessel: "VESSEL",
  berth: "BERTH",
  cls: "CLASS",
  hold: "HOLD",
  status: "STATUS",
  time: "TIME",
};
const STAGGER_MS = 15;
function makeHead() {
  const h = el("div", "board-cols board-head");
  for (const c of COLS) h.append(el("div", "cell", el("span", null, HEADS[c])));
  return h;
}
function makeRow(key, onOpen) {
  const btn = el("button", "board-row");
  btn.type = "button";
  btn.dataset.key = key;
  btn._cells = {};
  for (const c of COLS) {
    const cell = el("div", c === "ticket" ? "cell lease-id-cell" : "cell");
    cell.dataset.col = c;
    cell._tiles = [];
    btn.append(cell);
    btn._cells[c] = cell;
  }
  btn.addEventListener("click", () => onOpen(key));
  return btn;
}
function flipTile(tile, ch, delay) {
  if (ch === tile.textContent) return;
  if (isReducedMotion()) {
    tile.classList.remove("flipping");
    tile.textContent = ch;
    return;
  }
  // The character lands at once and the flap animation is only the visual:
  // the board must read correctly even where timers are throttled
  // (background tabs) or animations never run.
  tile.textContent = ch;
  tile.style.animationDelay = delay + "ms";
  tile.classList.remove("flipping");
  void tile.offsetWidth; // restart the animation
  tile.classList.add("flipping");
  if (!tile._endBound) {
    tile.addEventListener("animationend", () => tile.classList.remove("flipping"));
    tile._endBound = true;
  }
}
// Reconcile a cell's tiles with new text. tone: "" | cyan | amber | magenta | dim
function updateCell(cell, text, tone) {
  const chars = Array.from(String(text));
  let tiles = cell._tiles;
  while (tiles.length < chars.length) {
    const t = el("span", "tile");
    t.textContent = "";
    cell.append(t);
    tiles = cell._tiles;
    tiles.push(t);
  }
  while (tiles.length > chars.length) {
    const t = tiles.pop();
    cell._tiles = tiles;
    t.remove();
  }
  cell._tiles = tiles;
  const toneCls = tone ? " tone-" + tone : "";
  chars.forEach((ch, i) => {
    const tile = tiles[i];
    const cls = ch === " " ? "tile space" : "tile" + toneCls;
    if (tile.className !== cls) tile.className = cls;
    flipTile(tile, ch, i * STAGGER_MS);
  });
}
export class Board {
  constructor(root, onOpenTicket) {
    this.root = root;
    this.onOpen = onOpenTicket;
    this.head = makeHead();
    this.rows = new Map(); // key -> button
    this.empty = el("div", "board-empty", "No voyages. All berths free.");
    this.root.append(this.head, this.empty);
  }
  focusRow(key) {
    const row = this.rows.get(key);
    if (row) row.focus();
  }
  // data: [{ key, lease, isWaiting }] in display order
  render(data, berth) {
    const seen = new Set();
    let i = 0;
    for (const entry of data) {
      seen.add(entry.key);
      let row = this.rows.get(entry.key);
      if (!row) {
        row = makeRow(entry.key, this.onOpen);
        this.rows.set(entry.key, row);
      }
      const l = entry.lease;
      const st = entry.isWaiting ? "waiting" : l.state;
      const tone = { active: "cyan", draining: "amber", stuck: "magenta", waiting: "amber" }[st] || "dim";
      const c = row._cells;
      // HOLD: what the driver measures once docked, else what was asked for.
      const hold = !entry.isWaiting && l.used > 0 ? gib(l.used) : l.needs > 0 ? gib(l.needs) : "CARD";
      updateCell(c.ticket, fit(ticketNo(l.id), WIDTH.ticket), "");
      updateCell(c.vessel, fit(l.engine || "?", WIDTH.vessel), "");
      updateCell(c.berth, fit(berth, WIDTH.berth), "");
      updateCell(c.cls, fit(CLASS[l.priority] || l.priority || "", WIDTH.cls), "");
      updateCell(c.hold, fit(hold, WIDTH.hold), "");
      // status cell: flip tiles, then the contested flag
      updateCell(c.status, fit(statusText(st), WIDTH.status), tone);
      syncContest(c.status, l.contested);
      updateCell(c.time, fit(ageText(l.age_seconds), WIDTH.time), "");
      row.setAttribute("aria-label", "open ticket " + l.id + ", " + l.engine + ", " + statusText(st).toLowerCase());
      if (l.contested > 0) row.title = "granted while holders without a lease kept " + fmtBytes(l.contested) + " on this card";
      else row.removeAttribute("title");
      this.root.append(row); // keeps DOM order = display order
      i += 1;
    }
    for (const [key, row] of this.rows) {
      if (!seen.has(key)) {
        row.remove();
        this.rows.delete(key);
      }
    }
    const hasRows = seen.size > 0;
    this.empty.hidden = hasRows;
    this.head.hidden = !hasRows;
    return i;
  }
}
function statusText(st) {
  switch (st) {
    case "waiting": return "BOARDING";
    case "active": return "DOCKED";
    case "draining": return "CASTING OFF";
    case "releasing": return "ADRIFT";
    case "stuck": return "AGROUND";
    default: return String(st || "").toUpperCase();
  }
}
function ageText(sec) {
  if (sec == null || !Number.isFinite(sec)) return "-";
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return s + "S";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "M" + String(s % 60).padStart(2, "0") + "S";
  const h = Math.floor(m / 60);
  return h + "H" + String(m % 60).padStart(2, "0") + "M";
}
// The flag sits in the row's grid on top of the status column, not inside the
// status cell, which clips anything past its fixed tile width.
function syncContest(statusCell, contested) {
  const row = statusCell.parentElement;
  let flag = row.querySelector(".flag");
  const show = contested != null && contested > 0;
  if (show && !flag) {
    flag = el("span", "flag", "CONTESTED");
    row.append(flag);
  } else if (!show && flag) {
    flag.remove();
  }
  if (show) flag.title = "granted while holders without a lease kept " + fmtBytes(contested) + " on this card";
}
