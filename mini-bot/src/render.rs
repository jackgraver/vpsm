//! Human-readable rendering of a sample.
//!
//! This exists so that a sample can be *looked at* — on a new box, after a
//! config change, or via `--once` — without needing a collector and a UI to
//! be standing. The collector will receive structured data over the wire
//! instead (that protocol isn't settled yet; see the root `CLAUDE.md`), so
//! nothing downstream should ever parse this text.
//!
//! Formatting choices here are about what an operator reads first. Per-CPU
//! figures collapse to "the busiest one", because twenty-four lines of idle
//! hide the one core that's pinned; byte counts get binary units and one
//! decimal, because the question is always "roughly how much" and never
//! "exactly how many bytes".

use crate::metrics::Sample;
use crate::metrics::host::Identity;
use crate::metrics::process::Process;

/// Command lines are kept long in the sample but cut short for display, so
/// that one `java -cp ...` invocation doesn't wrap over the whole table.
const COMMAND_WIDTH: usize = 56;

/// Render a full sample as a block of text, without a trailing newline.
pub fn sample(host: &Identity, sample: &Sample) -> String {
    let mut out = String::with_capacity(2048);

    out.push_str(&format!(
        "{} · linux {} · {} CPU{} · up {} · sampled over {:.1}s\n",
        host.hostname,
        host.kernel,
        host.cpus,
        if host.cpus == 1 { "" } else { "s" },
        duration(sample.uptime_secs),
        sample.elapsed_secs,
    ));

    out.push_str(&format!(
        "\ncpu       {:.1}% busy   user {:.1}%  sys {:.1}%  iowait {:.1}%  steal {:.1}%\n",
        sample.cpu.busy_pct,
        sample.cpu.user_pct,
        sample.cpu.system_pct,
        sample.cpu.iowait_pct,
        sample.cpu.steal_pct,
    ));
    out.push_str(&format!(
        "          load {:.2} / {:.2} / {:.2} ({:.2} per CPU)   {} running, {} blocked\n",
        sample.load.one,
        sample.load.five,
        sample.load.fifteen,
        sample.load.per_cpu(host.cpus),
        sample.procs_running,
        sample.procs_blocked,
    ));
    if let Some((index, busiest)) = busiest_cpu(sample) {
        out.push_str(&format!(
            "          busiest cpu{} at {:.1}%   {} context switches/s\n",
            index,
            busiest,
            count(sample.context_switches_per_sec),
        ));
    }

    out.push_str(&format!(
        "\nmemory    {} of {} used ({:.1}%)   cache {}   available {}\n",
        bytes(sample.memory.used_bytes),
        bytes(sample.memory.total_bytes),
        sample.memory.used_pct(),
        bytes(sample.memory.cached_bytes),
        bytes(sample.memory.available_bytes),
    ));
    if sample.memory.swap_total_bytes > 0 {
        out.push_str(&format!(
            "swap      {} of {} used ({:.1}%)\n",
            bytes(sample.memory.swap_used_bytes),
            bytes(sample.memory.swap_total_bytes),
            sample.memory.swap_used_pct(),
        ));
    }

    out.push('\n');
    for (row, fs) in sample.filesystems.iter().enumerate() {
        out.push_str(&format!(
            "{:<10}{:<24}{:>10} of {:>10}  {:>5.1}%   inodes {:.0}%\n",
            if row == 0 { "disk" } else { "" },
            truncate(&fs.mount_point, 23),
            bytes(fs.used_bytes),
            bytes(fs.total_bytes),
            fs.used_pct(),
            fs.inodes_used_pct(),
        ));
    }

    out.push('\n');
    for (row, disk) in sample.disks.iter().enumerate() {
        out.push_str(&format!(
            "{:<10}{:<10}read {:>11}  write {:>11}  busy {:>5.1}%  queued {}\n",
            if row == 0 { "disk i/o" } else { "" },
            truncate(&disk.device, 9),
            rate(disk.read_bytes_per_sec),
            rate(disk.write_bytes_per_sec),
            disk.utilization_pct,
            disk.in_flight,
        ));
    }

    out.push('\n');
    for (row, interface) in sample.networks.iter().enumerate() {
        out.push_str(&format!(
            "{:<10}{:<10}in {:>11}  out {:>11}  {} in/{} out pkts/s  {} dropped\n",
            if row == 0 { "network" } else { "" },
            truncate(&interface.name, 9),
            rate(interface.rx_bytes_per_sec),
            rate(interface.tx_bytes_per_sec),
            count(interface.rx_packets_per_sec),
            count(interface.tx_packets_per_sec),
            interface.rx_dropped + interface.tx_dropped,
        ));
    }

    let processes = &sample.processes;
    out.push_str(&format!(
        "\nprocesses {} total, {} threads   {} running, {} sleeping, {} blocked, {} zombie\n",
        processes.total,
        processes.threads,
        processes.running,
        processes.sleeping,
        processes.blocked,
        processes.zombie,
    ));

    if !processes.top.is_empty() {
        out.push_str(&format!(
            "\n{:>8} {:<12} {:>6} {:>6} {:>10} {:>11} {:>11}  {}\n",
            "PID", "USER", "CPU%", "MEM%", "RSS", "READ/s", "WRITE/s", "COMMAND",
        ));
        // Worst CPU first, then by memory. The sample carries the union of
        // both rankings, so a process that uses no CPU but a lot of memory
        // still appears — just below the busy ones, which is where someone
        // scanning for a runaway process expects it.
        let mut top: Vec<&Process> = processes.top.iter().collect();
        top.sort_unstable_by(|a, b| {
            b.cpu_pct
                .total_cmp(&a.cpu_pct)
                .then(b.rss_bytes.cmp(&a.rss_bytes))
        });
        for process in top {
            out.push_str(&process_row(process));
        }
    }

    out
}

