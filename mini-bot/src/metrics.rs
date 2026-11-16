//! Sampling: one reading of everything mini-bot watches.
//!
//! Each submodule owns one source under `/proc` and knows nothing about the
//! others. This module is the part that holds them together, and its real job
//! is managing the thing they all have in common: most of what's interesting
//! about a running machine is a *rate*, and `/proc` only ever offers counters
//! since boot. So [`Sampler`] keeps the previous reading and every [`Sample`]
//! describes the window between two of them.
//!
//! That's also why a `Sampler` must be constructed before the first sample is
//! taken, and why the first sample worth reporting arrives one interval after
//! the agent starts rather than immediately.

pub mod cpu;
pub mod disk;
pub mod host;
pub mod memory;
pub mod net;
pub mod process;

use std::collections::HashSet;
use std::io;
use std::time::Instant;

/// Everything observed about the host over one interval.
#[derive(Clone, Debug)]
pub struct Sample {
    /// Length of the window these rates were measured over. Won't exactly
    /// equal the configured interval — sampling takes time, and a loaded box
    /// is exactly when the scheduler is least punctual — so the collector
    /// should trust this over the interval it was told about.
    pub elapsed_secs: f64,
    pub uptime_secs: f64,
    pub load: host::Load,
    pub cpu: cpu::Usage,
    /// Per-CPU usage. Worth keeping separate from the aggregate: a
    /// single-threaded process pinning one core shows as 100% here and as a
    /// forgettable 4% in `cpu` on a 24-CPU box.
    pub per_cpu: Vec<cpu::Usage>,
    pub context_switches_per_sec: f64,
    pub procs_running: u64,
    pub procs_blocked: u64,
    /// The aggregate tick counters this sample's `cpu` percentages were
    /// derived from. The wire carries these rather than the percentages, so
    /// the collector can diff across a gap in delivery.
    pub cpu_ticks: cpu::Times,
    /// Raw interface counters, for the same reason. `networks` is the rate
    /// view of the same readings, for local display.
    pub net_counters: Vec<net::Counters>,
    pub memory: memory::Memory,
    pub filesystems: Vec<disk::Filesystem>,
    pub disks: Vec<disk::IoRates>,
    pub networks: Vec<net::Rates>,
    pub processes: process::Summary,
}

/// The counters we need to keep in order to turn the next reading into rates.
struct Snapshot {
    at: Instant,
    cpu: cpu::Stat,
    disk_io: Vec<disk::IoCounters>,
    networks: Vec<net::Counters>,
}

/// Takes samples, holding the previous reading so rates can be computed.
pub struct Sampler {
    previous: Snapshot,
    processes: process::Tracker,
    warnings: Warnings,
}

impl Sampler {
    /// Prime the sampler by taking a first reading of every counter.
    ///
    /// Fails only if `/proc/stat` can't be read, which means there's no procfs
    /// to monitor and nothing this agent can usefully do. Every other source
    /// is allowed to be missing — a container without `/proc/diskstats` should
    /// still report CPU and memory.
    pub fn new(top_processes: usize, process_io: bool) -> io::Result<Self> {
        let mut warnings = Warnings::default();
        let mut processes = process::Tracker::new(top_processes, process_io);
        warnings.check("processes", processes.prime());

        Ok(Self {
            previous: Snapshot {
                at: Instant::now(),
                cpu: cpu::read()?,
                disk_io: warnings.check("disk I/O", disk::io_counters()).unwrap_or_default(),
                networks: warnings.check("network", net::counters()).unwrap_or_default(),
            },
            processes,
            warnings,
        })
    }

    /// Take a sample describing the window since the previous one.
    pub fn sample(&mut self) -> io::Result<Sample> {
        let now = Instant::now();
        let elapsed_secs = now.duration_since(self.previous.at).as_secs_f64();

        let cpu = cpu::read()?;
        let disk_io = self
            .warnings
            .check("disk I/O", disk::io_counters())
            .unwrap_or_default();
        let networks = self
            .warnings
            .check("network", net::counters())
            .unwrap_or_default();

        let memory = self
            .warnings
            .check("memory", memory::read())
            .unwrap_or_default();

        let processes = self
            .warnings
            .check(
                "processes",
                self.processes.sample(elapsed_secs, memory.total_bytes),
            )
            .unwrap_or_default();

        let sample = Sample {
            elapsed_secs,
            uptime_secs: self.warnings.check("uptime", host::uptime()).unwrap_or(0.0),
            load: self.warnings.check("load", host::load()).unwrap_or_default(),
            cpu: cpu.total.since(self.previous.cpu.total),
            // Zip rather than index: a CPU can be hotplugged out between
            // samples, and the ones still present are all comparable.
            per_cpu: cpu
                .per_cpu
                .iter()
                .zip(&self.previous.cpu.per_cpu)
                .map(|(now, before)| now.since(*before))
                .collect(),
            context_switches_per_sec: per_sec(
                cpu.context_switches,
                self.previous.cpu.context_switches,
                elapsed_secs,
            ),
            procs_running: cpu.procs_running,
            procs_blocked: cpu.procs_blocked,
            cpu_ticks: cpu.total,
            net_counters: networks.clone(),
            memory,
            filesystems: self
                .warnings
                .check("filesystems", disk::filesystems())
                .unwrap_or_default(),
            disks: rates(&disk_io, &self.previous.disk_io, |now, before| {
                (now.device == before.device).then(|| now.since(before, elapsed_secs))
            }),
            networks: rates(&networks, &self.previous.networks, |now, before| {
                (now.name == before.name).then(|| now.since(before, elapsed_secs))
            }),
            processes,
        };

        self.previous = Snapshot {
            at: now,
            cpu,
            disk_io,
            networks,
        };

        Ok(sample)
    }
}

