# Wiring engines through Berth

Berth does two separate things with an engine, and you can use either without the other:

1. **Leasing.** An engine started through `berth run` holds a lease. Berth decides when it
   may load, asks it to unload when something else needs the card, and waits for the driver to
   show its memory free before letting the next engine in. Anything on the card that was not
   started this way is reported as a stowaway and left alone.
2. **Reading.** The daemon reads each engine's own API for models, tokens, queue and context.
   Engines on their default ports are found on their own; anything else goes in `berth.toml`.

`berth run` is fail-open: if the daemon is down it warns, starts the engine anyway and records
it as an unprotected run. If the daemon restarts while an engine runs, `berth run` re-attaches
the engine to the new daemon within a few seconds.

## The pattern

```sh
berth run --as <name> --priority <batch|interactive|foreground> [--needs-mib N] \
  [--drain-post URL] [--drain-comfyui URL] [--wait 10m] [--gpu gpu1] -- <the engine's own command>
```

- `--priority`: an equal or higher request drains the holder; a lower one waits.
- `--needs-mib`: leave it out to take the whole card. Set it when engines can share the card
  (for example vLLM with `--gpu-memory-utilization`).
- `--drain-post`: a URL Berth POSTs first when it wants the card back, before any signal.
- `--drain-comfyui`: wait for ComfyUI's queue to empty before stopping it.
- `--gpu`: the card to lease on a machine with several, such as `gpu1`. `gpu0,gpu1` holds one lease on both.
  The engine starts with `CUDA_VISIBLE_DEVICES` set to the card's UUID unless its environment sets it. With
  no `--gpu`, the lease goes to the first card and the environment is left alone.
- With no command after `--`, `berth run --as NAME` runs the `start` of NAME's `[[engine]]` entry in
  berth.toml. Every setting no flag sets comes from that entry too.
- On drain, Berth tries the drain step, then SIGTERM to the engine's process group, then
  SIGKILL, with `--grace` (30 s) between steps.

An engine that should come back after it was drained (ComfyUI, Ollama, a vLLM service) runs
under a service manager with `Restart=always`; the restarted `berth run` queues for the card
and starts the engine when it is granted. Give it a long `--wait` so it does not give up.

## llama-swap

Every model waits for the card only if its own `cmd` runs through `berth run`. Wire the whole
roster in one pass instead of editing one model at a time:

```sh
berth wire llama-swap --config /etc/llama-swap/config.yaml --write
berth wire llama-swap --config /etc/llama-swap/config.yaml --check  # exit 1 when unwired
```

`--wait 10m` writes `--wait` on every line, for a batch job that has to queue behind a long foreground
lease; without it a request waits berthd's default (120 s). `--write --update` rewrites the flags wire
writes on lines that already exist and keeps any flag you added by hand, such as `--needs-mib 14000`,
in the order you wrote it. A hand-written `--wait` stays unless you pass `--wait`.

It keeps a backup beside the file and stays re-runnable when the roster changes; point `--api`
at llama-swap's base URL when it listens elsewhere than `http://127.0.0.1:9090`. Under the
hood each model's `cmd` gains one wrapper line, and llama-swap's own unload URL is the drain so
llama-swap's state stays consistent. The unload waits until the model's `/slots`, which llama-swap
serves under `/upstream/<model>`, show no request in flight:

```yaml
models:
  "qwen3-30b":
    cmd: >
      berth run --as qwen3-30b --group llama-swap --priority foreground
      --drain-post http://127.0.0.1:8080/api/models/unload/qwen3-30b
      --drain-slots http://127.0.0.1:8080/upstream/qwen3-30b --
      llama-server --port ${PORT} -m /models/qwen3-30b.gguf --metrics
```

llama-swap stops a model with SIGTERM; `berth run` forwards it to the engine's process group.

By default llama-swap runs one model at a time and unloads the old model itself when a request names
another. Berth sees that as the old model giving the card back, so `min_turn`, the engine ranking and
the juggle guard never decide between two llama-swap models. Two clients on two models then swap the
card on every request. Put the wired models in a group with `swap: false` so a request for another
model waits for a Berth lease instead, and the old model unloads through its drain once Berth says so:

```yaml
healthCheckTimeout: 600   # longer than min_turn plus the slowest model's load
routing:
  router:
    use: group
    settings:
      groups:
        "chat":
          swap: false
          exclusive: false
          members: ["qwen3-30b", "gemma-27b"]
```

