//! Per-process accounting — the task-manager view, from `/proc/<pid>/*`.
//!
//! This is the piece that answers "fine, but *what* is eating the box", which
//! is the question the whole-machine numbers always provoke and never answer.
//!
//! Two things shape the implementation.
//!
//! **CPU per process needs two samples**, same as the host-wide figures: a
//! process's `utime`/`stime` are tick counters since it started. So a
//! [`Tracker`] holds the previous reading and each sample differences against
//! it. Pid reuse is guarded by also remembering the process start time: the
//! kernel recycles pids, and a new process inheriting an old one's tick count
//! would briefly report an absurd percentage.
//!
//! **Sampling is two passes, to keep the cost down.** Every interval we read
//! one small file per process (a few hundred reads — cheap, and the only way
//! to rank anything). Only the processes that then make the top of a ranking
//! get their command line, owner and I/O read, which is three more files
//! each. Doing that for every process would roughly quadruple the agent's own
//! syscall load for rows nobody will ever look at.

use std::collections::HashMap;
use std::fs;
use std::io;
use std::os::unix::fs::MetadataExt;

use crate::metrics::per_sec;
use crate::procfs;

/// Command lines can be enormous (java, anything run by a build system).
/// Truncate before they dominate the report.
const MAX_COMMAND_CHARS: usize = 256;

/// One reported process.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Process {
    pub pid: u32,
    pub ppid: u32,
    /// The kernel's short name for the process (`comm`), max 15 characters.
    pub name: String,
    /// The full command line, truncated; `[name]` for kernel threads, which
    /// have no command line at all.
    pub command: String,
    /// `R`unning, `S`leeping, `D` uninterruptible, `Z`ombie, s`T`opped, `I`dle.
    pub state: char,
    pub uid: u32,
    /// Owner's login name, when it could be resolved.
    pub user: Option<String>,
    pub threads: u64,
    /// CPU as a share of *one* CPU, so a busy multi-threaded process can
    /// legitimately report more than 100 — same convention as `top`. Divide
    /// by the host's CPU count to compare against the whole-machine figure.
    pub cpu_pct: f64,
    /// Resident set: physical memory the process is currently using.
    ///
    /// Shared pages (libc, and everything a forking server shares with its
    /// children) are counted in full against every process mapping them, so
    /// these do not sum to the host's used memory and will usually overshoot
    /// it. It's still the right number for ranking, and the honest total is
    /// the one from `/proc/meminfo`.
    pub rss_bytes: u64,
    /// Address space size. Routinely enormous and mostly unbacked — kept
    /// because a runaway allocation shows here first, not because it's "memory
    /// used".
    pub virtual_bytes: u64,
    /// `rss_bytes` as a share of the host's total memory.
    pub mem_pct: f64,
    /// Disk I/O, when collection is enabled and permitted. `None` means "not
    /// measured", never "zero".
    pub io: Option<Io>,
}

/// Disk I/O attributable to one process.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Io {
    /// Bytes actually fetched from a block device — page-cache hits excluded,
    /// which is what makes this the figure that explains disk pressure rather
    /// than how much the process asked for.
    pub read_bytes_per_sec: f64,
    pub write_bytes_per_sec: f64,
}

/// Process-level view of the host for one sample.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Summary {
    pub total: u64,
    pub threads: u64,
    pub running: u64,
    pub sleeping: u64,
    /// Stuck in uninterruptible sleep — almost always waiting on I/O. A
    /// standing count here is how a storage problem looks from this angle.
    pub blocked: u64,
    pub zombie: u64,
    pub stopped: u64,
    /// The union of the top processes by CPU and by memory, worst CPU first.
    ///
    /// A union rather than two lists because the overlap is usually large and
    /// duplicating those entries would inflate every report. Sorting the
    /// union by memory still yields exactly the top-by-memory ranking, so
    /// nothing is lost.
    pub top: Vec<Process>,
}

/// The raw fields of `/proc/<pid>/stat` that we use.
#[derive(Clone, Debug, PartialEq)]
struct Raw {
    pid: u32,
    ppid: u32,
    comm: String,
    state: char,
    threads: u64,
    /// Ticks since boot at which the process started. Identifies a process
    /// across samples in a way a pid alone can't.
    starttime: u64,
    /// `utime + stime`: ticks of CPU consumed since the process started.
    cpu_ticks: u64,
    rss_pages: u64,
    virtual_bytes: u64,
}