fn process_row(process: &Process) -> String {
    let (read, write) = match process.io {
        Some(io) => (rate(io.read_bytes_per_sec), rate(io.write_bytes_per_sec)),
        // Not measured, as opposed to measured at zero — worth distinguishing,
        // because the usual cause is the agent lacking the privileges to look.
        None => ("-".to_string(), "-".to_string()),
    };

    format!(
        "{:>8} {:<12} {:>6.1} {:>6.1} {:>10} {:>11} {:>11}  {}\n",
        process.pid,
        truncate(process.user.as_deref().unwrap_or("?"), 12),
        process.cpu_pct,
        process.mem_pct,
        bytes(process.rss_bytes),
        read,
        write,
        truncate(&process.command, COMMAND_WIDTH),
    )
}

/// The most loaded CPU and its index, if per-CPU figures were collected.
fn busiest_cpu(sample: &Sample) -> Option<(usize, f64)> {
    sample
        .per_cpu
        .iter()
        .enumerate()
        .max_by(|(_, a), (_, b)| a.busy_pct.total_cmp(&b.busy_pct))
        .map(|(index, usage)| (index, usage.busy_pct))
}

/// Format a byte count with binary units.
fn bytes(value: u64) -> String {
    scale(value as f64, &["B", "KiB", "MiB", "GiB", "TiB", "PiB"])
}

/// Format a byte rate.
fn rate(value: f64) -> String {
    format!("{}/s", scale(value, &["B", "KiB", "MiB", "GiB", "TiB", "PiB"]))
}

/// Format a plain count, abbreviated once it stops being readable.
fn count(value: f64) -> String {
    scale(value, &["", "k", "M", "G"]).trim().to_string()
}

/// Reduce a value into the largest unit that leaves it above 1.
///
/// Byte units step by 1024 and plain counts by 1000, which would normally
/// mean two functions; here the only caller of the decimal version is the
/// packet/context-switch count, where the difference between "4.1k" and
/// "4.0k" is noise. One function, stepping by 1024, keeps it simple at the
/// cost of up to 2.4% on an abbreviated count.
fn scale(mut value: f64, units: &[&str]) -> String {
    let mut unit = 0;
    while value >= 1024.0 && unit + 1 < units.len() {
        value /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{value:.0} {}", units[unit])
    } else {
        format!("{value:.1} {}", units[unit])
    }
}

