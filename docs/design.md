# Design: one lease model for parks, lends, many cards and many machines

Berth started as one broker for one card: an engine leases the whole card or a fixed size, and a drain
stops it. Four features ask more of that model. The first three shipped in v0.4, and the last is planned:

- **Checkpoint parks.** NVIDIA's `cuda-checkpoint` moves an unmodified engine's VRAM into RAM and back.
  On an RTX 4090, NInfer's 23.1 GB comes back in 3.3 s, where a reload takes 14 s.
- **Lends.** An engine gives part of its memory on one card to another engine and keeps serving, then
  takes it back ([#189](https://github.com/T-Crypt/berth/issues/189)).
- **Many cards.** One berthd for every card, and one lease that covers several cards with a size on each.
- **Many machines.** Several nodes on one network, each serving its own cards, behind one endpoint.

This page is the model all four fit into, the rules none of them may break, and the order they ship
in. Code that adds one of them follows it; a change to the model changes this page in the same PR.

## The rules

Each rule below has cost a crash or a dead engine when it was missing.

1. **The driver decides.** Memory counts as free only after a driver read, taken after the event,
   shows it free. A release, a park, a checkpoint and a lend all wait for that read. An engine's word
   that it let go is not enough.
2. **No overlap.** Berth never grants memory a holder can still use. A holder grows back (a resume, the
   return of a lend) only after the memory it lent or left is confirmed gone from whoever used it.
3. **Own the request path before an engine sleeps.** Berth parks, checkpoints or shrinks an engine
   only when every request to it passes through a Berth front, or when the engine answers a request in
   its sleeping state without touching the card. Otherwise a request that arrives during the sleep
   either hangs (a checkpointed process blocks inside CUDA) or loads onto a card another engine holds
   ([#179](https://github.com/T-Crypt/berth/issues/179)), and Berth never sees it.
4. **Every sleep has a way back and a fallback.** A park or lend that does not free the memory within
   the release grace falls back to a drain. A wake that does not answer stops the engine. Nothing stays
   half asleep.
5. **State survives the daemon.** A run re-attaches with everything the broker needs to keep the lease
   true: parked, checkpointed, lent, and on which cards.
6. **Never signal what Berth did not start.** Unchanged from [leasing](leasing.md).
7. **Each card keeps its floor.** A grant, a lend or a resume leaves the card's reserve free.

## The data model

### Card

A card is named by its PCI address (`0000:05:00.0`), which survives renumbering and a passage to a
VM, and carries a short name (`gpu0`, or one the operator picks). Its budget is the driver's total
minus the reserve, the baseline, stowaways, claims, and the shares on it.

### Lease and share

A lease belongs to one engine run: its priority, its process (PID plus start time), its capabilities,
and one **share** per card it uses. A lease on one card has one share. A lease on five cards has five, which
berthd grants together and only when each fits.

A share is the lease on one card: a size (the whole card, or N bytes) and a state. The state is one
machine, so a share can never be parked and lending at once:

```
waiting ─▶ active ─┬─▶ draining ─▶ released ─▶ freed
                   ├─▶ parking ─▶ parked ─▶ resuming ─▶ active
                   └─▶ lending(N) ─▶ lent(N) ─▶ returning ─▶ active
```

- `parked` counts the residual the driver measures, nothing more.
- `lent(N)` counts the share's size minus N, and the borrower's share counts N.
- A share that loads more than it may while asleep (a client reached the engine around Berth) goes back
  to `active` and is reported (`lease.unparked`), so the next drain parks it again.

The broker, the scheduler and the board read shares. Before v0.4 a berthd governed one card, so every
lease held exactly one share and the model changed no behavior.

### Capabilities

An engine entry declares what its engine can do, and the broker uses nothing it did not declare:

| Capability | Values | Means |
|---|---|---|
| `park` | `hook` (adapter or Berth protocol), `checkpoint` | how its memory moves to RAM and back |
| `lend` | `hook` (Berth protocol) | it can give N MiB on a named card and take it back |
| front | the gateway, the ComfyUI port, `berth run --front` | where its requests enter, so rule 3 holds |

`park = "checkpoint"` requires a front, or rule 3 would not hold. A front is one run's own address
(llama-swap gives every model its own port), so `checkpoint` goes on a `berth run` line with `--front`,
and Berth refuses it on an engine entry, where every model run would share it.

## Fronts

A front is the only door to an engine. It forwards requests while the engine is awake. While the
engine sleeps it holds a request, asks berthd to resume the lease, waits for the wake, and then
forwards. It never answers for the engine.

- **The gateway** fronts the OpenAI routes and ComfyUI's API.
- **The ComfyUI port** (`berth gateway --comfyui-listen`) fronts ComfyUI's own surface, web UI included.
- **`berth run --front ADDR`** fronts one engine behind a router that Berth does not own, such as a
  llama-swap model: llama-swap sends to `ADDR`, and the engine listens on a port the run picks.

Before an engine sleeps, its front stops forwarding new requests and waits for the ones in flight to
finish. Only then does the park, checkpoint or lend begin.

**Why the run owns the front for a routed engine.** A llama-swap model could also be fronted by the
gateway (llama-swap's `proxy:` pointing at it), or by a hook inside llama-swap. The gateway would
become a single point of failure for every model, would need a registry of the ports llama-swap hands
out, and would need a protocol with the run to hold requests while the run parks. A hook means a fork
of llama-swap. A front inside `berth run` exists exactly while its engine does, uses the port llama-swap
already assigns, and sits in the same process as the park step, so "hold new requests, wait for the
ones in flight, then sleep" needs no protocol at all. With berthd down it still forwards, as `berth run`
already fails open.

A front has four states: **open** (forward), **closing** (hold new requests, wait for in-flight ones),
**asleep** (hold, and ask berthd for the card on the first request) and **waking** (hold until the
wake answers). A held request ends with the client's own deadline or the run's wait limit, with a
`503` that names the reason, never a hang. A read (`GET`, `HEAD`, `OPTIONS`) never wakes the engine:
monitors poll engines every few seconds, Berth's own engine reader included, and each poll would undo
the sleep. While the engine sleeps, the front answers a read with a `503` that says so.

## Making room

When a waiter does not fit on a card, the scheduler tries the cheapest way to make room first and
stops at the first that works:

1. **Fits now**: grant.
2. **Beside a parked residual**: grant beside it.
3. **Lend**: a holder that can lend gives the waiter's size on that card, and keeps serving.
4. **Park or checkpoint**: the holder sleeps in RAM, if the RAM budget admits it.
5. **Drain**: the holder stops.

Priorities, minimum turns, holds and the juggle guard decide whether the holder has to make room at
all; this list only decides how. Each step that fails within the grace falls through to the next.

## Many machines

Each machine runs one berthd for its cards and one gateway. A node's gateway lists its engines,
their capabilities and its measured switch costs. The network layer joins the gateways: a request to
any node goes to the node that can serve it soonest, or waits on the one that holds the model.
Leases never cross a machine: each berthd stays the only authority for its own cards, so rule 1 holds
locally. Nodes talk over the tailnet only, with keys, and the layer is off until the operator turns it
on.

## Order of work

Each stage ends in something a test and a live machine can prove, and leaves the next stage nothing
to undo.

| Stage | Ships | Proven by |
|---|---|---|
| A1, shipped in v0.4 | `park = "checkpoint"` with `berth run --front`, for llama-swap engines | a chat to a checkpointed NInfer through llama-swap restores it and answers; no request hangs |
| A2, shipped in v0.4 (experimental) | lends on one card through the protocol, with a stub engine | a sized borrower runs beside a lending holder; the return waits for the driver |
| B, shipped in v0.4 | one berthd for every card; leases with one share per card | a two-card engine leases both; the cards of a mixed box schedule apart |
| C, planned | parks and lends per share on multi-card leases | Maya on five cards lends from one card while the other four serve (#189) |
| D, v0.6 | gateways joined over the tailnet | a request to one node runs on another and the leases stay per node |

A stage that finds the model wrong changes this page first.
