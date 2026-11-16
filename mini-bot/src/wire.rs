//! The wire format: a [`Sample`] as the collector's protocol JSON.
//!
//! The authoritative definition is
//! `collector/internal/protocol/protocol.go`; field names here follow it, not
//! this crate's internal naming (`filesystems`/`mount_point`/`fs_type`,
//! `cpu.ticks`). JSON is written by hand rather than through serde: the
//! shapes are small and fixed, and `libc` stays the only dependency.
//!
//! Two properties matter more than the formatting:
//!
//! - **Counters, not rates.** CPU ticks and interface counters travel raw so
//!   the collector can diff across a gap in delivery. Per-process figures
//!   are the exception and stay rates, since a PID is not a stable identity.
//! - **Nothing the collector would reject.** It validates every report, and
//!   in a pull a rejected sample is discarded rather than retried. So the
//!   values it refuses — control characters in a process name, an
//!   over-length command, more top processes than it caps at, non-finite
//!   floats — are made safe here rather than left to cost data there.

use std::fmt::Write;

use crate::metrics::Sample;
use crate::metrics::host::Identity;
use crate::metrics::process;

/// Matches `protocol.MaxTopProcesses`.
const MAX_TOP_PROCESSES: usize = 64;

/// Matches `protocol.MaxCommandLen`, which is in bytes. The agent truncates
/// commands to 256 *characters*, which multi-byte text can push past this.
const MAX_COMMAND_BYTES: usize = 512;

/// The host identity block: `protocol.Host`.
pub fn host(id: &str, identity: &Identity) -> String {
    let mut out = String::with_capacity(256);
    out.push('{');
    key(&mut out, "id");
    string(&mut out, id);
    out.push(',');
    key(&mut out, "hostname");
    string(&mut out, &identity.hostname);
    if !identity.os.is_empty() {
        out.push(',');
        key(&mut out, "os");
        string(&mut out, &identity.os);
    }
    out.push(',');
    key(&mut out, "kernel");
    string(&mut out, &identity.kernel);
    let _ = write!(out, ",\"cores\":{},", identity.cpus);
    key(&mut out, "agent_version");
    string(&mut out, env!("CARGO_PKG_VERSION"));
    out.push('}');
    out
}

/// One `protocol.Sample`. `with_processes` attaches the process snapshot;
/// the collector keeps only the newest one per host, so spooled samples go
/// without it and only the latest reading carries it.
pub fn sample(at_ms: u64, s: &Sample, with_processes: bool) -> String {
    let mut out = String::with_capacity(if with_processes { 8192 } else { 2048 });
    out.push('{');
    key(&mut out, "at");
    string(&mut out, &rfc3339_ms(at_ms));
    out.push(',');
    number(&mut out, "elapsed_secs", s.elapsed_secs);
    let _ = write!(
        out,
        ",\"uptime_secs\":{},\"procs_running\":{},\"procs_blocked\":{},",
        s.uptime_secs.max(0.0) as u64,
        s.procs_running,
        s.procs_blocked,
    );

    let t = s.cpu_ticks;
    let _ = write!(
        out,
        "\"cpu\":{{\"ticks\":{{\"user\":{},\"nice\":{},\"system\":{},\"idle\":{},\
         \"iowait\":{},\"irq\":{},\"softirq\":{},\"steal\":{}}},",
        t.user, t.nice, t.system, t.idle, t.iowait, t.irq, t.softirq, t.steal,
    );
    number(&mut out, "load1", s.load.one);
    out.push(',');
    number(&mut out, "load5", s.load.five);
    out.push(',');
    number(&mut out, "load15", s.load.fifteen);
    out.push_str("},");

    // used is total - MemAvailable, computed once in metrics::memory and
    // never re-derived here.
    let m = &s.memory;
    let _ = write!(
        out,
        "\"memory\":{{\"total_bytes\":{},\"used_bytes\":{},\"available_bytes\":{},\
         \"swap_total_bytes\":{},\"swap_used_bytes\":{}}}",
        m.total_bytes, m.used_bytes, m.available_bytes, m.swap_total_bytes, m.swap_used_bytes,
    );

    if !s.filesystems.is_empty() {
        out.push_str(",\"filesystems\":[");
        for (i, f) in s.filesystems.iter().enumerate() {
            if i > 0 {
                out.push(',');
            }
            out.push('{');
            key(&mut out, "device");
            string(&mut out, &f.device);
            out.push(',');
            key(&mut out, "mount_point");
            string(&mut out, &f.mount_point);
            out.push(',');
            key(&mut out, "fs_type");
            string(&mut out, &f.fs_type);
            let _ = write!(
                out,
                ",\"total_bytes\":{},\"used_bytes\":{},\"available_bytes\":{},\
                 \"inodes_total\":{},\"inodes_used\":{}}}",
                f.total_bytes, f.used_bytes, f.available_bytes, f.inodes_total, f.inodes_used,
            );
        }
        out.push(']');
    }

    if !s.net_counters.is_empty() {
        out.push_str(",\"net\":[");
        for (i, n) in s.net_counters.iter().enumerate() {
            if i > 0 {
                out.push(',');
            }
            out.push('{');
            key(&mut out, "iface");
            string(&mut out, &n.name);
            let _ = write!(
                out,
                ",\"rx_bytes\":{},\"tx_bytes\":{},\"rx_packets\":{},\"tx_packets\":{},\
                 \"rx_errors\":{},\"tx_errors\":{},\"rx_dropped\":{},\"tx_dropped\":{}}}",
                n.rx_bytes,
                n.tx_bytes,
                n.rx_packets,
                n.tx_packets,
                n.rx_errors,
                n.tx_errors,
                n.rx_dropped,
                n.tx_dropped,
            );
        }
        out.push(']');
    }

    if with_processes {
        out.push(',');
        processes(&mut out, &s.processes);
    }
    out.push('}');
    out
}

