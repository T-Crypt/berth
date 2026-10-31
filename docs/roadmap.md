# Roadmap

| Phase | What it delivers | Status |
|---|---|---|
| P0 | Safety core: identity, measured release, stuck and contested detection, unprotected runs | **Done** |
| P1 | Node API: event log, collisions board, config, engine readers, `/metrics`, light probe | **Done** |
| P2 | Harbor web UI; native telemetry, request KPIs, ledger, leaderboard, stored history, measured against Speculum parity | **Done** |
| P3 | `berth hold` and claims for games and apps (**done**); drain and hold from the harbor, behind authentication (planned) | Partly done |
| P3.5 | Node gateway: one OpenAI-compatible port; engines start on demand under a lease and drain each other | **Preview** |
| P4 | Restart reconciliation: engines re-attach after a daemon restart, nothing left unprotected | **Done** |
| P5 | Fleet: `berth install --harbor <addr> --join <token>` on every machine; nodes push metrics into the harbor's store (already built for one box in P2) | Planned |
| P5.5 | Harbor router: one URL serves models from any node in the fleet | Planned |
| P6 | ComfyUI job-boundary handoff on Linux (**done**); ComfyUI started on demand through the gateway (**done**); Windows node (**experimental**) | Partly done |
| P7 | Offers: a waiting request can offer for an idle card on another machine; the holder decides | Planned |
| P8 | Multi-GPU scheduling: a lease table per card, "any card that fits" | Started: one berthd for every card, an engine on one card or a lease on several (experimental); per-card gateway URLs in v0.5 |
| P9 | Organisations: single sign-on, role-based access, audit log, Postgres, regional federation; a mobile app after | Planned |
| P10 | Opt-in auto-yield: a card's owner can pre-approve offers for their own card | Later |

## Shipped: v0.3, fast swap and proof (2026-10-20)

