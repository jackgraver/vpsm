//! Host identity and whole-machine load.
//!
//! Split into two pieces by how often they change. [`Identity`] is read once
//! at startup — the hostname and kernel of a box don't move, and re-reading
//! them every few seconds would be pure overhead. [`Load`] and the uptime go
//! into every sample.

use std::io;

use crate::procfs;

const HOSTNAME: &str = "/proc/sys/kernel/hostname";
const OSRELEASE: &str = "/proc/sys/kernel/osrelease";
const UPTIME: &str = "/proc/uptime";
const LOADAVG: &str = "/proc/loadavg";
const OS_RELEASE: &str = "/etc/os-release";

/// Which machine this is. Read once; the collector needs it to tell the
/// reports from different hosts apart.
#[derive(Clone, Debug)]
pub struct Identity {
    pub hostname: String,
    pub kernel: String,
    /// Distribution name, e.g. "Ubuntu 24.04.5 LTS". Empty when
    /// `/etc/os-release` is missing, which some minimal containers manage.
    pub os: String,
    pub cpus: usize,
}

impl Identity {
    pub fn read(cpus: usize) -> Self {
        Self {
            // A box with an unreadable hostname is odd but not worth refusing
            // to start over; the collector can fall back to the peer address.
            hostname: procfs::read_trimmed(HOSTNAME).unwrap_or_else(|_| "unknown".into()),
            kernel: procfs::read_trimmed(OSRELEASE).unwrap_or_else(|_| "unknown".into()),
            os: procfs::read(OS_RELEASE).map(|text| parse_os_release(&text)).unwrap_or_default(),
            cpus,
        }
    }
}

/// The `PRETTY_NAME` from os-release, unquoted.
pub fn parse_os_release(text: &str) -> String {
    text.lines()
        .find_map(|line| line.strip_prefix("PRETTY_NAME="))
        .map(|value| value.trim().trim_matches('"').to_string())
        .unwrap_or_default()
}

/// Run-queue pressure, from `/proc/loadavg`.
///
/// Load average is the one CPU-adjacent number that's meaningful from a
/// single reading, which makes it a useful cross-check on the sampled
/// percentages. Interpreting it needs the CPU count: a load of 8 is saturated
/// on 4 CPUs and half-idle on 24, hence [`Load::per_cpu`].
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Load {
    pub one: f64,
    pub five: f64,
    pub fifteen: f64,
}

impl Load {
    /// Load normalised by CPU count, where 1.0 means "exactly saturated".
    pub fn per_cpu(&self, cpus: usize) -> f64 {
        if cpus == 0 {
            return 0.0;
        }
        self.one / cpus as f64
    }
}

pub fn load() -> io::Result<Load> {
    Ok(parse_load(&procfs::read(LOADAVG)?))
}

pub fn parse_load(text: &str) -> Load {
    Load {
        one: procfs::field(text, 0).unwrap_or(0.0),
        five: procfs::field(text, 1).unwrap_or(0.0),
        fifteen: procfs::field(text, 2).unwrap_or(0.0),
    }
}

/// Seconds since boot. Also how the collector can tell a box rebooted rather
/// than just going quiet for a while.
pub fn uptime() -> io::Result<f64> {
    Ok(parse_uptime(&procfs::read(UPTIME)?))
}

pub fn parse_uptime(text: &str) -> f64 {
    procfs::field(text, 0).unwrap_or(0.0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_os_release_pretty_name() {
        let text = "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.5 LTS\"\nID=ubuntu\n";
        assert_eq!(parse_os_release(text), "Ubuntu 24.04.5 LTS");
        assert_eq!(parse_os_release("ID=alpine\n"), "");
    }

    #[test]
    fn parses_loadavg() {
        let load = parse_load("0.52 1.04 0.75 2/543 123456\n");
        assert_eq!(load.one, 0.52);
        assert_eq!(load.five, 1.04);
        assert_eq!(load.fifteen, 0.75);
    }

    #[test]
    fn load_per_cpu_normalises_by_core_count() {
        let load = parse_load("8.00 0 0 1/1 1\n");
        assert_eq!(load.per_cpu(4), 2.0);
        assert_eq!(load.per_cpu(8), 1.0);
        assert_eq!(load.per_cpu(0), 0.0, "no divide by zero");
    }

    #[test]
    fn parses_uptime_seconds_ignoring_idle_column() {
        assert_eq!(parse_uptime("12345.67 98765.43\n"), 12345.67);
    }

    #[test]
    fn unreadable_values_read_as_zero() {
        assert_eq!(parse_load(""), Load::default());
        assert_eq!(parse_uptime("garbage"), 0.0);
    }
}