/// What we need to remember about a process between samples.
#[derive(Clone, Copy, Debug)]
struct Previous {
    starttime: u64,
    cpu_ticks: u64,
    /// Cumulative `read_bytes`/`write_bytes`, kept only for processes we
    /// actually read I/O for.
    io: Option<(u64, u64)>,
}

/// Samples processes, remembering enough between calls to report rates.
pub struct Tracker {
    top_n: usize,
    with_io: bool,
    previous: HashMap<u32, Previous>,
    users: HashMap<u32, String>,
    /// Buffers reused across every process in a pass, so walking a few
    /// hundred `/proc` entries doesn't mean a few hundred allocations.
    path: String,
    contents: String,
}

impl Tracker {
    pub fn new(top_n: usize, with_io: bool) -> Self {
        Self {
            top_n,
            with_io,
            previous: HashMap::new(),
            users: read_passwd(),
            path: String::with_capacity(64),
            contents: String::with_capacity(4096),
        }
    }

    /// Take a first reading without reporting anything.
    ///
    /// Called once at startup so that the first real sample already has a
    /// baseline to difference against, instead of reporting every process at
    /// 0% CPU.
    pub fn prime(&mut self) -> io::Result<()> {
        let processes = self.scan()?;
        self.remember(&processes);

        if self.with_io {
            // Record an I/O baseline for every process, not just the ones
            // that would rank. Steady-state sampling only reads I/O for the
            // processes it reports, but at startup there is nothing to rank
            // by yet, and without a baseline here the first sample has
            // nothing to difference against and reports no I/O at all — most
            // visibly for `--once`, which only ever takes one sample. This
            // costs one extra read per process, once.
            for raw in &processes {
                if let Some(totals) = self.read_io(raw.pid) {
                    if let Some(previous) = self.previous.get_mut(&raw.pid) {
                        previous.io = Some(totals);
                    }
                }
            }
        }

        Ok(())
    }

    /// Sample every process, and detail the ones at the top.
    ///
    /// `elapsed_secs` must be the wall-clock time since the previous call, and
    /// `memory_total_bytes` the host's total RAM, for the percentages.
    pub fn sample(&mut self, elapsed_secs: f64, memory_total_bytes: u64) -> io::Result<Summary> {
        let processes = self.scan()?;

        let mut summary = Summary {
            total: processes.len() as u64,
            ..Summary::default()
        };
        for raw in &processes {
            summary.threads += raw.threads;
            match raw.state {
                'R' => summary.running += 1,
                // 'I' is an idle kernel thread: asleep, and the overwhelming
                // majority of the kernel threads on an idle box.
                'S' | 'I' => summary.sleeping += 1,
                'D' => summary.blocked += 1,
                'Z' => summary.zombie += 1,
                'T' | 't' => summary.stopped += 1,
                _ => {}
            }
        }

        let page_size = procfs::page_size();
        let cpu_pcts: Vec<f64> = processes
            .iter()
            .map(|raw| self.cpu_pct(raw, elapsed_secs))
            .collect();
        let rss: Vec<u64> = processes.iter().map(|raw| raw.rss_pages * page_size).collect();

        let chosen = rank(&cpu_pcts, &rss, self.top_n);

        // Record the baseline for the next sample before enriching, so that a
        // process vanishing mid-enrichment can't cost us its tick count.
        self.remember(&processes);

        let mut top = Vec::with_capacity(chosen.len());
        for index in chosen {
            let raw = &processes[index];
            let io = self.io_rates(raw.pid, elapsed_secs);
            let command = self.command(raw);
            let uid = self.uid(raw.pid);
            let user = self.users.get(&uid).cloned();
            top.push(Process {
                pid: raw.pid,
                ppid: raw.ppid,
                name: raw.comm.clone(),
                command,
                state: raw.state,
                uid,
                user,
                threads: raw.threads,
                cpu_pct: cpu_pcts[index],
                rss_bytes: rss[index],
                virtual_bytes: raw.virtual_bytes,
                mem_pct: if memory_total_bytes == 0 {
                    0.0
                } else {
                    100.0 * rss[index] as f64 / memory_total_bytes as f64
                },
                io,
            });
        }

        summary.top = top;
        Ok(summary)
    }

