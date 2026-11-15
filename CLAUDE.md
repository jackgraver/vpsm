# VPSM (VPS Monitor)

VPSM is a small two-part system for keeping an eye on VPS resource usage (CPU,
RAM, disk, network, etc.) across one or more hosts.

## Components

- **`mini-bot/`** — a tiny Rust agent deployed onto each monitored VPS. Samples
  system usage on an interval and reports it to the collector. Written in
  Rust specifically to stay small and low-overhead, since it runs alongside
  (and must not compete with) the services it's watching. See
  `mini-bot/CLAUDE.md`.
- **`collector/`** — the Go server that receives reports from every mini-bot
  instance, aggregates them, and is the place usage data gets stored/queried.
  See `collector/CLAUDE.md`.

This is a single monorepo: one git repository at the `vpsm/` root covering
both `mini-bot/` and `collector/`. They're still independently deployable
(different languages, different build/release artifacts) — the shared repo
is just for convenience of working on both together, not a sign they ship
together.

## Status

The collector is built out: ingest endpoint, SQLite storage, and terminal
views (`status`, `watch`, `host`). See `collector/CLAUDE.md`.

The mini-bot agent samples: CPU (with steal and iowait split out), memory,
filesystem capacity and inodes, disk and network throughput, and a
per-process task-manager view. It reads `/proc` directly and depends only on
`libc` (for `statvfs`). `--once` prints a sample; `--bench` measures what the
agent costs the host it watches. See `mini-bot/CLAUDE.md`.

The agent does not report yet — no HTTP, no serialisation, no buffering. What
it collects is wider than the protocol below carries, and
`AGENT-CONTRACT.md` is the working document between the two sides: what the
agent provides field by field, what it needs from the collector, and the
decisions reached on each. The collector side has answered everything open
there, so the protocol has stopped moving — including per-process data,
which v1 now carries as a per-host snapshot.

## Wire protocol

Decided and implemented on the collector side, and reconciled against
`AGENT-CONTRACT.md` (the working document between the two halves — read it
for the reasoning behind each choice). The authoritative definition is
`collector/internal/protocol/protocol.go`; change both ends together.

- **Transport**: HTTP `POST /v1/reports`, JSON body, optional
  `Content-Encoding: gzip`. Chosen for being debuggable with curl and
  needing no schema compiler on either end; the envelope leaves room for a
  compact binary encoding later without reshaping the data.
- **Auth**: `Authorization: Bearer <token>`, one shared token per
  deployment. The collector refuses to start without one unless
  `-allow-anonymous` is passed explicitly.
- **Payload**: one report = one host's identity + a batch of 1..1024
  samples. Host metadata travels once per report, not once per sample.
- **Batching**: an agent that cannot reach the collector buffers samples
  and flushes them on the next successful call, dropping oldest when full.
- **Idempotency**: `(host id, sample timestamp)` identifies a sample.
  Re-delivery is counted as a duplicate and dropped, not an error, so an
  agent can retry a flush it is unsure landed. The response is
  `{accepted, duplicate}`.
- **Errors carry a stable code**: every non-2xx body is
  `{"error": "...", "code": "..."}`. The prose is for logs and its wording
  is not stable; `protocol.ErrorCode` is what to branch on. It exists so an
  agent can tell `clock_skew` (the operator fixes it) from `malformed` (the
  agent author does) when both are 400s it must not retry.

### Counters, not rates

Where the kernel exposes a cumulative counter, the agent forwards the
counter and **the collector derives the rate** by diffing consecutive
samples — CPU time as all eight `/proc/stat` tick states, network bytes,
packets, errors and drops. The collector is the only end that can see
across a gap in delivery: if a report is lost, diffing counters costs one
interval of resolution, where pre-computed rates would leave a hole nothing
can reason about.

The exception is anything whose identity does not survive between samples.
**Per-process figures are rates the agent computes**, because PIDs are
reused and short-lived processes appear and vanish inside one interval.
`elapsed_secs` travels alongside so those rates can be interpreted.

`uptime_secs` is load-bearing: a drop between samples means the host
rebooted and every counter restarted, so the collector suppresses the
derived figures across that boundary rather than reporting a spike.

### Things that are easy to get wrong

These are documented in the protocol types because each one has a wrong
answer that looks plausible:

- **Memory `used_bytes` is `total - MemAvailable`**, never `total -
  MemFree`. Linux spends idle memory on page cache; by the naive definition
  a healthy box reads as ~100% full. Nothing downstream should recompute it.
- **Filesystem fullness is `used / (used + available)`**, the way `df` does
  it, which excludes the blocks reserved for root. This is why
  `available_bytes` is carried and is not redundant.
- **Inode usage is carried** — a filesystem with free bytes and no free
  inodes fails writes, and nothing in the byte figures hints at why.
- **`steal` is carried and graded on its own scale** (warn 5%, crit 15%).
  It is time the hypervisor gave to another guest, and often the only
  evidence that a VPS is oversold.
- **Process `cpu_pct` is a share of one core and may exceed 100.**
  Validating it against 100 rejects real data.
- **`procs_running` / `procs_blocked` are host gauges on every sample**, not
  part of the snapshot-only process block. Paired with CPU `iowait` they
  separate "stuck on storage" from "busy", and that only reads as an answer
  over time.
- **`null` is not zero.** A process's `io` is null when ptrace denied the
  read, and `user` is null when the uid did not resolve. Both stay null
  through the wire, the database, and the display.

### Deliberately out of v1

Per-CPU breakdowns, per-block-device disk I/O, and `cached`/`buffers`
memory. All are sampled by the agent and have nowhere to go on the wire
yet; adding any of them is an additive change.

## Conventions

- Each app's `main`/`main.rs`/`main.go` is kept to just a doc comment plus a
  call into that app's startup routine — see the per-app CLAUDE.md for the
  exact shape. Real logic (config, loops, servers) lives in dedicated
  modules/packages, not in main.
