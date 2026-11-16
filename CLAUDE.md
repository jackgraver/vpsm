# VPSM (VPS Monitor)

VPSM keeps an eye on one or more VPS hosts. It has two halves, resources and
logs, which share one agent on each VPS.

## Layout

- **`mini-bot/`** — the tiny Rust agent deployed onto each monitored VPS,
  shared by both halves. Today it samples system usage on an interval into
  an on-disk spool, which the collector pulls over SSH. Written in Rust
  specifically to stay small and low-overhead, since it runs alongside (and
  must not compete with) the services it's watching. See
  `mini-bot/CLAUDE.md`.
- **`resources/`** — host metrics: CPU, memory, disk, network, processes.
  `resources/collector/` is the Go side that pulls samples from every
  mini-bot, stores them in SQLite and shows them; `resources/e2e/` is the
  docker compose harness. The wire protocol between agent and collector is
  documented there. See `resources/CLAUDE.md`.
- **`logs/`** — log search. `logs/viewer/` is a Go Bubble Tea TUI that reads
  log files of any format and searches them by text and level. Local only
  so far; pulling logs from each VPS through mini-bot is next. See
  `logs/CLAUDE.md`.

This is a single monorepo for convenience of working on everything
together. The parts are still independently deployable (different
languages, different build/release artifacts, separate Go modules) — the
shared repo is not a sign they ship together.

## Conventions

- Each app's `main`/`main.rs`/`main.go` is kept to just a doc comment plus a
  call into that app's startup routine — see the per-app CLAUDE.md for the
  exact shape. Real logic (config, loops, servers) lives in dedicated
  modules/packages, not in main.
- mini-bot has no network code; whatever the laptop side needs from a VPS
  it gets by running `ssh <host> mini-bot <subcommand>`. New halves should
  keep it that way.
- Terminal UIs use Go with Bubble Tea and Lip Gloss, the same stack as the
  j-cicd project, rather than adding a UI framework to the agent.
