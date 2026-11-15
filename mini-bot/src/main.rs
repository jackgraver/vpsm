//! mini-bot — the VPSM monitoring agent.
//!
//! Deployed onto each VPS that's being watched. On an interval, samples
//! local system usage (CPU, memory, disk, network, and per-process usage)
//! and reports those samples to the central VPSM collector over the network.
//!
//! Built in Rust and kept deliberately small and dependency-light: this is
//! meant to run continuously (e.g. under systemd) on boxes that are also
//! running the actual workloads being monitored, so it can't afford to be
//! heavy or to compete for CPU/memory with them.
//!
//! This file is just the entry point — the module list below, which has to
//! live in the crate root, and the call into `startup::run`. All real work
//! happens there and in the modules it wires together; keep `main` minimal.

mod bench;
mod config;
mod metrics;
mod procfs;
mod render;
mod startup;

fn main() {
    startup::run();
}
