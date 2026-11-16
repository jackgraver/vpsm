# VPSM (VPS Monitor)

VPSM is a small two-part system for keeping an eye on VPS resource usage (CPU,
RAM, disk, network, etc.) across one or more hosts.

## Components

- **`mini-bot/`** — a tiny Rust agent deployed onto each monitored VPS. Samples
  system usage on an interval into an on-disk spool, which the collector
  pulls over SSH. Written in
  Rust specifically to stay small and low-overhead, since it runs alongside
  (and must not compete with) the services it's watching. See
  `mini-bot/CLAUDE.md`.
- **`collector/`** — the Go side that pulls reports from every mini-bot
  instance (or receives them over HTTP), and is the place usage data gets
  stored/queried.
  See `collector/CLAUDE.md`.
- **`e2e/`** — a local end-to-end harness: docker compose brings the
  collector up as one container and the things reporting to it as others, so
  the two halves are exercised across a real HTTP boundary. See
  `e2e/README.md`.

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

The two halves talk by **pull over SSH** (see Transport below): the agent
spools, the collector runs `mini-bot export` on the host whenever it runs
and acks what it stored. `mini-bot/deploy/` installs the agent as a systemd
service.

`e2e/` runs both halves in containers and asserts the protocol over real
HTTP (`e2e/check.sh`). The agent never pushes, so the agent side of that is a
`curl`-based stand-in; it still covers the HTTP ingest path, which the pull
path shares validation and storage with.

`AGENT-CONTRACT.md` is the working document the protocol was settled in:
what the agent provides field by field and the decisions reached on each.
It predates the move from push to pull, which changed only the transport.

## Wire protocol

Decided and implemented on the collector side, and reconciled against
`AGENT-CONTRACT.md` (the working document between the two halves — read it
for the reasoning behind each choice). The authoritative definition is
`collector/internal/protocol/protocol.go`; change both ends together.

- **Transport: pull over SSH.** The collector is not always running —
  typically a laptop, behind NAT, which a VPS cannot reach but which can
  always reach the VPS. So the agent spools every sample to disk and the
  collector runs `ssh <host> mini-bot export`, which streams one JSON
  `PullFrame` per line (`protocol/pull.go`). The collector writes
  `ack <cursor>` back on stdin once a frame is *committed*, and only then
  does the agent delete it. SSH is the auth; the agent has no network code
  and its unit denies it sockets entirely. `export --follow` keeps the
  session open for a live view instead of a handshake per refresh.
- **Acks use a spool sequence number, not a timestamp**, so a clock stepped
  backwards cannot make an ack cover samples never delivered.
- **The spool is a backlog, not a history**: with regular pulls it holds
  minutes. Past half its cap (`VPSM_SPOOL_MAX_MB`) the oldest samples are
  thinned to one per `VPSM_SPOOL_THIN_SECS` — lossless for averages, since
  the wire carries counters — and only past the full cap are they dropped.
- **HTTP push still exists** on the collector (`serve`, `POST /v1/reports`,
  bearer token, optional gzip) for a collector that does run all the time.
  The agent does not implement it.
- **Payload**: one report = one host's identity + a batch of 1..1024
  samples. Host metadata travels once per report, not once per sample.
  Spooled samples carry no process list; the newest reading carries it, on
  export, because the collector keeps only the newest snapshot anyway.
- **Invalid samples are discarded, not held.** A pulled sample that fails
  validation is dropped individually and acked past, the pull equivalent of
  a 4xx the agent must not retry; holding it would block everything behind.
- **Idempotency**: `(host id, sample timestamp)` identifies a sample.
  Re-delivery is counted as a duplicate and dropped, not an error, so an
  agent can retry a flush it is unsure landed. The response is
  `{accepted, duplicate}`.
- **Errors carry a stable code**: every non-2xx body is
  `{"error": "...", "code": "..."}` (HTTP ingest). The prose is for logs
  and its wording is not stable; `protocol.ErrorCode` is what to branch on.

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
