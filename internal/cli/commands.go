package cli

// commands is the command tree. Rows are alphabetical, and a group (Sub) is a
// row like any other: dispatch, help and the command list all read this table.
var commands = []*Command{
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
		Name:    "daemon",
		Summary: "run the broker for one card (berthd)",
		Args:    "[flags]",
		Help: `Run the lease broker for one card: it hands the GPU to one engine at a
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
		Summary: "the web UI, read-only; loopback by default",
		Args:    "[flags]",
		Help: `Serve the read-only web UI: the card and its leases, the engines, the event
log, the request KPIs and the tide of card memory over time, persisted in
SQLite. It listens on loopback by default; a non-loopback --listen address is
your explicit opt-in to expose it, and there is no authentication in this
release.`,
		Run: runHarbor,
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
		Name:    "version",
		Summary: "print the version",
		Args:    "[--format F]",
		Help:    `The release, the Go toolchain it was built with, and the wire version it speaks.`,
		Formats: []Format{FormatHuman, FormatJSON, FormatYAML},
		Run:     runVersion,
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
