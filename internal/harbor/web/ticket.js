// The passage ticket: a boat ticket with a perforated stub and a rubber stamp.
// Esc and the close button both return focus to the voyage that opened it.
import { fmtGiB, fmtBytes, fmtAge, classWord, stateTone, berthLabel } from "./util.js";
const STAMP_WORD = {
  active: "CLEARED",
  waiting: "BOARDING",
  draining: "CASTING OFF",
  releasing: "ADRIFT",
  stuck: "AGROUND",
};
const STAMP_INK = {
  cyan: "ink-cyan",
  amber: "ink-amber",
  magenta: "ink-magenta",
  dim: "ink-amber",
};
export class Ticket {
  constructor(dialog, board) {
    this.dlg = dialog;
    this.board = board;
    this.currentKey = null;
    this.current = null;
    this.f = {
      no: dialog.querySelector("#t-no"),
      vessel: dialog.querySelector("#t-vessel"),
      cls: dialog.querySelector("#t-class"),
      berth: dialog.querySelector("#t-berth"),
      requested: dialog.querySelector("#t-requested"),
      hold: dialog.querySelector("#t-hold"),
      pid: dialog.querySelector("#t-pid"),
      gpupids: dialog.querySelector("#t-gpupids"),
      issued: dialog.querySelector("#t-issued"),
      stamp: dialog.querySelector("#t-stamp"),
      stamp2: dialog.querySelector("#t-stamp2"),
      stubNo: dialog.querySelector("#t-stub-no"),
      stubCls: dialog.querySelector("#t-stub-class"),
    };
    const closeBtn = dialog.querySelector(".ticket-close");
    closeBtn.addEventListener("click", () => this.close());
    dialog.addEventListener("cancel", (e) => {
      e.preventDefault(); // we close ourselves so focus return always runs
      this.close();
    });
    dialog.addEventListener("close", () => {
      const key = this.currentKey;
      this.currentKey = null;
      this.current = null;
      if (key) this.board.focusRow(key);
    });
  }
  open(lease, status, key) {
    this.currentKey = key;
    this.current = { lease, status, key };
    this.fill(lease, status);
    if (typeof this.dlg.showModal === "function") this.dlg.showModal();
    else this.dlg.show();
    this.dlg.querySelector(".ticket-close").focus();
  }
  close() {
    if (this.dlg.open) this.dlg.close();
  }
  refresh() {
    if (this.current) this.fill(this.current.lease, this.current.status);
  }
  fill(lease, status) {
    const f = this.f;
    f.no.textContent = lease.id || "-";
    f.vessel.textContent = lease.engine || "?";
    f.cls.textContent = classWord(lease.priority);
    const sched = (status.gpus || []).find((g) => g.scheduled);
    f.berth.textContent = sched ? "gpu" + sched.index + " - " + sched.name : berthLabel(status);
    f.requested.textContent = lease.needs === 0 ? "whole card" : fmtGiB(lease.needs) + " GiB";
    f.hold.textContent = lease.used > 0 ? fmtGiB(lease.used) + " GiB" : "none measured yet";
    f.pid.textContent = lease.pid ? String(lease.pid) + " / " + String(lease.pid_start || "-") : "- / -";
    f.gpupids.textContent = Array.isArray(lease.gpu_pids) && lease.gpu_pids.length > 0
      ? lease.gpu_pids.join(", ")
      : "-";
    const age = lease.age_seconds;
    f.issued.textContent = age == null ? "-" : age < 1 ? "just now" : fmtAge(age) + " ago";
    const tone = stateTone(lease.state);
    f.stamp.textContent = STAMP_WORD[lease.state] || String(lease.state || "").toUpperCase();
    f.stamp.className = "stamp " + (STAMP_INK[tone] || "ink-amber");
    const contested = (lease.contested || 0) > 0;
    f.stamp2.hidden = !contested;
    if (contested) {
      f.stamp2.textContent = "CONTESTED " + fmtBytes(lease.contested);
      f.stamp2.className = "stamp stamp-second ink-magenta";
    }
    f.stubNo.textContent = f.no.textContent;
    f.stubCls.textContent = f.cls.textContent;
  }
}
