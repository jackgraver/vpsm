//! CPU accounting, from `/proc/stat`.
//!
//! `/proc/stat`'s CPU columns are monotonic counters of clock ticks spent in
//! each state since boot, so "percent busy" is never a single reading — it's
//! the difference between two. Callers keep the previous [`Times`] and ask
//! the newer one to diff itself via [`Times::since`].
//!
//! `steal` is broken out on its own rather than folded into "busy" because
//! this agent runs on rented hardware: steal time is time the hypervisor gave
//! to somebody else's VPS, and it's the clearest evidence that a host is
//! oversold. Lump it in with system time and a noisy neighbour looks like
//! your own kernel being slow; drop it entirely and it hides inside idle.

use std::io;

use crate::procfs;

const STAT: &str = "/proc/stat";

/// Tick counters for one CPU, or for the `cpu` aggregate line.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Times {
    pub user: u64,
    pub nice: u64,
    pub system: u64,
    pub idle: u64,
    pub iowait: u64,
    pub irq: u64,
    pub softirq: u64,
    pub steal: u64,
}

impl Times {
    /// Every tick accounted for on this CPU.
    ///
    /// The kernel's `guest` and `guest_nice` columns are intentionally not
    /// read: they're already counted inside `user` and `nice`, so including
    /// them would inflate the denominator and understate every percentage.
    pub fn total(self) -> u64 {
        self.user
            + self.nice
            + self.system
            + self.idle
            + self.iowait
            + self.irq
            + self.softirq
            + self.steal
    }

    /// Ticks spent doing something, where waiting on disk doesn't count.
    ///
    /// `iowait` is time the CPU sat idle because every runnable task was
    /// blocked on I/O. Counting it as busy makes a disk-bound box look CPU
    /// bound, which points at the wrong fix, so it's reported separately.
    pub fn busy(self) -> u64 {
        self.total() - self.idle - self.iowait
    }

    /// Break the ticks elapsed since `earlier` down into percentages.
    ///
    /// Saturating subtraction throughout: counters reset when a CPU is
    /// hotplugged out and back in, and a wrapped u64 here would otherwise
    /// produce an absurd spike in the reported load.
    pub fn since(self, earlier: Self) -> Usage {
        let elapsed = self.total().saturating_sub(earlier.total());
        if elapsed == 0 {
            // No ticks passed: either two samples landed inside the same tick
            // or the CPU went offline. Either way there's no usage to report.
            return Usage::default();
        }

        let pct = |now: u64, before: u64| 100.0 * now.saturating_sub(before) as f64 / elapsed as f64;
        Usage {
            busy_pct: pct(self.busy(), earlier.busy()),
            user_pct: pct(self.user + self.nice, earlier.user + earlier.nice),
            system_pct: pct(
                self.system + self.irq + self.softirq,
                earlier.system + earlier.irq + earlier.softirq,
            ),
            iowait_pct: pct(self.iowait, earlier.iowait),
            steal_pct: pct(self.steal, earlier.steal),
            idle_pct: pct(self.idle, earlier.idle),
        }
    }
}

/// How a CPU spent the window between two samples. Percentages sum to ~100.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Usage {
    /// Everything that isn't idle or waiting on I/O.
    pub busy_pct: f64,
    /// Userspace, including `nice`d work.
    pub user_pct: f64,
    /// Kernel, including hard and soft interrupt servicing.
    pub system_pct: f64,
    /// Idle only because the runnable tasks were blocked on I/O.
    pub iowait_pct: f64,
    /// Taken by the hypervisor for another guest.
    pub steal_pct: f64,
    pub idle_pct: f64,
}

/// One reading of `/proc/stat`.
#[derive(Clone, Debug, Default)]
pub struct Stat {
    pub total: Times,
    /// Per-CPU times, in the order the kernel lists them (so index == CPU id
    /// on any machine whose CPUs are all online).
    pub per_cpu: Vec<Times>,
    pub context_switches: u64,
    /// Tasks currently on a run queue.
    pub procs_running: u64,
    /// Tasks currently blocked waiting on I/O. Sustained non-zero values here
    /// alongside high `iowait_pct` means the storage is the bottleneck.
    pub procs_blocked: u64,
}

pub fn read() -> io::Result<Stat> {
    Ok(parse(&procfs::read(STAT)?))
}