While a model waits for the card, llama-swap's `healthCheckTimeout` keeps the request open. If the
timeout is shorter than the wait, llama-swap gives up on the start and the request fails. `berth wire
llama-swap` names every group where llama-swap still swaps wired models itself. Models whose learned
size fits beside each other (`learn_size`) now share the card instead of swapping.
Reading: `type = "llama-swap"` follows every running model's upstream metrics, which exist only
when the model's `cmd` starts `llama-server` with `--metrics`; without it the engine stays up and
the harbor shows n/a for decode, prefill and token totals.

## llama.cpp (`llama-server`), NInfer, KoboldCpp

```sh
berth run --as llama-server --priority interactive -- llama-server -m model.gguf --port 8080 --metrics
```

Start `llama-server` with `--metrics` for token counters and queue; `/slots` (on unless started
with `--no-slots`) gives the live KV pool. Reading: `type = "llamacpp"` (`ninfer` for
[NInfer](https://github.com/T-Crypt/ninfer-4090), `koboldcpp` for KoboldCpp).

These engines have no offload call, but they can park in RAM through NVIDIA's `cuda-checkpoint`. Add
`--park checkpoint --front 127.0.0.1:${PORT}` to the `berth run` line of a llama-swap model, and start
the engine on `{{BERTH_PORT}}`. [Park in RAM](https://github.com/T-Crypt/berth/blob/main/docs/gateway.md#park-in-ram-warm-switch)
has the full line.

## Ollama

Ollama is one server for many models. Run `ollama serve` itself under `berth run` (a systemd
override of `ollama.service`), so a drain stops the server and every model with it:

```ini
# systemctl edit ollama
[Service]
ExecStart=
ExecStart=/usr/local/bin/berth run --as ollama --priority interactive --wait 87600h -- /usr/local/bin/ollama serve
Restart=always
RestartSec=5
```

Reading: `type = "ollama"` (found on :11434 without config).

## ComfyUI

ComfyUI keeps models in memory between jobs, so Berth hands its card over at a **job
boundary**: on drain it waits until `/queue` shows nothing running and nothing pending (no time
limit; a job is never cut off), then stops ComfyUI. `contrib/comfyui.service` runs it as a user
service that re-queues for the card after every handoff:

```sh
berth run --as comfyui --priority batch --wait 87600h --drain-comfyui http://127.0.0.1:8188 -- \
  python main.py --listen 127.0.0.1 --port 8188
```

If a ComfyUI node calls another engine (a chat panel, a prompt expander), that call can drain
ComfyUI while the node waits for its answer, and stopping the server kills the request. Set
`keep_comfyui_up = true` and a short `park_ttl` on the ComfyUI engine entry, as
[the gateway docs](https://github.com/T-Crypt/berth/blob/main/docs/gateway.md#comfyui-nodes-that-call-other-engines)
show.

Reading: `type = "comfyui"` (found on :8188 without config) shows jobs running and queued.

## vLLM and SGLang

```sh
berth run --as vllm --priority interactive -- vllm serve Qwen/Qwen3-8B --port 8000
berth run --as sglang --priority interactive -- python -m sglang.launch_server --model-path Qwen/Qwen3-8B --port 30000
```

Both preallocate most of the card. To share it, cap them (`--gpu-memory-utilization 0.5`,
`--mem-fraction-static 0.5`) and pass a matching `--needs-mib`. Reading: `type = "vllm"` /
`"sglang"`; counters need `/metrics` (vLLM serves it by default, SGLang with `--enable-metrics`).

## Engines in Docker or Podman

A containerized engine is parented by containerd-shim, not by the `docker run` you
started, so the process tree cannot reach it and Berth cannot attribute its VRAM on
its own. `--container` closes that: the wrapper resolves the name you gave to the
full container ID (docker first, then podman), attaches it to the lease, and every
GPU process inside that container belongs to the lease from then on. On a drain the
wrapper runs `docker stop -t 30 vllm` (the image's own SIGTERM, then SIGKILL) instead
of signalling the wrapper's process group; if the stop fails it falls back to the
signals. Linux and Windows both work: the runtime CLI is the same on each.

The container is Berth's because this command started it, and only that container is
ever stopped. A container Berth did not start stays a stowaway: reported with its
container ID, never signalled.

```sh
# start and lease it (the wrapper keeps running, attached to the container's output)
berth run --as vllm --container vllm --drain-slots http://127.0.0.1:8000 \
    -- docker run --rm --name vllm --gpus all -p 8000:8000 \
       vllm/vllm-openai:latest --model Qwen/Qwen3-8B

# an existing container: --name is already stable, -a keeps the output attached
berth run --as vllm --container vllm --drain-slots http://127.0.0.1:8000 \
    -- docker start -a vllm
```

`--rm` with `--name` is what makes the name stable and reusable: the container is
gone when it stops, so the next `docker run --name vllm` takes the name again. The
same settings work as `container = "NAME"` on the engine's `[[engine]]` block in
berth.toml (a `--container` flag wins). Podman engines read the same:
`podman run --name vllm ...`; Berth resolves and stops with podman when docker is
not installed.

## A Compose project as one engine

A lease names containers, plural, so an engine that is a Compose project fits as it
is. PaddleOCR runs an OCR service beside a detection sidecar and both want the GPU; a
tagger runs beside a Tailscale sidecar that carries its network. Point Berth at the
Compose file and the whole project is one engine:

```toml
[[engine]]
name    = "pet-tagger"
type    = "custom"
url     = "http://127.0.0.1:8000"
health  = "/health"
start   = ["docker", "compose", "-f", "/srv/tagger/compose.yaml", "up"]
compose = "/srv/tagger/compose.yaml"
needs_mib = 2560
```

The path is absolute, and an entry sets `compose` or `container`, not both: a project
names its own containers. `berth config check` refuses both other shapes. `berth run
--compose PATH` is the run-line form. Your start command stays yours, and `docker
compose -f PATH up` is the one that fits: it stays in the foreground while the project
runs and exits after a stop, so the run is the project's lifecycle.

On every poll Berth asks the runtime which containers that file is running
(`docker compose -f PATH ps -q --all`, podman when docker does not answer) and names the
set on the lease. Every service's GPU processes then belong to the lease, so the second
CUDA container is its engine's memory rather than a stowaway, and a container that
restarts under a new ID comes back to the lease on the next poll. A drain runs `docker
compose -f PATH stop -t 30`. It never runs `down`: that deletes the containers, networks
and volumes the project owns.

Windows reads the same file and runs the same two commands. One limit comes with WSL2:
the driver reports no per-container VRAM there, so a container's memory arrives as the
WSL VM's single process row, with the VM's whole dedicated usage. While a container
engine is the only one on the card, Berth counts that row as its lease, which then counts
the larger of its `needs_mib` and the row, and the VM stops showing as a stowaway. With
two container engines at once Berth cannot split one row between them: each counts its
`needs_mib`, and the row is reported once as a stowaway.

Size `needs_mib` from your heaviest run. A batch engine's footprint follows its batch: one
operator's photo tagger measured 2.1 GiB on a small scan and 6.7 GiB on its biggest, and
the larger figure is the one that keeps the next engine from being over-committed.

## Strata

[Strata](https://github.com/T-Crypt/strata) fills the card and pins about 40 GiB of RAM, so it is a
whole-machine engine. Give its entry `type = "strata"`, because the `llamacpp` reader names no model and
Strata's requests would never reach the token ledger or the leaderboard. With `type = "strata"` each request
lands under the model `/health` names.

```toml
[[engine]]
name          = "strata"
type          = "strata"
url           = "http://127.0.0.1:8080"
whole_machine = true      # berthd stops what it drains and evicts parked engines before the grant
```

## Everything else

TabbyAPI, LocalAI, LM Studio, Unsloth Studio, FreeToken and any OpenAI-compatible server lease the same
way. Wrap the command that starts them. Their readers are `tabbyapi`, `localai`, `lmstudio`, `unsloth`,
`freetoken` and `openai`.

An engine with no reader of its own uses `type = "custom"`, with any of:

```toml
[[engine]]
name    = "my-engine"
type    = "custom"
url     = "http://127.0.0.1:7000"
health  = "/health"        # decides up/down
models  = "/v1/models"     # OpenAI-style list
metrics = "/metrics"       # Prometheus text, read through the map
slots   = "/slots"         # llama.cpp-style KV slots
api_key_env = "MY_ENGINE_KEY"   # bearer token, read from this variable
[engine.map]
output_tokens = "myengine_generated_tokens_total"
prompt_tokens = "myengine_prompt_tokens_total"
waiting       = "myengine_requests_waiting"
```

## Engines on other ports and other machines

Every engine type takes any URL, so a non-default port is just `url = "http://127.0.0.1:9123"`.
Discovery reads the default ports on this machine every minute; to widen it:

```toml
[engines]
discover       = true                 # default; false reads only [[engine]] entries
discover_ports = [9123, 18080]        # also probe these, with every reader
discover_hosts = ["192.168.1.40"]     # and the same ports on these hosts
```

Only engines on this machine can hold a lease from this machine's daemon; engines elsewhere are
read for metrics only.