fn processes(out: &mut String, p: &process::Summary) {
    let _ = write!(
        out,
        "\"processes\":{{\"total\":{},\"threads\":{},\"running\":{},\"sleeping\":{},\
         \"blocked\":{},\"zombie\":{},\"stopped\":{},\"top\":[",
        p.total, p.threads, p.running, p.sleeping, p.blocked, p.zombie, p.stopped,
    );
    for (i, proc) in p.top.iter().take(MAX_TOP_PROCESSES).enumerate() {
        if i > 0 {
            out.push(',');
        }
        let _ = write!(out, "{{\"pid\":{},\"ppid\":{},", proc.pid, proc.ppid);
        key(out, "name");
        string(out, &printable(&proc.name, usize::MAX));
        out.push(',');
        key(out, "command");
        string(out, &printable(&proc.command, MAX_COMMAND_BYTES));
        out.push(',');
        key(out, "state");
        string(out, &printable(&proc.state.to_string(), usize::MAX));
        let _ = write!(out, ",\"uid\":{},", proc.uid);
        // Null, not absent-and-defaulted: an unresolved uid is not "root".
        key(out, "user");
        match &proc.user {
            Some(user) => string(out, &printable(user, usize::MAX)),
            None => out.push_str("null"),
        }
        let _ = write!(out, ",\"threads\":{},", proc.threads);
        // A share of one core; legitimately over 100 on a multi-core box.
        number(out, "cpu_pct", proc.cpu_pct);
        let _ = write!(
            out,
            ",\"rss_bytes\":{},\"virtual_bytes\":{},",
            proc.rss_bytes, proc.virtual_bytes
        );
        number(out, "mem_pct", proc.mem_pct);
        // Omitted when not measured (ptrace denied) — null, never zero.
        if let Some(io) = proc.io {
            out.push_str(",\"io\":{");
            number(out, "read_bytes_per_sec", io.read_bytes_per_sec);
            out.push(',');
            number(out, "write_bytes_per_sec", io.write_bytes_per_sec);
            out.push('}');
        }
        out.push('}');
    }
    out.push_str("]}");
}

/// Replace anything the collector refuses as a possible terminal escape
/// (C0 controls and DEL) and cap the length in bytes, on a char boundary.
/// Process names and command lines are chosen by whoever started the
/// process, so they are the one place such bytes can come from.
fn printable(text: &str, max_bytes: usize) -> String {
    let mut out = String::with_capacity(text.len().min(max_bytes));
    for c in text.chars() {
        let c = if c < ' ' || c == '\u{7f}' { '?' } else { c };
        if out.len() + c.len_utf8() > max_bytes {
            break;
        }
        out.push(c);
    }
    out
}

fn key(out: &mut String, name: &str) {
    out.push('"');
    out.push_str(name);
    out.push_str("\":");
}

