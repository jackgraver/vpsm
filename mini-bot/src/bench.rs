//! Self-measurement: what the agent costs the box it is watching.
//!
//! mini-bot's whole justification is being small enough to run next to the
//! workloads it measures, and that claim is worth checking rather than
//! asserting. `--bench` measures the real binary, on the real host, doing the
//! real work — which is why this lives in the shipped agent rather than in a
//! separate harness that would benchmark a different build against a
//! different `/proc`.
//!
//! Three questions, in the order they matter:
//!
//! 1. **What does a sample cost, and which source dominates?** Reported as
//!    wall-clock latency per source, so optimisation effort can go where the
//!    time actually is instead of where it feels like it should be.
//! 2. **What does the agent cost when idle?** Separately measured, because
//!    "cheap per sample" and "cheap overall" are different claims and only
//!    the second one is what an operator is buying.
//! 3. **What interval should this host use?** Two figures pull in opposite
//!    directions and the table at the end shows both.
//!
//! Note the distinction between the two time measurements here. *Wall* time
//! per sample is latency — how long a sample takes. *CPU* time per sample is
//! cost — how much of the machine the agent actually took, excluding time
//! spent blocked. The second is the number that competes with the workload,
//! and it is the one the duty-cycle projection is built from.

use std::time::{Duration, Instant};

use crate::config::Config;
use crate::metrics::{Sampler, cpu, disk, host, memory, net, process};
use crate::procfs;

/// Roughly how long to spend measuring each source. Enough iterations to
/// smooth out scheduler noise, short enough that the whole run stays
/// interactive.
const BUDGET: Duration = Duration::from_millis(250);
const MIN_ITERATIONS: usize = 8;
const MAX_ITERATIONS: usize = 4096;

/// How long to sit idle when measuring the cost of doing nothing.
const IDLE_WINDOW: Duration = Duration::from_secs(1);

/// How many samples to run back-to-back when measuring steady-state cost.
///
/// Needs to be enough that the total lands well above the kernel's accounting
/// granularity: CPU time is reported in clock ticks, typically 10ms, so a
/// handful of samples would be measuring rounding error.
const STEADY_STATE_SAMPLES: usize = 60;

/// Intervals worth comparing, in seconds.
const INTERVALS: &[u64] = &[1, 5, 10, 15, 30, 60, 300];

pub fn run(config: &Config) {
    let cpus = cpu::read().map(|stat| stat.per_cpu.len()).unwrap_or(0);
    let identity = host::Identity::read(cpus);
    let memory_total = memory::read().map(|m| m.total_bytes).unwrap_or(0);
    let process_count = process::Tracker::new(0, false)
        .sample(1.0, memory_total)
        .map(|summary| summary.total)
        .unwrap_or(0);

    println!("mini-bot self-measurement");
    println!(
        "{} · {} CPUs · {} processes · {} Hz clock · {} KiB pages\n",
        identity.hostname,
        identity.cpus,
        process_count,
        procfs::clock_ticks(),
        procfs::page_size() / 1024,
    );

    per_source_latency(memory_total, process_count);
    let steady = steady_state(config);
    interval_tradeoff(&steady);
}

/// Time each source independently, so the dominant cost is visible.
fn per_source_latency(memory_total: u64, process_count: u64) {
    println!("per-source sample latency");
    println!("  {:<26}{:>7}{:>10}{:>10}{:>10}", "source", "iters", "mean", "p50", "p99");

    for (label, stats) in [
        ("cpu", measure(|| drop(cpu::read()))),
        ("memory", measure(|| drop(memory::read()))),
        ("load + uptime", measure(|| drop((host::load(), host::uptime())))),
        ("filesystems (statvfs)", measure(|| drop(disk::filesystems()))),
        ("disk i/o", measure(|| drop(disk::io_counters()))),
        ("network", measure(|| drop(net::counters()))),
    ] {
        row(label, &stats);
    }

    // Process sampling is measured at several settings rather than once,
    // because it is the only part with knobs worth tuning and the only part
    // whose cost depends on them. The gap between the first row and the rest
    // is the price of the detail; the gap between the last two is what each
    // extra reported process costs.
    let mut ranking_only = None;
    for (label, top_n, with_io) in [
        ("processes: ranking only", 0, false),
        ("processes: top 10", 10, false),
        ("processes: top 10 + io", 10, true),
        ("processes: top 50 + io", 50, true),
    ] {
        let mut tracker = process::Tracker::new(top_n, with_io);
        if tracker.prime().is_err() {
            continue;
        }
        let stats = measure(|| drop(tracker.sample(1.0, memory_total)));
        row(label, &stats);
        if top_n == 0 {
            ranking_only = Some(stats);
        }
    }


    let full = match Sampler::new(10, true) {
        Ok(mut sampler) => {
            let stats = measure(|| drop(sampler.sample()));
            println!();
            row("full sample", &stats);
            Some(stats)
        }
        Err(error) => {
            println!("  full sample unavailable: {error}");
            None
        }
    };

    // Say outright where the time goes, rather than leaving it to be read off
    // the table. The split matters for optimisation: the ranking pass is a
    // floor that scales with how many processes the host runs and is not
    // configurable, whereas everything above it is.
    if let (Some(ranking), Some(full)) = (ranking_only, full) {
        let share = 100.0 * ranking.mean.as_secs_f64() / full.mean.as_secs_f64().max(f64::MIN_POSITIVE);
        println!(
            "\n  ranking every process is {share:.0}% of a sample and scales with the\n  \
             {process_count} processes on this host — it is the floor. The top-N and io\n  \
             settings only move the remainder."
        );
    }
    println!();
}