    /// Read `/proc/<pid>/stat` for every process on the host.
    fn scan(&mut self) -> io::Result<Vec<Raw>> {
        let mut processes = Vec::with_capacity(self.previous.len().max(64));

        for entry in fs::read_dir("/proc")? {
            let Ok(entry) = entry else { continue };
            // /proc holds plenty of non-pid entries; the numeric ones are the
            // processes, which makes "does the name parse as a number" the
            // whole filter.
            let Some(pid) = entry.file_name().to_str().and_then(|n| n.parse::<u32>().ok()) else {
                continue;
            };

            self.path.clear();
            self.path.push_str("/proc/");
            push_u32(&mut self.path, pid);
            self.path.push_str("/stat");

            // Processes exit constantly, and one that exits between the
            // read_dir and the open is the normal case, not an error worth
            // reporting.
            if procfs::read_into(&self.path, &mut self.contents).is_err() {
                continue;
            }
            if let Some(raw) = parse_stat(&self.contents) {
                processes.push(raw);
            }
        }

        Ok(processes)
    }

    fn cpu_pct(&self, raw: &Raw, elapsed_secs: f64) -> f64 {
        if elapsed_secs <= 0.0 {
            return 0.0;
        }
        let Some(previous) = self
            .previous
            .get(&raw.pid)
            // A different start time means the pid was recycled: this is a
            // new process wearing an old one's number, and its ticks are not
            // comparable to what we stored.
            .filter(|previous| previous.starttime == raw.starttime)
        else {
            // Brand new process. It has used CPU since it started, but we
            // don't know how much of our window it existed for, so attributing
            // all of it would overstate; it'll be accurate next sample.
            return 0.0;
        };

        let ticks = raw.cpu_ticks.saturating_sub(previous.cpu_ticks) as f64;
        100.0 * (ticks / procfs::clock_ticks() as f64) / elapsed_secs
    }

    /// Read a process's cumulative block-I/O totals.
    ///
    /// Reading another user's `/proc/<pid>/io` needs privileges the agent may
    /// not have been given. That's a deployment choice rather than a fault,
    /// so it degrades to "not measured" without complaint.
    fn read_io(&mut self, pid: u32) -> Option<(u64, u64)> {
        self.path.clear();
        self.path.push_str("/proc/");
        push_u32(&mut self.path, pid);
        self.path.push_str("/io");
        procfs::read_into(&self.path, &mut self.contents).ok()?;
        parse_io(&self.contents)
    }

    /// Per-process disk I/O, if enabled, permitted, and we have a baseline.
    fn io_rates(&mut self, pid: u32, elapsed_secs: f64) -> Option<Io> {
        if !self.with_io {
            return None;
        }

        let (read_bytes, write_bytes) = self.read_io(pid)?;
        let previous = self.previous.get_mut(&pid)?;
        let baseline = previous.io.replace((read_bytes, write_bytes));

        // First time we've looked at this process: record the totals, report
        // nothing. A rate needs two readings and inventing one from a counter
        // since process start would read as a huge spike.
        let (previous_read, previous_write) = baseline?;
        Some(Io {
            read_bytes_per_sec: per_sec(read_bytes, previous_read, elapsed_secs),
            write_bytes_per_sec: per_sec(write_bytes, previous_write, elapsed_secs),
        })
    }

    /// The process's full command line, or a kernel thread's bracketed name.
    fn command(&mut self, raw: &Raw) -> String {
        self.path.clear();
        self.path.push_str("/proc/");
        push_u32(&mut self.path, raw.pid);
        self.path.push_str("/cmdline");

        // Read as bytes: a command line is whatever was exec'd and need not
        // be valid UTF-8.
        let raw_cmdline = fs::read(&self.path).unwrap_or_default();
        let command = format_cmdline(&raw_cmdline);
        if command.is_empty() {
            // Kernel threads have an empty command line. Brackets are how ps
            // and top mark them, and the distinction matters: a kernel thread
            // is not something an operator can go and restart.
            return format!("[{}]", raw.comm);
        }
        command
    }