/// Pair each current counter with its previous reading and difference them.
///
/// Devices and interfaces come and go — a volume gets attached, a container
/// creates a veth pair — and one with no previous reading is skipped rather
/// than reported as a spike from zero. It'll appear in the next sample, which
/// is the first one where its rate means anything.
fn rates<C, R>(current: &[C], previous: &[C], diff: impl Fn(&C, &C) -> Option<R>) -> Vec<R> {
    current
        .iter()
        .filter_map(|now| previous.iter().find_map(|before| diff(now, before)))
        .collect()
}

/// Turn the change in a counter into a per-second rate.
///
/// Saturating, because counters do go backwards: interfaces are recreated,
/// disks are detached and reattached, and a wrapped subtraction here would
/// show up as a preposterous spike in whatever dashboard is downstream.
pub(crate) fn per_sec(now: u64, before: u64, elapsed_secs: f64) -> f64 {
    if elapsed_secs <= 0.0 {
        return 0.0;
    }
    now.saturating_sub(before) as f64 / elapsed_secs
}

/// Tracks which sources are currently failing, so a broken one is reported
/// once rather than every interval.
///
/// A monitoring agent that floods the journal with the same line every few
/// seconds is its own kind of outage, but a source that fails silently is
/// worse. Reporting each transition — and the recovery — gives both.
#[derive(Default)]
struct Warnings {
    failing: HashSet<&'static str>,
}

impl Warnings {
    fn check<T>(&mut self, source: &'static str, result: io::Result<T>) -> Option<T> {
        match result {
            Ok(value) => {
                if self.failing.remove(source) {
                    eprintln!("mini-bot: {source} is readable again");
                }
                Some(value)
            }
            Err(error) => {
                if self.failing.insert(source) {
                    eprintln!("mini-bot: cannot sample {source}: {error}");
                }
                None
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn per_sec_divides_the_delta_by_the_window() {
        assert_eq!(per_sec(300, 100, 2.0), 100.0);
        assert_eq!(per_sec(100, 100, 2.0), 0.0);
    }

    #[test]
    fn per_sec_survives_counter_resets_and_zero_windows() {
        assert_eq!(per_sec(5, 500, 1.0), 0.0, "reset must not wrap");
        assert_eq!(per_sec(500, 5, 0.0), 0.0, "no window, no rate");
        assert_eq!(per_sec(500, 5, -1.0), 0.0);
    }

    #[test]
    fn rates_skips_entries_with_no_previous_reading() {
        let previous = vec![net::Counters { name: "eth0".into(), rx_bytes: 1_000, ..Default::default() }];
        let current = vec![
            net::Counters { name: "eth0".into(), rx_bytes: 3_000, ..Default::default() },
            // Attached since the last sample: no baseline, so no rate.
            net::Counters { name: "eth1".into(), rx_bytes: 9_999, ..Default::default() },
        ];
        let computed = rates(&current, &previous, |now, before| {
            (now.name == before.name).then(|| now.since(before, 2.0))
        });
        assert_eq!(computed.len(), 1);
        assert_eq!(computed[0].name, "eth0");
        assert_eq!(computed[0].rx_bytes_per_sec, 1_000.0);
    }

    #[test]
    fn warnings_report_each_transition_once() {
        let mut warnings = Warnings::default();
        let fail = || -> io::Result<u8> { Err(io::Error::other("nope")) };

        assert!(warnings.check("disks", fail()).is_none());
        assert!(warnings.failing.contains("disks"));
        // Still failing: already reported, state unchanged.
        assert!(warnings.check("disks", fail()).is_none());
        assert_eq!(warnings.failing.len(), 1);

        assert_eq!(warnings.check("disks", Ok(7u8)), Some(7));
        assert!(warnings.failing.is_empty(), "recovery clears the state");
    }

    #[test]
    fn sampler_produces_a_coherent_sample_of_the_live_host() {
        let mut sampler = Sampler::new(5, false).expect("/proc is readable");
        let sample = sampler.sample().expect("samples");

        assert!(sample.elapsed_secs >= 0.0);
        assert!(sample.uptime_secs > 0.0, "the host has been up for a while");
        assert!(sample.memory.total_bytes > 0);
        assert!(sample.memory.used_bytes <= sample.memory.total_bytes);
        assert!(!sample.per_cpu.is_empty(), "at least one CPU");
        assert!(sample.processes.total > 0);

        // Percentages are percentages.
        assert!((0.0..=100.0).contains(&sample.cpu.busy_pct), "{}", sample.cpu.busy_pct);
        assert!((0.0..=100.0).contains(&sample.memory.used_pct()));
        for fs in &sample.filesystems {
            assert!((0.0..=100.0).contains(&fs.used_pct()), "{}: {}", fs.mount_point, fs.used_pct());
            assert!(fs.total_bytes > 0);
        }
    }

    #[test]
    fn consecutive_samples_measure_a_real_window() {
        let mut sampler = Sampler::new(5, false).expect("/proc is readable");
        sampler.sample().expect("samples");
        let second = sampler.sample().expect("samples");
        assert!(second.elapsed_secs > 0.0, "the second window has duration");
    }
}
