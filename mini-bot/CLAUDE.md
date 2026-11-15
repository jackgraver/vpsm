# mini-bot

The VPSM monitoring agent. Deployed onto each VPS being watched; samples
local system usage (CPU, memory, disk, network, and per-process usage) on an
interval and reports it to the collector (see `../collector/CLAUDE.md`).

## Why Rust

Needs to be a small, dependency-light, low-overhead static binary that can
run continuously (e.g. under systemd or cron) without itself becoming a
resource problem on the box it's monitoring. Measured on a 24-CPU, 489-process host: 445 KB binary, 2.3 MiB RSS, 3.67 ms
of CPU per sample, zero when idle. See Self-measurement below — `--bench`
re-measures all of it on whatever box you run it on.

## Layout

- `src/main.rs` — fixed entry point: a doc comment describing the app, the
  module list (which has to live in the crate root), then
  `fn main() { startup::run(); }`. Don't add logic here.
- `src/startup.rs` — `startup::run()` is where real wiring happens: load
  config, prime the sampler, run the sample loop.
- `src/config.rs` — environment-variable configuration (`VPSM_*`), plus the
  `--once` flag. Every setting has a working default; bad values warn and
  fall back rather than being fatal.
- `src/procfs.rs` — shared `/proc` plumbing: buffer-reusing file reads and
  the `sysconf` values (page size, clock ticks) needed to convert kernel
  units.
- `src/metrics.rs` — `Sampler`, which holds the previous reading so each
  `Sample` can describe the window between two of them, and degrades
  per-source when something under `/proc` can't be read.
- `src/metrics/{cpu,memory,disk,net,process,host}.rs` — one `/proc` source
  each, independent of one another.
- `src/render.rs` — human-readable text for a `Sample`. A local debugging
  view only (`--once`, or a box with no collector); nothing should ever
  parse it.

A `report` module for talking to the collector is still to be built — see
Status below.

## Sampling approach

`/proc` is read directly rather than via a crate like `sysinfo`. The files
are stable, documented kernel interfaces, parsing them is a few lines each,
and it keeps full control over which files get opened per interval — which is
the thing that actually determines this agent's overhead.

Two conventions hold across the metrics modules, and new ones should follow
them:

- **Parsing is separated from I/O.** Each module exposes a `parse(&str)` that
  is unit-tested against captured fixtures, plus a thin reader. Tests don't
  depend on what the host they run on happens to look like.
- **Counters become rates in one place.** Almost everything in `/proc` is a
  monotonic counter since boot, so a `Sampler` differences consecutive
  readings via `metrics::per_sec`. Subtraction is always saturating: counters
  really do go backwards (CPU hotplug, interfaces recreated, disks
  reattached) and a wrapped `u64` becomes an absurd spike downstream.

A consequence worth knowing: the first sample arrives one interval after
startup, not immediately.

## Per-process metrics

The task-manager view (`metrics/process.rs`) is the costliest part of a
sample, and is built around keeping that cost down:

- Each interval reads one small file per process to rank them, then reads
  command line, owner and I/O only for the processes that make the top of a
  ranking. Doing the full read for every process would roughly quadruple the
  agent's syscall load for rows nobody looks at.
- Reports are capped (`VPSM_TOP_PROCESSES`) and carry the *union* of the top
  by CPU and the top by memory. Sorting that union by memory still gives
  exactly the top-by-memory ranking, so the cap costs nothing in fidelity.
- Per-process disk I/O needs `PTRACE_MODE_READ` on the target. It degrades to
  "not measured" (`None`, which is deliberately distinct from zero) when the
  agent lacks the privileges — e.g. under `yama/ptrace_scope=1` as a normal
  user.

## Toolchain

Rust edition 2024, plain `cargo build` / `cargo run` / `cargo test`.
`cargo run -- --once` prints a single sample and exits, which is the quickest
way to check the agent against a real box. `cargo run --release -- --bench`
measures the agent cost on this host.