    fn uid(&mut self, pid: u32) -> u32 {
        self.path.clear();
        self.path.push_str("/proc/");
        push_u32(&mut self.path, pid);
        // The owner of /proc/<pid> is the owner of the process, so this comes
        // from the stat we'd be doing anyway rather than from parsing
        // /proc/<pid>/status.
        fs::metadata(&self.path).map(|meta| meta.uid()).unwrap_or(0)
    }

    /// Replace the baseline with this pass's readings.
    ///
    /// Processes that have exited drop out, which is also what keeps the map
    /// from growing without bound on a host that churns through short-lived
    /// processes.
    fn remember(&mut self, processes: &[Raw]) {
        let mut next = HashMap::with_capacity(processes.len());
        for raw in processes {
            // Carry forward the I/O totals so a process that stays in the top
            // keeps a usable baseline across samples.
            let io = self
                .previous
                .get(&raw.pid)
                .filter(|previous| previous.starttime == raw.starttime)
                .and_then(|previous| previous.io);
            next.insert(
                raw.pid,
                Previous {
                    starttime: raw.starttime,
                    cpu_ticks: raw.cpu_ticks,
                    io,
                },
            );
        }
        self.previous = next;
    }
}

/// What this agent itself is costing the box, for the self-measurement mode.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct OwnUsage {
    /// CPU seconds consumed since the agent started.
    pub cpu_secs: f64,
    pub rss_bytes: u64,
}

/// Read this process own CPU and memory use.
///
/// Deliberately goes through the same `parse_stat` as every other process,
/// pointed at `/proc/self`: the agent measures itself with the code it uses
/// to measure everything else, so a bug in that parsing cannot make the
/// agent look cheaper than it is.
pub fn own_usage() -> io::Result<OwnUsage> {
    let text = procfs::read("/proc/self/stat")?;
    let raw = parse_stat(&text).ok_or_else(|| {
        io::Error::new(io::ErrorKind::InvalidData, "could not parse /proc/self/stat")
    })?;
    Ok(OwnUsage {
        cpu_secs: raw.cpu_ticks as f64 / procfs::clock_ticks() as f64,
        rss_bytes: raw.rss_pages * procfs::page_size(),
    })
}

/// Pick the indices of the top `n` by CPU, unioned with the top `n` by
/// memory, ordered worst CPU first.
fn rank(cpu_pcts: &[f64], rss: &[u64], n: usize) -> Vec<usize> {
    let n = n.min(cpu_pcts.len());
    if n == 0 {
        return Vec::new();
    }

    let mut by_cpu: Vec<usize> = (0..cpu_pcts.len()).collect();
    // total_cmp rather than partial_cmp: a NaN percentage shouldn't be able to
    // make the comparator inconsistent and scramble the ranking.
    by_cpu.sort_unstable_by(|a, b| cpu_pcts[*b].total_cmp(&cpu_pcts[*a]));

    let mut by_memory: Vec<usize> = (0..rss.len()).collect();
    by_memory.sort_unstable_by(|a, b| rss[*b].cmp(&rss[*a]));

    let mut chosen: Vec<usize> = by_cpu[..n].to_vec();
    for index in &by_memory[..n] {
        // Linear scan over at most a couple of dozen entries, which beats
        // allocating a set to find out the same thing.
        if !chosen.contains(index) {
            chosen.push(*index);
        }
    }
    chosen
}

/// Parse `/proc/<pid>/stat`.
///
/// The second field is the executable name in parentheses, and it can contain
/// both spaces and parentheses of its own (`(Web Content)`, `(foo) bar`), so
/// splitting the line on whitespace mis-aligns every field after it. The fix
/// the kernel's own consumers use: cut at the *last* `)`, and count fields
/// from there.
fn parse_stat(text: &str) -> Option<Raw> {
    let open = text.find('(')?;
    let close = text.rfind(')')?;
    let pid = text.get(..open)?.trim().parse().ok()?;
    let comm = text.get(open + 1..close)?.to_string();

    // Fields after the name start at field 3 (state), so proc(5)'s field N
    // lives at index N - 3.
    let fields: Vec<&str> = text.get(close + 1..)?.split_ascii_whitespace().collect();
    let field = |n: usize| -> u64 { fields.get(n - 3).and_then(|v| v.parse().ok()).unwrap_or(0) };
    // rss is the last field we read, at index 21.
    if fields.len() < 22 {
        return None;
    }

    Some(Raw {
        pid,
        comm,
        state: fields[0].chars().next().unwrap_or('?'),
        ppid: field(4) as u32,
        cpu_ticks: field(14) + field(15),
        threads: field(20),
        starttime: field(22),
        virtual_bytes: field(23),
        rss_pages: field(24),
    })
}

