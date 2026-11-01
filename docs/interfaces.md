# Interfaces

- **CLI:** one command tree with one output contract. `berth status`, `berth events -f`,
  `berth run`, `berth hold`, `berth stop`, `berth cordon|uncordon`, `berth notify`, `berth claims`, `berth pull`, `berth verify`, `berth models`, `berth gateway`,
  `berth wire llama-swap`, `berth engine check`, `berth config check`, `berth config show`,
  `berth export`, `berth key`, `berth skill install|show`, `berth update`, `berth version`,
  and for the node itself `berth daemon`, `berth harbor`, `berth service install|uninstall|status`,
  `berth reclaim`, `berth resume`, `berth prefetch`, `berth storage`, `berth civitai search|versions`
  and the ComfyUI suite under `berth engine install|show|update|rollback|remove`.
  Berth runs fully headless.

  ```sh
  berth wire llama-swap --config cfg.yaml          # plan: which models lack a wrapper
  berth wire llama-swap --config cfg.yaml --write  # apply; a backup stays beside the file
  berth wire llama-swap --config cfg.yaml --check  # exit 1 when a model is unwired
  berth wire llama-swap --config cfg.yaml --write --update --wait 10m  # rewrite wire's flags, keep yours
  berth config check --strict          # a warning fails the check, exit 1
  berth config show --format toml      # the evaluated config, ready to save
  berth engine check -v                # what each engine reports, right now
  berth -q status; echo $?             # exit codes only, for CI
  berth status --format json | jq .leases
  ```

  Results go to stdout, diagnostics go to stderr. `--format` is `human` (the default), `json` or
  `yaml`, and `berth config show` also takes `toml`, the config's own file format. `-v` adds
  detail to the human view, and `-q` suppresses stdout and leaves the exit code as the answer.
  Exit codes are 0 (done), 1 (validation or logic: an invalid config, a warning under `--strict`,
  an engine that did not answer, a lease the broker refused) and 2 (berth could not do its job:
  unknown flag, missing file, berthd unreachable). `berth run` passes the engine's own status
  through and exits 75 when no berth was granted. Every read-only command changes nothing. When berthd
  governs more than one card, `berth status` adds a CARD column and `berth events` names the card on each
  line. `--format json` carries the card on every event.
- **Harbor web UI** (`berth harbor`): a departures board of every lease (the HOT / WARM
  tiers included: WARM marks a parked engine, with its parked RAM and age), a boat ticket
  per lease, the collisions counter, berths with measured memory, stowaways, the event
  log, and engine metrics. The tide (card memory by holder) and the model timeline share a
  1H / 6H / 24H / 7D / 30D picker; the Engines tab adds the library (every model on the
  machine with its shelf, quant, size, cached tier, the engine serving it and that
  engine's EST. VRAM, with the terminal commands one row of copy buttons away), a queue
  KPI (the berth's own queue; each engine's own queue stays on its card), the KV pool (the live slots of each
  engine against their model windows, stored ranges the per-minute peak slot fill), context
  residency (the last requests' token figures against the model's window, plus the live
  KV sessions), a throughput chart, a 24H / 7D / 30D leaderboard and, in advanced mode, a
  storage panel. The top bar shows the overall state (LIVE / DEGRADED / OFFLINE) and a
  strip of the alerts currently breaching, colored from the node's own threshold state.
  Basic and advanced modes. While its update check is on and a newer
  release exists, the header shows it: the release, a link to it, and a
  button that copies `berth update`, the command that does the replacing.
  Read-only, strict content security policy. It listens on loopback by default
  (`127.0.0.1:9798`); passing a non-loopback `--listen` address (e.g. `0.0.0.0:9798`;
  a specific interface IP works too) is the explicit opt-in to expose it on your
  network: the startup logs a warning and the page shows a banner on every screen while
  exposed. Reading needs no login: expose it only on a network you trust, where phones get
  the full dashboard. Switching or stopping an engine on the SWITCH tab needs a device paired
  with `berth pair`. To reach it from elsewhere
  without exposing it, keep the loopback listener and put a same-machine reverse proxy in
  front (`tailscale serve`, caddy), passing the proxy's hostname with `--allow-host`. A systemd user unit is
  in `contrib/berth-harbor.service`.
- **Prometheus** `/metrics` for Grafana and friends, on the `[metrics] listen` address. It
  serves loopback only and refuses a `Host` that is not a loopback name, so a web page cannot read
  it through DNS rebinding. The endpoint names your engines, leases and queue, so Berth never
  serves it off the machine. To scrape it from a Prometheus on another host, keep the endpoint
  as it is and either:
  - run a Prometheus agent or Grafana Alloy on the Berth machine, scrape `127.0.0.1`, and
    `remote_write` to your Prometheus; or
  - put Caddy or nginx on the same machine in front of it, sending a loopback `Host`
    (`header_up Host 127.0.0.1`, `proxy_set_header Host 127.0.0.1;`), with the proxy's own
    allowlist or auth on the network side. `tailscale serve` passes the tailnet name as `Host`,
    which `/metrics` refuses.

  Every GPU series carries `card` (`gpu0`), so a machine with several cards reads per card.
  - `berth_gpu_memory_{total,used,free}_bytes{gpu,card,name,scheduled}`
  - `berth_gpu_baseline_bytes{gpu,card}`
  - `berth_gpu_utilization_ratio`, `berth_gpu_temperature_celsius`, `berth_gpu_power_watts`,
    `berth_gpu_power_limit_watts` and `berth_gpu_fan_ratio`, each `{gpu,card}`. A card that does not
    report a value has no series for it.
  - `berth_leases{card,state}` for the states active, draining, parking, parked, releasing and stuck
  - `berth_lease_used_bytes{card,engine,priority,state}`, summed per label set
  - `berth_waiting{card}`, `berth_squatters{card}` and `berth_squatter_bytes{card}`
  - `berth_build_info{version}`, always 1
- **Footprint:** the node uses about 0.2% of one CPU core and ~21 MB of memory on the
  reference box; the harbor about 0.4% and ~23 MB, writing to disk once a minute.
