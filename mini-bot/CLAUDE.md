# mini-bot

The VPSM monitoring agent. Deployed onto each VPS being watched; samples
local system usage (CPU, memory, disk, network, and per-process usage) on an
interval into an on-disk spool, which the collector pulls over SSH (see
`../collector/CLAUDE.md`).

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
  view only (`--once`); nothing should ever parse it.
- `src/wire.rs` — a `Sample` as the collector's protocol JSON, hand-written
  (no serde). Also where values the collector would reject are made safe.
- `src/spool.rs` — the on-disk backlog: append, rotate, thin, drop, ack.
- `src/export.rs` — `mini-bot export [--follow]`, the command the collector
  runs over SSH to drain the spool.
- `deploy/` — systemd unit and `install.sh`.

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

Sampling, self-measurement, serialisation, the spool and `export` are built
and covered by tests. The agent has no network code and should not grow
any: the collector pulls. The transport is described in the root
`CLAUDE.md` and in `../collector/internal/protocol/pull.go`, which is
authoritative.

## Modes

    mini-bot                     sample into the spool forever (the service)
    mini-bot --once              print one sample and exit
    mini-bot --bench             measure the agent's own cost and exit
    mini-bot export [--follow]   stream the spool to a collector on stdout

Configuration is environment variables (`VPSM_*`, see `config.rs`). The
service reads them from `/etc/vpsm/mini-bot.env`. **`VPSM_HOST_ID` is
required to monitor and the agent exits non-zero without it**: hostnames
are not unique across VPSes, and data filed under the wrong host is found
weeks later. `export` runs over a bare SSH session without that
environment, so it reads the host block from the spool (`host.json`) and
finds the spool at the default path, or `--spool <dir>`.

## The spool

`spool.rs` has the full layout in its module doc. What is worth knowing
before changing it:

- **Acks name a sequence number, never a time.** Lines are
  `<seq>\t<at_ms>\t<json>`; seqs only increase, and resume past the
  `cursor` file after a restart even if every segment was deleted.
- **Nothing is deleted on read**, only on ack, and the collector acks only
  what it has committed. Re-sending is safe (duplicates).
- **The newest segment is never deleted by an ack** — the daemon may still
  be appending to it. Deletes and rewrites take a `flock`; appends do not.
- **Spooled samples carry no process list.** The collector keeps only the
  newest snapshot, so the full newest sample lives in `latest` and export
  attaches it. This keeps a sample around 1.5–2 KB.
- **Thinning before dropping.** Past half the cap, the oldest sealed
  segments are rewritten to one sample per `VPSM_SPOOL_THIN_SECS`. The wire
  carries counters, so averages across a thinned window stay exact.
- **Sharing.** The service runs as root (per-process I/O needs it), `export`
  as the SSH user. `install.sh` puts both in group `vpsm` with a setgid
  directory, and both ends set umask 007.

## Wire format notes

Field names follow `../collector/internal/protocol/protocol.go`, not this
crate's internal naming (`filesystems`/`mount_point`/`fs_type`, `cpu.ticks`
rather than percentages). The rules from the protocol that this side
enforces:

- **Counters, not rates**, for CPU ticks (all eight states) and interface
  bytes/packets/errors/drops. `Sample` carries the raw readings alongside
  the rates `--once` displays. Per-process figures stay rates.
- `procs_running`/`procs_blocked` and `uptime_secs` on every sample.
- `at` from the local clock even if it looks unsynchronised.
- Anything the collector rejects is fixed in `wire.rs` rather than shipped:
  control characters in process names/commands, commands over 512 bytes,
  more than 64 top processes, non-finite floats. A rejected sample is
  discarded, so a bug here is silent data loss — keep the tests honest.

Not carried by v1: per-CPU breakdowns, per-block-device disk I/O, and
`cached`/`buffers` memory. They stay in `Sample` for `--once`.

## Deploying

Build a static binary (the Dockerfile's build stage produces one, musl), then
on the host, from a directory holding `mini-bot`, `install.sh` and
`mini-bot.service`:

    sudo ./install.sh <host-id> [pull-user]

It is idempotent: it upgrades the binary and unit and keeps an existing
`/etc/vpsm/mini-bot.env` and spool.

Note that an agent inside a container reads the *host's* `/proc/stat` and
`/proc/meminfo` but only its own PID namespace, so host gauges and the
process list describe different things. See `../e2e/README.md`.
