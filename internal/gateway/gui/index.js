// The gui plane's index: every engine in berth.toml as one row, its live
// state and one button per engine: Launch, Launching... then Open, or
// Running for an engine with no web UI.
(() => {
  const rows = [...document.querySelectorAll(".row[data-name]")];
  const gw = document.getElementById("gw");

  // Rows this tab launched: when one comes up, the tab goes to its web UI.
  const launched = new Set();
  // The last state each row showed, so a click knows which button it pressed.
  const seen = new Map();

  // On a node with several cards each row names its card, so launching the
  // copy of a model on gpu1 is how a phone picks the card.
  function showCards(list) {
    const several = new Set(list.flatMap((s) => s.cards || [])).size > 1;
    for (const s of list) {
      const chip = rows.find((r) => r.dataset.name === s.name)?.querySelector(".gpu");
      if (!chip) continue;
      chip.textContent = (s.cards || []).join(", ");
      chip.hidden = !several || !chip.textContent;
    }
  }

  function showRow(s) {
    const li = rows.find((r) => r.dataset.name === s.name);
    if (!li) return; // the engine left berth.toml while the page is open
    seen.set(s.name, s.state);
    const chip = li.querySelector(".state");
    const status = li.querySelector(".row-status");
    const detail = li.querySelector(".row-detail");
    const btn = li.querySelector(".row-btn");
    li.classList.toggle("busy", s.state === "waiting" || s.state === "starting");
    chip.textContent = s.state;
    for (const c of ["up", "offline", "waiting", "starting", "failed"]) chip.classList.remove(c);
    chip.classList.add(s.state);
    if (s.state === "offline" && !li.hasAttribute("data-launch")) {
      status.textContent = s.name + " is not started by Berth";
    } else {
      status.textContent = s.message || s.state;
    }
    detail.textContent = s.detail || li.dataset.api || "";
    if (!btn) return; // no start command: nothing to push
    switch (s.state) {
      case "up":
        if (!li.dataset.gui) {
          btn.textContent = "Running";
          btn.disabled = true;
          break;
        }
        if (launched.has(s.name)) {
          location.replace(li.dataset.gui); // this tab launched it: go there
          return;
        }
        btn.textContent = "Open";
        btn.disabled = false;
        break;
      case "waiting":
      case "starting":
        btn.textContent = "Launching...";
        btn.disabled = true;
        break;
      default:
        btn.textContent = "Launch " + s.name;
        btn.disabled = false;
    }
  }

  for (const li of rows) {
    const btn = li.querySelector(".row-btn");
    if (!btn) continue;
    btn.addEventListener("click", async () => {
      if (seen.get(li.dataset.name) === "up") {
        location.replace(li.dataset.gui); // already live: the tab goes there
        return;
      }
      btn.disabled = true;
      btn.textContent = "Launching...";
      launched.add(li.dataset.name);
      li.querySelector(".row-status").textContent = "Asking berth for the card ...";
      try {
        const r = await fetch("/gui/" + encodeURIComponent(li.dataset.name) + "/wake", { method: "POST" });
        // The answer names no row (it is the engine's state); this tab knows which.
        const body = await r.json();
        if (r.ok) {
          showRow({ name: li.dataset.name, ...body });
        } else {
          launched.delete(li.dataset.name);
          showRow({ name: li.dataset.name, state: "failed", message: li.dataset.name + " did not start", detail: body.error?.message || "" });
        }
      } catch {
        // The gateway is not answering; the line under the heading says it.
      }
    });
  }

  async function poll() {
    let list;
    try {
      const r = await fetch("/gui/state", { cache: "no-store" });
      list = await r.json();
      gw.hidden = true;
      for (const s of list) showRow(s);
      showCards(list);
    } catch {
      gw.hidden = false;
      setTimeout(poll, 2000);
      return;
    }
    // Offline is polled slowly so the card stays current; a start fast.
    const busy = list.some((s) => s.state === "waiting" || s.state === "starting");
    setTimeout(poll, busy ? 1000 : 3000);
  }

  poll();
})();
