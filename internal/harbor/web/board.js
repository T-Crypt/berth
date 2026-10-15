// The departures board: split-flap tiles, one tile per character.
// Rows are real buttons so Tab reaches every voyage and Enter opens its ticket.
import { el, fmtBytes, isReducedMotion } from "./util.js";
import { icon, pulseClass } from "./ui.js";
const COLS = ["ticket", "vessel", "berth", "cls", "hold", "status", "time"];
// Columns the header can sort. The board keeps the broker's queue order
// until the operator picks one.
const SORTABLE = { vessel: "vessel", hold: "hold", status: "status", time: "time" };
const STATUS_RANK = { waiting: 0, active: 1, draining: 2, releasing: 3, stuck: 4 };
function holdBytes(entry) {
  const l = entry.lease;
  if (!entry.isWaiting && l.used > 0) return l.used;
  return l.needs > 0 ? l.needs : -1;
}
function sortEntries(entries, sort) {
  if (!sort || !SORTABLE[sort]) return entries;
  const arr = [...entries];
  const stateOf = (e) => (e.isWaiting ? "waiting" : e.lease.state);
  const ageOf = (e) => (e.lease.age_seconds == null ? -1 : e.lease.age_seconds);
  switch (sort) {
    case "vessel": arr.sort((a, b) => String(a.lease.engine || "").localeCompare(String(b.lease.engine || ""))); break;
    case "hold": arr.sort((a, b) => holdBytes(b) - holdBytes(a)); break;
    case "status": arr.sort((a, b) => (STATUS_RANK[stateOf(a)] ?? 9) - (STATUS_RANK[stateOf(b)] ?? 9)); break;
    case "time": arr.sort((a, b) => ageOf(b) - ageOf(a)); break;
  }
  return arr;
}

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
function makeHead(onSort) {
  const h = el("div", "board-cols board-head");
  h._sortBtns = {};
  for (const c of COLS) {
    const cell = el("div", "cell");
    if (SORTABLE[c] && onSort) {
      const b = el("button", "bhead-btn");
      b.type = "button";
      b.dataset.sort = c;
      b.setAttribute("aria-label", "sort departures by " + HEADS[c].toLowerCase());
      b.append(el("span", null, HEADS[c]), icon("swap", 10));
      b.addEventListener("click", () => onSort(c));
      cell.append(b);
      h._sortBtns[c] = b;
    } else {
      cell.append(el("span", null, HEADS[c]));
    }
    h.append(cell);
  }
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
  constructor(root, onOpenTicket, onSort) {
    this.root = root;
    this.onOpen = onOpenTicket;
    this.onSort = onSort || null;
    this.head = makeHead(this.onSort);
    this.rows = new Map(); // key -> button
    this.prevState = new Map(); // key -> status word, for the change flash
    this.empty = el("div", "board-empty", "No voyages. All berths free.");
    this.root.append(this.head, this.empty);
    this.foot = el("div", "board-foot");
    this.root.append(this.foot);
  }
  focusRow(key) {
    const row = this.rows.get(key);
    if (row) row.focus();
  }
  // data: [{ key, lease, isWaiting }] in display order; sort names a
  // SORTABLE column or is null for the broker's own queue order
  render(data, berth, sort) {
    const ordered = sortEntries(data, sort);
    for (const c of Object.keys(this.head._sortBtns)) {
      this.head._sortBtns[c].classList.toggle("sorted", sort === c);
    }
    const seen = new Set();
    let i = 0;
    for (const entry of ordered) {
      seen.add(entry.key);
      let row = this.rows.get(entry.key);
      const isNew = !row;
      if (isNew) {
        row = makeRow(entry.key, this.onOpen);
        this.rows.set(entry.key, row);
      }
      const l = entry.lease;
      const st = entry.isWaiting ? "waiting" : l.state;
      const word = statusText(st);
      if (!isNew && this.prevState.get(entry.key) !== word) pulseClass(row, "row-changed");
      this.prevState.set(entry.key, word);
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
        this.prevState.delete(key);
      }
    }
    const hasRows = seen.size > 0;
    this.empty.hidden = hasRows;
    this.head.hidden = !hasRows;
    this.foot.hidden = !hasRows;
    this.foot.textContent = hasRows
      ? seen.size + (seen.size === 1 ? " voyage" : " voyages") + (sort && SORTABLE[sort] ? ", ordered by " + HEADS[sort].toLowerCase() : ", broker order")
      : "";
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
