# Leasing, games and holds

## Leasing (the core)
- **Explicit leases.** Engines start through `berth run`, which waits for a berth, starts
  the engine in its own process group, and gives the berth back when the engine exits.
- **Priorities.** `foreground` > `interactive` > `batch`. An equal or higher request
  asks the holder to drain; a lower one waits and is refused with a reason naming the
  holder (exit 75), never an out-of-memory crash.
- **Handoff in steps.** A drain tries the engine's own unload first (for llama-swap, its
  unload URL), then SIGTERM to the process group, then SIGKILL, each after a grace period.
- **Measured release.** Berth grants the next engine only after the driver shows the old
  holder's processes gone, since an exited process can still hold memory.
- **Stuck releases are caught.** A released lease whose memory never frees turns
  *aground* and keeps blocking, with a reason naming the process and how much it holds.
- **A way out for a stuck release.** `berth reclaim [lease]` lists what the stuck lease
  recorded, PID, start time, memory and command, and anything on the card no lease
  recorded ("not Berth's", report only). With `--yes` it stops exactly the recorded
  ones, SIGTERM first and SIGKILL to the survivors after `--grace`, re-checking each
  PID and start time immediately before every signal.
- **Process identity is PID plus start time**, so a reused PID never inherits a lease.
- **Children stay on the lease.** On Linux with a systemd user manager, `berth run`
  gives the engine its own cgroup scope, which every child inherits on fork: a child
  that escapes its process group and reparents keeps the lease stuck under its own
  name instead of turning into a stowaway. Without a user manager, Berth records every
  descendant of the wrapper each tick, which covers everything except a fork whose
  parent exits inside one tick. On Windows, a child keeps its parent's PID after the
  parent exits, so Berth follows each GPU process's parent chain from a process
  snapshot it refreshes every second, and the job object `berth run` puts the engine
  in carries every process it starts, so a drain stops them all.
- **Stop signals.** A SIGTERM, SIGINT or SIGHUP to `berth run` drains the engine and releases the lease. On
  Windows, Ctrl-C, Ctrl-Break, closing the console window and logging off do the same, and the engine's drain
  has to finish in the few seconds Windows allows after a close. `berth gateway` starts each run in its own
  process group on Windows, so Ctrl-C in the gateway's console does not drain engines it meant to leave running.
- **Whole-card or sized leases.** `--needs-mib N` lets engines that fit share a card. `--learn-size`, or
  `learn_size = true` on the engine entry, sizes the lease for you. It takes the most the engine has been measured
  holding plus 512 MiB for its load, so you never type or update a number. The first run takes the whole card,
  because nothing is measured yet. The grant in `berth events` says which size it used. A run that grows past its
  size still counts at what the driver measures, so a waiter never gets memory the engine holds.
