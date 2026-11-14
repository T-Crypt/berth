# The swap bench

`contrib/bench/swap` measures what a model switch costs on a live node,
through the Berth gateway, which is the supported path. Per cycle it asks
the gateway for model A once, then per cycle for model B and A again, so each request
drains the running engine and takes the other. For every request it
records:

- the time to the engine's first token, counted from when the request
  went out, so a cold start's drain and load are in the number. Lines
  starting with a colon are the gateway's keep-alive comments and do not
  count.
- the total time to the end of the stream.
- the card's used VRAM just before the request, and after the previous
  engine had drained (nvidia-smi, the same read Berth itself uses).
- the host's available RAM before and after (the kernel's `MemAvailable`,
  Linux only).
- the greedy output of one fixed prompt (temperature 0, seed 1, 32
  tokens), so a parked resume can be checked against the fresh run for
  identical output.

It takes the card through the gateway only. It starts or stops nothing
itself, and it does not touch berthd directly. Run it only when the card
can be given to the bench, and from an idle machine: the card should not
be holding either model when the run starts, so that each engine's first
request is a true cold switch.

## Run it

```sh
go run ./contrib/bench/swap -a MODEL_A -b MODEL_B -cycles 5
```

The defaults assume the gateway on loopback 9799 and the node's API key
in its usual place. The full flag set:

```
  -a string       model A
  -b string       model B (replaced by the -comfy workflow when that is set)
  -comfy string   path to a ComfyUI workflow JSON; its slot replaces model B
  -cycles int     how many B-then-A cycles after the opening A (default 5)
  -gateway string the gateway base URL (default "http://127.0.0.1:9799")
  -gateway-pid int the gateway's PID, for the RSS reading of -leak (Linux)
  -gpu-index int  the card the bench reads (default 0)
  -json           print the raw samples as JSON
  -key string     the gateway's API key file
  -leak int       run N A-B cycles and print VRAM after drain and the gateway RSS at cycle 1 and N
  -max-tokens int the max_tokens of each request (default 256)
  -no-think       send chat_template_kwargs enable_thinking=false, which llama.cpp-style servers honour
```

With `-comfy WORKFLOW.json` the B slot becomes a ComfyUI workflow posted
to the gateway's `/prompt`, timed until ComfyUI's `/history` entry shows
the job done. The post holds until the gateway has ComfyUI up under a
lease, so the time includes the drain and the start, as the model
switches do. There is no time-to-first-token for a workflow, and that
row reads `n/a`.

`-leak N` runs N A-B cycles and prints the card's used VRAM after drain
and the gateway's RSS, read from `/proc/<pid>/status` at the PID of
`-gateway-pid`, at cycle 1 and cycle N:

```sh
go run ./contrib/bench/swap -a MODEL_A -b MODEL_B -leak 100 -gateway-pid $(pgrep -f 'berth gateway')
```

Run it for 100 cycles or more: a leak shows as growth between the two
readings, and short runs average it out.

## Read the table

The human output is two tables, one per engine:

```
ENGINE      KIND   N   TTFT MEDIAN   TTFT P90
model-a     cold   1   41.20 s       41.20 s
model-a     warm   4   1.10 s        1.40 s
model-b     cold   1   107.00 s      107.00 s
model-b     warm   4   3.80 s        4.90 s

ENGINE      TOTAL MEDIAN   PEAK RAM DROP   VRAM HELD          OUTPUT IDENTICAL
model-a     12.40 s        0.4 GiB         21.9 GiB           yes
model-b     15.10 s        0.2 GiB         40.1 GiB           yes
```

- `cold` is the engine's first request in the run; `warm` is every
  switch back to it. The median and p90 are over that engine's samples
  of each kind.
- `PEAK RAM DROP` is the biggest `MemAvailable` fall between the before
  and after readings of one of the engine's requests. A parked engine's
  resume can draw on RAM the kernel had handed back, so this is where a
  park that does not fit would show.
- `VRAM HELD` is the median used VRAM of the card once the engine
  answered, next to the run's baseline (the card's used VRAM before the
  run started): the engine's footprint while it serves.
- Switching on every request is the ping-pong Berth's juggle guard holds
  back. A switch inside the juggle window waits for the holder, and that
  wait is part of the measured time.
- `OUTPUT IDENTICAL` says whether every run of that engine produced the
  same greedy answer. A parked resume that is not bit-identical to the
  fresh run is not the same model state, and this column is how you see
  it. The answer and the reasoning a thinking model streams first
  (`reasoning_content` or `reasoning`) both count. When every run of an
  engine came back empty, the column reads `n/a (empty)`: equal empty
  strings prove nothing. Give a thinking model room with `-max-tokens`, or
  ask it to skip the thinking with `-no-think`.

The parked tier needs no separate mode. When park is on for an engine,
its warm switches go through the park and resume path, and the warm row
is the parked number.

## What passes

The acceptance from the fast-swap design: for a footprint around 20 GB,
a warm resume answers under 5 s, the greedy output is identical between
the fresh run and the parked resume, and 100 cycles show no growth in
the card's used VRAM or the gateway's RSS. A cold switch of a 20 GB
model takes its full load time and is not part of the acceptance; the
park exists to remove it.

The numbers a run produces belong in the `Swap numbers` table of the
main README, through a pull request, or on the
[drain report issue form](https://github.com/T-Crypt/berth/issues/new?template=drain-report.yml)
when they came out of a problem. Record the card, the driver, the
models and their footprints, and whether park was on.
