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

Early scaffolding stage. The wire protocol between mini-bot and collector
(transport, payload shape, auth) is not decided yet — figure that out before
building either side's networking in earnest, and keep both ends in sync when
it is.

## Conventions

- Each app's `main`/`main.rs`/`main.go` is kept to just a doc comment plus a
  call into that app's startup routine — see the per-app CLAUDE.md for the
  exact shape. Real logic (config, loops, servers) lives in dedicated
  modules/packages, not in main.