v0.3.0 went after the cost you feel most, a reload from disk on every switch, and after proof
that Berth holds up when an engine misbehaves. It shipped park in RAM with adapters for ComfyUI,
vLLM and SGLang and an open [park protocol](https://github.com/T-Crypt/berth/blob/main/contrib/PARK-PROTOCOL.md); `berth stop`, `berth resume`
and `berth reclaim`; engines in Docker and Podman; a place and an estimate in every wait reason;
`berth update` and `berth service install`; the page-cache CACHED column and `berth prefetch`;
per-process VRAM on Windows and the test suite on Windows in CI; and the proof kit:
[contrib/chaos](https://github.com/T-Crypt/berth/blob/main/contrib/chaos/README.md), [contrib/bench/swap](https://github.com/T-Crypt/berth/tree/main/contrib/bench/swap), the drain report
form and the [tested hardware](benchmarks.md#tested-hardware) table. v0.3.1 fixed `berth update` answering from
a stale cache. The [CHANGELOG](https://github.com/T-Crypt/berth/blob/main/CHANGELOG.md) has every entry.

## Shipped since: v0.3.6 (2026-10-23)

v0.3.2 and v0.3.3 carried the first Windows testers' reports: the Epic launcher no longer holds the
card from the tray ([#81](https://github.com/T-Crypt/berth/issues/81)), per-process VRAM counts only
the governed card ([#82](https://github.com/T-Crypt/berth/issues/82)), the squatter total stops at
the memory the driver reports in use ([#103](https://github.com/T-Crypt/berth/issues/103)), and a
lease whose `berth run` process dies after attach frees itself
([#104](https://github.com/T-Crypt/berth/issues/104)). The gateway warms the next engine's files
during a drain and serves measured switch costs at `/v1/berth/cost` and `/v1/berth/plan`.

v0.3.4 gives the gateway a keyed API for the card: `/v1/berth/switch`, `/v1/berth/stop` and
`/v1/berth/board`, plus fallback routing to an engine that needs no card. `berth pair` pairs a phone
or browser with the harbor, the groundwork for switching engines from it. Linux AMD support and
`berth vfio` arrive as experimental. On Windows, desktop apps and dwm.exe leave the stowaway list
([#149](https://github.com/T-Crypt/berth/issues/149)) and the CPU meter reads
([#139](https://github.com/T-Crypt/berth/issues/139)). Engine rates hold through a llama.cpp
generation ([#147](https://github.com/T-Crypt/berth/issues/147)), and the `chat` shelf is now `text`
([#148](https://github.com/T-Crypt/berth/issues/148)).

v0.3.5 lets a box with two NVIDIA cards pass one to a VM while the other keeps serving: berthd follows
its card by PCI address, starts while that card is in the VM, and `berth vfio run` ignores engines on
the other card. Hermes Agent gets a `berth skill install` target and a [setup page](hermes.md), and the
skill teaches agents to run workflows across engines on one card. On Windows, logon tasks start without
a console window ([#165](https://github.com/T-Crypt/berth/issues/165)) and `berth service status` reads
them right ([#166](https://github.com/T-Crypt/berth/issues/166)).

v0.3.6 keeps a parked ComfyUI alive when a client renders on its own port and across a berthd restart
([#179](https://github.com/T-Crypt/berth/issues/179)). The gateway serves ComfyUI's port behind Berth, and
ComfyUI jobs sent while the card is in a VM wait on disk and run when it comes back. `berth notify` brings
desktop notifications on Linux, `berth skill install` adds a ComfyUI skill, and background engines run when
the card is free ([#172](https://github.com/T-Crypt/berth/issues/172)). [Swap numbers](benchmarks.md#swap-numbers)
now has page cache, park and `cuda-checkpoint` timings from the RTX 4090.

## Shipped: v0.4, many cards and parks for any engine (2026-10-29)

v0.4.0 puts a whole machine under one berthd. Testers ran it first as pre-releases (`v0.4.0-rc.1` to `rc.8`, see
[Testing a pre-release](prerelease.md)).

- **Many cards.** berthd and `berth service install` govern every card by default (`--gpu all`).
  `berth run --gpu gpu1` puts an engine on one card, `--gpu gpu0,gpu1` holds one lease on several, and
  `berth run --pinned` keeps a training run's cards ([Training runs](leasing.md#training-runs)). The gateway,
  `berth status`, `berth events`, `berth notify`, `/metrics` and the harbor all name the card. One card can go
  to a VM while the others keep serving. A lease on several cards cannot park or lend yet.
- **Parks for any engine.** Engines with no offload call of their own, llama.cpp and NInfer included, park
  in RAM through `cuda-checkpoint` behind `berth run --front`. A stop kills a parked checkpointed engine at once.
  [Park in RAM](gateway.md#park-in-ram-warm-switch) has the setup and the numbers.
- **Engines that need the whole machine.** `whole_machine = true` makes berthd stop what it drains and evict
  parked engines before the grant. Strata is the case it was written for.
- **Lends (experimental).** An engine that can shrink a cache gives part of its card to a sized waiter and keeps
  serving. [Lending part of a card](gateway.md#lending-part-of-a-card) states the rules, and
  [LEND-PROTOCOL.md](https://github.com/T-Crypt/berth/blob/main/contrib/LEND-PROTOCOL.md) is the contract for
  engines.
- **More engines under a lease.** A Docker Compose project runs as one engine, container engines work on
  Windows, and `berth run` warns about a restart policy that brings a container back without a lease.
- **Better readings.** Strata's requests reach the token ledger and the leaderboard under the model its
  `/health` names, a GPU process in a Proxmox LXC container shows as a stowaway named by its container, and
  the harbor and berthd use less CPU and bandwidth.
- **Less wiring by hand.** `berth wire llama-swap` takes `--wait`, keeps the flags you added with `--update`
  and adds `--drain-slots` so a drain waits for the request in flight. `berth run --as NAME` with no command
  runs the entry's `start`, and `[squatters] allow` in `berth.toml` replaces a hand-written `--allow`.
- **Windows.** `berth service install` and `uninstall` end the running tasks, closing the console or logging
  off drains `berth run`, and `berth wire` reads Windows paths.

The [CHANGELOG](https://github.com/T-Crypt/berth/blob/main/CHANGELOG.md) has every entry.

## On `next`, for v0.4.1

Merged to the `next` branch and in no release yet. Testers get them in a `v0.4.1-rc.N` pre-release first
([Testing a pre-release](prerelease.md)).

- **The card for holds, VFIO runs and claims.** `berth hold --gpu`, `berth vfio run --gpu` and `gpu = ...` on a
  `[[claim]]` take only the cards they name on a node with several, so the other cards keep serving.
- **Engines under a systemd slice.** `slice = "NAME.slice"` on an engine entry, or `berth run --slice`, puts the
  engine's scope under that slice's CPU, memory and swap policy on Linux.
- **The harbor.** A CLAIMS panel on the card tab, a rewritten About tab, and a header that no longer clips.
- **Status after a stop.** An engine Berth stopped shows as offline, not unresponsive.

## Next: v0.5, per-card gateway endpoints

v0.5 lets a client choose the card on a node with several. Each slice ships on its own, in this order:

1. **One base URL per card**, merged in [#278](https://github.com/T-Crypt/berth/pull/278). `/cards/CARD/v1` reaches only
   the engines on that card, and one model can be served on several cards, one engine entry per card.
2. **Placement for a plain `/v1` request**, merged in [#280](https://github.com/T-Crypt/berth/pull/280). When
   several cards serve the model, the gateway picks one: the card where it already runs, then one where it is
   parked, then a free card, then one whose holder drains, and last a card a VM, a game or a cordon keeps.
3. **Cordon a card.** `berth cordon gpu1` stops new grants on the card, holds aside, until `berth uncordon`, so it
   can be drained for maintenance or a VM while the others serve.
4. **A card picker on the board.** The harbor and `GET /v1/berth/board` name the card each engine can run on.

## Also queued

Each item has its building blocks merged or a spec written:

- Measured switch costs on the live daemon.
- Models started on demand for a standalone llama-server.
- Parked resume numbers for vLLM and SGLang in [Swap numbers](benchmarks.md#swap-numbers), which take
  their park adapters out of experimental.
- `berth notify` on Windows: a tray icon with native toasts ([#186](https://github.com/T-Crypt/berth/pull/186)),
  after a test on a real Windows desktop.
- VFIO on a box with two NVIDIA cards: keep the other card's berthd from opening the card being
  switched, after a measurement on a real two-card box.
- Hermes Agent: an overnight recipe in the setup page, built on the gateway workflow test, where Hermes
  hands work to other engines and ComfyUI renders on the same card.

Not started, and in no release yet: `berth fit` and scenes, so engines that fit together never swap.

## Later

| Release | What it brings |
|---|---|
| next | Parks and lends on a lease that holds several cards, one share per card ([Design](design.md)) |
| next | Broader AMD Linux hardware validation beyond the experimental RDNA3/RDNA4 probe; the Windows node out of experimental (a packaged installer, a locked-down control pipe) |
| next | Proxmox in every common layout. Berth detects whether it runs on a Proxmox host, in an LXC container or in a VM and says where it belongs (`berth env`). Engines in LXC containers lease the host's card, and a system service covers hosts without a user session |
| later | Cards you name yourself, and "any card that fits" for leases outside the gateway |
| v0.6 | The fleet: Berth on every machine, one harbor, one URL for every model |
| later | Offers: a waiting request can ask for an idle card on another machine, and its holder decides |

On hold: engine installers beyond the ComfyUI suite, the migration manager, and Berth swapping
models itself. llama-swap already swaps models well. Berth works beside it, and a proposal to
llama-swap for generic load hooks is planned, so Berth needs no wrapper line.
