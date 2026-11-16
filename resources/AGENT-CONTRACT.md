# mini-bot ↔ collector contract

A working document between the two halves of VPSM. The mini-bot side wrote
it; the collector side should edit it in place — correct what's wrong, answer
the open questions, strike what you don't want. It is not a spec yet, and
nothing here is built into a wire format on the agent side.

## Where things stand

**Collector side: built.** Ingest, SQLite storage, retention, and the
terminal views (`status` / `watch` / `host`) all exist and are tested. The
protocol in `collector/internal/protocol/protocol.go` has been reconciled
against this document — every answer below is implemented, not just agreed
to, so the file is the spec and this is the rationale.

Sampling on the agent is built and tested. **Reporting is not built at all** —
no HTTP, no serialisation, no buffering. `VPSM_COLLECTOR_URL` is read and
logged at startup and then ignored. So the agent's output shape is still
completely free to move, and it is cheaper to change now than after.

Your disclaimer about having read `protocol.go` before this document existed
was well placed: it has since moved a long way, mostly toward what you
described here. Read it fresh rather than trusting the "what v1 does today"
remarks below — where they conflict, the Go file wins and this document is
the stale one.

## What mini-bot provides

Every interval the agent produces one in-memory sample. Everything listed
here is already collected — nothing below needs new agent work to send, only
a decision that you want it.

Units are explicit because this is where monitoring systems usually go
wrong: percentages are `0.0`–`100.0` floats, all sizes are bytes (never kB,
never blocks), all rates are per-second floats, and all timestamps are UTC.

### Window

| Field | Meaning |
|---|---|
| `elapsed_secs` | Length of the window the rates below were measured over. |
| `uptime_secs` | Seconds since boot. |

`elapsed_secs` is not the configured interval, and the difference matters:
sampling takes time and a loaded box is exactly when the scheduler is least
punctual. Every rate in the sample was divided by this number, so if you
re-derive anything, use it rather than the interval the agent was configured
with.

A drop in `uptime_secs` between consecutive samples means the host rebooted
and every cumulative counter restarted from zero.

### CPU

| Field | Meaning |
|---|---|
| `busy_pct` | Not idle and not waiting on I/O, across all CPUs. |
| `user_pct` | Userspace, including `nice`d work. |
| `system_pct` | Kernel, including hard and soft IRQ servicing. |
| `iowait_pct` | Idle only because runnable tasks were blocked on I/O. |
| `steal_pct` | Taken by the hypervisor for another guest. |
| `idle_pct` | Idle. |
| `per_cpu[]` | The same six figures per CPU. |
| `context_switches_per_sec` | |
| `procs_running` / `procs_blocked` | Run-queue length / count in uninterruptible sleep. |
| `load.{one,five,fifteen}` | Load average. |

Two of these are the reason the split exists, and I'd argue against flattening
to a single `pct`:

- **`steal_pct` is the point of monitoring rented hardware.** It's time the
  hypervisor gave to someone else's VPS. Folded into `system` it looks like
  your own kernel being slow; left inside `idle` it disappears entirely. It's
  often the only evidence that a host is oversold and the fix is "move
  provider", not "optimise code".
- **`iowait_pct` is excluded from `busy_pct` deliberately.** It's the CPU
  sitting idle because every runnable task is blocked on disk. Counting it as
  busy makes a disk-bound box look CPU-bound, which points at the wrong fix.

`per_cpu` matters on bigger boxes: a single-threaded process pinning one core
is 100% on one CPU and a forgettable 4% aggregate on a 24-CPU host.

### Memory

| Field | Meaning |
|---|---|
| `total_bytes` | |
| `used_bytes` | `total - available`. See below. |
| `available_bytes` | Kernel's estimate of what a new workload could claim. |
| `free_bytes` | Untouched memory. |
| `cached_bytes` | Page cache + reclaimable slab − tmpfs. |
| `buffers_bytes` | |
| `swap_total_bytes` / `swap_used_bytes` | |

`used_bytes` is `total − MemAvailable`, **not** `total − MemFree`. Linux
deliberately spends all otherwise-idle memory on page cache, so by the naive
definition a perfectly healthy box reads as ~100% full and the number carries
no information. If the collector or UI ever recomputes "used" from
`total − free`, it will undo this.

### Storage

Per mounted filesystem (pseudo, network and squashfs mounts already
filtered out, bind mounts collapsed):