/// What the agent costs over a run: idle, per sample, and in memory.
struct SteadyState {
    idle_cpu_secs: f64,
    cpu_secs_per_sample: f64,
    wall_secs_per_sample: f64,
    rss_before: u64,
    rss_after: u64,
}

fn steady_state(config: &Config) -> SteadyState {
    println!("agent cost");

    // Idle first, while nothing else has run. The agent sleeps between
    // samples with no timers and no background threads, so this is expected
    // to be indistinguishable from zero — but "expected to be" is exactly
    // the kind of claim that quietly stops being true, so it gets measured.
    let idle_cpu_secs = match process::own_usage() {
        Ok(before) => {
            std::thread::sleep(IDLE_WINDOW);
            process::own_usage()
                .map(|after| after.cpu_secs - before.cpu_secs)
                .unwrap_or(0.0)
        }
        Err(_) => 0.0,
    };
    println!(
        "  {:<34}{:>12}   over {:.1}s of sleeping",
        "idle cpu", duration(idle_cpu_secs), IDLE_WINDOW.as_secs_f64(),
    );

    let mut sampler = match Sampler::new(config.top_processes, config.process_io) {
        Ok(sampler) => sampler,
        Err(error) => {
            println!("  cannot sample: {error}\n");
            return SteadyState {
                idle_cpu_secs,
                cpu_secs_per_sample: 0.0,
                wall_secs_per_sample: 0.0,
                rss_before: 0,
                rss_after: 0,
            };
        }
    };

    // One sample first, so the figures below describe an agent that has
    // already grown whatever buffers it needs rather than one still warming
    // up.
    let _ = sampler.sample();
    let before = process::own_usage().ok();

    let start = Instant::now();
    for _ in 0..STEADY_STATE_SAMPLES {
        let _ = sampler.sample();
    }
    let wall = start.elapsed();

    let after = process::own_usage().ok();
    let samples = STEADY_STATE_SAMPLES as f64;

    let cpu_secs_per_sample = match (before, after) {
        (Some(before), Some(after)) => (after.cpu_secs - before.cpu_secs) / samples,
        _ => 0.0,
    };
    let wall_secs_per_sample = wall.as_secs_f64() / samples;

    println!(
        "  {:<34}{:>12}   the cost that competes with the workload",
        "cpu time per sample",
        duration(cpu_secs_per_sample),
    );
    println!(
        "  {:<34}{:>12}   latency, including time blocked on /proc",
        "wall time per sample",
        duration(wall_secs_per_sample),
    );

    let (rss_before, rss_after) = (
        before.map(|u| u.rss_bytes).unwrap_or(0),
        after.map(|u| u.rss_bytes).unwrap_or(0),
    );
    println!("  {:<34}{:>12}", "resident memory, first sample", bytes(rss_before));
    println!(
        "  {:<34}{:>12}   {} after {} samples",
        "resident memory, settled",
        bytes(rss_after),
        growth(rss_before, rss_after),
        STEADY_STATE_SAMPLES,
    );
    println!(
        "  {:<34}{:>12}   top {} processes, io {}",
        "configuration measured",
        "",
        config.top_processes,
        if config.process_io { "on" } else { "off" },
    );
    println!();

    SteadyState {
        idle_cpu_secs,
        cpu_secs_per_sample,
        wall_secs_per_sample,
        rss_before,
        rss_after,
    }
}