fn number(out: &mut String, name: &str, value: f64) {
    key(out, name);
    // JSON has no NaN or infinity, and a decoder rejects the whole report
    // over one. Neither is a real reading, so it goes out as zero.
    let value = if value.is_finite() { value } else { 0.0 };
    let _ = write!(out, "{value}");
}

/// A JSON string literal, escaped.
pub fn string(out: &mut String, text: &str) {
    out.push('"');
    for c in text.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c < ' ' => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out.push('"');
}

/// Epoch milliseconds as RFC 3339 UTC, e.g. `2026-11-15T21:58:03.123Z`.
pub fn rfc3339_ms(at_ms: u64) -> String {
    let secs = at_ms / 1000;
    let millis = at_ms % 1000;
    let days = (secs / 86_400) as i64;
    let rem = secs % 86_400;
    let (year, month, day) = civil_from_days(days);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{millis:03}Z",
        rem / 3600,
        (rem % 3600) / 60,
        rem % 60,
    )
}

/// Days since 1970-01-01 to a proleptic Gregorian (year, month, day).
/// Howard Hinnant's `civil_from_days`.
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let month = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let year = yoe + era * 400 + i64::from(month <= 2);
    (year, month, day)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::render::tests::fixture;

    #[test]
    fn formats_timestamps_as_rfc3339_utc() {
        assert_eq!(rfc3339_ms(0), "1970-01-01T00:00:00.000Z");
        assert_eq!(rfc3339_ms(951_782_400_123), "2000-02-29T00:00:00.123Z");
        assert_eq!(rfc3339_ms(1_794_779_883_456), "2026-11-15T21:58:03.456Z");
    }

    #[test]
    fn escapes_strings() {
        let mut out = String::new();
        string(&mut out, "a\"b\\c\nd\u{1b}");
        assert_eq!(out, r#""a\"b\\c\nd\u001b""#);
    }

    #[test]
    fn printable_replaces_controls_and_caps_bytes_on_a_char_boundary() {
        assert_eq!(printable("ok\u{1b}[2J\u{7f}", usize::MAX), "ok?[2J?");
        assert_eq!(printable("ééé", 5), "éé", "never splits a char");
    }

    #[test]
    fn sample_carries_counters_not_rates() {
        let (_, s) = fixture();
        let json = sample(1_794_779_883_456, &s, false);
        assert!(json.starts_with(r#"{"at":"2026-11-15T21:58:03.456Z","elapsed_secs":5,"uptime_secs":90061,"procs_running":3,"procs_blocked":1,"#), "{json}");
        assert!(json.contains(r#""cpu":{"ticks":{"user":4000,"nice":10,"system":900,"idle":30000,"iowait":200,"irq":50,"softirq":80,"steal":15},"load1":0.5,"load5":1,"load15":0.75}"#), "{json}");
        assert!(json.contains(r#""net":[{"iface":"eth0","rx_bytes":9000000,"tx_bytes":4000000,"#), "{json}");
        assert!(json.contains(r#""mount_point":"/","fs_type":"ext4""#), "{json}");
        assert!(!json.contains("processes"), "spooled samples carry no process list");
    }

    #[test]
    fn process_nulls_stay_null() {
        let (_, mut s) = fixture();
        s.processes.top[0].user = None;
        let json = sample(0, &s, true);
        assert!(json.contains(r#""uid":33,"user":null"#), "{json}");
        // The kernel thread's io was not measured: no key at all.
        let kworker = &json[json.find("kworker").unwrap()..];
        assert!(!kworker.contains("\"io\""), "{json}");
        assert!(json.contains(r#""io":{"read_bytes_per_sec":1048576,"write_bytes_per_sec":0}"#));
    }

    #[test]
    fn non_finite_numbers_go_out_as_zero() {
        let (_, mut s) = fixture();
        s.elapsed_secs = f64::NAN;
        s.processes.top[0].cpu_pct = f64::INFINITY;
        let json = sample(0, &s, true);
        assert!(json.contains(r#""elapsed_secs":0,"#));
        assert!(json.contains(r#""cpu_pct":0,"#));
    }

    #[test]
    fn host_block() {
        let (identity, _) = fixture();
        assert_eq!(
            host("web-01", &identity),
            format!(
                r#"{{"id":"web-01","hostname":"web-01","os":"Debian GNU/Linux 13","kernel":"6.18.53","cores":2,"agent_version":"{}"}}"#,
                env!("CARGO_PKG_VERSION")
            )
        );
    }
}
