# mini-bot

The VPSM monitoring agent. Deployed onto each VPS being watched; samples
local system usage (CPU, memory, disk, network) on an interval and reports it
to the collector (see `../collector/CLAUDE.md`).

## Why Rust

Needs to be a small, dependency-light, low-overhead static binary that can
run continuously (e.g. under systemd or cron) without itself becoming a
resource problem on the box it's monitoring.

## Layout

- `src/main.rs` — fixed entry point: a doc comment describing the app, then
  `fn main() { startup::run(); }`. Don't add logic here.
- `src/startup.rs` — `startup::run()` is where real wiring happens: load
  config, start the sampling loop, report to the collector.

Add new modules under `src/` as pieces are built out (e.g. a `metrics`
module for sampling, a `report` module for talking to the collector) rather
than growing `startup.rs` indefinitely.

## Toolchain

Rust edition 2024, plain `cargo build` / `cargo run`. No dependencies chosen
yet — pick a metrics-sampling approach (e.g. reading `/proc` directly vs. a
crate like `sysinfo`) deliberately, weighing binary size/overhead against
convenience, before adding one.
