//! mini-bot — the VPSM monitoring agent.
//!
//! Deployed onto each VPS that's being watched. On an interval, samples
//! local system usage (CPU, memory, disk, network) and reports those
//! samples to the central VPSM collector over the network.
//!
//! Built in Rust and kept deliberately small and dependency-light: this is
//! meant to run continuously (e.g. under systemd) on boxes that are also
//! running the actual workloads being monitored, so it can't afford to be
//! heavy or to compete for CPU/memory with them.
//!
//! This file is just the entry point. All real work — config loading, the
//! sampling loop, reporting to the collector — happens in `startup::run`
//! and the modules it wires together; keep `main` itself minimal.

mod startup;

fn main() {
    startup::run();
}
