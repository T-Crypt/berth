// Small shared helpers: formatting and safe DOM building.
// Nothing here touches the API; API strings only ever reach textContent.
export const GIB = 1073741824;
export const MIB = 1048576;
export function $(id) {
  return document.getElementById(id);
}
// el("div", "a b", "text") builds a node; children may be strings or nodes.
export function el(tag, cls, ...children) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  for (const c of children) {
    if (c == null) continue;
    n.append(c instanceof Node ? c : String(c));
  }
  return n;
}
export function fmtGiB(bytes, dp = 1) {
  if (!Number.isFinite(bytes)) return "-";
  return (bytes / GIB).toFixed(dp);
}
export function fmtMiB(bytes, dp = 0) {
  if (!Number.isFinite(bytes) || bytes === 0) return "";
  return (bytes / MIB).toFixed(dp) + " MiB";
}
// Ages are seconds at the wire; human form, no sub-minute noise.
export function fmtAge(sec) {
  if (sec == null || !Number.isFinite(sec)) return "";
  const s = Math.max(0, Math.round(sec));
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m " + (s % 60) + "s";
  const h = Math.floor(m / 60);
  if (h < 48) return h + "h " + (m % 60) + "m";
  return Math.floor(h / 24) + "d " + (h % 24) + "h";
}
export function fmtClock(d) {
  const p = (x) => String(x).padStart(2, "0");
  return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
}
export function fmtDate(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return String(iso);
  const p = (x) => String(x).padStart(2, "0");
  return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate());
}
export function pad(n, width) {
  return String(n).padStart(width, "0");
}
export function fmtNum(n) {
  if (n == null || !Number.isFinite(n)) return "";
  if (Math.abs(n) >= 1000000) return (n / 1000000).toFixed(2) + "M";
  if (Math.abs(n) >= 10000) return (n / 1000).toFixed(1) + "k";
  return String(Math.round(n));
}
// v0 schedules whole cards on one berthd, so the berth label comes from the
// scheduled GPUs the status reports.
export function berthLabel(status) {
  if (!status || !Array.isArray(status.gpus)) return "";
  const sched = status.gpus.filter((g) => g.scheduled);
  if (sched.length === 0) return "no card scheduled";
  return sched.map((g) => "gpu" + g.index).join("+");
}
// proto Priority -> ticket class
export function classWord(priority) {
  switch (priority) {
    case "foreground": return "FIRST";
    case "interactive": return "CABIN";
    case "batch": return "STEERAGE";
    default: return (priority || "UNSORTED").toUpperCase();
  }
}
// proto Lease.State -> tile tone (words live in board.js)
export function stateTone(state) {
  switch (state) {
    case "waiting": return "amber";
    case "active": return "cyan";
    case "draining": return "amber";
    case "releasing": return "dim";
    case "stuck": return "magenta";
    default: return "dim";
  }
}
// proto Event.Kind -> short word on the harbor log
export function kindWord(kind) {
  switch (kind) {
    case "lease.requested": return "REQUESTED";
    case "lease.waiting": return "WAITING";
    case "lease.granted": return "GRANTED";
    case "lease.draining": return "DRAINING";
    case "lease.released": return "RELEASED";
    case "lease.freed": return "FREED";
    case "lease.stuck": return "AGROUND";
    case "lease.refused": return "REFUSED";
    case "lease.reattached": return "REBOARDED";
    case "lease.juggle": return "JUGGLE";
    case "squatter.appeared": return "STOWAWAY";
    case "squatter.gone": return "STOWAWAY GONE";
    case "probe.failed": return "PROBE DOWN";
    case "probe.recovered": return "PROBE UP";
    case "alert.raised": return "ALERT";
    case "alert.cleared": return "ALERT CLEAR";
    default: return String(kind || "EVENT").toUpperCase();
  }
}
export function kindGroup(kind) {
  if (kind.startsWith("lease.")) return "leases";
  if (kind.startsWith("squatter.")) return "stowaways";
  if (kind.startsWith("probe.")) return "probe";
  if (kind.startsWith("alert.")) return "alerts";
  return "leases";
}
export function kindCss(kind) {
  switch (kind) {
    case "lease.granted":
    case "lease.reattached":
      return "kind-granted";
    case "lease.stuck": return "kind-stuck";
    case "lease.refused": return "kind-refused";
    case "squatter.appeared": return "kind-stowaway";
    case "probe.failed": return "kind-down";
    case "probe.recovered": return "kind-probe-up";
    case "lease.waiting": return "kind-waiting";
    case "lease.draining": return "kind-draining";
    case "alert.raised":
    case "alert.cleared":
      return "kind-alert";
    default: return "";
  }
}
// A reading is "n/a" when the driver reported -1 or the field is absent.
export function isNA(v) {
  return v == null || Number.isNaN(v) || v === -1;
}
// fmt(v) or "n/a" when the driver never reported it.
export function na(v, fmt) {
  return isNA(v) ? "n/a" : fmt(v);
}
export function fmtPct(v, dp = 0) {
  if (isNA(v)) return "n/a";
  return v.toFixed(dp) + "%";
}
export function fmtMS(v, dp = 0) {
  if (isNA(v)) return "n/a";
  if (v >= 1000) return (v / 1000).toFixed(dp == null ? 1 : Math.max(dp, 1)) + "s";
  return v.toFixed(dp) + "ms";
}
export function fmtTok(v) {
  if (isNA(v)) return "n/a";
  if (v >= 1e6) return (v / 1e6).toFixed(2) + "M";
  if (v >= 1e4) return (v / 1e3).toFixed(0) + "k";
  if (v >= 1e3) return (v / 1e3).toFixed(1) + "k";
  return String(Math.round(v));
}
export function fmtClockShort(d) {
  const p = (x) => String(x).padStart(2, "0");
  return p(d.getHours()) + ":" + p(d.getMinutes());
}
// Human size, in the unit that keeps the number below a thousand.
export function fmtBytes(b) {
  if (b == null || !Number.isFinite(b)) return "n/a";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = b;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) { v /= 1024; u++; }
  return (u === 0 ? String(Math.round(v)) : v.toFixed(1)) + " " + units[u];
}
// Short local date for chart axes: "Oct 12".
export function fmtDayShort(d) {
  const m = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  return m[d.getMonth()] + " " + d.getDate();
}
let reduced = null;
export function isReducedMotion() {
  if (reduced == null && typeof matchMedia === "function") {
    reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;
  }
  return reduced === true;
}