/// Pull `read_bytes` and `write_bytes` out of `/proc/<pid>/io`.
///
/// Not `rchar`/`wchar`, which count bytes the process asked for including
/// everything served from page cache. These two count what reached a block
/// device, which is the part that competes for the disk.
fn parse_io(text: &str) -> Option<(u64, u64)> {
    let mut read_bytes = None;
    let mut write_bytes = None;
    for line in text.lines() {
        let Some((key, value)) = line.split_once(':') else {
            continue;
        };
        let parsed = value.trim().parse().ok();
        match key {
            "read_bytes" => read_bytes = parsed,
            "write_bytes" => write_bytes = parsed,
            _ => {}
        }
    }
    Some((read_bytes?, write_bytes?))
}

/// Turn a raw `/proc/<pid>/cmdline` into something printable.
///
/// The file is NUL-separated and NUL-terminated, which displays as one run-on
/// word; arguments are joined with spaces instead.
fn format_cmdline(bytes: &[u8]) -> String {
    let text: String = String::from_utf8_lossy(bytes)
        .split('\0')
        .filter(|arg| !arg.is_empty())
        .collect::<Vec<_>>()
        .join(" ");

    let mut truncated: String = text.chars().take(MAX_COMMAND_CHARS).collect();
    if truncated.len() < text.len() {
        truncated.push('…');
    }
    truncated
}

/// Map uids to login names by reading `/etc/passwd`.
///
/// Read once, at startup. The agent reports a uid regardless, so a user
/// created after the agent started shows as a bare number until it restarts —
/// which is a fair trade for not re-reading the file every few seconds.
/// Directory-backed users (LDAP, SSSD) won't resolve at all; looking those up
/// means NSS, which can block on the network, and this loop can't afford to.
fn read_passwd() -> HashMap<u32, String> {
    let Ok(text) = fs::read_to_string("/etc/passwd") else {
        return HashMap::new();
    };
    text.lines()
        .filter_map(|line| {
            // name:password:uid:gid:comment:home:shell
            let mut fields = line.split(':');
            let name = fields.next()?;
            let uid = fields.nth(1)?.parse().ok()?;
            Some((uid, name.to_string()))
        })
        .collect()
}

