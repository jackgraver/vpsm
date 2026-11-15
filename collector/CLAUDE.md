# collector

The VPSM central server. Receives usage reports sent by every `mini-bot`
agent (see `../mini-bot/CLAUDE.md`), stores them, and renders them for
review in the terminal.

One binary does both jobs: `serve` is the server, and `status`/`watch`/`host`
are the views over what it collected.

## Commands

    vpsm-collector serve -token <tok>      # receive reports (default 127.0.0.1:8777)
    vpsm-collector status                  # one line per host: the "anything on fire?" view
    vpsm-collector watch -interval 5s      # same, redrawn live
    vpsm-collector host web-01             # everything known about one host

The display commands read the SQLite file directly by default, or a remote
collector over HTTP with `-remote <url> -token <tok>`. Both satisfy the same
`query.Source` interface, so the views cannot tell which they were handed —
`internal/api.TestClientMatchesStorage` is the contract test that keeps the
two readings identical.

## Layout

- `main.go` — fixed entry point: a doc comment describing the app, then
  `func main() { startup.Run() }`. Don't add logic here.
- `internal/startup/startup.go` — `startup.Run()` parses the command line
  and wires each subcommand up.
- `internal/protocol` — the mini-bot↔collector wire format. This is the
  shared contract; changing it means changing the agent too. Start here.
- `internal/storage` — SQLite: schema, ingest, reads (where counters become
  rates), retention sweep.
- `internal/query` — the read-side types and the `Source` interface that
  both storage and the HTTP client implement.
- `internal/api` — HTTP handlers, auth, request limits.
- `internal/client` — reads a *remote* collector's API as a `query.Source`.
- `internal/view` — terminal rendering. Pure functions from query data to
  text; colour and width are passed in, not discovered.
- `internal/cli` — the command implementations.
- `internal/config` — settings, resolved flag > environment > default.

## Decisions worth not re-litigating

Most of these were settled against `../AGENT-CONTRACT.md`, which carries the
longer reasoning.

- **SQLite**, via pure-Go `modernc.org/sqlite` (no CGO, so the binary still
  cross-compiles static with no C toolchain and nothing to install on the
  host). WAL mode is not optional: it is what lets a CLI reader work against
  the file while the server writes to it.
- **The schema is versioned** (`PRAGMA user_version`) and `Open` refuses a
  file written by a different version rather than failing later on a column
  that moved. There is no upgrade path yet, deliberately — while the
  protocol settles, telling the operator to start a fresh file beats
  silently reinterpreting old columns.
- **Timestamps are stored as epoch milliseconds** — compact,
  integer-sortable, no timezone ambiguity. All times handed back are UTC.
- **Counters are stored raw and rates derived on read.** This is the whole
  reason the protocol carries counters: if a report is lost, the next diff
  spans the gap instead of leaving a hole. CPU percentages are ratios of
  tick deltas, so the collector never needs to know `USER_HZ`.
- **Process data is a snapshot, not a series.** One row per host, replaced
  per report, never per sample — a busy host has 500+ processes and
  high-cardinality command strings, and the question it answers is almost
  always about right now. A backfilled report cannot overwrite a newer
  snapshot. Widening to full history later is a new table, not a reshape.
- **Auth is required to serve.** No token and no `-allow-anonymous` is a
  startup error, because the alternative — quietly accepting unauthenticated
  reports — means anyone who can reach the port can forge or flood metrics.
- **`serve` binds loopback by default.** A collector should be exposed
  deliberately (reverse proxy with TLS, or a private network).
- **Agent timestamps are trusted, within a day.** Ordering by the agent's
  clock is what makes buffered backfill land correctly. Samples stamped
  more than 24h in the future are rejected; past ones never are.
- **Sparklines use a fixed 0-100 scale.** An auto-fitted scale would make an
  idle host's 2-3% jitter look identical to a saturated one's, which is
  backwards for a fleet overview.
- **Unknown is not zero.** A rate the collector cannot derive — a single
  sample, a reboot, a counter reset — renders as a dash, never `0 B/s`. A
  process's `io` is null when ptrace denied it, not zero. A stale host is
  graded unknown regardless of what its last numbers said, because a silent
  agent is a finding and not a healthy host.
- **Some signals are graded on their own scale.** Steal warns at 5% and
  crits at 15%, against 75/90 for ordinary utilisation, because a few
  percent of stolen CPU means something a few percent of user CPU does not.
  Filesystem pressure is the worse of bytes and inodes, since a disk out of
  inodes fails writes with bytes to spare.
- **The overview has a flags column** so a red dot always has a visible
  cause. Steal, inode exhaustion, swap and moving error counters are
  exactly the problems a percentage column hides.
- **The views are a TUI in waiting.** Rendering is already a pure function
  of the data; `cli.Watch` is a redraw loop around it. Moving to a TUI
  framework replaces that loop and keeps everything beneath it.

## Terminal output

`internal/view` lays out its own columns rather than using `text/tabwriter`,
because cells carry ANSI colour codes that occupy no visible space — widths
are measured with `visibleLen`, and padding goes *outside* a cell's style
wrapper so trailing whitespace can still be trimmed. Colour is suppressed
when stdout is not a terminal, and when `NO_COLOR` is set.

## Toolchain

Go 1.25, module `vpsm-collector`. One direct dependency
(`modernc.org/sqlite`); everything else is stdlib, and worth keeping that
way. `go test ./...` covers storage, protocol validation, the API, the
client/storage contract, and the renderers.

`Dockerfile` builds the server as a container image — `CGO_ENABLED=0`, which
the pure-Go SQLite driver is what makes possible. `../e2e` uses it to run the
collector against agents on a container network, and `../e2e/check.sh`
asserts over real HTTP the things unit tests can only assert in process:
which code comes back on each rejection, that a retried flush counts as a
duplicate, and that a `null` is still a `null` after JSON, SQLite and the
renderer have each had a turn at it.
