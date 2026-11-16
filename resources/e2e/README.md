# Local end-to-end harness

Runs the collector and the things that report to it as separate containers on
a shared network, so the two halves of VPSM are exercised across a real HTTP
boundary — real sockets, real auth, real SQLite — rather than in one process.

Unit tests on either side already cover the pieces. What this catches is the
wiring between them, and the handful of properties that are only observable
end to end: that a retried flush is counted rather than written twice, that a
rejection carries a code an agent can branch on, and that a `null` is still a
`null` after crossing JSON, SQLite and a terminal renderer.

## Read this first: this harness covers HTTP ingest only

Since this was written the agent and collector moved to pull over SSH (see
`../CLAUDE.md`); the agent no longer pushes at all, so the stand-in
below is the only HTTP sender there will be. The pull path shares
validation and storage with HTTP ingest and is covered by
`collector/internal/pull` tests. The original note follows.

### The agent cannot report over HTTP

`mini-bot` has no HTTP client, no serialisation and no buffering — see
`../../mini-bot/CLAUDE.md`. It reads `VPSM_COLLECTOR_URL`, logs that reporting
isn't built, and samples locally. So the agent→collector leg cannot be tested
with the real agent today, and this harness does the next best thing:

| | What it is | What it proves today |
|---|---|---|
| `collector` | The real collector | Everything. Serves ingest and reads from a container. |
| `agent-sim` | A stand-in: `curl` plus a shell script that generates protocol-conformant reports | The ingest path, over the network, from a separate container |
| `mini-bot` | The real agent (profile `agent`) | That it builds for and runs in a container, and samples `/proc` there |

`agent-sim` exists to be deleted. Once the agent reports, drop the `agent`
profile from `mini-bot`, give it the same `VPSM_*` variables agent-sim has,
and remove the sim — the checks in `check.sh` do not care which of them is
sending, so they become the agent's acceptance tests unchanged.

## Running it

```sh
docker compose up --build -d     # collector + two stand-in agents
./check.sh                       # 35 assertions over HTTP
docker compose down              # tear down; the database is ephemeral
```

`check.sh` exits non-zero on the first failing property and names what broke
rather than which line did. It needs `curl` and GNU `date` on the host; the
rest runs inside the containers.

There is deliberately **no volume**: every `down` discards the database. That
is what you want from a fixture, and it sidesteps the collector's refusal to
open a file written by a different schema version — so a protocol change
needs no migration dance here, just `down` and `up`.

## Looking at it

The two stand-in agents report every 5s as `sim-web-01` (healthy) and
`sim-db-01` (in trouble). Each opens with a 30-sample backfill batch, the way
an agent does after an outage, so the views have history immediately instead
of after three minutes of waiting.

```sh
# The fleet overview — the "anything on fire?" view.
docker compose exec collector vpsm-collector status

# Everything about one host: trends, filesystems, interfaces, process table.
docker compose exec collector vpsm-collector host sim-db-01

# The same, redrawn live. Ctrl-C to stop.
docker compose exec collector vpsm-collector watch

# Reading over HTTP instead of the SQLite file. Both satisfy query.Source,
# and this is the path a remote operator uses.
docker compose exec collector \
    vpsm-collector status -remote http://127.0.0.1:8777 -token dev
```

`sim-db-01` is the interesting one. It is built to be in trouble on exactly
the axes the collector grades separately, so the overview's flags column
reads `steal 25%  inodes 99%  swap 75%  net errs +18` — four problems a
plain utilisation percentage would hide. Its disk cell says `99% / inodes`
because inodes, not bytes, are the binding limit.

Its process table is where the contract's sharp edges are visible:

```
 PID  USER       CPU%  MEM%   RSS  S           IO ↓/↑  COMMAND
1234  postgres  780.5   6.3  520M  R  1.0M/s / 256K/s  postgres: parallel worker
2048  www-data   12.5   1.2  100M  S              n/a  nginx: worker process
4096  ?           0.3  25.0  2.0G  S      0B/s / 0B/s  /opt/vendor/bin/daemon
  57  root        0.1   0.0    0B  I              n/a  [kworker/3:1]
```

Four things there are deliberate, and `check.sh` asserts each:

- **780.5%** — `cpu_pct` is a share of one core and routinely exceeds 100.
  Anything in the stack validating it against 100 would reject real data.
- **`n/a` on pid 2048 against `0B/s` on pid 4096** — the first had its I/O
  read denied by ptrace, the second genuinely did no I/O. Null is not zero,
  and the two must not look alike.