/// Append a pid to a path buffer without going through a formatter.
fn push_u32(buf: &mut String, mut value: u32) {
    if value == 0 {
        buf.push('0');
        return;
    }
    let mut digits = [0u8; 10];
    let mut len = 0;
    while value > 0 {
        digits[len] = b'0' + (value % 10) as u8;
        value /= 10;
        len += 1;
    }
    for digit in digits[..len].iter().rev() {
        buf.push(*digit as char);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // A real /proc/<pid>/stat line, shortened past the fields we read.
    const STAT: &str = "1234 (nginx) S 1 1234 1234 0 -1 4194624 500 0 0 0 150 50 0 0 20 0 4 0 987654 12582912 2048 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0\n";

    #[test]
    fn parses_the_fields_we_report() {
        let raw = parse_stat(STAT).expect("parses");
        assert_eq!(raw.pid, 1234);
        assert_eq!(raw.comm, "nginx");
        assert_eq!(raw.state, 'S');
        assert_eq!(raw.ppid, 1);
        // utime 150 + stime 50
        assert_eq!(raw.cpu_ticks, 200);
        assert_eq!(raw.threads, 4);
        assert_eq!(raw.starttime, 987_654);
        assert_eq!(raw.virtual_bytes, 12_582_912);
        assert_eq!(raw.rss_pages, 2048);
    }

    #[test]
    fn process_names_containing_spaces_do_not_shift_the_fields() {
        let line = STAT.replace("(nginx)", "(Web Content)");
        let raw = parse_stat(&line).expect("parses");
        assert_eq!(raw.comm, "Web Content");
        assert_eq!(raw.cpu_ticks, 200, "fields stay aligned");
        assert_eq!(raw.rss_pages, 2048);
    }

    #[test]
    fn process_names_containing_parens_do_not_shift_the_fields() {
        let line = STAT.replace("(nginx)", "((sd-pam))");
        let raw = parse_stat(&line).expect("parses");
        assert_eq!(raw.comm, "(sd-pam)");
        assert_eq!(raw.cpu_ticks, 200, "cut at the last paren, not the first");
        assert_eq!(raw.rss_pages, 2048);
    }

    #[test]
    fn truncated_stat_is_rejected_rather_than_reported_as_zeroes() {
        assert_eq!(parse_stat("1 (init) S 1 1 1\n"), None);
        assert_eq!(parse_stat("nonsense"), None);
        assert_eq!(parse_stat(""), None);
    }

    #[test]
    fn parses_block_io_not_syscall_io() {
        let text = "\
rchar: 99999
wchar: 88888
syscr: 100
syscw: 200
read_bytes: 4096
write_bytes: 8192
cancelled_write_bytes: 0
";
        assert_eq!(parse_io(text), Some((4096, 8192)));
    }

    #[test]
    fn io_without_the_byte_counters_is_not_measured() {
        assert_eq!(parse_io("rchar: 1\nwchar: 2\n"), None);
    }

    #[test]
    fn cmdline_arguments_are_joined_with_spaces() {
        assert_eq!(format_cmdline(b"nginx\0-g\0daemon off;\0"), "nginx -g daemon off;");
    }

    #[test]
    fn empty_cmdline_stays_empty_so_kernel_threads_can_be_detected() {
        assert_eq!(format_cmdline(b""), "");
        assert_eq!(format_cmdline(b"\0"), "");
    }

    #[test]
    fn long_cmdlines_are_truncated_with_a_marker() {
        let long = vec![b'x'; MAX_COMMAND_CHARS * 2];
        let formatted = format_cmdline(&long);
        assert_eq!(formatted.chars().count(), MAX_COMMAND_CHARS + 1);
        assert!(formatted.ends_with('…'));
    }

    #[test]
    fn invalid_utf8_in_cmdline_does_not_lose_the_process() {
        let formatted = format_cmdline(b"we\xffird\0arg");
        assert!(formatted.contains("arg"), "got {formatted:?}");
    }

    #[test]
    fn ranking_unions_the_two_orderings() {
        //                  idx:  0    1    2    3
        let cpu_pcts: Vec<f64> = vec![0.0, 90.0, 5.0, 0.0];
        let rss: Vec<u64> = vec![900, 1, 2, 800];
        let chosen = rank(&cpu_pcts, &rss, 2);
        // Top 2 by CPU: 1, 2. Top 2 by memory: 0, 3.
        assert_eq!(chosen, vec![1, 2, 0, 3]);
    }

    #[test]
    fn ranking_does_not_repeat_a_process_high_in_both() {
        let cpu_pcts = vec![90.0, 1.0, 0.0];
        let rss = vec![900, 1, 0];
        // Index 0 leads both rankings and must appear once.
        assert_eq!(rank(&cpu_pcts, &rss, 1), vec![0]);
    }

    #[test]
    fn ranking_handles_fewer_processes_than_requested() {
        assert_eq!(rank(&[1.0], &[1], 10), vec![0]);
        assert_eq!(rank(&[], &[], 10), Vec::<usize>::new());
        assert_eq!(rank(&[1.0], &[1], 0), Vec::<usize>::new());
    }

    #[test]
    fn parses_passwd_uids() {
        let text = "root:x:0:0:root:/root:/bin/bash\ndeploy:x:1001:1001::/home/deploy:/bin/sh\n";
        // Exercised through the same field positions read_passwd uses.
        let users: HashMap<u32, String> = text
            .lines()
            .filter_map(|line| {
                let mut fields = line.split(':');
                let name = fields.next()?;
                let uid = fields.nth(1)?.parse().ok()?;
                Some((uid, name.to_string()))
            })
            .collect();
        assert_eq!(users.get(&0).map(String::as_str), Some("root"));
        assert_eq!(users.get(&1001).map(String::as_str), Some("deploy"));
    }

    #[test]
    fn own_usage_reads_this_process() {
        let usage = own_usage().expect("a process can read its own stat");
        assert!(usage.rss_bytes > 0, "we are resident");
        assert!(usage.cpu_secs >= 0.0);

        // Burn CPU and confirm the figure moves, so this cannot silently
        // report a constant and make the agent look free.
        let start = std::time::Instant::now();
        let mut spin = 0u64;
        while start.elapsed() < std::time::Duration::from_millis(60) {
            spin = spin.wrapping_add(1);
        }
        assert_ne!(spin, 0);

        let after = own_usage().expect("reads");
        assert!(
            after.cpu_secs > usage.cpu_secs,
            "{} should exceed {}",
            after.cpu_secs,
            usage.cpu_secs
        );
    }

    #[test]
    fn push_u32_matches_formatting() {
        for value in [0u32, 1, 7, 42, 1234, 4_294_967_295] {
            let mut buf = String::new();
            push_u32(&mut buf, value);
            assert_eq!(buf, value.to_string());
        }
    }

    #[test]
    fn tracker_reports_cpu_only_once_it_has_a_baseline() {
        // A high cap, so these assertions are about the CPU arithmetic rather
        // than about whether this process out-ranks whatever else the machine
        // running the tests happens to be busy with.
        let mut tracker = Tracker::new(1000, false);

        let first = tracker.sample(1.0, 1 << 30).expect("reads /proc");
        assert!(first.total > 0, "the test process itself is running");
        assert!(
            first.top.iter().all(|p| p.cpu_pct == 0.0),
            "no baseline yet, so nothing can be attributed"
        );

        // Spin for a known wall-clock window, so this process is expected to
        // have used almost all of one CPU across it.
        let start = std::time::Instant::now();
        let mut spin = 0u64;
        while start.elapsed() < std::time::Duration::from_millis(100) {
            spin = spin.wrapping_add(1);
        }
        assert_ne!(spin, 0, "the loop must not be optimised away");

        let second = tracker
            .sample(start.elapsed().as_secs_f64(), 1 << 30)
            .expect("reads /proc");
        let mine = second
            .top
            .iter()
            .find(|p| p.pid == std::process::id())
            .expect("this process is in its own process list");
        assert!(mine.cpu_pct > 0.0, "spinning should show as CPU, got {}", mine.cpu_pct);
    }

    #[test]
    fn tracker_reads_live_process_details() {
        let mut tracker = Tracker::new(1000, true);
        tracker.prime().expect("reads /proc");
        let summary = tracker.sample(1.0, 1 << 30).expect("reads /proc");

        assert!(summary.total > 0);
        assert!(summary.threads >= summary.total, "every process has a thread");
        assert!(summary.sleeping + summary.running > 0);

        let me = summary
            .top
            .iter()
            .find(|p| p.pid == std::process::id())
            .expect("the test binary is in its own process list");
        assert!(!me.name.is_empty());
        assert!(!me.command.is_empty());
        assert!(me.rss_bytes > 0, "a running process has resident memory");
        assert!(me.threads >= 1);
        assert_eq!(me.uid, unsafe { libc::getuid() });
        // A process can always read its own I/O counters, and prime() took a
        // baseline, so this must be measured even if the rate is zero.
        assert!(me.io.is_some(), "own I/O should be readable after priming");
    }

    #[test]
    fn kernel_threads_are_bracketed() {
        let mut tracker = Tracker::new(1000, false);
        tracker.prime().expect("reads /proc");
        let summary = tracker.sample(1.0, 1 << 30).expect("reads /proc");
        // pid 2 is kthreadd on Linux; skip the assertion in containers where
        // it isn't visible rather than failing for an unrelated reason.
        if let Some(kthread) = summary.top.iter().find(|p| p.pid == 2) {
            assert!(
                kthread.command.starts_with('[') && kthread.command.ends_with(']'),
                "got {:?}",
                kthread.command
            );
        }
    }
}