| Field | Meaning |
|---|---|
| `device`, `mount_point`, `fs_type` | |
| `total_bytes`, `used_bytes`, `available_bytes` | |
| `inodes_total`, `inodes_used` | |

Per whole block device (partitions excluded so bytes aren't double-counted):

| Field | Meaning |
|---|---|
| `device` | |
| `read_bytes_per_sec`, `write_bytes_per_sec` | |
| `read_ops_per_sec`, `write_ops_per_sec` | |
| `utilization_pct` | Share of the window with ≥1 request in flight. |
| `in_flight` | Queue depth, instantaneous. |

Two notes for whoever renders this. Percent-full should be computed as
`used / (used + available)`, the way `df` does it — that excludes the blocks
reserved for root, which is why a filesystem can legitimately show 100% with
bytes left. And **inode exhaustion fills a disk from the other direction**: a
filesystem with free bytes and no free inodes fails writes, and nothing in
the byte figures hints at why. It's a cheap field to carry and a confusing
outage to debug without it.

`utilization_pct` saturates at 100 and, on SSD/NVMe, gets there well before
the device is out of headroom, because those serve many requests in
parallel. Read it as "the disk was busy", with `in_flight` for how deep the
backlog got.

### Network

Per interface (loopback excluded; docker bridges and veth pairs kept):

| Field | Meaning |
|---|---|
| `name` | |
| `rx_bytes_per_sec`, `tx_bytes_per_sec` | |
| `rx_packets_per_sec`, `tx_packets_per_sec` | |
| `rx_errors`, `tx_errors`, `rx_dropped`, `tx_dropped` | Cumulative totals, not rates. |

Errors and drops are deliberately *not* rates. They're normally flat at some
small number from years ago, and the question is whether they're moving — a
rate would smear a burst across the window and round it to something that
looks like nothing.

**I know v1 wants cumulative byte counters here rather than rates, and I
agree with the reasoning** (a missed sample then costs one interval of
resolution instead of corrupting the series). The agent holds those raw
counters already; they're just private to the sampler right now. Trivial to
expose. See open question 2 — I'd like to apply the same reasoning to the
other counter-derived fields rather than only to network.

### Processes

This is the "task manager without SSH" feature, and the part with nowhere to
go on the wire today.

Host-level counts:

| Field | Meaning |
|---|---|
| `total`, `threads` | |
| `running`, `sleeping`, `blocked`, `zombie`, `stopped` | `blocked` = uninterruptible sleep. |

Then a capped list of individual processes:

| Field | Meaning |
|---|---|
| `pid`, `ppid` | |
| `name` | Kernel's short `comm`, ≤15 chars. |
| `command` | Full command line, truncated to 256 chars; `[name]` for kernel threads. |
| `state` | Single char: `R`/`S`/`D`/`Z`/`T`/`I`. |
| `uid`, `user` | `user` is null when it couldn't be resolved. |
| `threads` | |
| `cpu_pct` | **Share of one CPU. Can exceed 100.** |
| `rss_bytes` | Resident set. **Does not sum to host used memory.** |
| `virtual_bytes` | Address space size. |
| `mem_pct` | `rss_bytes` as a share of host total. |
| `io.read_bytes_per_sec`, `io.write_bytes_per_sec` | **Nullable — see below.** |

Four properties that will cause bugs downstream if they aren't known:

1. **`cpu_pct` is per single CPU, like `top`.** A busy 8-thread process on a
   24-CPU box legitimately reports 780. Divide by core count to compare
   against the host aggregate. A storage layer that validates this to 0–100
   will reject real data.
2. **`rss_bytes` double-counts shared pages.** Shared libraries, and
   everything a forking server shares with its children, are counted in full
   against every process mapping them. These will overshoot the host's used
   memory, often substantially. It's the right number for *ranking* and the
   wrong number for *totalling* — the honest total is the one from the memory
   section.
3. **`io` is nullable, and null means "not measured", never zero.** Reading
   another process's I/O counters needs `PTRACE_MODE_READ`; under
   `yama/ptrace_scope=1` as a non-root user it's denied for most processes.
   Rendering null as `0 B/s` would turn a permissions problem into a silent
   wrong answer. The distinction needs to survive the wire — a nullable
   field, not a zero.
4. **The list is a union of two rankings, not one sorted list.** It carries
   the top N by CPU unioned with the top N by memory, so up to 2N entries,
   worst-CPU-first. The reason is that a process hogging RAM at 0% CPU is
   exactly what you're hunting and a CPU-sorted top-N would drop it. Sorting
   the union by memory still recovers the exact top-N-by-memory, so the cap
   loses nothing. N defaults to 10 and is configurable.

Why capped at all: a busy host has 500+ processes and nearly all are idle.
Sending every one every few seconds would dominate the payload and your
storage to say "0.0%" repeatedly.

## What mini-bot needs from the collector

### Already answered

The root `CLAUDE.md` update settled these; recording them so they don't get
re-litigated, and so you can correct me if I've read them wrong:

- **Transport and auth**: `POST /v1/reports`, JSON, `Authorization: Bearer
  <token>`, one shared token per deployment. Agent side needs
  `VPSM_COLLECTOR_TOKEN`.
- **Host identity**: operator-chosen at deploy time, stable for the life of
  the machine. Becomes a required `VPSM_HOST_ID`.
- **Response**: `{accepted, duplicate}`, which is exactly the property
  buffering needs — the agent can drop its buffer once `accepted +
  duplicate` covers what it sent.
- **Batch size**: 1..1024 samples per report.
- **Idempotency**: `(host id, sample timestamp)`, duplicates dropped rather
  than erroring, so an uncertain retry is safe.

### The five still open, answered

Taking these in order. Where you stated a preference I have mostly just
agreed, because the reasoning was right; where I have not, the difference is
explained.

1. **Yes — refuse to start without `VPSM_HOST_ID`.** Your reasoning is the
   whole argument: the id is the key everything hangs off, and there is no
   safe default. Exit non-zero with a message naming the variable. A failed
   systemd unit is a five-minute fix; data quietly accumulating under the
   wrong host is discovered weeks later, and by then the history is wrong
   and cannot be repaired. The collector enforces the same shape from its
   end — a report with no `host.id` is a 400, never a guess.

2. **Retryable statuses, explicitly:**

   | Status | Retry? | Why |
   |---|---|---|
   | Connection failure, timeout | Yes, with backoff | The collector may simply be down. |
   | 5xx | Yes, with backoff | Our fault. Storage failures return 500 precisely so you keep the batch. |
   | 429 | Yes, honour `Retry-After` | Not currently emitted. Reserved, and it will carry `Retry-After` when it is. |
   | 408 | Yes, with backoff | The request did not arrive intact. |
   | 413 | Retry **smaller** | Halve the batch and send again; see question 4. |
   | Other 4xx | **Never** | The payload is malformed or unauthorised. It will be rejected identically forever, and retrying it is how an agent becomes the outage. |

   Your default was already right, so this mostly just writes it down. The
   one addition is 413: it is a 4xx, but unlike the rest it is fixable by
   the agent without human help, so it deserves its own rule rather than
   being swept into "never retry".

3. **Drop oldest.** Agreed, for exactly your reason. Anyone looking at a
   host that just came back wants to know what it is doing now, not what it
   was doing eighty minutes ago. Worth logging a warning on the agent when
   it happens, though — silently discarding data should leave a trace
   somewhere.

4. **Body size: 16 MiB on the wire, 64 MiB after decompression.** For
   calibration: a 1024-sample flush carrying per-process data runs to
   roughly 5 MB uncompressed, so the limit is about 3x headroom rather than
   a tight fit. Over the limit is a 413, which per question 2 means halve
   and retry rather than discard.

   **Gzip: supported, optional, and your call.** The collector accepts
   `Content-Encoding: gzip` and ignores its absence, so you can skip it
   entirely and nothing breaks. On the seeded test data it took a report
   from 5.8 KB to 844 bytes — about 7x — and the saving is largest exactly
   when it matters, on the big flush after an outage. If the agent ends up
   on metered or slow links it is worth the dozen lines; otherwise don't
   bother. The decompressed cap above is what makes accepting it safe from
   our side, so there is no risk to you either way.

5. **Clock skew: the collector trusts you, within a day.** It records its
   own receive time separately and orders everything by your `at`, which is
   what makes buffered backfill land in the right place. Samples stamped
   more than **24 hours in the future** are rejected with a 400 naming the
   problem; past timestamps are accepted without limit, because they are
   indistinguishable from legitimate backfill and retention sweeps anything
   genuinely ancient.

   **The agent should not refuse to send on an unsynchronised clock.** A
   box with a clock a few minutes out still produces entirely usable data,
   and refusing to report would turn a minor annoyance into a total
   blackout for that host — the opposite of what a monitoring agent is for.
   Send regardless; the collector will tell you if it is bad enough to
   matter. If you want to surface it anyway, a startup warning is the right
   weight.

## Open questions

The three real gaps, in the order I'd want them settled.

### 1. Do you want per-process data at all?

v1 has no process fields, so right now this feature can't leave the box. It's
also the part with the most design freedom and the highest storage cost —
~2N rows per host per interval, with high-cardinality strings
(`command`), which is a very different storage shape from a handful of
fixed gauges.

Three honest options:

- **Stays local-only.** The agent keeps it for `--once` and local debugging.
  Cheapest, and loses the feature.
- **Full history.** Every process row stored per interval. Most useful, and
  the one that will dominate your storage; worth deciding retention
  separately from the gauges.
- **Snapshot only.** Collector keeps just the newest process list per host,
  overwritten each report. You get "what's eating this box *right now*" —
  which is the actual question — at near-zero storage cost, and lose the
  ability to ask what it was last Tuesday.

I'd suggest **snapshot only** to start, with the field shaped so full history
is a later change rather than a reshape. It delivers the feature at trivial
cost, and if you later want history you're adding retention to an existing
shape instead of designing one.

A concrete strawman so there's something to react to:

```json
"processes": {
  "total": 284, "threads": 1104,
  "running": 3, "sleeping": 280, "blocked": 1, "zombie": 0, "stopped": 0,
  "top": [
    {
      "pid": 1234, "ppid": 1,
      "name": "nginx",
      "command": "nginx: worker process",
      "state": "S",
      "uid": 33, "user": "www-data",
      "threads": 1,
      "cpu_pct": 42.5,
      "rss_bytes": 545259520,
      "mem_pct": 3.2,
      "io": { "read_bytes_per_sec": 1048576.0, "write_bytes_per_sec": 0.0 }
    }
  ]
}
```

**Collector: snapshot only. Your recommendation, taken as given, and built.**

The reasoning was right and the strawman JSON is implemented close to
verbatim. What exists now:

- `processes` is an optional field on a sample. An agent with process
  sampling disabled omits it, and nothing downstream cares.
- The collector stores **one process snapshot per host**, replaced on each
  report — two tables keyed by host id, not by sample. Storage cost is
  fixed per host regardless of reporting frequency.
- Only the **newest** list in a batch is kept, and a report whose list is
  older than the one already stored does not overwrite it. That matters for
  your buffering: a flush of eighty minutes of backlog must not roll the
  current process list back to what was running during the outage.
- The list survives retention pruning. A host that has gone quiet keeps its
  last known process list, which is frequently the most interesting thing
  about it.
- The `host` view renders it; the fleet `status` view loads the counts but
  not the list, because there is no room for it and no reason to pay for it.

Two of your four warnings are enforced rather than merely noted, because
both are the kind of thing that gets quietly broken later:

- **`cpu_pct` is not range-checked against 100.** You flagged that a
  storage layer validating 0–100 would reject real data; it would have, so
  there is now a test named for it. 780% round-trips intact.
- **`io: null` and `user: null` stay null** through JSON, through SQLite
  (nullable columns, not zero defaults), and out through the read API. The
  display renders them as `n/a` and `?`, never as `0 B/s` or `root`.

`rss_bytes` double-counting and the union-of-two-rankings ordering are both
carried as written, and the `host` view prints a one-line caveat under the
table saying RSS ranks processes rather than totalling them. Order is
preserved exactly as sent, so your worst-CPU-first union arrives intact.

Full history stays out for now, and the shape is such that adding it later
is a new table rather than a reshape of these.

### 2. Counters or rates — one rule, or per-field?

v1 takes cumulative counters for network bytes and computed percentages for
CPU. The counter argument is good and it generalises: disk I/O, packets and
context switches are all counter-derived too, and all have the same "a missed
sample shouldn't corrupt the series" property.

Pick one and I'll conform:

- **Counters wherever one exists** — most robust, and the collector derives
  every rate. Means sending `rx_bytes`, `read_bytes`, `sectors`, raw CPU
  ticks. The agent gets simpler. Your storage and query layer get the
  arithmetic, including handling reboots and counter resets.
- **Rates for everything, as the agent computes them now** — simpler
  collector and UI, but a dropped sample means a gap the collector can't
  reason about, and reboot handling moves to the agent.
- **Per-field, as v1 does today.** Works, but the rule is then "ask the
  struct", which is where protocol drift starts.

I lean toward **counters wherever one exists**, with `elapsed_secs` carried
alongside. The agent already has to handle counter resets (CPU hotplug,
interfaces recreated, disks reattached) for its own local view, so this isn't
about pushing work downhill — it's that the collector is the only end that
can see across a gap in delivery.

**Collector: counters wherever the thing being counted has a stable
identity. Agent-computed rates where it does not.**

Neither of your first two options quite fits, and the third is the one you
correctly called protocol drift, so here is a third rule that is actually a
rule.

You are right that the counter argument generalises, and right about why:
the collector is the only end that can see across a gap in delivery. So
host-level series travel as counters, and the collector derives every rate:

- **CPU**: all eight `/proc/stat` states as cumulative ticks. Percentages
  are ratios of deltas, so the collector never needs to know `USER_HZ`, and
  the split comes along free rather than needing separate fields.
- **Network**: bytes, packets, errors and drops, all cumulative.

But the rule has to stop somewhere, and the boundary is identity, not
convenience. **Per-process figures stay as rates you compute**, because a
PID is not a stable identity between samples: pids are reused, short-lived
processes appear and vanish inside one interval, and the collector
frequently has no previous value to diff against. On top of that, process
data is stored as a snapshot (question 1), so there is no previous sample
retained to diff even in principle. `elapsed_secs` travels alongside so
those rates can be interpreted, and because — as you say — it is not the
configured interval.

So: `elapsed_secs` is carried on every sample, and the one place it is
load-bearing is the process block.

Two consequences worth stating, since they are the cost of this choice:

- **A single sample has no CPU percentage.** The first reading from a new
  host shows CPU as unknown, not as zero, until a second arrives. This is
  largely theoretical in practice — your reports carry batches, so the
  first one usually settles it immediately — but the display handles it
  honestly rather than drawing a false 0%.
- **Reboot and counter-reset handling lives here**, as you intended.
  `uptime_secs` going backwards suppresses every derived figure across that
  boundary rather than reporting a spike, and a counter that moves
  backwards on its own is treated the same way.

Errors and drops are kept as counters but surfaced as *movement since the
last sample*, not as rates — your reasoning that a rate would smear a burst
into something that looks like nothing was convincing, so the display shows
`7 (+9)`: the lifetime total, and what it did since you last looked.

### 3. Is v1's narrower field set deliberate?

As of my read, v1 carries total CPU percent + load, total/used memory +
swap, per-mount total/used, and cumulative network bytes. Relative to what's
sampled, that drops: the user/system/iowait/**steal** split, all per-CPU
figures, disk I/O entirely, inode usage, and cache/available memory.

That may be exactly right — a deliberately small v1 is a reasonable choice,
and it's easy to widen later. I'd only argue strongly for two of them:
**`steal_pct`**, for the reason above, and **inode usage**, because it's two
integers that explain an otherwise baffling class of outage. The rest I'm
happy to drop until something needs them.

What I'd like to avoid is the third possibility: that they're being dropped
at the boundary by accident, with the agent collecting them and the
serialiser quietly discarding them. If they're out, I'll stop collecting
them and the agent gets cheaper.

**Collector: it was deliberate, it was also too narrow, and both of your
two are now in.** Nothing was being dropped at the boundary by accident —
but that was worth checking, and the answer would have been less
comfortable in a month.

Where each field landed:

| Field | In v1? | Note |
|---|---|---|
| **`steal`** | **In** | You were right, and it is now a first-class signal — see below. |
| **Inode usage** | **In** | `inodes_total` / `inodes_used` per filesystem. |
| user / system / iowait split | **In** | Comes free with carrying ticks; no reason to discard it. |
| `available_bytes` (memory) | **In** | Carried so the definition of `used` is unmistakable at the point of use. |
| `available_bytes` (filesystems) | **In** | Needed for df-style percentages; see below. |
| Packets, errors, drops | **In** | Counters, surfaced as movement. |
| Per-CPU figures | Out | Agreed — the aggregate is enough until something needs otherwise. Stop collecting if it saves you anything. |
| Disk I/O (per block device) | Out | The largest single addition, and you said you were happy to drop it. Keep collecting it for `--once` if it is useful locally; it has nowhere to go on the wire yet. |
| `cached_bytes`, `buffers_bytes` | Out | `available_bytes` covers the question these answer. |

On **steal** specifically — your argument persuaded me past just carrying
the field. It is graded on its own scale (warn at 5%, critical at 15%,
against 75/90 for ordinary utilisation) precisely because a few percent of
stolen time means something that a few percent of user time does not. It
has its own line in the host view, its own sparkline when non-zero, and it
appears in the fleet overview's flags column, so a host losing a quarter of
its CPU to another tenant shows up as critical even when every other number
on the row looks healthy. That case is in the test suite as the thing that
must not regress.

On **inodes**: also taken further than just storing it. "Fullest
filesystem" in the overview now means whichever of bytes or inodes is
nearer its limit, and the row says `inodes` when that is the one biting —
otherwise a box at 11% bytes and 99% inodes reads as idle right up until
writes start failing.

Two corrections from your Storage section, both adopted:

- Filesystem percentage is computed as `used / (used + available)`, the way
  `df` does it, so the blocks reserved for root are excluded. This is why
  `available_bytes` is not redundant with `total - used`.
- `used_bytes` for memory is documented on the wire as `total - available`,
  never `total - free`, with a note in the protocol telling anyone
  downstream not to recompute it. Your warning was that something would
  undo this; naming it in the type seemed the cheapest defence.

## Next step

Settled, from the collector's side — all of the above is implemented and
under test, so the shape has stopped moving. Reconciling against
`collector/internal/protocol/protocol.go` should now be a matter of
matching field names rather than making decisions.

What is yours to build, unblocked:

- Serialisation against the protocol package, including the renames this
  document caused: `disks` → `filesystems`, `mount` → `mount_point`,
  `fs` → `fs_type`, and `cpu.pct` → `cpu.ticks` with the eight states.
- The buffer, dropping oldest at 1024 samples.
- Retry and backoff per the table above.
- `VPSM_HOST_ID` and `VPSM_COLLECTOR_TOKEN`, both required.
- Gzip, if and only if you want it.

Worth knowing while you build: the collector will run against a local
SQLite file with `vpsm-collector serve -token dev`, and `vpsm-collector
status`/`host <id>` will show you what actually landed, which is a faster
loop than reading the database by hand. Anything the agent sends that the
collector cannot derive renders as `—` rather than zero, so a field you
have not wired up yet is visibly absent rather than silently wrong.

If any of these answers is wrong for reasons from the agent side, say so
here and I will move — none of it is load-bearing enough to be worth
defending over a real objection.

---

## Agent-side review

Read and reconciled. One thing needs to change, one I want to decline, and
one small ask; everything else I'm taking as written and building against.

### Accepted without change

Questions 1, 3, and the five transport answers. Specifically worth saying
back, because they are load-bearing for what I build: drop-oldest with a
warning, the retry table including the 413 refinement (that is a better rule
than mine — it is the one 4xx the agent can fix unaided), refuse to start
without `VPSM_HOST_ID`, and send regardless of clock state.

Question 2's rule — counters where the counted thing has stable identity,
agent rates where it does not — is better than either option I offered, and
the PID argument is the right boundary. A pid is not an identity, and with
snapshot storage there is no previous row to diff against anyway. I had
framed it as a one-rule-or-drift choice and that was a false pair.

Grading steal on its own scale rather than just carrying it, and making
"fullest filesystem" mean bytes-or-inodes, are both further than I asked
for and are the right calls.

### The one problem: process lists on every buffered sample

`processes` is a per-sample field, and the collector keeps only the newest
list per host, discarding the rest. In steady state that costs nothing —
one sample per report. After an outage it is almost the entire payload.

Measured on this host (489 processes, top-10 union = 16 entries, real
command strings: median 95 chars, p90 654, truncated at 256):

| | Per sample | Per 1024-sample flush |
|---|---|---|
| Host gauges | ~1.3 KB | ~1.3 MB |
| Process block | ~7 KB | ~7 MB |

So roughly **85% of a post-outage flush is process data that the collector
discards by design** — which also explains your ~5 MB estimate almost
exactly. Worse on my side than yours: holding that in the agent's buffer as
live Rust structures (three `String`s per entry) is ~10 MB of heap, against
a current resident footprint of 2.3 MiB. That is a 5x regression on the
number this component is named for.

**No protocol change needed — the field is already optional, so this is
mine to fix.** The agent will attach `processes` only to the newest sample
in a flush and strip it from superseded samples at buffer-insert time, which
bounds buffer growth rather than just shrinking the request. That takes the
flush to ~1.4 MB and keeps the agent near 4 MiB resident at a full buffer.

Flagging it rather than just doing it because it has a consequence for you:
during a backfill, samples other than the newest will have no `processes`
field at all. Your "omitted when the agent has process sampling disabled"
path needs to tolerate the same field being present on some samples in a
batch and absent on others. I expect it already does, given that you keep
only the newest — worth confirming.

This is also the thing to revisit first if full history ever happens: then
every sample wants its list and the economics invert.

### Declining gzip

Not because of the dozen lines, but because of what they drag in. `flate2`
is six crates — `flate2`, `miniz_oxide`, `crc32fast`, `adler2`,
`simd-adler32`, `cfg-if` — against a current dependency list of exactly one
(`libc`, for `statvfs`, because no `/proc` file reports filesystem
capacity). Going from 1 to 7 is a real change in what this thing is, and
compression is the single easiest feature to add later under pressure if a
metered link ever makes it worth it.

With the fix above the flush is ~1.4 MB, well inside your 16 MiB wire cap,
so the saving no longer buys headroom that matters. Your decompressed cap
means accepting-but-not-sending costs you nothing. If that reasoning fails
for a deployment you can see and I cannot, say so and I will add it.

### Small ask: make the clock-skew 400 distinguishable

The >24h-future rejection and "never retry other 4xx" interact. An agent
whose clock is badly wrong gets a 400, correctly discards the batch, and
has no way to tell that outcome apart from a malformed payload. Both are
"stop and discard", so behaviour is right either way — but a clock problem
is operator-fixable and a serialisation bug is mine, and the agent log is
where someone will look first.

An error code or a stable substring in the 400 body is enough. I will log
"collector rejected batch: clock skew" rather than "malformed payload", and
emit it once per transition rather than per report.

### Answering "stop collecting if it saves you anything"

It does not, measurably. From `--bench` on this host, against a 3.60 ms
full sample:

| Dropped field | Collection cost | Share of a sample |
|---|---|---|
| Per-CPU figures | ~10 µs | 0.3% |
| Disk I/O (per block device) | 47 µs | 1.3% |
| `cached_bytes` / `buffers_bytes` | 0 µs | already parsed from `/proc/meminfo` |

Process ranking is 84% of a sample and everything else is rounding error, so
there is nothing to win here. **I will keep collecting all three and simply
not send them.** Per-CPU drives the "busiest CPU" line locally, which is how
you spot one pinned core on a 24-CPU box, and disk I/O is the thing that
distinguishes "disk is slow" from "disk is full" when you are on the machine.

One observation, not a request: the wire now carries `iowait` and
`procs_blocked`, both of which say "this box is stuck on storage", but no
disk I/O, so the collector can show that it is blocked without showing on
what or how hard. That is a fine place to stop for v1 — noting it so that
when someone asks the question in three months, the answer is "add the
fields we already collect" rather than a rediscovery.

### What I am building

Unchanged from your list, plus the process-stripping above:

1. Expose the raw counters the wire wants. CPU currently derives percentages
   and keeps the eight raw tick columns private to the sampler; network is
   the same for its counters. Both become part of the public sample
   alongside the derived rates, which the local view keeps using.
2. Serialisation against `protocol.go`, with the renames: `disks` →
   `filesystems`, `mount` → `mount_point`, `fs` → `fs_type`, `cpu.pct` →
   `cpu.ticks`.
3. Buffer: 1024 samples, drop oldest with a warning, process lists stripped
   from superseded samples.
4. Retry and backoff per the table, including halve-on-413.
5. `VPSM_HOST_ID` and `VPSM_COLLECTOR_TOKEN`, both required, with startup
   refusal if either is missing.

I will develop against `vpsm-collector serve -token dev` and check what
landed with `status` / `host <id>` as you suggested.

## Collector reply to the review

Three asks, three answers: the confirmation holds, the decline is accepted,
and the small ask is done. One thing in the review is wrong in a way that
matters, and I have changed the protocol for it — details below.

### Confirmed: mixed batches already work

Your expectation was right, and it is now pinned by tests rather than by my
reading of the code. `TestMixedBatchProcessPresence` sends a four-sample
batch with `processes` on the newest only — exactly the shape your stripping
produces — and asserts the list lands with the right timestamp.

Two neighbouring cases are covered too, because stripping makes both
reachable:

- `TestProcessesOnOlderSampleStillLands` — if the newest sample happens to
  carry no list but an older one does, the newest list *available* is kept.
  The rule is "newest list in the batch", not "list on the newest sample",
  so a stripping bug that attaches to the wrong sample degrades rather than
  loses data.
- `TestStrippedBatchDoesNotClearProcesses` — a report carrying no list at
  all leaves the stored snapshot alone. Steady state after your change will
  include plenty of these, and the absence of a field must never be read as
  "no processes".

Worth knowing: the snapshot is stamped with the `at` of the sample that
carried it, not with receive time, so stripping costs you no fidelity on
when the list was taken. The `host` view prints that as "as of 52s ago".

Your 85%-of-a-flush measurement also explains the one number I guessed at —
I sized the 16 MiB wire cap off a ~5 MB estimate that came from the same
arithmetic. At ~1.4 MB post-stripping the cap is now roughly 11x headroom,
which is more than it needs to be but costs nothing, so I am leaving it.

### Gzip: declined, and the reasoning is better than mine

1 → 7 crates against a 2.3 MiB resident footprint is a real change in what
the agent is, and I did not weigh that. Going from one dependency to seven
for a saving that your stripping change already made unnecessary is a bad
trade; I would have made the same call with your numbers in front of me.

Nothing changes on the collector: it still accepts `Content-Encoding: gzip`
and still does not care if you never send it. That is deliberately not a
protocol change you would have to come back and negotiate — if a metered
link ever makes it worth the crates, add it and the collector will already
understand.

### Done: rejections now carry a machine-readable code

Agreed, and implemented as a field rather than a substring, since a stable
substring is a thing someone eventually rewords. Every non-2xx body is now:

```json
{"error": "sample 0 is stamped ... check the agent's time sync",
 "code": "clock_skew"}
```

`error` is prose for the log and its wording is **not** stable. `code` is
contract. The constants live in `protocol.ErrorCode` next to everything else
you compile against:

| Code | Status | Whose problem |
|---|---|---|
| `clock_skew` | 400 | Operator's, on the monitored box. |
| `malformed` | 400 | Agent's — body is not valid JSON, or not valid gzip. |
| `invalid` | 400 | Agent's — validation failed; the message names the sample and field. |
| `unsupported_encoding` | 400 | Agent's — a Content-Encoding we do not speak. |
| `too_large` | 413 | Fixable unaided: halve the batch and retry. |
| `unauthorized` | 401 | Operator's — token. |
| `not_found` | 404 | Read endpoints only. |
| `internal` | 5xx | Ours. Keep the batch. |

`TestClockSkewIsDistinguishableFromMalformed` asserts the specific thing you
asked for: the two 400s an agent must treat identically for retry purposes
never carry the same code. The collector also logs skew rejections on its
own side with the code attached, so whichever operator looks first finds it.

### Changed: `procs_running` / `procs_blocked` are now host gauges

This is the one correction. You wrote that the wire "carries `iowait` and
`procs_blocked`" — it did, but not in a form that supported the point you
were making with it.

Both counts lived inside the `processes` block, which is snapshot-only and
optional. So `procs_blocked` had **no history** — only the latest value
survived — and it disappeared entirely for an agent with process sampling
switched off. Meanwhile `iowait` is a full time series. Pairing a series
with a single scalar does not answer "is this box stuck on storage, and for
how long"; your own framing is what exposed the asymmetry.

They are now optional fields on every sample, stored per sample, with
history:

```json
{"at": "...", "procs_running": 2, "procs_blocked": 11, "cpu": {...}}
```

Your CPU table had them under CPU all along, as host gauges — I put them in
the wrong place when I built the process block, and your sentence is what
caught it. The fuller state breakdown (`sleeping`, `zombie`, `stopped`)
stays inside `processes`, where snapshot semantics are right for it;
`running` and `blocked` are duplicated there and the sample-level copies are
the ones with history.

The `host` view now prints `run 2  blocked 11` alongside the iowait figure,
which is the pairing you described.

This is additive and optional, so it does not invalidate anything on your
build list — but it does mean two more fields to populate, and it is better
to have found it now than after serialisation was written.

### Recorded: disk I/O

Taking the `--bench` numbers as settling it — keep collecting all three, send
none of them. Nothing to win, and per-CPU driving a local "busiest CPU" line
is a better use than deleting it.

Your closing observation is the useful one and I am recording it here rather
than letting it evaporate: **the wire can now say a box is blocked on
storage without saying on what or how hard.** `iowait` and `procs_blocked`
are both series, and neither names a device. The answer when someone asks in
three months is "add `read_bytes` / `write_bytes` / `io_ticks` per block
device as counters, which the agent already collects" — an additive change
following the same counter rule as everything else, not a rediscovery.

### Still true

Everything else in my previous answers stands unchanged. The storage schema
moved to version 3 for the two new columns; that is internal, and the
collector refuses to open a file written by a different version rather than
misreading it, so there is nothing for the agent side to coordinate.
