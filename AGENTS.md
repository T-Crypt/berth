# AGENTS.md — Berth

Berth is the VRAM leasing service for every GPU you run. One Go daemon per machine
hands a single card to many inference engines in turn: engines lease the card,
the holder is drained, Berth waits until the driver reports the memory free, then
admits the next engine. Processes holding VRAM without a lease are stowaways,
reported, never touched. Linux + NVIDIA. Alpha.

## Commands

Run from the repo root (a worktree):

    # the one command that proves the tree is sound: build, format, vet, all tests
    go build ./... && test -z "$(gofmt -l .)" && go vet ./... && go test ./...

    # lint: the exact CI command; only `go` is needed on PATH
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...

    # build the binary (quick start: README.md)
    go build -o ~/.local/bin/berth ./cmd/berth

CI (`.github/workflows/ci.yml`) runs exactly those commands, in that order. Keep the
local proof command green before opening a PR; CI must end green at merge.

## Packages

- `cmd/berth` — CLI entrypoint: `main` and the release version only.
- `internal/broker` — the core: the lease table for one GPU, priorities, the drain
  ladder, process attribution, stowaway detection, the queue. In-memory only; never
  kills.
- `internal/cli` — the `berth` command tree: one flag set, one output contract
  (human, json, yaml) and one exit-code contract (0 done, 1 validation, 2 system)
  for every command.
- `internal/config` — TOML config (`~/.config/berth/berth.toml`), its defaults and its
  warnings; `berth config check` and `berth config show`.
- `internal/engines` — engine readers (every engine in the README's table, `custom`),
  port discovery by response fingerprint, and the 24 h request log.
- `internal/export` — the store's rows as CSV or JSON for the `berth export` command.
- `internal/gpu` — nvidia-smi polling (memory, compute apps) and each card's real
  limits.
- `internal/harbor` — the `berth harbor` web UI server and its collectors; UI assets
  live in `internal/harbor/web/`.
- `internal/history` — the node's last hour of samples in memory at 5 s resolution.
- `internal/host` — host telemetry: CPU, RAM, swap, load.
- `internal/insights` — request KPIs, token ledger, leaderboard; one implementation
  over the node's in-memory log and the harbor's SQLite rows.
- `internal/procs` — /proc parent-walk process attribution on Linux, a stub elsewhere.
- `internal/proto` — NDJSON message types over the Unix socket.
- `internal/runner` — the `berth run` wrapper: own process group, drain ladder,
  fail-open start.
- `internal/runs` — unprotected-run records (daemon down, or gone mid-run).
- `internal/server` — the Unix socket server: leases, event log fan-out, stowaway
  notifications, the loopback `/metrics` listener.
- `internal/store` — the harbor's SQLite store (modernc.org/sqlite; pure Go, no CGO).
- `test/ui` — dev-only Playwright click/hover check; not part of `go test ./...`,
  never in CI.
- `contrib/` — systemd user units, the llama-swap patch, INTEGRATION.md.

## What Berth never does (invariants — do not break them)

- Never kill a process it did not start, or any process by default. Stowaway
  handling is report-only.
- Never grant a card because a process exited; grant only after the driver shows
  the memory free.
- Never claim memory is reserved when it is only estimated to be free.
- Never take a card on another user's machine without their consent (on one machine, only the
  priorities you set decide).
- No cloud account, no telemetry upload, no other software needed to run.
- Process identity is PID plus start time; a reused PID never inherits a lease.
- `berth run` is fail-open: with the daemon down the engine still starts and is
  recorded as an unprotected run. Only `--fail-closed` refuses.
- The web UI listens on loopback by default; a non-loopback `--listen`
  address is the operator's explicit opt-in to expose it (warning at
  startup, banner on the page, no authentication in this release).
  `--allow-host` names a same-machine reverse proxy's hostname for a
  loopback harbor; the listener stays on loopback.
  `/metrics` stays loopback-only.
- Berth collects every stat itself (driver, /proc, the engines' own APIs). It has
  no dependency on Speculum or any other tool.

## Do not touch

- No package managers (Makefile, Taskfile, npm) and no JS CI stage; the native Go
  toolchain is the only harness. `test/ui` Playwright checks stay dev-only.
- Do not swap `modernc.org/sqlite` for a CGO sqlite driver; pure Go is a hard
  requirement.
- Do not pin a different Go or linter version than CI does: Go comes from `go.mod`
  (`1.27.1`), the linter from the `@v2.14.0` line in `AGENTS.md` and `ci.yml`.
  Bumping either means changing both in the same commit.
- An engine reader decides up/down only from what the engine serves unconditionally;
  optional endpoints (a `/metrics` that is off by default, a list behind a key) add
  detail and are never a liveness signal.

## Git

- One change per branch; deliver through a pull request against `main`. No
  force-push to shared branches.
- Commit subjects: `<type>(<scope>): <summary>` with type in feat, fix, refactor,
  test, docs, chore, ci, build (e.g. `fix(engines): read vLLM's window`).
- Keep the proof command and lint green before opening a pull request.
