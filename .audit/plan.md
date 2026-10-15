# Harbor UI overhaul: designed workflow

Run: figure-it-out. Rigor: high. The whole product surface changes, the repo's only
regression gate (test/ui/check.py) is DOM-coupled, and a live harbor plus demo fixtures
give a real artifact to measure against.

## Definition of done (Phase A predicate)

1. `test/ui/check.py` passes unchanged against the live harbor (the old gate, same
   selectors, same behavior), and `test/ui/overhaul.py` passes on demo, live and phone
   viewports.
2. `go build ./...`, `go vet ./...`, `gofmt -l .` empty, `go test ./...` green, lint
   green (AGENTS.md proof command).
3. Every new interaction is reachable by keyboard, announces itself to a screen reader
   where it changes state, and survives `prefers-reduced-motion: reduce`.
4. No new fetch, poll, or per-render allocation: the 3 s status path does no more DOM
   work than before, and the three unguarded full repaints are gone.
5. The identity stays: dark departures-board harbor, ticket/berth/stowaway/aground
   words, the existing palette family. New components look like the old ones.

## Phases

A. Frame (done): predicate, scope, blockers. Land the baseline first.
B. Design (done before code): DOM/class contract, token sheet, phase list below.
C. Build, one verifiable unit at a time, each with its own check.
D. Decision trail in .audit/harbor-ui-overhaul.tsv, a row per unit.
E. Verify whole against the predicate on the live harbor, then the PR, then unslop.

## Units, in order (each ends in a check)

1. `web/ui.js` (new): icon set, toast host, focus-trapped dialog helper, relative
   time, clipboard, shortcut manager, skeleton/empty helpers. Pure additions.
   Check: `node --check` per file, then the demo page loads with no console error.
2. `harbor.css`: token sheet (color, space, radius, shadow, motion, z), then the
   sections in place. Visual overhaul lands here. Check: overhaul gate + screenshots.
3. `index.html`: landmarks, sticky sub-nav, panel shells, new control markup.
   Check: overhaul gate structural asserts, no overflow at 390/768/1440/1920.
4. Status cluster + toasts + alert cards (harbor.js + ui.js + css).
   Check: overhaul gate, live pill behavior, aria-live present.
5. Log: search, counts, jump-to-latest, empty states (harbor.js + css).
   Check: gate filters by text and restores, count text honest.
6. Charts: keyboard focus tooltips, shared tooltip math removed from residency.js,
   per-chart aria-labels, range transition. Check: focus shows the same tip as hover.
7. Board: header sort, row state flash, accessible full-text disclosure instead of
   title-only. Check: gate sorts, titles still carry the contested bytes.
8. Ticket dialog: copy JSON, copy link, stamp animation, focus return. Check: gate.
9. Shortcuts overlay + scroll-spy sub-nav. Check: gate presses keys, dialog traps.
10. Motion pass: entrance stagger, reduced-motion static final state. Check: gate
    asserts animation counts under both motion preferences.
11. Repaint gates: timeline/machine/berth-charts behind the 10 s heavy gate plus
    signature checks. Check: gate counts history fetches and repaints.
12. Copy cleanup: sentence case, one word per state, one age format, units inline.
    Check: eyeball of rendered text in screenshots; no "undefined" or "NaN".
13. README preview screenshots refreshed.