- **Several cards.** One berthd governs every card by default. A foreground request on one card never drains a
  holder on another. `berth run --gpu gpu1` puts a lease on one card, and `--gpu gpu0,gpu1` holds one lease on
  both (see [Training runs](#training-runs)). A running game holds every card, because Berth cannot tell which
  card it will use. A `[[claim]]` with `gpu = "gpu1"` holds only the cards it names. A lease on several cards
  cannot park or lend yet.
- **Fail-open, never silent.** If the daemon is down, `berth run` still starts the engine
  but records it as an *unprotected run* that status shows (`--fail-closed` refuses). It
  keeps dialling, and re-attaches when a daemon answers, so a service that starts before
  berthd at boot ends up leased.
- **Survives a daemon restart.** A running engine stays up when berthd restarts or is
  upgraded; `berth run` dials the new daemon and re-attaches by PID plus start time. For
  15 s after a start the daemon grants nothing new while such engines are still due back,
  so nothing is loaded onto a card an engine already holds.
- **ComfyUI at a job boundary.** `--drain-comfyui` waits until ComfyUI's queue is empty
  before stopping it; a job in progress is never cut off.
- **No juggling.** Two engines at the same priority drain each other: the last one asked for
  wins, which is what you want when you switch models. When two clients want different
  engines at once, that flips the card back and forth and every request fails. Berth calls it
  a juggle when an engine is drained within `juggle_window` (15 s) of getting the card while it
  is still busy (a request in flight, a ComfyUI job queued, a model loading). The engine ranking
  decides a juggle: the winner keeps or takes the card, and the loser waits for it instead of
  draining it back. An idle holder hands over at once, so chat, then an image, then chat again
  is three clean handoffs. A person's switch (Launch on the gateway's wake page, or
  `/v1/berth/switch`) is never a juggle: it takes the card whatever the ranking says, and inside
  the window a request no person made (a chat client's background call) does not take it back.
  A switch to an engine whose start a request already queued, and lost a juggle, reaches that
  start's lease and takes the card too. To keep it longer, give the engine a `min_turn`.

  ```toml
  [priority]
  order = ["llama-swap", "strata", "comfyui"]  # first is best; omit to rank by use
  juggle_window = "15s"                        # a negative value turns it off
  ```

  Without an `order`, the engine with the most hours on the card through Berth over the last
  7 days ranks first (kept in `~/.local/state/berth/usage.json`); a tie leaves the holder in
  place. `berth run --group NAME` files a lease under an engine family: llama-swap runs each
  model under its own name, so give each `--group llama-swap`. `berth status` shows the ranking
  and a count of juggles stopped; the harbor log shows each one with the reason.

  - **A minimum turn per engine.** `min_turn = "5m"` on the engine entry (or
    `berth run --min-turn 5m`) gives the engine a turn it keeps for that long after the
    grant: in that time a request of equal or lower priority cannot drain it, and neither can
    a higher one, because a request interrupted halfway is a lost one. Only a hold (a game, a
    call, `berth hold`) takes the card out before the turn ends.

    ```toml
    [[engine]]
    name  = "comfyui"
    min_turn = "5m"   # the card stays this engine's for 5m after the grant
    ```

    A blocked waiter is told how much of the turn is left ("next in line: comfyui has its
    minimum turn, 3m12s left"), and `berth status` shows the time a lease has left in the
    TURN column. When the turn ends the normal rules apply: a drain still starts at the slot
    or job boundary, and a minimum turn never cuts a request off. After a daemon restart, a
    re-attach starts a fresh turn.

  - **A batch.** A minimum turn is a fixed time; a batch lasts as long as the work. Before an
    agent or a script queues many jobs on one engine, `berth batch comfyui` (or
    `POST /v1/berth/batch`) keeps the card on it until it has had no job running or queued for
    the grace (`--grace 30s`), or until `--max` (4h) runs out. While it lasts, no waiter but a
    hold drains the engine, so a chat turn between two images waits for the batch instead of
    costing both engines a reload. If the engine is the one waiting, it takes the card as a
    switch does. Waiters read "next in line: comfyui runs a batch; it keeps the card until it
    has been idle 30s", and the event log has `batch.started` and `batch.ended`. A single job
    with no batch hands the card back and forth as before. `berth batch comfyui --end` ends it
    now; a berthd restart ends it too.

    A batch is not the `batch` priority. That one makes an engine wait behind everything
    else; this one keeps an engine on the card while its queue has work.

  - **A yield.** A drain of ComfyUI waits for the job boundary, so a job that calls an LLM
    in the middle can deadlock: the job waits on the LLM, the LLM waits for the card, and the
    drain waits on the job ([#299](https://github.com/T-Crypt/berth/issues/299)). A node in
    that job can yield the card instead:

    1. It frees ComfyUI's models in-process, which `/free` cannot do while a job runs.
    2. It calls `POST /v1/berth/yield` with `{"engine":"comfyui","seconds":120}`. For that
       long, a drain treats the job as at its boundary, so the LLM's drain parks ComfyUI as
       it would between jobs. The card read after the park still decides what is free.
    3. It makes its LLM call. The LLM is granted in the room the park left.
    4. It calls `POST /v1/berth/yield` with `{"engine":"comfyui","end":true}`, which resumes
       ComfyUI and answers once it holds the card again. The next node loads its models.

    A yield only ends the wait for the job boundary. A node that yields without freeing
    its models gets the usual fallback: the park does not free the card within the grace,
    and the drain stops ComfyUI. A yield that is never ended runs out after its seconds.

## Games, apps and holds
- **Claims cover work Berth does not start.** berthd scans the process table every 2 s.
  A *hold* claim keeps the card while its process runs: engines are drained and wait, with a
  15 s linger so a launcher restarting the game does not bounce the card back. A *track*
  claim names an app's card memory so it counts against the card without being reported as a
  stowaway; nothing waits on it.
- **Built-in pack, on by default.** Holds: Steam games (anything under Steam's `reaper`),
  Lutris and gamescope on Linux; Steam libraries (`steamapps\common`), Epic, GOG, EA,
  Ubisoft, Xbox and Battle.net games on Windows, with launchers, Wallpaper Engine and
  Lossless Scaling left out. Tracks: Blender, DaVinci Resolve, OBS, Kdenlive, Premiere,
  After Effects. `berth claims` shows every rule and what it matches now.
- **Your own rules:**

  ```toml
  [[claim]]
  name = "training"
  cmdline = 'train\.py'      # regex; on Windows it sees the exe's full path
  action = "hold"            # or "track"
  linger = "30s"
  gpu = "gpu1"               # hold only these cards (names or PCI addresses, comma list); default every card

  [claims]
  builtin = false            # turn the built-in pack off
  ```

  A rule can also match `exe` names (globs, `.exe` optional), a launcher `parent` (with
  `parent_cmdline`, a regex that launcher's own command line must match too), and exclude with
  `not_cmdline`. The built-in Steam rule matches Steam's `reaper` only when it runs as
  `reaper SteamLaunch AppId=...`, so the REAPER DAW, also named `reaper`, holds nothing.
- **`berth hold [--for 2h] [--reason game]`** takes the card for you. It is a pinned lease at
  the top priority, so nothing drains it, and it ends on Ctrl-C, after `--for`, or when the
  process dies. On a node with several cards it takes the first one. `--gpu gpu1` holds another
  card, and `--gpu gpu0,gpu1` holds both under one lease.
- **`berth run --pinned`** gives an app a lease that is never drained (it keeps the card until
  it exits).
- A hold cannot free memory held by something outside Berth: that memory is still reported,
  as contested.

## Cordon a card

On a node with several cards, `berth cordon gpu1` stops new grants on that card while the others keep serving, for
maintenance or before a VM takes it. Name the card by its name or its PCI address.

- What holds the card keeps running. A cordon drains nothing: `berth stop ENGINE` does that.
- Waiters stay in line, and their reason says the card is cordoned. A hold (`berth hold`, a game claim) is still
  granted.
- The gateway treats the card as out of reach. A plain `/v1` request goes to another card that serves its model,
  a background engine for the card does not start, and a ComfyUI job for it queues.
- `berth status` marks the card `CORDONED`, and `berth events` records `card.cordoned` and `card.uncordoned`.
- `berth uncordon gpu1` lets new work in, and the card's waiters are scheduled at once. A berthd restart also
  clears the cordon.

## Run work when you're away or power is cheap

- **A batch lease yields.** `berth run --priority batch` waits for the card and hands it back to any request that
  asks, at the drain boundary. A batch job makes progress while you are not asking for the card, and never cuts you
  off.
- **A background engine waits for a free card.** `background = true` on an engine entry has the gateway start it
  once the card has been free for `idle_after`. Read
  [Run an engine whenever the card is free](gateway.md#run-an-engine-whenever-the-card-is-free).
- **A window puts either on the clock.** `window = "23:00-07:00"` on the entry, or `berth run --window 23:00-07:00`,
  holds the lease to those hours on the node's wall clock. It may cross midnight. A lease that asks outside them
  waits for them with no deadline (unless `--wait` sets one) and holds up nobody behind it. A background engine is
  not started at all outside its hours. `berth status` (SCHEDULE) and the harbor (SCHEDULES) list every engine that
  has a window, whether or not it runs, with its hours and when it next opens or closes. When the window closes its holder drains at its usual boundary, so a slot or a job boundary comes first and a
  request in flight finishes.

  ```toml
  [[engine]]
  name   = "nightly"
  window = "23:00-07:00"   # 24 h, two digits per field, the node's clock
  ```

  ```sh
  berth run --as nightly --priority batch --window 23:00-07:00 -- ./finetune.sh
  ```

  A waiter reads its hours in its reason ("outside its window 23:00-07:00; opens in 3h12m"), and
  `berth events` shows the close as the drain it is. A window needs a lease that can be asked back, so it goes with
  neither `--pinned` nor `--priority hold`.

Work that is not a server has no slot or job boundary to stop at, so a window close stops its process. A script that
runs for hours saves checkpoints, and the next night's start picks up from the last one.

A timer can start a one-off batch job at the window's open instead: a systemd user timer with
`OnCalendar=*-*-* 23:00:00` running `berth run --priority batch -- ./finetune.sh`, or the same command line in a
Task Scheduler task triggered daily at 23:00. The window is what keeps the job out of the expensive hours when it
overruns or the machine wakes late.

## Split one model across cards

llama.cpp splits a model that is too big for one card across several with `--tensor-split` (`-ts`). Run it under one
lease on those cards:

```sh
berth run --as llama-70b --priority foreground --gpu gpu0,gpu1 \
  -- llama-server -m llama-70b-q4.gguf -ngl 99 --tensor-split 3,1 --port 8080
```

- berthd grants both cards at once, or neither, so the model never loads onto one card while it waits for the other.
- The engine starts with `CUDA_VISIBLE_DEVICES` set to the cards' UUIDs in the order `--gpu` names them. The first
  `--tensor-split` value goes to the first card named, the second to the second.
- A request that drains either card stops the engine, and its exit frees both. A lease on several cards cannot park
  yet, so the next start loads the model from disk again.
- Without `--needs-mib` the lease takes the whole of each card. That is the safe choice when nothing else runs there.
- To leave room beside an uneven split, give one size per card, in the same order:
  `--needs-mib 18000,6000`, or on the engine entry:

  ```toml
  [[engine]]
  name      = "llama-70b"
  gpu       = "gpu0,gpu1"
  needs_mib = [18000, 6000]   # MiB on gpu0, then gpu1
  ```

  A size is what the engine holds on that card at its peak, KV cache and compute buffers included, not the share of
  the weights. Start it once with the whole cards, read each card's HOLDS in `berth status`, and add some headroom.
  One value, as before, is the size on every card.

## Training runs

A training run that loses its cards loses everything since its last checkpoint. Start it with `berth train`:

```sh
berth train --as train-mymodel --gpu gpu0,gpu1,gpu2 -- torchrun --nproc_per_node 3 train.py
```

- The run takes the whole of every card `--gpu` names. It drains the engines on them, waits with no deadline, and
  keeps the cards until the command exits or you press Ctrl-C. Without `--gpu` it takes the node's first card.
- While a training holds a card or waits for it, berthd locks that card. It refuses every other request there at
  once: a gateway request, `berth run`, a llama-swap model, a scheduled job and `berth hold`. A client gets the
  refusal right away and can try another node, instead of waiting out its timeout. Cards the training does not
  name keep serving.
- A second `berth train` for the same card waits in line with no deadline and starts when the first one ends.
- A game or a hold that already has the card is not drained. The training waits for it.
- `berth train` refuses to start when berthd is not running, because the lock lives in berthd.
- berthd grants the cards together and starts the command with `CUDA_VISIBLE_DEVICES` set to their UUIDs, in the
  order you named them. Do not set `CUDA_VISIBLE_DEVICES` yourself, since Berth leaves it alone when it is set. Set
  `--nproc_per_node` to the number of cards.
- The workers torchrun or `accelerate launch` starts belong to the lease, so their memory is the run's on every card.
- `berth status` shows the lease as `hold,pinned,training`. `berth stop train-mymodel` still stops it, because a
  person asked.
- A berthd restart or upgrade does not stop the run, and the lock holds while berthd is away. With berthd down,
  `berth run` refuses to start an engine while the training runs, instead of starting it without a lease. A new
  berthd refuses every other request until the training re-attaches, with no time limit, and the training comes
  back as a training.

`berth run --pinned --priority foreground` still works for a run that must not be drained but may share its card.

### Lock the card in the driver too

The training lock covers everything that asks Berth for the card. A program that opens CUDA without Berth only
shows up as a stowaway. `berth train --exclusive` closes that gap on Linux with an NVIDIA card. After the grant, it
puts the cards in the driver's exclusive-process compute mode, so the training's process is the only one that can
open a CUDA context there. When the run ends, it puts them back to the default mode.

```sh
berth train --exclusive --as train-mymodel -- python train.py
```

- Setting the mode needs root. Give your user a sudo rule for `nvidia-smi -c` alone, with `sudo visudo -f
  /etc/sudoers.d/berth-exclusive`:

  ```
  YOUR_USER ALL=(root) NOPASSWD: /usr/bin/nvidia-smi -i * -c EXCLUSIVE_PROCESS, /usr/bin/nvidia-smi -i * -c DEFAULT
  ```

  Without the rule, `berth train --exclusive` refuses to start instead of running without the guard.
- The run refuses to start when another compute process is already on the card, because that process would own
  the card's one context and the training could not open it.
- Exclusive mode allows one process per card. A training that opens CUDA from several processes on one card
  (DataLoader workers that use CUDA, two ranks on one card) fails to start in it. One rank per card, as torchrun
  gives each card, works.
- Graphics are not compute: the desktop and games keep drawing on the card.
- If the `berth train` process is killed with SIGKILL, the mode stays set. `sudo nvidia-smi -c DEFAULT` puts it
  back.
A pinned run does not lock the card: other requests wait for it until their own deadline.

A pin does not stop memory taken outside Berth. A game started on a leased card waits behind the run in Berth but
still allocates memory, and that can run the training out of memory. Keep the card that drives your display out of
`--gpu`. A crash or a power cut needs checkpoints. With Hugging Face's `Trainer`, set `save_steps` and resume with
`trainer.train(resume_from_checkpoint=True)`.