- **`?` for pid 4096's user** — the uid didn't resolve. Not `root`, not blank.
- **A single-sample host shows CPU as `—`** — percentages are ratios of tick
  deltas, so one sample has no percentage to give. Unknown, not 0%.

### Other things worth poking

```sh
# Follow the stand-in agents reporting, and the collector logging each
# request with its status — a misconfigured agent's 401s are visible here.
docker compose logs -f agent-sim collector

# The real agent, sampling inside a container.
docker compose --profile agent up --build -d mini-bot
docker compose logs -f mini-bot

# One sample from the real agent and exit — the quickest look at what it
# collects, including the per-CPU and per-disk figures v1 does not carry.
docker compose --profile agent run --rm mini-bot --once

# What the agent costs the box it watches.
docker compose --profile agent run --rm mini-bot --bench

# Hand-build a report and post it. The protocol is JSON over HTTP precisely
# so this works; `code` is what an agent branches on, and its prose is not.
./agent-sim/gen-report.sh my-host 1 | curl -sS -X POST \
    http://127.0.0.1:8777/v1/reports \
    -H 'Authorization: Bearer dev' -H 'Content-Type: application/json' \
    --data-binary @-

# A host in trouble, on demand: PROFILE=hot is what sim-db-01 uses.
PROFILE=hot ./agent-sim/gen-report.sh my-host 1 | ...
```

`gen-report.sh <host-id> [samples] [tick] [end-epoch]` is also the readable
reference for the wire bytes, with the reasoning for each field inline — the
agent side has to hand-roll this JSON, and the generator is what it can be
diffed against.

### Testing an outage

The behaviour batching exists for. Stop the collector, watch the stand-in
agent fail, then bring it back:

```sh
docker compose stop collector
docker compose logs -f agent-sim          # cannot reach the collector
docker compose start collector
docker compose logs -f agent-sim          # resumes on its own
```

The failure shows up as `Could not resolve host: collector` rather than a
refused connection, because compose withdraws the DNS name along with the
container. Same class of failure from the agent's side — the collector is
unreachable, retry later — and worth knowing so it doesn't read as a
misconfiguration.

The real agent will buffer and flush the missed samples here. The stand-in
does not: it advances its counters across the outage and drops what it could
not send, so what this shows is the "keep trying and recover" half, not the
buffer. (Its backoff only runs on the opening batch; in steady state it
simply retries on the next interval.)

What the collector does with the gap is the part worth looking at, and it is
the whole argument for sending counters rather than rates:

```sh
docker compose exec collector vpsm-collector host sim-web-01
```

The CPU trend stays flat at 22% straight across the outage. The counters on
either side of the gap diff cleanly, so the missing reports cost one interval
of resolution and nothing else — where pre-computed rates would have left a
hole, and a naive counter reset would have drawn a spike.

## What the numbers mean in a container

Worth knowing before trusting anything `mini-bot` reports from inside the
`mini-bot` container, because it is not a bug and it will look like one:

- **Host gauges are the host's.** `/proc/stat`, `/proc/meminfo` and
  `/proc/loadavg` are not namespaced, so CPU, memory and load are your whole
  machine's — the container shows 24 CPUs and 30 GiB because the laptop has
  them, not because the container does.
- **The process list is the container's.** PIDs *are* namespaced, so the
  table has one row: `mini-bot` itself. `pid: host` in the compose file gets
  the real view, at the cost of giving the container visibility into every
  process on your machine — fine for a deliberate local look, not something
  to leave switched on.
- **Filesystems are the overlay's, plus Docker's bind mounts.** `/` reports
  the host disk's figures through the overlay, and `/etc/hosts` shows up as
  its own mount with the same numbers, because Docker bind-mounts it.
- **cgroup limits are invisible.** The agent reads `/proc`, not
  `/sys/fs/cgroup`, so a memory-limited container still reports the host's
  total. That is correct for the VPS it is designed for and wrong for a
  container, which is a thing to know if this ever gets pointed at one.

None of this affects the collector-side checks: `agent-sim` sends fixed
synthetic numbers precisely so the assertions are about the protocol and not
about whatever the machine running it happens to be doing.

## Leftovers

`check.sh` writes under host ids prefixed `zz-` so they sort below the real
hosts in the fleet view. All but one are fixed ids that re-runs reuse; the
single-sample check needs a never-before-seen host each time, so repeated
runs leave a `zz-check-single-<epoch>` row behind. Retention in the harness
is an hour with a one-minute sweep, and `docker compose down` clears
everything regardless.