The only dependency is `libc`, for `statvfs(3)`: filesystem capacity is the
one metric with no `/proc` file behind it. Keep it that way — anything new
should clear the same bar.

## Self-measurement

`cargo run --release -- --bench` measures what the agent costs the box it is
running on, and prints the interval trade-off. It measures the real binary on
the real host rather than a separate harness, which is why it ships in the
agent (it costs ~20 KB of the binary).

It reports three things: per-source sample latency, so optimisation effort
goes where the time actually is; the agent's own CPU and resident memory,
read from `/proc/self/stat` through the same parser used for every other
process; and a projection of duty cycle against sample interval.

Two measurement distinctions the output depends on, worth not collapsing:

- **Wall time per sample is latency; CPU time per sample is cost.** Only the
  second competes with the workload, and the duty-cycle projection is built
  from it.
- **Idle is measured, not assumed.** The agent sleeps between samples with no
  timers and no background threads, so idle cost should be zero — which is
  exactly the sort of property that quietly stops being true.

### Baseline

On a 24-CPU, 489-process host (NixOS 6.18, 100 Hz, NVMe), release build:

| | |
|---|---|
| Release binary | 445 KB |
| Resident memory | 2.3 MiB, unchanged over 60 samples |
| Idle CPU | below the kernel's accounting granularity |
| CPU per sample | 3.67 ms |
| Duty cycle @ 5s | 0.073% of one CPU |

Per-source, mean of a full sample at 3.60 ms:

| Source | Mean | Note |
|---|---|---|
| process ranking | 3.02 ms | **84% of a sample** |
| process top-10 enrichment | +0.21 ms | |
| process disk I/O | +0.07 ms | effectively free |
| process top-50 instead of top-10 | +0.87 ms | |
| network | 98 µs | scales with interface count |
| disk I/O | 47 µs | |
| filesystems (`statvfs`) | 44 µs | |
| cpu | 32 µs | |
| memory | 11 µs | |
| load + uptime | 10 µs | |

### What the baseline implies

- **Process ranking is the floor and everything else is noise.** It is one
  `/proc/<pid>/stat` read per process and scales with process count, not with
  any setting. Optimising the other sources is not worth the code.
- **`VPSM_PROCESS_IO` is free — leave it on.** 0.07 ms, because I/O is only
  read for processes that make the top of a ranking.
- **`VPSM_TOP_PROCESSES` is cheap but not free.** Going from 10 to 50
  roughly quadruples the enrichment cost; it is still only 0.87 ms. Payload
  size is the better reason to keep it small.
- **Interval choice is not primarily about agent cost.** Even at 1s the agent
  is 0.37% of one CPU. The real constraint pulls the other way: per-process
  CPU comes from a 100 Hz tick counter, so a 1s window quantises it to whole
  percentage points and a 10s window to 0.1. Short intervals buy frequency,
  not accuracy. 5s–15s is the sensible range; 1s only if frequency genuinely
  matters more than per-process precision.

If the process ranking pass ever does need to get cheaper, the honest options
are sampling processes on a slower cycle than the host gauges, or filtering
by a threshold during the scan rather than after it. Neither is worth doing
at 3 ms.

## Status

Sampling and self-measurement are built and covered by tests. Reporting is
not built at all: no HTTP, no serialisation, no buffering.
`VPSM_COLLECTOR_URL` is read and logged at startup, then ignored. The
contract to build it against is in the next section.

What this agent samples is wider than that protocol carries — most
significantly there is nowhere to put per-process data, which is the entire
task-manager feature. Those gaps, and the decisions needed to close them,
are written up in the root `AGENT-CONTRACT.md`, which is the working
document between this side and the collector side. Don't re-derive them
here; that file is where the answers will land.

## Reporting to the collector

Not built yet, and the next piece of work. The protocol is settled: the
authoritative definition is `../collector/internal/protocol/protocol.go`,
the reasoning is in `../AGENT-CONTRACT.md` (every open question there has
been answered by the collector side), and the summary is in the root
`CLAUDE.md`. Keep this end in sync with that file.

