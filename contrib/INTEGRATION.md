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
  [--drain-post URL] [--drain-comfyui URL] [--wait 10m] -- <the engine's own command>
```

- `--priority`: an equal or higher request drains the holder; a lower one waits.
- `--needs-mib`: leave it out to take the whole card. Set it when engines can share the card
  (for example vLLM with `--gpu-memory-utilization`).
- `--drain-post`: a URL Berth POSTs first when it wants the card back, before any signal.
- `--drain-comfyui`: wait for ComfyUI's queue to empty before stopping it.
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

It keeps a backup beside the file and stays re-runnable when the roster changes; point `--api`
at llama-swap's base URL when it listens elsewhere than `http://127.0.0.1:9090`. Under the
hood each model's `cmd` gains one wrapper line, and llama-swap's own unload URL is the drain so
llama-swap's state stays consistent:

```yaml
models:
  "qwen3-30b":
    cmd: >
      berth run --as qwen3-30b --group llama-swap --priority foreground
      --drain-post http://127.0.0.1:8080/api/models/unload/qwen3-30b --
      llama-server --port ${PORT} -m /models/qwen3-30b.gguf --metrics
```

llama-swap stops a model with SIGTERM; `berth run` forwards it to the engine's process group.
While Berth waits for the card, llama-swap's `healthCheckTimeout` keeps the request open.
Reading: `type = "llama-swap"` follows every running model's upstream metrics.

## llama.cpp (`llama-server`), NInfer, KoboldCpp

```sh
berth run --as llama-server --priority interactive -- llama-server -m model.gguf --port 8080 --metrics
```

Start `llama-server` with `--metrics` for token counters and queue; `/slots` (on unless started
with `--no-slots`) gives the live KV pool. Reading: `type = "llamacpp"` (`ninfer` for
[NInfer](https://github.com/T-Crypt/ninfer-4090), `koboldcpp` for KoboldCpp).

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

Reading: `type = "comfyui"` (found on :8188 without config) shows jobs running and queued.

## vLLM and SGLang

```sh
berth run --as vllm --priority interactive -- vllm serve Qwen/Qwen3-8B --port 8000
berth run --as sglang --priority interactive -- python -m sglang.launch_server --model-path Qwen/Qwen3-8B --port 30000
```

Both preallocate most of the card. To share it, cap them (`--gpu-memory-utilization 0.5`,
`--mem-fraction-static 0.5`) and pass a matching `--needs-mib`. Reading: `type = "vllm"` /
`"sglang"`; counters need `/metrics` (vLLM serves it by default, SGLang with `--enable-metrics`).

## Everything else

TabbyAPI, LocalAI, LM Studio, Unsloth Studio, FreeToken, [Strata](https://github.com/T-Crypt/strata)
and any OpenAI-compatible server lease the same way: wrap the command that starts them. Their
readers are `tabbyapi`, `localai`, `lmstudio`, `unsloth`, `freetoken`, `strata` and `openai`.

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