/// The interval trade-off, which is the whole reason to care about the above.
///
/// Two figures move in opposite directions as the interval grows, and picking
/// an interval means choosing between them:
///
/// - **Duty cycle** falls. The cost per sample is fixed, so sampling less
///   often costs less. This is the obvious one.
/// - **Per-process CPU resolution** gets *better* — which is the one people
///   get backwards. A process's CPU use is derived from a kernel tick
///   counter, so over a short window there are very few ticks to divide by
///   and the result is coarsely quantised. At 100 Hz and a 1s window the
///   smallest non-zero reading is a whole 1 percentage point; over 10s it is
///   0.1. Short intervals give more frequent data, not more accurate data.
fn interval_tradeoff(steady: &SteadyState) {
    println!("interval trade-off");
    println!(
        "  {:>9}{:>14}{:>16}{:>22}",
        "interval", "duty cycle", "cpu per hour", "per-process cpu step",
    );

    for &secs in INTERVALS {
        let interval = Duration::from_secs(secs);
        println!(
            "  {:>9}{:>13.4}%{:>16}{:>20.3} pp",
            format!("{secs}s"),
            duty_cycle_pct(steady.cpu_secs_per_sample, interval),
            duration(steady.cpu_secs_per_sample * (3600.0 / secs as f64)),
            cpu_resolution_points(interval),
        );
    }

    println!(
        "\n  duty cycle is the share of one CPU the agent occupies; resident\n  \
         memory is flat at {} regardless of interval.",
        bytes(steady.rss_after.max(steady.rss_before)),
    );
    if steady.wall_secs_per_sample > 0.0 {
        let floor = crate::config::MIN_INTERVAL.as_secs_f64();
        println!(
            "  a sample takes {} of wall time, {:.1}% of the {:.0}s minimum interval,\n  \
             so sampling never overlaps its own next run.",
            duration(steady.wall_secs_per_sample),
            100.0 * steady.wall_secs_per_sample / floor,
            floor,
        );
    }

    if steady.idle_cpu_secs <= 0.0 {
        println!("  idle cost was below the kernel's accounting granularity.");
    }
}

/// Share of one CPU the agent occupies at a given interval.
fn duty_cycle_pct(cpu_secs_per_sample: f64, interval: Duration) -> f64 {
    let interval_secs = interval.as_secs_f64();
    if interval_secs <= 0.0 {
        return 0.0;
    }
    100.0 * cpu_secs_per_sample / interval_secs
}

/// The smallest non-zero per-process CPU reading, in percentage points.
///
/// One clock tick is the finest the kernel accounts CPU time, so over a
/// window of `interval` the quantum is one tick expressed as a share of it.
fn cpu_resolution_points(interval: Duration) -> f64 {
    let ticks = procfs::clock_ticks() as f64 * interval.as_secs_f64();
    if ticks <= 0.0 {
        return 0.0;
    }
    100.0 / ticks
}

/// Latency distribution for one source.
struct Stats {
    iterations: usize,
    mean: Duration,
    p50: Duration,
    p99: Duration,
}

/// Time `f` repeatedly, within a rough time budget.
fn measure(mut f: impl FnMut()) -> Stats {
    // Warm up and discard. The first calls pay for cold page cache, a cold
    // allocator and cold branch predictors — a real cost, but one paid once
    // at startup, so averaging it into a per-sample figure would overstate
    // the steady state this is trying to describe.
    for _ in 0..3 {
        f();
    }

    let mut timings: Vec<Duration> = Vec::with_capacity(MIN_ITERATIONS);
    let start = Instant::now();
    while timings.len() < MAX_ITERATIONS
        && (timings.len() < MIN_ITERATIONS || start.elapsed() < BUDGET)
    {
        let at = Instant::now();
        f();
        timings.push(at.elapsed());
    }

    Stats::from_timings(timings)
}

impl Stats {
    fn from_timings(mut timings: Vec<Duration>) -> Self {
        if timings.is_empty() {
            return Self {
                iterations: 0,
                mean: Duration::ZERO,
                p50: Duration::ZERO,
                p99: Duration::ZERO,
            };
        }
        timings.sort_unstable();
        let total: Duration = timings.iter().sum();
        Self {
            iterations: timings.len(),
            mean: total / timings.len() as u32,
            p50: timings[timings.len() / 2],
            // Guarded against the index running off the end on small samples.
            p99: timings[(timings.len() * 99 / 100).min(timings.len() - 1)],
        }
    }
}

fn row(label: &str, stats: &Stats) {
    println!(
        "  {:<26}{:>7}{:>10}{:>10}{:>10}",
        label,
        stats.iterations,
        duration(stats.mean.as_secs_f64()),
        duration(stats.p50.as_secs_f64()),
        duration(stats.p99.as_secs_f64()),
    );
}