pub fn parse(text: &str) -> Stat {
    let mut stat = Stat::default();

    for line in text.lines() {
        let Some((label, rest)) = line.split_once(' ') else {
            continue;
        };
        match label {
            "cpu" => stat.total = times(rest),
            "ctxt" => stat.context_switches = procfs::field(rest, 0).unwrap_or(0),
            "procs_running" => stat.procs_running = procfs::field(rest, 0).unwrap_or(0),
            "procs_blocked" => stat.procs_blocked = procfs::field(rest, 0).unwrap_or(0),
            // cpu0, cpu1, ... The aggregate "cpu" line is matched above, so
            // anything still starting with "cpu" here is a single CPU.
            _ if label.starts_with("cpu") => stat.per_cpu.push(times(rest)),
            _ => {}
        }
    }

    stat
}

/// Parse the tick columns that follow a `cpu`/`cpuN` label.
///
/// Missing columns read as zero rather than failing the whole line: the
/// kernel has appended columns over the years (`steal` and the `guest` pair
/// are all newer than the original set), so an older kernel legitimately
/// stops early.
fn times(fields: &str) -> Times {
    let mut columns = fields.split_ascii_whitespace();
    let mut next = || columns.next().and_then(|v| v.parse().ok()).unwrap_or(0);
    Times {
        user: next(),
        nice: next(),
        system: next(),
        idle: next(),
        iowait: next(),
        irq: next(),
        softirq: next(),
        steal: next(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Trimmed from a real /proc/stat: the aggregate line, two CPUs, and the
    // scalar lines we care about.
    const SAMPLE: &str = "\
cpu  100 20 50 1000 10 5 5 10 3 0
cpu0 60 10 30 500 6 3 3 6 2 0
cpu1 40 10 20 500 4 2 2 4 1 0
intr 1234 0 0
ctxt 987654
btime 1700000000
processes 4321
procs_running 3
procs_blocked 1
softirq 555 1 2
";

    #[test]
    fn parses_aggregate_and_per_cpu_lines() {
        let stat = parse(SAMPLE);
        assert_eq!(
            stat.total,
            Times {
                user: 100,
                nice: 20,
                system: 50,
                idle: 1000,
                iowait: 10,
                irq: 5,
                softirq: 5,
                steal: 10,
            }
        );
        assert_eq!(stat.per_cpu.len(), 2);
        assert_eq!(stat.per_cpu[0].user, 60);
        assert_eq!(stat.per_cpu[1].user, 40);
    }

    #[test]
    fn parses_scalar_lines() {
        let stat = parse(SAMPLE);
        assert_eq!(stat.context_switches, 987_654);
        assert_eq!(stat.procs_running, 3);
        assert_eq!(stat.procs_blocked, 1);
    }

    #[test]
    fn guest_columns_are_excluded_from_the_total() {
        // 100+20+50+1000+10+5+5+10 = 1200; the trailing "3 0" are the guest
        // columns and must not be added on top.
        assert_eq!(parse(SAMPLE).total.total(), 1200);
    }

    #[test]
    fn older_kernels_without_steal_still_parse() {
        let stat = parse("cpu  100 20 50 1000\n");
        assert_eq!(stat.total.user, 100);
        assert_eq!(stat.total.steal, 0);
        assert_eq!(stat.total.total(), 1170);
    }

    #[test]
    fn usage_splits_the_window_into_percentages() {
        let earlier = Times::default();
        let now = Times {
            user: 25,
            system: 25,
            idle: 40,
            iowait: 10,
            ..Times::default()
        };
        let usage = now.since(earlier);
        assert_eq!(usage.user_pct, 25.0);
        assert_eq!(usage.system_pct, 25.0);
        assert_eq!(usage.iowait_pct, 10.0);
        assert_eq!(usage.idle_pct, 40.0);
        // Busy excludes both idle and iowait.
        assert_eq!(usage.busy_pct, 50.0);
    }

    #[test]
    fn identical_samples_report_no_usage() {
        let times = parse(SAMPLE).total;
        assert_eq!(times.since(times), Usage::default());
    }

    #[test]
    fn counter_resets_do_not_produce_spikes() {
        // A CPU that went offline and came back has smaller counters than the
        // previous sample; the subtraction must not wrap.
        let earlier = Times { user: 500, idle: 500, ..Times::default() };
        let now = Times { user: 10, idle: 90, ..Times::default() };
        let usage = now.since(earlier);
        assert_eq!(usage, Usage::default(), "reset should read as no usage");
    }
}
