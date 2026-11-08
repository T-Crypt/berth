---
name: comfyui
description: Use when asked to generate, edit or upscale an image or video with ComfyUI on a machine that runs Berth, to set up ComfyUI so an agent can generate on its own, to run or tune a ComfyUI workflow from the terminal, or when a ComfyUI render fails with out-of-memory, "VRAM grow failed" or a ComfyUI that stopped answering.
---

# ComfyUI under Berth

ComfyUI shares the GPU with chat models, games and other engines on this machine. Berth decides
who holds the card. Generate through Berth and ComfyUI wakes when you ask for it, waits its turn,
and hands the card back when a chat model needs it. Talk to ComfyUI behind Berth's back and your
render loads onto a card another engine holds.

Load the `berth` skill too: it covers `berth status`, leases and what never to kill.

## First, look

```sh
berth status                      # who holds the card; comfyui HOT, WARM (parked in RAM) or absent
berth engine check -v             # every engine, ComfyUI's queue and version included
berth config show                 # the [[engine]] entries; find the comfyui one and its url
```

A WARM (parked) ComfyUI still answers HTTP on its own port with no models on the card. Never read
"its /system_stats answered" as "ready to render".

## Set it up once

Skip a step that `berth status` and `berth config show` already show done.

1. **Install or adopt ComfyUI.** `berth engine install comfyui --adopt ~/ComfyUI` records an
   existing checkout. `--path DIR` clones a fresh one, and PyTorch for the user's GPU is then the
   user's step. Berth writes `extra_model_paths.yaml` so ComfyUI reads the node's model library.
2. **Give the gateway a ComfyUI entry** in `~/.config/berth/berth.toml` (`berth config check`
   after editing):

   ```toml
   [[engine]]
   name     = "comfyui"
   type     = "comfyui"
   url      = "http://127.0.0.1:8188"
   start    = ["comfy", "--workspace", "/path/to/comfy", "launch", "--", "--listen", "127.0.0.1", "--port", "8188"]
   park     = "hook"      # a drain keeps the models in RAM instead of stopping ComfyUI
   on_busy  = "queue"    # a prompt while a game or a VM holds the card waits on disk
   ```
3. **Serve ComfyUI's port through Berth.** Start the gateway with
   `--comfyui-listen 127.0.0.1:8189` (in its systemd unit or Windows service). Port 8189 looks
   like ComfyUI's own: the web UI, API, websocket and custom node routes pass through, and a
   queued prompt first wakes or starts ComfyUI under its lease. Point every client at 8189,
   yourself included. A LAN or tailnet forwarder for ComfyUI should point at 8189 too.
4. **Models.** `berth civitai search "QUERY"` and `berth pull civitai:VERSION_ID` for checkpoints
   and LoRAs, `berth pull hf:ORG/REPO[:FILE]` for Hugging Face. `berth models` lists the library.

Ask the user before steps 1 to 3: they change the machine's services.

## Generate

ComfyUI runs API-format workflows: a JSON object of nodes keyed by id, each with `class_type`
and `inputs`. Export one from the web UI with "Export (API)", or start from a workflow the user
keeps. Set the prompt, seed, size and steps by editing node inputs.

```sh
COMFY=http://127.0.0.1:8189   # Berth's ComfyUI port; never :8188 directly
id=$(curl -s -X POST "$COMFY/prompt" -H 'Content-Type: application/json' \
  -d "{\"prompt\": $(cat workflow-api.json)}" | jq -r .prompt_id)
until curl -s "$COMFY/history/$id" | jq -e --arg id "$id" '.[$id].outputs' >/dev/null; do sleep 2; done
curl -s "$COMFY/history/$id" | jq -r --arg id "$id" \
  '.[$id].outputs[].images[]? | "\(.subfolder)/\(.filename)"'
curl -s -o out.png "$COMFY/view?filename=FILE&subfolder=SUB&type=output"
```

- The first prompt after a chat model held the card waits for that model's reply to finish and
  for its unload. Expect seconds, not an error.
- A `prompt_id` that starts with `berth-` means the card is away (a VM, a game, a hold) and the
  job waits on disk. `/history/<berth id>` answers `{}` until it has run.
- `node_errors` in the answer names the node and input ComfyUI rejected. Fix that input; don't
  resubmit unchanged.
- ComfyUI caches a workflow it has already run. The same JSON with the same seed returns the old
  result at once and loads nothing. Change the seed for a new image.

## Many jobs in a row

Before you queue a run of jobs (a set of images, a sheet, a video in passes), start a batch:

```sh
berth batch comfyui
# over the network: curl -s http://127.0.0.1:9799/v1/berth/batch \
#   -H "Authorization: Bearer $(berth key)" -d '{"engine":"comfyui"}'
```

Without one, every chat turn between two jobs takes the card back, and each job reloads its
models. With one, ComfyUI keeps the card until its queue has been empty for 30 s (`--grace`), then
hands it on by itself. Chat requests wait for the batch instead of draining it. Submit the jobs
back to back, or queue them all at once: a gap longer than the grace ends the batch. `berth batch
comfyui --end` hands the card on now. One image needs no batch.

## Hand the card back

You don't release anything by hand. When a chat model asks for the card, Berth waits for
ComfyUI's queue to empty and parks it in RAM (`berth status` shows WARM), and your next prompt
on port 8189 wakes it. To give the card back sooner, run `berth stop comfyui`, which drains it
at its job boundary.

## Never

- Never start ComfyUI by hand (`python main.py`, `comfy launch`): it holds no lease, Berth reports
  it as a stowaway, and the next chat model fails to load beside it.
- Never post to ComfyUI's own port (:8188) when Berth's port exists. A parked ComfyUI accepts
  the prompt, loads onto a card another engine holds, and fails with "VRAM grow failed" or
  out-of-memory.
- Never kill a ComfyUI process to free the card. `berth stop comfyui` drains it at the job
  boundary.
- Never call `POST /free` yourself to make room. A drain does it at the right moment.

## When it fails

| You see | Do |
|---|---|
| connection refused on 8189 | the gateway runs without `--comfyui-listen`; check `berth status` and ask the user |
| 503 "ComfyUI is not running" on a GET | ComfyUI is down; POST a prompt to start it, or `berth resume comfyui` if it is WARM |
| out-of-memory or "VRAM grow failed" | something reached ComfyUI around Berth; `berth events --since 0` and look for `lease.unparked` |
| `systemctl --user start comfyui...` does nothing | a parked ComfyUI keeps its unit running; `berth resume comfyui` wakes it |
| the prompt waits a long time | `berth status`: a game, a hold, a VM or a pinned app has the card; tell the user who |