What it asks of this side:

- `POST /v1/reports`, JSON body, `Authorization: Bearer <token>` from
  `VPSM_COLLECTOR_TOKEN`. `Content-Encoding: gzip` is accepted but entirely
  optional.
- **`VPSM_HOST_ID` is required and the agent should exit non-zero without
  it.** There is no safe default — the hostname is not unique across VPSes,
  and the id is the key every stored row hangs off. A failed unit is a
  five-minute fix; data landing under the wrong host is found weeks later.
- A report carries the host's identity once plus 1..1024 samples. Buffer
  when the collector is unreachable, flush on the next successful call, and
  **drop oldest** when the buffer is full.
- Retrying a flush that may already have landed is safe: duplicates are
  counted and dropped, and the response reports `{accepted, duplicate}`.
  The buffer can be released once `accepted + duplicate` covers what was
  sent.
- Retry 5xx, 429, 408 and connection failures with backoff. Never retry
  other 4xx — a rejected payload stays rejected, and retrying forever is
  how an agent becomes the outage. On 413, halve the batch and retry.
- **Branch on `code`, not on the message.** Every non-2xx body carries a
  stable `protocol.ErrorCode`; the prose beside it is for logs and may be
  reworded. `clock_skew` means an operator must fix the clock on this box;
  `malformed` and `invalid` mean the agent has a bug. Both are 400s that
  must not be retried, which is exactly why they need telling apart.
- Send `procs_running` and `procs_blocked` on **every** sample, not only
  inside the process block. They are host gauges, and paired with CPU
  `iowait` they are what separates "stuck on storage" from "busy" — which
  only reads as an answer if they have history.
- **Send counters, not rates**, for anything the kernel counts: CPU ticks
  (all eight states), network bytes, packets, errors and drops. The
  collector diffs consecutive samples, which is what lets it bridge a lost
  report. Per-process figures stay as rates computed here, since a PID is
  not a stable identity between samples.
- Always send `uptime_secs`. A drop in it is how the collector detects a
  reboot and suppresses a bogus spike across the boundary.
- Send `at` from the local clock even if it looks unsynchronised. The
  collector rejects only timestamps more than 24h in the future; a slightly
  wrong clock still produces usable data, and refusing to report would turn
  a minor annoyance into a blackout for that host.

Field names differ from this side's internal naming in a few places —
`filesystems`/`mount_point`/`fs_type`, and `cpu.ticks` rather than a
percentage. The protocol package is the source of truth.

Not carried by v1, so no need to serialise them: per-CPU breakdowns,
per-block-device disk I/O, and `cached`/`buffers` memory. Keep collecting
them for `--once` if they are useful locally.

While developing this side, run the other half locally with
`vpsm-collector serve -token dev`, then `vpsm-collector status` and
`vpsm-collector host <id>` to see exactly what landed. Anything the
collector cannot derive renders as `—` rather than zero, so a field that is
not wired up yet is visibly absent rather than silently wrong.

`../e2e` is the same loop with the collector in a container and the agent
talking to it over a network, which is the arrangement a deployed agent
actually faces. Two things there are worth having before writing the
serialiser rather than after:

- `e2e/agent-sim/gen-report.sh` emits a report this collector accepts, with
  the reasoning for each field inline. It is the reference to diff the
  serialiser's output against — including the mixed-batch shape, where only
  the newest sample carries `processes`.
- `e2e/check.sh` asserts the protocol end to end and does not care which
  agent is sending. Point `mini-bot` at the harness collector, swap the
  stand-in out of `docker-compose.yml`, and it is an acceptance test for
  this side unchanged.

Note that an agent inside a container reads the *host's* `/proc/stat` and
`/proc/meminfo` but only its own PID namespace, so host gauges and the
process list describe different things. See `../e2e/README.md`.
