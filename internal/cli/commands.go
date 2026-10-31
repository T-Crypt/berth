// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

// commands is the command tree. Rows are alphabetical, and a group (Sub) is a
// row like any other: dispatch, help and the command list all read this table.
var commands = []*Command{
	{
		Name:    "claims",
		Summary: "the claim rules and what they match right now",
		Args:    "[--socket P] [--format F]",
		Help: `Claims cover work Berth does not run. A hold claim (games by default) keeps
the card while its process runs: engines are drained and wait. A track claim
(Blender, Resolve, OBS, Kdenlive by default) names that app's card memory, so
it is counted and never reported as a stowaway. Rules come from [[claim]] in
berth.toml plus the built-in pack ([claims] builtin = false turns it off).
Claims never stop or signal a process.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runClaims,
	},
	{
		Name:    "config",
		Summary: "read berth.toml: validate it, or print it evaluated",
		Help: `The config file is ~/.config/berth/berth.toml ($BERTH_CONFIG). Without one,
berth runs on its built-in defaults and discovers engines on their usual ports,
so a missing file is a valid setup, not an error.`,
		Sub: []*Command{
			{
				Name:    "check",
				Summary: "validate berth.toml without applying it",
				Args:    "[--config PATH] [--strict] [--format F]",
				Help: `Read the config, report what is wrong with it, and stop. Nothing is
applied and no daemon is touched: check is read only.

A warning is a setting that works but is likely a mistake. --strict makes any
warning fail the check, which is what a CI or a pre-commit hook wants.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runConfigCheck,
			},
			{
				Name:    "show",
				Summary: "print the evaluated configuration",
				Args:    "[--config PATH] [--format F]",
				Help: `Print the config the node would run with: the file, every default
materialised, and the environment that decides which file that is.

--format toml prints the config as a berth.toml document, so
"berth config show --format toml > berth.toml" writes a valid file.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML, FormatTOML},
				Run:     runConfigShow,
			},
		},
	},
	{
		Name:    "civitai",
		Summary: "search and browse CivitAI, for finding things to pull",
		Help: `CivitAI is a search and a listing, not a pull: search finds a model and its
version ids, and a pull takes them: berth pull civitai:<version-id>. A model
id pulls its latest version: berth pull civitai:model:<model-id>.`,
		Sub: []*Command{
			{
				Name:    "search",
				Summary: "search CivitAI's models by query and asset kind",
				Args:    "QUERY [--type T]... [--limit N] [--token-env V] [--format F]",
				Help: `Ask CivitAI for models matching QUERY. Public models need no key; --token-env
names the environment variable holding an API key when you want gated results.
The printed ids are what berth pull takes.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runCivitaiSearch,
			},
			{
				Name:    "versions",
				Summary: "list a CivitAI model's versions, newest first",
				Args:    "MODEL_ID [--token-env V] [--format F]",
				Help: `List the versions of a model, newest first, with the version ids a pull
takes. The id of the first line is what civitai:model:<MODEL_ID> resolves to.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runCivitaiVersions,
			},
		},
	},
	{
		Name:    "daemon",
		Summary: "run the broker for every card (berthd)",
		Args:    "[flags]",
		Help: `Run the lease broker for every card: it hands each GPU to one engine at a
time, drains the holder when another engine asks for it, and reports any
process holding VRAM without a lease. It never kills a process it did not
start. Logs go to stderr; a user unit is in contrib/berthd.service.`,
		Run: runDaemon,
	},
	{
		Name:    "engine",
		Summary: "the inference engines this machine runs",
		Help: `Engines are found on their usual ports, and any other one is named in
berth.toml. What berth reads from an engine is the engine's own report, kept
apart from what the driver measures.`,
		Sub: []*Command{
			{
				Name:    "check",
				Summary: "read every configured engine once and report which answer",
				Args:    "[--config PATH] [--discover] [--timeout D] [--format F] [NAME]",
				Help: `Read every engine the config names, once, without a daemon. A URL, a
type or a key that does not work shows up here instead of as an engine that
never appears in status. NAME limits the check to one engine.

Each engine's figures are its own claims. Exit 1 when an engine was configured
but did not answer.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runEngineCheck,
			},
			{
				Name:    "install",
				Summary: "add a ComfyUI install to the suite: adopt a checkout or clone a fresh one",
				Args:    "NAME [--adopt PATH | --path PATH] [--port N] [--priority P] [--keep-up] [--channel REF] [--repo URL] [--pack URL]... [--config P] [--format F]",
				Help: `Record one ComfyUI install in the suite: its core, ComfyUI-Manager and every
git-based node pack, each pinned to a git commit with one previous commit kept
as the rollback snapshot. The suite state lives in engines.toml beside berth.toml.

--adopt PATH records an existing checkout in place (it is read, never changed);
--path PATH clones a fresh one (default location: $XDG_DATA_HOME/berth/comfyui/
NAME). The derived engine appears in berth engine check, berth status and the
gateway's wake page; the daemon and gateway pick it up at their next start.

Each install also gets an extra_model_paths.yaml naming the node's model roots
from berth.toml, so its models come from the same library. PyTorch on a fresh
install is the operator's step; the install is registered either way, and
ComfyUI's own start error names the missing piece.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runEngineInstall,
			},
			{
				Name:    "remove",
				Summary: "drop a suite install, keeping or removing its directory",
				Args:    "NAME [--delete --yes] [--config P] [--format F]",
				Help: `Drop the install from the suite state. Without --delete only the state entry
goes: the directory, its venv and its extra_model_paths.yaml stay where they
are. --delete also removes the install directory, and it requires --yes: a
directory removal is the one irreversible step the suite has, and the flag pair
is the consent for it.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runEngineRemove,
			},
			{
				Name:    "rollback",
				Summary: "swap a component back to its previous commit",
				Args:    "NAME --component C [--config P] [--format F]",
				Help: `Swap the component's pin with its previous commit and check out the restored
sha. A second rollback flips back to the newer pin; a following update
replaces the snapshot again. A component with no previous commit, or with
local changes, is refused. An install with jobs running is refused: the
handoff happens at the job boundary.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runEngineRollback,
			},
			{
				Name:    "show",
				Summary: "the suite installs: their components, pins and snapshots",
				Args:    "[NAME] [--config P] [--format F]",
				Help: `Print the suite state: one row per install (name, kind, path, port, priority,
venv) and one per component (name, path, repo, channel, the pinned commit,
its previous commit, whether the tree is dirty). NAME limits the listing to
one install. No network: this is what the state file records, not what the
checkouts do right now.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runEngineShow,
			},
			{
				Name:    "update",
				Summary: "pin the core and the node packs to newer commits",
				Args:    "NAME [--component C | --all] [--channel REF] [--pack URL]... [--config P] [--format F]",
				Help: `Move every component (or one with --component) to the tip of its channel,
keeping the old pin as the rollback snapshot. --pack URL... clones node packs
into custom_nodes first and updates them. --channel REF pins a component that
has no channel of its own (an adopted checkout that is detached).

An install with jobs running is refused: the update happens at the job
boundary, when its queue is empty. A component whose tree gained local
changes since the state was written is refused on its own; the rest proceed.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runEngineUpdate,
			},
		},
	},
	{
		Name:    "events",
		Summary: "what berthd saw and did; -f follows it live",
		Args:    "[-f] [--since N] [--socket P] [--format F]",
		Help: `Every request, wait, drain, release, stuck lease, stowaway and probe change,
with gap-free sequence numbers, so a client that reconnects asks for
--since <the last seq it saw>. Without -f the backlog is printed and the command
exits; with it, new events stream until you interrupt it.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runEvents,
	},
	{
		Name:    "notify",
		Summary: "desktop notifications for what berthd does",
		Args:    "[--level error|info] [--socket P] [--config P]",
		Help: `Follow berthd's events and show the ones worth a look as desktop
notifications. The error level, the default, shows refused requests, memory
that never came back, stowaways and a parked engine loaded behind Berth's
back. info adds every switch, drain, park and resume. [notify] enabled = false
in berth.toml turns it off; [notify] level sets the level. Linux only for now:
it uses notify-send, so any desktop or notification daemon shows it.`,
		Run: runNotify,
	},
	{
		Name:    "export",
		Summary: "dump the harbor's stored history: requests and samples",
		Args:    "[--db P] [--range R] [--as csv|json] [--out DIR] [--format F]",
		Help: `Write the harbor's stored rows for one range to two files in --out:
requests.<ext> and samples.<ext>. The harbor keeps the data, so run it first.
The command only ever reads the store, and prints where the dump went.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runExport,
	},
	{
		Name:    "gateway",
		Summary: "one OpenAI-compatible port for every engine (preview)",
		Args:    "[flags]",
		Help: `Serve one OpenAI-compatible port in front of the node's engines: a request
naming a model an engine serves goes there, and an engine that is down starts
under a lease, draining the holder first. Loopback only in this release, and
every route needs the API key (berth key prints it).`,
		Run: runGateway,
	},
	{
		Name:    "harbor",
		Summary: "the web UI; loopback by default",
		Args:    "[flags]",
		Help: `Serve the read-only web UI: the card and its leases, the engines, the event
log, the request KPIs and the tide of card memory over time, persisted in
SQLite. It listens on loopback by default; a non-loopback --listen address is
your explicit opt-in to expose it. Reading needs no login; berth pair pairs a
device for the actions.`,
		Run: runHarbor,
	},
	{
		Name:    "hold",
		Summary: "keep the card free for a game or a call; engines wait",
		Args:    "[--gpu CARD] [--for D] [--reason R] [--needs-mib N] [--wait D]",
		Help: `Hold the card for yourself. Every engine on it is drained the usual way,
none starts while the hold lasts, and nothing drains the hold: it is a pinned
lease at the top priority. It ends on Ctrl-C, after --for, or when this
process dies, so a forgotten terminal never strands the card for long.
--gpu names the card on a node with several (gpu1, its PCI address, or a comma
list); without it the hold takes the node's first card.

Games you launch from Steam, Lutris or gamescope are held for automatically
by the built-in claims (see berth claims); berth hold is for everything else.`,
		Run: runHold,
	},
	{
		Name:    "key",
		Summary: "print the node's API key, creating it on first use",
		Args:    "[--file P]",
		Help: `Print the key the gateway's clients send as their bearer token. It is
created on first use, mode 600. A client that runs commands for its key never
has the secret written into its config: "apiKey": "!berth key".`,
		Run: runKey,
	},
	{
		Name:    "models",
		Summary: "the model files this machine holds, their shelves, and what they were used for",
		Args:    "[--dir D]... [--shelf S] [--all] [--usage W] [--no-usage] [--no-cache-check] [--format F]",
		Help: `List the model files where the usual tools keep them - the Hugging Face
cache, LM Studio's model directories, Ollama's store and ~/models - plus any
--dir you name, or the [models] dirs berth.toml names.

One row per model: MODEL is the file's stem (the name that never collides - a
model's quants all answer to the same name inside the file), SHELF (text,
image, audio, or - when nothing in the file states one), then QUANT, SIZE,
CACHED, PARAMS, CTX, ARCH, how many companion files belong to it (+COMP) and
USED, when the harbor last saw it serve a request. CACHED is how much of the
model's files the kernel page cache holds (its own parts and its companions),
and the tier that share reads as: COOL at half or more, COLD below it, and -
when the check is skipped with --no-cache-check or measures nothing. The
kernel takes the pages back when the machine needs the RAM, so the column says
a restart is likely to read from memory, never that any RAM is held. --shelf
image lists one shelf only, and the totals under the listing say how much of
the library each shelf holds;
a root declared in [library] with a shelf (say shelf = "image") beats what the
files themselves say. --usage 30d adds REQ and TOKENS for that window. Sorted
by name. --all also lists the companion files themselves: a
vision projector (mmproj, clip) and a speculative draft (mtp, eagle, dflash)
are run with a model, not on their own, so they are counted rather than listed.
A split GGUF is one model: its parameter count is summed over every part.
Headers only: berth never loads weights, hashes a file or downloads anything.
Every scan leaves its snapshot in each root under .berth/catalog.json, so a
second machine reading the same root sees what this one resolved.

The harbor's request log decides USED, REQ and TOKENS, and it is opened
read-only. No harbor database is no usage: every model reads "never". --no-usage
skips the read entirely. A llama-swap id is tied to the file its -m path loads
when berth.toml's llama-swap engine names a --config; request names that no file
on disk answers to are "served but not on disk", in the JSON only.

show NAME|PATH [--dir D] prints one model in full: the metadata that has no
column, what sits in the page cache, the first line of its chat template and
the usage block. A model that
will not read is reported as that one model's error and exit 1; the rest of the
library still lists.

export ollama NAME|PATH [--as NAME] [--dry-run] [--dir D] writes a Modelfile for
Ollama - FROM the GGUF where it lies, its mmproj as a second FROM, and
PARAMETER num_ctx capped at 32768 - and runs "ollama create NAME -f <Modelfile>".
No TEMPLATE: Ollama reads a GGUF's own chat template, and rewriting one is where
an export loses the tool calls it no longer emits. --dry-run prints both and runs
nothing. A split GGUF is refused: Ollama reads one file, so merge it with
llama-gguf-split --merge first. Berth copies and deletes no weight file, and
ollama create does (it takes its own blob), which is why --dry-run exists.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runModels,
	},
	{
		Name:    "prefetch",
		Summary: "ask the kernel to read a model's files into the page cache",
		Args:    "MODEL_OR_PATH [--dir D] [--format F]",
		Help: `Ask the kernel to read the model's files - every part of a split model,
and the vision projector and draft that go with it - into the page cache.
The name resolves the way models show resolves it: a path that exists, else
the listing's name, the name the file's header gave it, or the file itself.

The ask is made with posix_fadvise WILLNEED and returns at once: the kernel
does the reading in the background, at its own pace, and it takes the pages
back whenever the machine needs the RAM. Prefetching commits no memory of
Berth's and reads no file byte itself, so it is safe to ask while another
engine drains. This is the manual form; the gateway makes the same ask for
the next engine while the holder drains.

Exit 1 when no model answers to the name. Off Linux the kernel has no
page-cache interface and the ask is refused.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runPrefetch,
	},
	{
		Name:    "pull",
		Summary: "download a model from Hugging Face or CivitAI, checked byte for byte",
		Args:    "REF [--dir D] [--dry-run] [--token-env V] [--format F]",
		Help: `REF is one of:

  hf:ORG/REPO[:QUANT|FILE|GLOB][@REV]  the Hugging Face Hub
  civitai:VERSION_ID                   a CivitAI model version
  civitai:model:MODEL_ID               that model's latest version
  https://civitai.com/model-versions/ID  the site's own URLs

A Hub pull downloads to DIR/<org>/<repo>/ (~/models by default). A quant name
picks the matching GGUF and every part of a split model, plus the vision
projector when the repo has one; a file name or a glob picks exactly that. A
CivitAI pull resolves the version and downloads the files it lists to
DIR/civitai/<model>/.

Every file comes from one snapshot of its source, resolved once at the start,
and is checked against the size and sha256 the source publishes before it is
moved into place. An interrupted pull resumes where it stopped when you run it
again, and a file already present and correct is not downloaded twice. Before
anything is downloaded the pull measures the destination root and refuses when
the files would leave it under the [library] floor, a tenth of the root by
default; a root whose space this platform cannot measure says the floor was
not checked. A manifest beside the files records the source, the reference
and every hash, so berth verify can re-check them later without the network.
Gated and private models read a token from $HF_TOKEN or $CIVITAI_API_KEY.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runPull,
	},
	{
		Name:    "reclaim",
		Summary: "what a stuck lease left on the card; --yes stops it",
		Args:    "[LEASE] [--yes] [--grace D] [--socket P] [--format F]",
		Help: `List the card processes a stuck lease recorded, each with its lease, PID,
start time, memory and command, plus any card process no lease recorded
("not Berth's", report only). No LEASE covers every stuck lease; a named
lease may still be releasing.

Without --yes nothing is signalled: rerun with --yes to stop exactly the
recorded processes, SIGTERM first and SIGKILL to the survivors after
--grace. berthd re-checks each PID and start time immediately before every
signal and skips a mismatch, so a reused PID is never signalled.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runReclaim,
	},
	{
		Name:    "resume",
		Summary: "put a parked engine back on the card",
		Args:    "ENGINE [--socket P] [--format F]",
		Help: `Queue a parked engine for the card again, at its own priority. The runner
keeps its connection, hears the grant with resume set, and calls the adapter's
resume; the lease is active when the engine answers again. An engine that is
not parked is refused, so the caller starts it the normal way.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runResume,
	},
	{
		Name:    "run",
		Summary: "start CMD once it holds a berth, stop it when asked",
		Args:    "--as NAME [flags] -- CMD [ARGS...]",
		Help: `Run an engine under a lease: wait for the card, start CMD in its own process
group, and hand the berth back when the engine exits or another engine asks for
the card. With berthd down the engine still starts and is recorded as an
unprotected run (--fail-closed refuses instead).

The engine's own stdout goes to stdout, so --quiet has nothing to silence here:
berth's notices are diagnostics on stderr and always print.`,
		Run: runRun,
	},
	{
		Name:    "skill",
		Summary: "the agent skills: teach a coding agent to use Berth and ComfyUI",
		Help: `Berth ships two skill files for coding agents (Claude Code, Codex, Pi, OpenCode,
Hermes and others that read SKILL.md). berth: how to read what holds the card,
how to ask for it instead of killing a process, holds, claims and the gateway.
comfyui: how to set up ComfyUI under Berth and generate through it.`,
		Sub: []*Command{
			{
				Name:    "install",
				Summary: "write the skills where agents look for them",
				Args:    "[--target claude,agents,opencode|all] [--dir D] [--format F]",
				Help: `Write berth/SKILL.md and comfyui/SKILL.md to ~/.claude/skills,
~/.agents/skills and ~/.config/opencode/skills (or --dir D). A different
existing file is kept as SKILL.md.bak. Running it again after an upgrade
refreshes them.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runSkillInstall,
			},
			{
				Name:    "show",
				Summary: "print a skill",
				Args:    "[berth|comfyui]",
				Help:    `Print a skill's SKILL.md to stdout (berth by default), for an agent or harness that takes it inline.`,
				Run:     runSkillShow,
			},
		},
	},
	{
		Name:    "service",
		Summary: "run berth as a user service: install, uninstall, status",
		Help: `Berth's services: the node (berthd) always, and the gateway and the
harbor when you ask for them. On Linux they are systemd user units,
rendered from the files in contrib/ with your own binary's path in place
of the one those files name; on Windows they are Task Scheduler logon
tasks under \Berth, one per service, no admin needed, running in your
session where your GPU apps are.

install [--gateway] [--harbor] [--dry-run] writes the units and starts
them (systemctl --user enable --now, or a schtasks create). A unit file
that differs is kept beside it as .bak.
uninstall stops and removes them; a unit that was never installed is
reported, not an error.
status says for each of the three: installed or not, running or not.

--dry-run prints what would be written and run and changes nothing.
Exit 1 on bad flags, 2 when a system command fails, with its output.`,
		Sub: []*Command{
			{
				Name:    "install",
				Summary: "install and start the services: the node, plus the gateway and harbor when asked",
				Args:    "[--gateway] [--harbor] [--dry-run] [--format F]",
				Help: `Write the unit files and start the services. On Linux the files are the
contrib/ systemd units with your binary's path substituted, written to
~/.config/systemd/user/; a file that differs is kept as .bak, and
systemctl --user daemon-reload and enable --now follow. On Windows a
logon task is created per service under \Berth in Task Scheduler.

--dry-run prints the files and the commands it would run, and changes
nothing.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runServiceInstall,
			},
			{
				Name:    "uninstall",
				Summary: "stop and remove the services",
				Args:    "[--dry-run] [--format F]",
				Help: `Stop and remove every service the install could have written: the node,
the gateway and the harbor. On Linux the unit files are removed and
systemd is reloaded; on Windows the tasks are deleted. A service that
was never installed is reported, not an error.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runServiceUninstall,
			},
			{
				Name:    "status",
				Summary: "the services: installed or not, running or not",
				Args:    "[--format F]",
				Help: `Report the three services, the node always and the gateway and harbor
when installed: whether the platform says each is installed and whether
it is running. On Linux, installed means the unit file is where the
install writes it and running means systemctl calls it active; on
Windows, installed means Task Scheduler knows the task and running
means its state is Running. Read only: it changes nothing.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runServiceStatus,
			},
		},
	},
	{
		Name:    "status",
		Summary: "leases, waiters, squatters, the engines and the board",
		Args:    "[--socket P] [--format F]",
		Help: `Ask berthd for its view: the cards, who is docked on them, who is waiting and
why, what holds memory without a lease, the engines found, and the collisions
board. Read only: nothing here changes the node.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runStatus,
	},
	{
		Name:    "pair",
		Summary: "let a phone or browser switch and stop engines from the harbor",
		Args:    "[--url URL] [--list] [--revoke ID] [--revoke-all]",
		Help: `Print a one-time link, and its QR code, that pairs a device with this node's
harbor. Open it on the phone (or scan the code) and tap Pair this device: the
browser keeps a device cookie for a year, and the harbor lets it switch and
stop engines. Reading the harbor needs no pairing.

The code works once, for 15 minutes. --url is the harbor's address as the
device reaches it, such as https://box.tailnet.ts.net behind tailscale serve.
--list shows the paired devices; --revoke ID unpairs one and --revoke-all
unpairs every device.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runPair,
	},
	{
		Name:    "stop",
		Summary: "stop an engine on purpose: its leases drain through their own wrappers",
		Args:    "ENGINE [--wait] [--socket P] [--format F]",
		Help: `Ask berthd to stop the named engine. Every active or draining lease of it,
by name or engine family, is asked to give the card back the way a
higher-priority waiter would: the lease's own wrapper runs the normal
stop ladder (its job boundary, its unload, SIGTERM, then SIGKILL), and
the lease frees only after a card read shows the engine's memory gone.

A pinned lease is asked too: the pin keeps the scheduler off it, and a
person who says stop means stop. What is not asked: an engine Berth
never leased (hand-started, or a run that lost its daemon and never
re-attached) is not Berth's to stop, and it is refused with its name. A
lease that is already stuck after release is not stoppable either; berth
reclaim names what it holds and, with --yes, stops it.

--wait follows the event log until every asked lease is freed or stuck.
It exits 0 when all are freed, 2 when any is stuck, naming it and the
reclaim that finishes the job.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runStop,
	},
	{
		Name:    "storage",
		Summary: "the model roots: their filesystems, space, floors and shelves",
		Args:    "[--dir D]... [--format F]",
		Help: `Check each model root: the filesystem it sits on, the space it has left,
what it supports (hard links, symlinks, reflinks), which other operating
systems it is shared with, and how much of it each shelf (text, image,
audio) holds. A root that is under its floor marks the check failed; the
floor is [library] free_floor, a tenth of the root by default.

Roots come from --dir, else the [models] dirs and the [library] roots in
the config, else the usual stores. The catalog age is how long ago a berth
last scanned the root.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runStorage,
	},
	{
		Name:    "update",
		Summary: "check for a newer berth, or replace this binary with it",
		Args:    "[--check] [--yes] [--force]",
		Help: `With --check, says whether a newer release exists; exit 0 either way.
Without it, downloads the release's binary for this machine and its
SHA256SUMS, checks the sha256, and replaces the running binary in place
after confirmation (--yes skips the question). The new file is written
beside the running binary, fsynced and renamed over it; on Windows the
running .exe is moved aside to .old first, since it cannot be
overwritten. Berth never restarts anything: the output names what to
restart. The check is one GET to the GitHub API, cached 24 h; with
[update] check = false Berth on its own never contacts GitHub, and this
command is the operator asking. A dev build is refused unless --force.`,
		Run: runUpdate,
	},
	{
		Name:    "verify",
		Summary: "re-check pulled models against their recorded hashes",
		Args:    "[DIR...] [--dir D] [--format F]",
		Help: `Re-hash every file berth pull fetched and compare it with the size and
sha256 in its manifest. No DIR checks every pulled model under the models
directory. Exit 1 when a file is missing, truncated or changed; pulling the
same reference again repairs only what does not match.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runVerify,
	},
	{
		Name:    "version",
		Summary: "print the version",
		Args:    "[--format F]",
		Help:    `The release, the Go toolchain it was built with, and the wire version it speaks.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runVersion,
	},
	{
		Name:    "vfio",
		Summary: "pass the card to a VM through VFIO: watch it, wrap the switch script",
		Help: `Berth drains the engines, holds the card while your own script switches it to
vfio-pci and runs the VM, and refuses to let the next engine load until the card
is back on its host driver and answering nvidia-smi. Berth never switches a
driver itself, never asks for a password, and never stops a process: your script
(sudo, virsh) owns the switch and the VM. Experimental, Linux only;
docs/vfio.md has the requirements and an example script.`,
		Sub: []*Command{
			{
				Name:    "run",
				Summary: "hold the card, run the switch script, wait for the card back",
				Args:    "[--gpu CARD] [--reason NAME] [--wait D] [--before CMD] [--after CMD] -- CMD [ARGS...]",
				Help: `Acquire the VFIO hold the way berth hold does: engines drain at their
boundaries and none starts. Then the script runs with the terminal attached, so
its sudo prompt reaches you, and with BERTH_VFIO_DEVICES set to the card's PCI
functions, so it hardcodes no address. --gpu names the card on a node with
several (gpu1 or its PCI address); without it the run takes the node's first
card.

--before runs once the engines are off the card: stop your own services that
hold it there. --after runs once the hold has ended, whenever --before ran,
so what --before stopped always comes back, never while the card is out.

Before the script starts, berthd stops its own probe and the openers are
re-read: any process holding /dev/nvidia* or the card's DRM nodes refuses the
run, names every PID and node, releases the hold and exits 1. Berth never stops
a process.

When the script exits, the hold ends only when every function of the card is
off vfio-pci and the card answers nvidia-smi again (up to a minute). Until
then the hold stays, the message names the function that is out and what to do,
and Ctrl-C rechecks: it releases only when the card is back, so the next engine
never loads onto a card that is out.`,
				Run: runVfioRun,
			},
			{
				Name:    "status",
				Summary: "the card's VFIO state: IOMMU, group, drivers, openers, berthd's view",
				Args:    "[ADDR] [--socket P] [--format F]",
				Help: `Print the card's VFIO state and change nothing: whether IOMMU is on, the card's
IOMMU group and the driver each function in it is bound to (anything but the
card's own functions blocks the passthrough), the processes holding the card's
device nodes open, and what berthd reports about the card.

ADDR is the card's PCI address, as in 0000:01:00.0. Without it the card berthd
leases is used, which needs nvidia-smi to answer; nvidia-smi cannot see a card
bound to vfio-pci, so pass the address while the VM runs.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runVfioStatus,
			},
		},
	},
	{
		Name:    "wire",
		Summary: "make another engine's whole roster run under berth",
		Help: `An engine that loads its own models only waits for the card if its model
starts run through berth. Wire edits that engine's config so every model in
the roster does, in one pass, and stays re-runnable as the roster changes.`,
		Sub: []*Command{
			{
				Name:    "llama-swap",
				Summary: "wrap every model's cmd in berth run",
				Args:    "--config PATH [--api URL] [--bin P] [--group G] [--priority P] [--exclude PAT] [--update] [--write|--check] [--format F]",
				Help: `Reduce the whole roster of a llama-swap config so every model starts through
berth: each model's cmd gets one wrapper line, ` + "`berth run --as NAME ... --`" + `,
and the model's original command follows unchanged. llama-swap loads a model on
the first request that names it, so without the wrapper a whole-card load
cannot wait for the card; that is how an unwired model dies against whatever
else holds it.

The default run prints the plan and writes nothing. --write applies it and
keeps a timestamped backup beside the file; llama-swap reloads a config it
watches by itself. --check writes nothing and exits 1 when any model still
needs a wrapper, which is what a timer or a CI job runs.

Re-run it whenever the roster changes; already wired models are left alone.
--update also rewrites existing wrapper lines whose flags no longer match.
Models whose cmd is a single-line scalar are skipped; make it a > block first.`,
				Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
				Run:     runWireLlamaSwap,
			},
		},
	},
}