/// Format a duration in whichever unit keeps it readable.
fn duration(secs: f64) -> String {
    if secs <= 0.0 {
        return "0".to_string();
    }
    if secs < 0.000_001 {
        return format!("{:.0} ns", secs * 1e9);
    }
    if secs < 0.001 {
        return format!("{:.0} µs", secs * 1e6);
    }
    if secs < 1.0 {
        return format!("{:.2} ms", secs * 1e3);
    }
    format!("{secs:.2} s")
}

fn bytes(value: u64) -> String {
    if value == 0 {
        return "-".to_string();
    }
    format!("{:.1} MiB", value as f64 / (1024.0 * 1024.0))
}

/// Describe memory growth across a run, as a leak check.
fn growth(before: u64, after: u64) -> String {
    if before == 0 || after == 0 {
        return String::new();
    }
    match after.cmp(&before) {
        std::cmp::Ordering::Greater => format!("+{:.0} KiB", (after - before) as f64 / 1024.0),
        std::cmp::Ordering::Less => format!("-{:.0} KiB", (before - after) as f64 / 1024.0),
        std::cmp::Ordering::Equal => "unchanged".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn duty_cycle_scales_with_the_interval() {
        // 10ms of CPU per sample.
        assert_eq!(duty_cycle_pct(0.010, Duration::from_secs(1)), 1.0);
        assert_eq!(duty_cycle_pct(0.010, Duration::from_secs(10)), 0.1);
        assert_eq!(duty_cycle_pct(0.010, Duration::ZERO), 0.0);
    }

    #[test]
    fn cpu_resolution_improves_with_longer_windows() {
        let one = cpu_resolution_points(Duration::from_secs(1));
        let ten = cpu_resolution_points(Duration::from_secs(10));
        assert!(ten < one, "longer windows quantise less coarsely");
        // At the near-universal 100 Hz, a 1s window quantises to 1pp.
        assert_eq!(one, 100.0 / procfs::clock_ticks() as f64);
        assert_eq!(cpu_resolution_points(Duration::ZERO), 0.0);
    }

    #[test]
    fn stats_summarise_a_distribution() {
        let timings: Vec<Duration> = (1..=100).map(|ms| Duration::from_millis(ms)).collect();
        let stats = Stats::from_timings(timings);
        assert_eq!(stats.iterations, 100);
        assert_eq!(stats.mean, Duration::from_micros(50_500));
        assert_eq!(stats.p50, Duration::from_millis(51));
        assert_eq!(stats.p99, Duration::from_millis(100));
    }

    #[test]
    fn stats_handle_too_few_timings_to_have_percentiles() {
        let stats = Stats::from_timings(vec![Duration::from_millis(7)]);
        assert_eq!(stats.p50, Duration::from_millis(7));
        assert_eq!(stats.p99, Duration::from_millis(7), "must not index past the end");

        let empty = Stats::from_timings(Vec::new());
        assert_eq!(empty.iterations, 0);
        assert_eq!(empty.p99, Duration::ZERO);
    }

    #[test]
    fn measure_runs_at_least_the_minimum_iterations() {
        let mut calls = 0;
        let stats = measure(|| calls += 1);
        assert!(stats.iterations >= MIN_ITERATIONS);
        // Plus the warmup, which is measured by nobody.
        assert_eq!(calls, stats.iterations + 3);
    }

    #[test]
    fn measure_stays_within_its_budget_for_slow_work() {
        let start = Instant::now();
        let stats = measure(|| std::thread::sleep(Duration::from_millis(20)));
        // The floor is MIN_ITERATIONS, so the budget cannot cut it shorter
        // than that; it must not run away past it either.
        assert!(stats.iterations >= MIN_ITERATIONS);
        assert!(stats.iterations < 40, "ran {} iterations", stats.iterations);
        assert!(start.elapsed() < Duration::from_secs(3));
    }

    #[test]
    fn durations_pick_a_readable_unit() {
        assert_eq!(duration(0.0), "0");
        assert_eq!(duration(0.000_000_5), "500 ns");
        assert_eq!(duration(0.000_123), "123 µs");
        assert_eq!(duration(0.008_9), "8.90 ms");
        assert_eq!(duration(2.5), "2.50 s");
    }

    #[test]
    fn growth_reports_direction() {
        assert_eq!(growth(1024, 3072), "+2 KiB");
        assert_eq!(growth(3072, 1024), "-2 KiB");
        assert_eq!(growth(1024, 1024), "unchanged");
        assert_eq!(growth(0, 1024), "", "nothing measured");
    }
}
