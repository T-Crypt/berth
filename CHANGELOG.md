# Changelog

Every notable change to Berth is recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); each tagged version
has a matching GitHub release whose body comes from its section below.

## [Unreleased]

### Added

- `berth notify` shows berthd's events as desktop notifications, so a refused request, a stuck lease, a stowaway
  or a parked engine loaded behind Berth's back shows up without the harbor open. `[notify] level = "info"` adds
  every switch, drain, park and resume; `[notify] enabled = false` turns it off. `berth service install --notify`
  runs it as a user unit. Linux shows them through `notify-send`.
- On Windows, `berth notify` is a tray icon in the taskbar. Its notifications are Windows' own toasts, and a click
  opens the harbor. The menu opens the harbor, turns the toasts off and on, lists the last 10 events and quits.
  `berth service install --notify` starts it at logon for the current user.
- `berth gateway --comfyui-listen ADDR` serves ComfyUI's own port behind Berth: the web UI, API, websocket and
  custom node routes pass through with no key, and a queued workflow (`/prompt` or the web UI's `/api/prompt`)
  first wakes or starts ComfyUI under its lease as a person's switch. A client or a LAN forwarder pointed there
  never sends work to a parked ComfyUI that has no card ([#179](https://github.com/T-Crypt/berth/issues/179)).
- ComfyUI jobs wait for the card on disk: with `on_busy = "queue"`, a `/prompt` sent while the card is in a VM or
  held by a game, a hold or a pinned app is answered at once with a `berth-` job id and submitted once the card is
  free. `/history/<berth id>` reads the result. The queue survives a gateway restart.
- Background engines: `background = true` on an engine makes the gateway run it at `batch` whenever the card has
  been free for `idle_after` (default 60 s), with no lease, waiter, stowaway or running claimed app on it. Any other
  request drains it, and it stays out until the card is free again, so a game or a hold keeps it out. A tagger or an
  indexer in Docker uses `start = ["docker", "start", "-a", NAME]` and `container = NAME`
  ([#172](https://github.com/T-Crypt/berth/issues/172)).

### Fixed

- A parked engine survives a berthd restart. Its run re-attached as active while the run itself still knew it was
  parked, so the next request drained it and the run stopped it. The run now tells the new daemon it is parked, and
  its lease comes back parked: a waiter goes in beside it and a resume wakes it. Its residual counts as its own
  again instead of showing as a squatter.
- A parked engine that a client wakes directly is no longer killed by the next drain
  ([#179](https://github.com/T-Crypt/berth/issues/179)). A ComfyUI `/prompt` sent to :8188 while ComfyUI was parked
  loaded its models back onto the card without telling Berth, so the next request saw an oversized parked residual,
  evicted it, and the unit exited 143 with no reason in its log. berthd now records the load as `lease.unparked`,
  treats the lease as active again, and the next drain parks it again. A parked engine that Berth does stop (ttl,
  RAM floor, pressure) logs `stopping parked ENGINE instead of parking it again: WHY` first.
- A person's switch wins: Launch on the wake page and `/v1/berth/switch` take the card from an equal-priority
  holder whatever the juggle ranking says, and a request no person made (a chat client's background call) does
  not take it back inside the juggle window. Before, Launch of ComfyUI lost to llama-swap whenever
  `[priority] order` ranked llama-swap first, and a later Launch inherited that loss and waited behind an idle
  llama-swap.
- The built-in `steam-game` claim no longer drains engines when the REAPER DAW runs. Steam's launcher and the
  DAW are both named `reaper`, and the rule matched any child of either, such as GTK's sandboxed image loader
  (`bwrap`, `glycin-image-rs`). The rule now also needs the launcher's command line to carry `SteamLaunch`, which
  Steam passes on every game launch. A `[[claim]]` can do the same with the new `parent_cmdline`
  ([#171](https://github.com/T-Crypt/berth/issues/171)).
- A parked engine that holds the whole card (ComfyUI with `keep_comfyui_up`) comes back when asked. Its resume
  waited on its own parked residual ("waiting for comfyui [waiting] to free memory") until something released it.
  A resume released while it waited (the gateway gave up on the wake) could then be granted anyway, as a lease with
  no process behind it. Two leases of one engine no longer count as a juggle ("comfyui keeps the card over
  comfyui").

## [v0.3.5] - 2026-10-22

A box with two NVIDIA cards can pass one card to a VM while the other keeps serving: berthd follows its
card by PCI address (`--gpu 0000:02:00.0`), starts while that card is in the VM, and `berth vfio run` no
longer counts engines on the other card as holders. Multi-GPU stays experimental, one berthd per card.
Hermes Agent joins the agents `berth skill install` covers, with a setup page, and the skill now teaches
any agent to run workflows across engines on one card. On Windows, logon tasks start without a console
window and `berth service status` reads them right, both reported by @azrael-229. Run `berth service
install` again on Windows to switch existing tasks.

### Added

- The Berth skill teaches an agent to run workflows across engines through the gateway: hand a task to
  another model by name, give the card back with `/v1/berth/stop` when that part is done, render with
  ComfyUI and collect the result from `/history`, and run unattended work at `batch`. A gateway test runs
  that loop (an agent's model, strata, ComfyUI, the agent's model again) and checks three clean handoffs.
- `berth skill install --target hermes` installs the Berth skill for Hermes Agent, in
  `$HERMES_HOME/skills/mlops/berth/` (`~/.hermes` when `HERMES_HOME` is unset). `--target all` includes
  Hermes when its home exists, and never creates it. [Hermes Agent](https://github.com/T-Crypt/berth/blob/main/docs/hermes.md) shows Hermes's
  `config.yaml` for the gateway and why its auxiliary tasks should stay on the main model.
- `berth daemon --gpu` and `berth service install --gpu` take an NVIDIA card's PCI address
  (`--gpu 0000:02:00.0`) as well as an index. berthd follows that card from the first reading, and it
  starts while the card is bound to `vfio-pci`: it shows the card as passed through and governs it once
  the card is back. With an index, berthd still refuses to start without its card.

### Fixed

- On Windows, the logon tasks `berth service install` creates start without a console window. Each
  task runs a small wscript launcher in the state directory (`units\<unit>.vbs`), which starts berth
  hidden with its output on the unit's log. Run `berth service install` again to switch existing tasks
  ([#165](https://github.com/T-Crypt/berth/issues/165), reported by @azrael-229).
- On Windows, `berth service status` shows a running task as running. It read the next run time, which
  is N/A for a logon task, instead of the status ([#166](https://github.com/T-Crypt/berth/issues/166),
  reported by @azrael-229).
- A berthd that starts with its card already in a VM names the card by its PCI address in `berth status`
  and in what waiting requests read. Before, both said gpu0, which on a two-card box can be the other card.
- On a box with two NVIDIA cards, `berth vfio run` no longer refuses because an engine on the other card
  is running. The holder check counts the switched card's own `/dev/nvidiaN` and DRM nodes, and counts
  the shared `/dev/nvidiactl` and `/dev/nvidia-uvm` only for a process that holds no other card. An
  engine started without `CUDA_VISIBLE_DEVICES` opens both cards, so it is still named, and the refusal
  says how to pin it.
- On a box with more than one NVIDIA card, berthd keeps governing its own card when another card is passed
  through to a VM. nvidia-smi renumbers the cards that stay, so `--gpu N` now picks the card on the first reading
  only, and from then on berthd follows that card's PCI address. Before, the daemon for the card left on the host
  read a frozen copy of its old index, and the daemon for the card in the VM could read the other card as its own.

## [v0.3.4] - 2026-10-22

The gateway can now switch the card and give it back without a prompt (`/v1/berth/switch`,
`/v1/berth/stop`), read the whole node in one call (`/v1/berth/board`), and fall back to an engine
that needs no card while the primary starts. `berth pair` pairs a phone or browser with the harbor.
Two experimental additions: Linux AMD support, contributed by @jenerestain, and passing the card to
a VM with `berth vfio`. Windows testers' reports fixed the stowaway list, the CPU meter and engine
rates. One upgrade note: the gateway no longer forwards the node's API key to engines (see Security).

### Added

- Gateway: `POST /v1/berth/switch` and `POST /v1/berth/stop` switch the card to an engine or give it back without
  sending a prompt. Stop never touches a pinned holder. The `/gui/` board's Launch now uses the same switch.
- `berth pair` pairs a phone or browser with the harbor: a one-time link and its QR code, a
  **Pair this device** page, and a device cookie for a year. `--list`, `--revoke ID` and `--revoke-all`
  manage devices. berthd keeps only hashes. Pairing is the groundwork for switching engines from the harbor.
- Gateway: `GET /v1/berth/board` reads the node in one call: the card and who holds it, every engine's state and
  actions, and each engine's models with which are loaded.
- Linux AMD support (experimental), contributed by @jenerestain. `berth daemon --gpu-backend amd`
  governs a discrete RDNA3 or RDNA4 card, or a Phoenix APU (Radeon 780M), read from sysfs and DRM
  `fdinfo` with no ROCm. `--gpu N` names DRM cardN. On an APU, reserved VRAM and GTT (shared host
  RAM) are tracked apart, only whole-card leases are granted, and parking is refused. `auto` keeps
  NVIDIA whenever `nvidia-smi` is installed, so existing NVIDIA setups are unchanged.
- Card passed through to a VM (experimental). While the governed card is bound to `vfio-pci`,
  `berth status` and its JSON show it as passed through to a VM, and waiters are told why nothing is
  granted. While a VFIO hold is granted, berthd does not open the card, so a driver switch can run.
  `card.passthrough` and `card.returned` events mark each change.
- The harbor shows a card passed through to a VM (experimental): a GPU IN VM tag on every tab, the
  card's panel and the departures board marked, the change named in the log, and the session left as a
  gap in the tide.
- `berth vfio status` and `berth vfio run` (experimental, Linux only) for passing the governed card to
  a VM. `status` shows IOMMU, the card's IOMMU group with each function's driver, and the processes
  holding the card. `run -- CMD` drains the card, stops berthd's own probe for the hold, refuses while
  any process holds it, runs your own switch script with `BERTH_VFIO_DEVICES` set, and releases the
  hold only when the card is back on its host driver. `--before` and `--after` run your own commands
  around the session, for example to stop a service that holds the card and start it again once the
  card is back. Berth never switches drivers or asks for passwords. See docs/vfio.md; `contrib/vfio`
  has an example launcher and the libvirt device entries.
- Gateway fallback routing: an `[[engine]]` may name a `fallback`, another engine in the file that
  needs no card (a CPU llama-server, an Ollama on another box), and `fallback_after` (10 s by
  default). A request that has waited that long for its own engine is proxied to the fallback when
  the fallback answers its probe at that moment, with the body's model renamed when `fallback_model`
  is set. A non-streaming answer carries `X-Berth-Fallback: <fallback>`; a streaming one, whose SSE
  preamble was already sent, gets the fallback's stream after one `: berth: fallback <fallback>`
  comment. The gateway never starts, leases or drains the fallback, and the primary's start is never
  cancelled, so later requests land on it. The fallback never receives the node's API key, only its
  own key from its entry when it has one. One hop: `berth config check` refuses chains.
- Cgroup attribution closes the one known stowaway window (chaos scenario 3: a child
  whose parent exits within one 2 s tick). On Linux with a systemd user manager,
  `berth run` execs the engine in its own transient cgroup scope (`systemd-run --user
  --scope`, unit `berth-run-<name>-<rand>`), which every child inherits on fork: a
  child that escapes its process group and reparents stays the lease's, the lease
  turns stuck naming it, `berth reclaim` lists it, and a whole-card request is never
  granted `contested` over it. A GPU process in a scope no live lease recorded is a
  stowaway as before, and a process outside every scope is never attributed to a
  scopeless lease. Without a user manager, inside a `Type=notify` service (where
  systemd only accepts readiness from the service's own cgroup), and on Windows, the run
  starts as before and the window stands.
- `kv_snapshots` and `kv_snapshots_mib` on an `[[engine]]` entry: the drain saves an idle slot's
  prompt cache into the engine's `--slot-save-path` directory once the boundary is reached, keeps
  the directory within its budget (16 GiB by default, above the library free floor), and the
  gateway restores the newest snapshot of the engine's model into its empty slots before the first
  request, so a sequence of agents skips the prefill after each switch. The snapshots are keyed by
  the model the engine itself names on `/v1/models`, so an engine that lists none or many
  (llama-swap) never snapshots, and nothing crosses models. The restore also warms the files into
  the page cache during the start's wait: cached, never reserved. A save is bounded at 60 s and a
  restore at 30 s, so whatever goes wrong along the way costs one line and the switch goes on as
  before. A snapshot holds the conversation's tokens: the ledger is owner-only, and the README asks
  for a private directory.
- `park_ttl` on an `[[engine]]` entry sets how long that engine stays parked before Berth stops it,
  in place of `[park] ttl`. With `keep_comfyui_up = true` and a short `park_ttl`, a ComfyUI node that
  calls another engine (a chat panel calling llama-swap) gets its answer. The drain leaves ComfyUI
  up, and Berth stops the server once the ttl passes
  ([#121](https://github.com/T-Crypt/berth/issues/121)).

### Changed

- The `chat` shelf is now called `text`: the shelf names the kind of engine that loads a model, and
  embedders, rerankers and vision or OCR models sit there too. `berth models`, its JSON and the
  harbor say `text`. `shelf = "chat"` in berth.toml, `--shelf chat` and catalogs from older
  machines still read as `text`. Asked by @azrael-229
  ([#148](https://github.com/T-Crypt/berth/issues/148)).
- The README keeps the overview, quick start and screenshots, and the reference moved to pages under
  `docs/`: install, engines, leasing, models, gateway, the harbor, interfaces, benchmarks, roadmap and
  FAQ. The README's old Tested hardware, Swap numbers and Roadmap headings link to their new pages.

### Security

- The gateway no longer forwards the node's API key to engines. A proxied request, fallback and
  ComfyUI routes included, carries the engine's own key when its entry has one (`api_key_env` or
  `api_key_file`) and no `Authorization` when it has none. If an engine of yours checked the node's
  key, give it its own key in its `berth.toml` entry.

### Fixed

- Windows: the host's CPU is read (GetSystemTimes), so the harbor's machine meter and `berth status` show real CPU
  use instead of 0%. Windows has no load averages: the node now leaves them out of its JSON and the harbor and
  `berth status` say n/a instead of three zeros. Reported by @azrael-229 (#139).
- One empty `models--*` husk no longer makes a whole store read as an HF cache with no models,
  reported by @azrael-229 (#138). A root is an HF cache only when a `models--*` directory holds
  `snapshots`, so an abandoned download beside plain models keeps them listed.
- The gateway's `/gui/` board launches an engine that has a `start` command but no `gui` URL (Strata, a
  llama-server). Its Launch button answered 404 and stayed on "Launching..."; it now starts the engine under a
  lease and shows Running once it answers. A refused start shows its reason instead of hanging.
- `berth vfio status ADDR` asks berthd for its view instead of calling it unreachable, and while the
  card is in a VM it says so instead of printing the last memory reading as current.
- The wake page and start errors show an engine's last log line without terminal color codes
  (`[32m[INFO][0m`), and a progress bar's line as its last redraw.
- Windows: a process the GPU counters report but nvidia-smi does not list, such as dwm.exe
  holding the card's memory, now shows its name instead of a blank one.
- The harbor engine cards and `berth status` draw an unreported rate as unknown, not zero. An
  engine whose counters endpoint never answered (a llama-swap whose llama-server starts without
  `--metrics`) shows n/a for decode, prefill and the token totals, and the ENGINE table prints a
  dash. Reported by @azrael-229 ([#140](https://github.com/T-Crypt/berth/issues/140)).
- Windows: desktop apps no longer fill the stowaway list. nvidia-smi lists every graphics client with
  no memory figure, so explorer, Chrome, OneDrive and 30 more showed as 0.0 GiB stowaways. A listed
  process now needs the same 64 MiB in the GPU counters as an unlisted one. dwm.exe, whose counter
  reports a committed reservation of 11 to 14 GiB, counts as the desktop baseline, as Xorg does on
  Linux, so DOCKED and STOWAWAY no longer add up past the card. Reported by @azrael-229
  ([#149](https://github.com/T-Crypt/berth/issues/149)).
- An engine card no longer reads 0.0 t/s for most of a llama.cpp generation. llama.cpp adds a
  request's tokens to its counters only when the request ends, so the decode and prefill rates
  sat at 0 and spiked once per request. While a request runs, the card now shows the rate the
  last finished request measured; an idle engine still reads 0. Reported by @azrael-229
  ([#147](https://github.com/T-Crypt/berth/issues/147)).

## [v0.3.3] - 2026-10-21

Fixes from the Windows testers and two gateway additions. On a single-GPU Windows desktop, the
squatter total no longer passes the card's used memory, so `berth run` gets its grant while a model
is loaded. A lease whose `berth run` process dies after attach now frees itself, and the Windows check
kit prints its per-process rows and events again. The gateway reads the next engine's files into the
page cache while the holder drains, and it measures every start it runs and prices a switch, or a
chain of them, from those times.

### Added

- Measured switch costs on the gateway: it times every start it runs, from the lease
  grant to the engine answering, and keeps the last 20 times per engine and tier in
  `~/.local/state/berth/swapcost.json`. `GET /v1/berth/cost?engine=NAME` answers one
  estimate per tier from those times (an estimate with `known:false` has no samples
  behind it), and `POST /v1/berth/plan` prices a declared chain: the per-step estimate, a
  lower-bound total with its unmeasured count, the engine to prefetch at each step, and
  the engines the chain comes back to. A failed start records nothing, and every route
  takes the node's key like the rest of the gateway.
- `prefetch` on an `[[engine]]` entry: absolute file and directory paths the gateway asks the
  kernel to read into the page cache the moment it decides to start that engine, so the disk
  reads overlap the drain of the engine it replaces (when the machine has the RAM to keep them). A directory means its own files,
  not its subdirectories. The ask commits no RAM, since the kernel takes the pages back when the
  machine needs them, and it never delays or fails a start: a file that cannot be asked for is
  one WARN line. A relative path fails `berth config check`, and `berth prefetch MODEL` makes the
  same ask by hand.

### Fixed

- The Windows check kit (`contrib/windows/check.ps1`): the per-process rows render again, the label and
  the MiB figure computed before the row list, and the status reads the keys it actually has
  (`squatters`, `unprotected`, `leases`; there is no `holders`). `berth events` is called with
  `--format json`, and a berth that answers nothing leaves its stderr in the report instead of
  a silent "returned nothing".
- The squatter sum is bounded by the memory the driver reports the card as
  using: on Windows a process's reading counts its committed reservation, so
  while the v0.3.2 per-process cap bounds one process, the sum could still
  exceed the card's real usage (the desktop compositor read as 11 GB on a
  16 GB card the driver reported at 15.5 GB in use) and a `berth run` request
  that fit the free space the driver actually shows was refused forever.
  Admission, the contested-grant figure and the "squatters hold N MiB" wait
  reason now count the bounded total, and the per-squatter status and event
  entries keep the raw readings. While it clamps, the bound logs at most one
  INFO line per change of that state, never per tick.
  [#103](https://github.com/T-Crypt/berth/issues/103)
- A lease whose attached run process dies without the wrapper's disconnect
  reaching the daemon (the run's tree force-killed, a crash, a power loss)
  is no longer left granted with an empty PIDS list: the broker now checks
  the attached process itself on every tick, and after it has been seen
  gone on two consecutive ticks it releases the lease exactly as a
  disconnect would, once the card shows nothing of the run left. A PID the
  kernel reused counts as gone, a lease that names a container or still
  counts a process on the card is left alone, and nothing is signalled.
  The event log gets `lease.orphaned`, then `lease.released` and
  `lease.freed` ([#104](https://github.com/T-Crypt/berth/issues/104)).

## [v0.3.2] - 2026-10-20

Fixes from the first Windows testers, and four contributions. On Windows, the Epic Games
launcher no longer holds the card from the tray, per-process VRAM counts only the governed card,
and `berth service install` starts its tasks and writes their logs. Engines get a minimum turn,
drain and park requests carry the engine's key, the key can live in an owner-only file,
`--fail-closed` now covers drain boundaries, and a ComfyUI session can hand the card back when
its work is done. The harbor gets a Home Screen icon.

### Added

- `min_turn` on an `[[engine]]` entry and `berth run --min-turn`: a minimum turn. For that
  long after the grant the engine keeps the card: a request of equal, lower or higher
  priority cannot drain it, only a hold can. A blocked waiter is told how much of the turn is
  left, and `berth status` shows it in the TURN column. When the turn ends the normal rules
  apply; a re-attach after a daemon restart starts a fresh turn.
- `[harbor] listen` and `[gateway] listen` in berth.toml, read by `berth harbor` and
  `berth gateway` when their `--listen` flags are not given, and validated by
  `berth config check`: the gateway key is loopback only, the harbor key takes any
  host:port, a LAN address being the explicit opt-in it already is. `berth service install`
  no longer writes a hardcoded `--listen` into the units and tasks, so a port change needs
  a restart, not a reinstall (#89).
- On Windows, `berth service install` starts each task it creates with `schtasks /Run` and
  reports it, keeps a task it replaces whose XML differs as a .bak in the state directory
  like the systemd path keeps a differing unit, and each task appends its output to
  `<state>\logs\berthd.log` and its siblings, which `berth service status` prints (#89).
- `api_key_file` on an `[[engine]]` entry and `berth run --api-key-file`: the engine's key in
  an absolute file only its owner can read, as an alternative to `api_key_env`. Symlinks and
  FIFOs are refused, the two sources are exclusive, and on Windows the file form is refused
  with a message. [#85](https://github.com/T-Crypt/berth/pull/85)
- `--fail-closed` covers drain boundaries too: when a slot list or a ComfyUI queue cannot be
  read, or `drain_max` passes, the drain keeps the engine and its lease instead of stopping it,
  and the next drain tries again. Without the flag nothing changes.
  [#86](https://github.com/T-Crypt/berth/pull/86)
- `exit_comfyui_idle` and `exit_comfyui_idle_after` (and their `berth run` flags): a ComfyUI
  run hands the card back once the session's new work is done, or after an interval with no
  job. Off by default. [#87](https://github.com/T-Crypt/berth/pull/87)
- The harbor's iOS Home Screen icon: Pilot, the harbor master llama, with `berth` as its
  title. [#93](https://github.com/T-Crypt/berth/pull/93)

### Fixed

- Drain, park, resume and readiness requests carry the engine's `api_key_env` key, so an
  engine that requires it no longer refuses the slot check and unload of its own drain.
  [#84](https://github.com/T-Crypt/berth/pull/84)
- The built-in `epic-game` claim no longer holds the card while only Epic's background services
  run. Its exclusion named `EpicOnlineServices`, but the folder on disk is `Epic Online Services`,
  so the overlay and helper processes matched as a running game and every engine waited behind
  them. [#81](https://github.com/T-Crypt/berth/issues/81)
- On Windows, per-process VRAM now counts only the card Berth governs and cannot exceed it
  ([#82](https://github.com/T-Crypt/berth/issues/82)). The `Dedicated Usage` counter PDH reads reports a process's committed reservation,
  which a driver can report many times above the card's real memory, and on a multi-GPU
  machine it mixes every adapter together, so one process could read as holding more VRAM than
  the card holds while the other card's memory landed on the governed one. Berth now keeps
  only the instances on the governed card's adapter, found by matching its `pci.bus_id`
  against the adapters D3DKMT lists, and caps each process's figure at the card's total. A
  capped reading is logged at most once per process per minute, and if the card's adapter
  cannot be pinned to one LUID Berth counts every adapter and says so once.

## [v0.3.1] - 2026-10-20

### Fixed

- `berth update` and `berth update --check` always ask GitHub; before, a "no newer release" the
  harbor had kept for up to a day could answer them right after a release.
- A pre-release or pseudo-version build (`0.3.0-rc`, `0.2.1-0.2026...`) now sees its release as
  newer; a pre-release is still never offered as an update.

## [v0.3.0] - 2026-10-20

Fast swap and proof. Engines that can offload themselves park in RAM on a drain and come back
without a reload from disk, under the same lease rules: Berth admits a park only when measured RAM
covers it and evicts parked engines first when memory runs short. Leases free only after a card
read that came after the release, a gateway restart keeps the engines it started, and streaming
requests stay alive while an engine loads. `berth reclaim`, `berth stop` and `berth resume` give
the operator a way out of every lease state. Engines in Docker or Podman belong to their lease.
The failure scenarios in `contrib/chaos` run on any card, and Windows runs the full test suite in
CI.

Known gaps, for v0.3.x: a child that escapes its wrapper and whose parent exits within one 2 s
tick still shows as a report-only stowaway (cgroup attribution will close it); `cuda-checkpoint`
parking is not measured yet; the vLLM and SGLang park adapters are experimental until measured;
KV snapshots, swap-cost estimates on the live daemon, gateway fallback routing and JIT models for
a standalone llama-server have their building blocks merged and are wired in v0.3.x.

### Added

- The Berth park protocol: three endpoints an engine can serve on its own
  HTTP API to be parkable without a Berth-side adapter, `POST /berth/park`,
  `POST /berth/resume` (both without a body) and `GET /berth/state`, which
  answers one of `hot`, `parking`, `parked`, `resuming`. Berth detects it
  with a single state read of 3 s or less, parks an engine through it, and
  waits for a card read to show the residual before the next engine takes
  the card; a park that leaves the card full falls back to the normal
  drain. The contract, timing, idempotency rules and a minimal reference
  server are in contrib/PARK-PROTOCOL.md, written for engine authors.
- `park = "hook"` with no `park_url`/`resume_url` on an engine type
  outside the built-in adapter table is now valid: on a drain Berth asks
  the engine's `GET /berth/state`, and if it answers the Berth park
  protocol the engine parks through it, otherwise it drains as today. A
  config warning names the fall-back, and one URL without the other
  stays an error.

- `berth update [--check] [--yes] [--force]`: asks the release page whether a newer
  berth exists, and without `--check` downloads this machine's binary and the
  release's SHA256SUMS, checks the hash, and replaces the running binary in place
  after confirmation, then names what to restart without restarting anything. The
  check is one GET to api.github.com's releases/latest, at most once every 24 h
  (the answer is cached in the state directory, the User-Agent is the running
  version and nothing else); the `[update] check` config key, on by default, is
  Berth's own contact with GitHub, off by that one line. A dev build is refused
  unless `--force`, and a checksum mismatch refuses the download.
- The harbor's header shows a newer release while the update check is on:
  `GET /v1/update` serves the cached answer (one GET to the GitHub API at
  most a day, retried within the hour when it fails), and the notice carries
  the release, a link to it, and a button that copies `berth update`. The
  page makes no request of its own beyond reading that one endpoint, and
  with `[update] check = false` the harbor never touches GitHub and shows
  nothing.
- The harbor's LIBRARY panel: every model in the configured roots with its
  shelf, quant, size, CACHED share (COOL or COLD), the engine serving it when
  one reports it resident, and that engine's estimated VRAM. The data comes
  from the new `GET /v1/models` on the harbor, which computes what `berth
  models --format json` prints and walks the roots at most once per 30 s.
  Each row carries the `berth prefetch NAME` and `berth models show NAME`
  commands as copy buttons; the harbor stays read-only.
- The harbor's lease views show the v0.3 parking states: a WARM badge on a
  parked engine, with the RAM its weights hold (the engine's learned
  footprint) and the age; PARKING as the transitional state on the
  departures board and in the log. Boarding rows carry a Q flag with the
  queue position and the node's completion estimate when the status payload
  names them. A lease's ticket names the tier the weights live in (HOT on
  the card or WARM in RAM), the queue rows, the container a run is in, and
  the exact terminal order for it (`berth stop ENGINE`, `berth resume
  ENGINE`, `berth reclaim LEASE`) beside copy buttons.
- The harbor's engine cards show each engine's learned footprint as
  `est. VRAM`, beside the engine's own reports.

- `berth service install [--gateway] [--harbor] [--dry-run]`, `berth
  service uninstall` and `berth service status`: run the node, and the
  gateway and the harbor when asked, as user services. On Linux they are
  systemd user units rendered from the files in contrib/ (the binary the
  user runs in place of the one those files name) under
  ~/.config/systemd/user/, started with `systemctl --user enable --now`;
  on Windows they are Task Scheduler logon tasks under \Berth, created
  with schtasks, no admin needed, running in the user's session where
  their GPU apps are, hidden, without a time limit, and restarted on
  failure (three tries a minute apart). A unit file that differs is kept
  as .bak, `--dry-run` prints what would be written and run and changes
  nothing, and status says for each of the three whether it is installed
  and running. On Windows the services run; the driver's per-process VRAM
  reading is the next gap, W-1 in docs/WINDOWS-2026-10-20.md.
- `contrib/windows/check.ps1`: the operator's checklist for a Windows
  node on a real NVIDIA box. It writes one report file, ready for the
  drain-report issue form: the berth version and the node's services,
  nvidia-smi, the card and its processes with berth's reading next to the
  driver's (the same counter Task Manager reads, plus the processes the
  driver sees that berth does not), one run of a CUDA app of your choice
  under `berth run`, and the event log around the run. Plain Windows
  PowerShell 5.1, no modules.
- A place in the queue and an estimate in every wait reason: every reason
  the broker sends a waiter starts with its place ("next in line", "2nd in
  line (behind interactive comfyui)"), and the status waiting table shows
  the place and a start estimate for each waiting lease. The estimate
  prices only what a book of measured switch times has measured, one start
  per waiter ahead plus its own; an unmeasured start says unmeasured and
  adds no number, and the holder's drain is never in it, because a request
  runs as long as it runs. The broker takes the book on its config
  (SwapCost); with none, the reasons carry the place only.

- The prediction of switch times: `internal/swapcost` records the times
  completed switches took, per engine and tier (hot on the card, warm
  parked in RAM, cool in the kernel page cache, cold on disk), keeping
  the last 20, and answers the next switch as the median of what it has
  seen. A tier with no samples stays unknown and is never filled with a
  guess, so a plan total with unmeasured steps is a lower bound, "at
  least N s, M steps unmeasured", not a promise. `Plan` works out a
  chain an orchestrator declares ahead of time, step by step: the cost
  of each switch, the engines to prefetch while the current holder
  drains, and the engines the chain comes back to, so the caller parks
  them in RAM instead of stopping them. Groundwork; wired to the daemon
  and the gateway in a later PR.
- The building blocks for KV snapshots: `internal/kvsnap` saves a
  llama.cpp engine's busy slots to files on disk and restores them
  before the next engine's first request, so a sequential run of
  sub-agents does not prefill its context again after every model
  switch. The slot client reads the engine's /slots list and drives the
  engine's --slot-save-path actions; the index keeps the snapshot
  directory in a ledger (a hash-named file per model and slot, never
  across models), within a byte budget and above the library's storage
  floor, and warms a model's snapshots into the kernel page cache, the
  RAM tier, which is cached, never reserved. Groundwork; wired into the
  drain and start paths in a later PR.
- Windows sees what the card holds: per-process VRAM comes from the GPU
  Process Memory counter (the PDH counter Task Manager reads), which
  fills in an engine's own figure where the driver says [N/A] and lists
  a graphics process or game holding more than 64 MiB of dedicated VRAM,
  so squatters and claims count it. Host RAM and the page file come
  from GlobalMemoryStatusEx, so the gateway headroom guard and park
  admission work on Windows, and root space comes from GetDiskFreeSpaceEx,
  so roots, floors and `berth storage` work on an NTFS drive.
- The test suite runs on windows-latest in CI, alongside the Linux job
  and the existing Windows cross-build check. Tests that shell out to
  sh, sleep or true, or depend on the Unix signal ladder, run where
  those exist and are marked `//go:build unix`.

- The page-cache warmth of a model library: `berth models` gains a CACHED
  column (the share of a model's files the kernel page cache still holds,
  summed over its parts and its companions, with the tier that share reads
  as: COOL at half or more, COLD below), `berth models show` says what of
  one model sits in the page cache, and the JSON carries `cached_bytes`.
  The check reads the kernel's page table (mincore) in 1 GiB slices and
  never a byte of a file, so it cannot pull a page in; `--no-cache-check`
  skips it. The page cache is the kernel's and can vanish when the machine
  needs the RAM, so the column says a restart is likely to read from
  memory, never that any RAM is held.
- `berth prefetch MODEL_OR_PATH`: ask the kernel to read a model's files
  into the page cache (posix_fadvise WILLNEED). The command returns as
  soon as the ask is made; the kernel does the reading in the background
  and commits no RAM of its own. This is the manual form of what the
  gateway will do for the next engine while the holder drains.
- The `internal/pagecache` package behind both: `Resident` (a file's
  resident share, measured read-only) and `Prefetch`. Off Linux both
  report an unsupported interface, which the listing shows as unknown
  rather than a failure.
- `berth reclaim [LEASE] [--yes]`: lists the processes a stuck lease recorded
  and, only with `--yes`, stops exactly those (SIGTERM, then SIGKILL after
  `--grace`), re-checking PID and start time before every signal. Processes
  no lease recorded are listed as report only.
  [#48](https://github.com/T-Crypt/berth/pull/48)
- Every descendant of an engine's wrapper is recorded each tick (PID plus
  start time), so a child that escapes its group and reparents keeps the
  lease stuck under its own name instead of showing as a stowaway.
  [#48](https://github.com/T-Crypt/berth/pull/48)
- `contrib/chaos`: `hog.py` and the failure scenarios anyone can run, a drain
  report issue form, and a tested-hardware table in the README.
  [#44](https://github.com/T-Crypt/berth/pull/44)
- `contrib/bench/swap`: times model switches through the gateway (cold and
  warm, per engine), with VRAM after drain, RAM, identical-output and leak
  checks. [#47](https://github.com/T-Crypt/berth/pull/47)
- The engines' VRAM footprints: a freed lease folds its measured peak into
  its engine's record (the most memory seen, the last run's peak, the run
  count), kept in `~/.local/state/berth/footprint.json` across restarts.
  `berth status` shows the last run's peak as EST. VRAM beside the
  engine's own figures, and the JSON names it `estimated_vram_bytes`; it
  is an estimate, never a reservation. A run that holds far more than the
  learned figure (1.5x, after three runs) logs an `engine.footprint`
  event.
- `drain_max` (berth.toml) and `berth run --drain-max DURATION`: a ceiling
  on how long a drain waits at a boundary (a busy slot, a running ComfyUI
  job) before it runs the normal stop ladder; 0 = unbounded, the default.
  While a boundary is busy the wrapper prints a line each minute naming
  the slots and the wait age, and the ceiling says so when it ends the
  wait.
- `berth stop ENGINE [--wait]`: stop an engine Berth started, on purpose.
  The daemon asks each active or draining lease of the engine (by name or
  engine family) to give the card back through the normal stop ladder, and
  a lease frees only after a card read shows the engine's memory gone. A
  pinned lease is asked too: the pin keeps the scheduler off it, not an
  explicit stop. An engine Berth never leased is refused by name, and a
  stuck lease is left to `berth reclaim`. `--wait` follows the event log
  until the leases are freed (exit 0) or stuck (exit 2, naming them).
  Stopping the gateway no longer stops its engines: they run outside its
  cgroup, and `berth stop ENGINE` is how one of them is stopped on purpose.
- Park in RAM, the broker side: a draining lease whose engine can offload
  itself asks to park; admission runs over the machine's readings, the
  parked lease keeps its residual counted on the card, and a waiter builds
  beside a small residual or evicts a big one (past the ttl, under the RAM
  floor, or a step at a time under memory pressure). Parked RAM is a
  reading, never a reservation.
- The built-in park adapters: ComfyUI (its unload keeps the models in RAM
  until pressure), vLLM (`/sleep?level=1`, `/wake_up`; the server starts
  with `--enable-sleep-mode` and `VLLM_SERVER_DEV_MODE=1`), and SGLang
  (`/release_memory_occupation`, `/resume_memory_occupation`; the server
  starts with `--enable-memory-saver --enable-weights-cpu-backup`). Berth's
  own park protocol (the same three-call shape any engine can serve) is
  documented in `contrib/PARK-PROTOCOL.md` and auto-detected:
  `park = "hook"` with no hook URLs probes `GET /berth/state` once.
- `berth resume ENGINE`, and the gateway's requests to a parked engine,
  resume the lease at its own priority and never proxy to an engine whose
  wake has not finished.

### Changed

- One ComfyUI client, one atomic file write that fsyncs the file and its
  directory, and one `/slots` rule, shared across the daemon.
  [#45](https://github.com/T-Crypt/berth/pull/45)
- Releases publish their CHANGELOG section as the release notes.
  [#44](https://github.com/T-Crypt/berth/pull/44)

### Fixed

- A released lease frees only on a tick whose card read came after the
  release and shows none of its processes, never inside the release itself.
  [#48](https://github.com/T-Crypt/berth/pull/48)

## [v0.2.0] - 2026-10-19

### Added

- Holds and claims: games and other apps get the card while they run, and
  Steam, Lutris and gamescope are covered out of the box.
  [#22](https://github.com/T-Crypt/berth/pull/22)
- The Windows node, experimental: process attribution, claims and
  job-object drains work today. [#23](https://github.com/T-Crypt/berth/pull/23)
- `berth skill install`: an agent skill that teaches coding agents to ask
  Berth for the card instead of killing GPU processes.
  [#24](https://github.com/T-Crypt/berth/pull/24)
- A reader for voice engines, starting with Kokoro-FastAPI.
  [#25](https://github.com/T-Crypt/berth/pull/25)
- `berth pull hf:ORG/REPO` and `berth verify`: Hugging Face downloads with
  a resume, checked byte for byte against the size and sha256 the Hub
  publishes. [#26](https://github.com/T-Crypt/berth/pull/26)
- `berth models`: the model library on disk, GGUF headers, and the
  `berth models export ollama` conversion.
  [#28](https://github.com/T-Crypt/berth/pull/28)
- The gateway's wake page: a bookmark to a stopped engine's web UI that
  shows who holds the card and starts it, plus `drain_slots` so servers
  with `/slots` hand off at their request boundary.
  [#30](https://github.com/T-Crypt/berth/pull/30)
- Shelves for models (chat, image, audio), `berth storage` with each root's
  filesystem and free-space floor, and `berth pull civitai:VERSION_ID` with
  the site's own hashes. [#33](https://github.com/T-Crypt/berth/pull/33)
- The ComfyUI suite commands: `berth engine install`, `update`, `show`,
  `rollback` and `remove`, with the git-pinned components and the trailing
  model-roots rewrite. [#34](https://github.com/T-Crypt/berth/pull/34)
- The gateway's gui plane index, every engine with its state and its launch
  button. [#35](https://github.com/T-Crypt/berth/pull/35)

### Changed

- The README leads with what Berth does and documents the model library.
  [#27](https://github.com/T-Crypt/berth/pull/27)
- Fresh visuals: an animated banner, a how-it-works diagram and a shorter
  first screen. [#31](https://github.com/T-Crypt/berth/pull/31)
- The harbor interface polish: one head rhythm, one KPI grid on every
  screen, touch-sized phone layout and a documents-first nav.
  [#32](https://github.com/T-Crypt/berth/pull/32)

### Fixed

- `berth run` reads an engine's settings from berth.toml, and a flag set by
  hand still wins. [#36](https://github.com/T-Crypt/berth/pull/36)

## [v0.1.0] - 2026-10-11

Berth v0.1.0: the VRAM leasing service for every GPU you run. Leases with
priorities, the measured release (a grant only after the driver shows the
memory free), stuck and contested detection, unprotected runs, the event
log, the harbor web UI and the first engine readers.