/// Format a duration in seconds as the two largest useful units.
fn duration(seconds: f64) -> String {
    let seconds = seconds.max(0.0) as u64;
    let (days, hours, minutes) = (seconds / 86_400, (seconds % 86_400) / 3_600, (seconds % 3_600) / 60);
    match (days, hours) {
        (0, 0) => format!("{minutes}m"),
        (0, _) => format!("{hours}h {minutes}m"),
        _ => format!("{days}d {hours}h"),
    }
}

/// Cut a string to `max` characters, marking it when something was lost.
fn truncate(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_string();
    }
    // Leave room for the marker so the result still fits the column.
    let mut out: String = text.chars().take(max.saturating_sub(1)).collect();
    out.push('…');
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::metrics::{cpu, disk, host, memory, net, process};

    #[test]
    fn bytes_uses_binary_units() {
        assert_eq!(bytes(0), "0 B");
        assert_eq!(bytes(512), "512 B");
        assert_eq!(bytes(1024), "1.0 KiB");
        assert_eq!(bytes(1536), "1.5 KiB");
        assert_eq!(bytes(5 * 1024 * 1024), "5.0 MiB");
        assert_eq!(bytes(16 * 1024 * 1024 * 1024), "16.0 GiB");
    }

    #[test]
    fn rates_carry_a_per_second_suffix() {
        assert_eq!(rate(0.0), "0 B/s");
        assert_eq!(rate(2048.0), "2.0 KiB/s");
    }

    #[test]
    fn counts_abbreviate_without_a_unit() {
        assert_eq!(count(7.0), "7");
        assert_eq!(count(4096.0), "4.0 k");
    }

    #[test]
    fn durations_show_the_two_largest_units() {
        assert_eq!(duration(0.0), "0m");
        assert_eq!(duration(90.0), "1m");
        assert_eq!(duration(3_600.0), "1h 0m");
        assert_eq!(duration(3_900.0), "1h 5m");
        assert_eq!(duration(90_000.0), "1d 1h");
        assert_eq!(duration(-5.0), "0m", "a clock that went backwards");
    }

    #[test]
    fn truncate_marks_what_it_cut() {
        assert_eq!(truncate("short", 10), "short");
        assert_eq!(truncate("exactlyten", 10), "exactlyten");
        assert_eq!(truncate("far too long to fit", 10), "far too l…");
        assert_eq!(truncate("far too long to fit", 10).chars().count(), 10);
    }

    fn fixture() -> (Identity, Sample) {
        let identity = Identity {
            hostname: "web-01".into(),
            kernel: "6.18.53".into(),
            cpus: 2,
        };
        let sample = Sample {
            elapsed_secs: 5.0,
            uptime_secs: 90_061.0,
            load: host::Load { one: 0.5, five: 1.0, fifteen: 0.75 },
            cpu: cpu::Usage {
                busy_pct: 12.5,
                user_pct: 8.0,
                system_pct: 4.5,
                iowait_pct: 0.5,
                steal_pct: 1.5,
                idle_pct: 85.5,
            },
            per_cpu: vec![
                cpu::Usage { busy_pct: 2.0, ..Default::default() },
                cpu::Usage { busy_pct: 98.0, ..Default::default() },
            ],
            context_switches_per_sec: 4096.0,
            procs_running: 3,
            procs_blocked: 1,
            memory: memory::Memory {
                total_bytes: 16 * 1024 * 1024 * 1024,
                used_bytes: 4 * 1024 * 1024 * 1024,
                available_bytes: 12 * 1024 * 1024 * 1024,
                free_bytes: 1024 * 1024 * 1024,
                buffers_bytes: 0,
                cached_bytes: 8 * 1024 * 1024 * 1024,
                swap_total_bytes: 2 * 1024 * 1024 * 1024,
                swap_used_bytes: 512 * 1024 * 1024,
            },
            filesystems: vec![disk::Filesystem {
                device: "/dev/vda1".into(),
                mount_point: "/".into(),
                fs_type: "ext4".into(),
                total_bytes: 40 * 1024 * 1024 * 1024,
                used_bytes: 12 * 1024 * 1024 * 1024,
                available_bytes: 28 * 1024 * 1024 * 1024,
                inodes_total: 1000,
                inodes_used: 40,
            }],
            disks: vec![disk::IoRates {
                device: "vda".into(),
                read_bytes_per_sec: 1024.0 * 1024.0,
                write_bytes_per_sec: 2048.0,
                read_ops_per_sec: 10.0,
                write_ops_per_sec: 4.0,
                utilization_pct: 12.0,
                in_flight: 1,
            }],
            networks: vec![net::Rates {
                name: "eth0".into(),
                rx_bytes_per_sec: 4096.0,
                tx_bytes_per_sec: 1024.0,
                rx_packets_per_sec: 20.0,
                tx_packets_per_sec: 10.0,
                rx_errors: 0,
                tx_errors: 0,
                rx_dropped: 2,
                tx_dropped: 0,
            }],
            processes: process::Summary {
                total: 284,
                threads: 1104,
                running: 3,
                sleeping: 280,
                blocked: 1,
                zombie: 0,
                stopped: 0,
                top: vec![
                    process::Process {
                        pid: 1234,
                        ppid: 1,
                        name: "nginx".into(),
                        command: "nginx: worker process".into(),
                        state: 'S',
                        uid: 33,
                        user: Some("www-data".into()),
                        threads: 1,
                        cpu_pct: 42.5,
                        rss_bytes: 520 * 1024 * 1024,
                        virtual_bytes: 1024 * 1024 * 1024,
                        mem_pct: 3.2,
                        io: Some(process::Io {
                            read_bytes_per_sec: 1024.0 * 1024.0,
                            write_bytes_per_sec: 0.0,
                        }),
                    },
                    process::Process {
                        pid: 9,
                        ppid: 2,
                        name: "kworker/0:1".into(),
                        command: "[kworker/0:1]".into(),
                        state: 'I',
                        uid: 0,
                        user: Some("root".into()),
                        threads: 1,
                        cpu_pct: 0.0,
                        rss_bytes: 0,
                        virtual_bytes: 0,
                        mem_pct: 0.0,
                        io: None,
                    },
                ],
            },
        };
        (identity, sample)
    }

    #[test]
    fn renders_every_section() {
        let (identity, snapshot) = fixture();
        let text = sample(&identity, &snapshot);

        assert!(text.contains("web-01 · linux 6.18.53 · 2 CPUs · up 1d 1h"));
        assert!(text.contains("12.5% busy"), "{text}");
        assert!(text.contains("steal 1.5%"));
        assert!(text.contains("load 0.50 / 1.00 / 0.75 (0.25 per CPU)"));
        assert!(text.contains("4.0 GiB of 16.0 GiB used (25.0%)"));
        assert!(text.contains("swap"));
        assert!(text.contains("/dev/vda1") || text.contains("inodes 4%"));
        assert!(text.contains("vda"));
        assert!(text.contains("eth0"));
        assert!(text.contains("284 total, 1104 threads"));
        assert!(text.contains("nginx: worker process"));
    }

    #[test]
    fn highlights_the_busiest_cpu_rather_than_listing_all_of_them() {
        let (identity, snapshot) = fixture();
        let text = sample(&identity, &snapshot);
        assert!(text.contains("busiest cpu1 at 98.0%"), "{text}");
    }

    #[test]
    fn unmeasured_process_io_is_not_shown_as_zero() {
        let (_, snapshot) = fixture();
        let kworker = snapshot.processes.top.iter().find(|p| p.pid == 9).unwrap();
        let row = process_row(kworker);
        assert!(row.contains('-'), "{row}");
        assert!(!row.contains("0 B/s"), "{row}");
    }

    #[test]
    fn swap_line_is_omitted_when_there_is_no_swap() {
        let (identity, mut snapshot) = fixture();
        snapshot.memory.swap_total_bytes = 0;
        snapshot.memory.swap_used_bytes = 0;
        let text = sample(&identity, &snapshot);
        assert!(!text.contains("\nswap"), "{text}");
    }

    #[test]
    fn empty_sections_do_not_panic() {
        let (identity, mut snapshot) = fixture();
        snapshot.filesystems.clear();
        snapshot.disks.clear();
        snapshot.networks.clear();
        snapshot.per_cpu.clear();
        snapshot.processes.top.clear();
        let text = sample(&identity, &snapshot);
        assert!(text.contains("web-01"));
        assert!(!text.contains("PID"), "no table without rows");
    }
}
