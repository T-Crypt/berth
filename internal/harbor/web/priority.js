import { el } from "./util.js";

function windowWord(w) {
  if (!(w > 0)) return "off";
  if (w < 60) return Math.round(w) + "s";
  return Math.round(w / 60) + "m";
}

function hoursWord(h) {
  if (h == null || !Number.isFinite(h)) return "-";
  return (Math.round(h * 10) / 10) + "h";
}

export class PriorityView {
  constructor(root, meta) {
    this.root = root;
    this.meta = meta || null;
  }
  render(priority) {
    const ranks = priority && Array.isArray(priority.engines) ? priority.engines : [];
    if (ranks.length === 0) {
      this.root.replaceChildren(el("div", "empty-note", "no engine ranking reported by this node"));
      if (this.meta) this.meta.textContent = "card ranking";
      return;
    }
    const src = priority.source === "order" ? "config order" : "7 day card hours";
    if (this.meta) this.meta.textContent = "decided by " + src + ", juggle window " + windowWord(priority.juggle_window);
    const table = el("table", "ptable");
    const head = el("tr");
    for (const h of ["RANK", "ENGINE", "7D HOURS"]) {
      const th = el("th");
      th.textContent = h;
      head.append(th);
    }
    table.append(el("thead", null, head));
    const body = el("tbody");
    const rows = [...ranks].sort((a, b) => (a.rank || 0) - (b.rank || 0));
    for (const r of rows) {
      const tr = el("tr");
      const rank = el("td", "num");
      rank.textContent = String(r.rank || "-");
      const eng = el("td");
      eng.textContent = r.engine || "?";
      const hrs = el("td", "num");
      hrs.textContent = hoursWord(r.hours_7d);
      tr.append(rank, eng, hrs);
      body.append(tr);
    }
    table.append(body);
    this.root.replaceChildren(
      el("div", "priority-note", "when two engines at one priority ask for the card inside the window, the higher rank takes it without flipping the card"),
      table
    );
  }
}
