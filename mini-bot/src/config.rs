//! Agent configuration.
//!
//! Environment variables, not a config file: mini-bot is deployed as a single
//! binary under systemd, where an `Environment=`/`EnvironmentFile=` line in
//! the unit is already the natural place to put this, and it saves shipping a
//! config parser and deciding where the file lives.
//!
//! Everything has a working default, so the agent runs with no configuration
//! at all. Bad values are reported and then replaced with the default rather
//! than being fatal — a typo'd interval shouldn't take monitoring offline.

use std::env;
use std::path::PathBuf;
use std::time::Duration;

/// Shortest interval we'll honour. Sampling costs a `/proc` walk, and below
/// about a second the agent starts showing up in its own measurements.
pub(crate) const MIN_INTERVAL: Duration = Duration::from_secs(1);

/// Where the spool lives unless `VPSM_SPOOL_DIR` or `--spool` says
/// otherwise. `export` runs over a bare SSH session with no environment
/// from the systemd unit, so the default is what it will usually find.
pub const DEFAULT_SPOOL_DIR: &str = "/var/lib/vpsm/spool";

/// What this invocation is for.
#[derive(Clone, Debug, PartialEq)]
pub enum Mode {
    /// Sample on an interval into the spool. The service.
    Monitor,
    /// Print one sample and exit.
    Once,
    /// Measure what the agent costs this host and exit.
    Bench,
    /// Stream the spool to a collector on stdout (see `export`).
    Export { follow: bool },
    /// Read this host's journal for the laptop (see `logs`). Carries the
    /// arguments after `logs`, which that module parses itself.
    Logs { args: Vec<String> },
}

#[derive(Clone, Debug)]
pub struct Config {
    /// How long to wait between samples.
    pub interval: Duration,
    /// How many processes to report, per ranking (CPU and memory are ranked
    /// separately, so a sample carries up to twice this many).
    ///
    /// The point of a cap is that a busy host has hundreds of processes and
    /// almost all of them are idle: reporting every one of them every few
    /// seconds would dominate both the payload and the collector's storage,
    /// to say what "0.0%" already says.
    pub top_processes: usize,
    /// Whether to attribute disk I/O to individual processes. Costs one extra
    /// file read per reported process and needs privileges the agent may not
    /// have, so it degrades quietly when unavailable.
    pub process_io: bool,
    /// The operator-assigned identity every stored row hangs off
    /// (`VPSM_HOST_ID`). Required to monitor: there is no safe default,
    /// because hostnames are not unique across VPSes and data filed under
    /// the wrong host is found weeks later.
    pub host_id: Option<String>,
    /// The spool directory (`VPSM_SPOOL_DIR`, or `--spool <dir>`).
    pub spool_dir: PathBuf,
    /// Cap on uncollected data (`VPSM_SPOOL_MAX_MB`). Past half of it the
    /// oldest samples are thinned; past all of it they are dropped.
    pub spool_max_bytes: u64,
    /// Resolution thinned samples are kept at (`VPSM_SPOOL_THIN_SECS`).
    pub spool_thin_secs: u64,
    pub mode: Mode,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            interval: Duration::from_secs(5),
            top_processes: 10,
            process_io: true,
            host_id: None,
            spool_dir: PathBuf::from(DEFAULT_SPOOL_DIR),
            spool_max_bytes: 200 * 1024 * 1024,
            spool_thin_secs: 60,
            mode: Mode::Monitor,
        }
    }
}

impl Config {
    /// Build a config from the environment, falling back to defaults.
    ///
    /// Arguments pick the mode — `--once`, `--bench`, `export [--follow]`,
    /// `logs [...]` —
    /// since those are about this single invocation rather than about how the
    /// agent is deployed. `--spool <dir>` overrides the spool location for
    /// `export`, which runs without the unit's environment.
    pub fn from_env() -> Self {
        let mut cfg = Self::default();

        if let Some(raw) = var("VPSM_INTERVAL_SECS") {
            match raw.parse::<u64>() {
                Ok(secs) => {
                    let requested = Duration::from_secs(secs);
                    if requested < MIN_INTERVAL {
                        eprintln!(
                            "mini-bot: VPSM_INTERVAL_SECS={secs} is below the {}s floor; using {}s",
                            MIN_INTERVAL.as_secs(),
                            MIN_INTERVAL.as_secs()
                        );
                    }
                    cfg.interval = requested.max(MIN_INTERVAL);
                }
                Err(_) => warn_ignored("VPSM_INTERVAL_SECS", &raw, "expected whole seconds"),
            }
        }

        if let Some(raw) = var("VPSM_TOP_PROCESSES") {
            match raw.parse::<usize>() {
                Ok(n) => cfg.top_processes = n,
                Err(_) => warn_ignored("VPSM_TOP_PROCESSES", &raw, "expected a count"),
            }
        }

        if let Some(raw) = var("VPSM_PROCESS_IO") {
            match parse_bool(&raw) {
                Some(on) => cfg.process_io = on,
                None => warn_ignored("VPSM_PROCESS_IO", &raw, "expected true or false"),
            }
        }

        cfg.host_id = var("VPSM_HOST_ID");
        if let Some(dir) = var("VPSM_SPOOL_DIR") {
            cfg.spool_dir = PathBuf::from(dir);
        }
        if let Some(raw) = var("VPSM_SPOOL_MAX_MB") {
            match raw.parse::<u64>() {
                // Below a few segments' worth, thinning has nothing to work with.
                Ok(mb) if mb >= 4 => cfg.spool_max_bytes = mb * 1024 * 1024,
                _ => warn_ignored("VPSM_SPOOL_MAX_MB", &raw, "expected whole megabytes, at least 4"),
            }
        }
        if let Some(raw) = var("VPSM_SPOOL_THIN_SECS") {
            match raw.parse::<u64>() {
                Ok(secs) if secs >= 1 => cfg.spool_thin_secs = secs,
                _ => warn_ignored("VPSM_SPOOL_THIN_SECS", &raw, "expected whole seconds"),
            }
        }

        let args: Vec<String> = env::args().skip(1).collect();
        cfg.apply_args(&args);
        cfg
    }

    fn apply_args(&mut self, args: &[String]) {
        let has = |flag: &str| args.iter().any(|arg| arg == flag);
        self.mode = if args.first().is_some_and(|arg| arg == "logs") {
            // Everything after `logs` belongs to it: `--follow` there is not
            // export's `--follow`.
            Mode::Logs { args: args[1..].to_vec() }
        } else if args.first().is_some_and(|arg| arg == "export") {
            Mode::Export { follow: has("--follow") }
        } else if has("--bench") {
            Mode::Bench
        } else if has("--once") {
            Mode::Once
        } else {
            Mode::Monitor
        };
        if let Some(i) = args.iter().position(|arg| arg == "--spool") {
            match args.get(i + 1) {
                Some(dir) => self.spool_dir = PathBuf::from(dir),
                None => eprintln!("mini-bot: --spool needs a directory; using {}", self.spool_dir.display()),
            }
        }
    }
}

/// Read an environment variable, treating empty as unset.
///
/// systemd and shell wrappers both have a habit of exporting empty strings
/// for "not configured", and an empty collector URL is not a collector URL.
fn var(name: &str) -> Option<String> {
    let value = env::var(name).ok()?;
    let value = value.trim();
    (!value.is_empty()).then(|| value.to_string())
}

fn parse_bool(raw: &str) -> Option<bool> {
    match raw.to_ascii_lowercase().as_str() {
        "1" | "true" | "yes" | "on" => Some(true),
        "0" | "false" | "no" | "off" => Some(false),
        _ => None,
    }
}

fn warn_ignored(name: &str, raw: &str, expected: &str) {
    eprintln!("mini-bot: ignoring {name}={raw:?} ({expected}); using the default");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_truthy_and_falsy_spellings() {
        for raw in ["1", "true", "TRUE", "yes", "on"] {
            assert_eq!(parse_bool(raw), Some(true), "{raw}");
        }
        for raw in ["0", "false", "FALSE", "no", "off"] {
            assert_eq!(parse_bool(raw), Some(false), "{raw}");
        }
        assert_eq!(parse_bool("maybe"), None);
    }

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn arguments_pick_the_mode() {
        let mut cfg = Config::default();
        cfg.apply_args(&args(&[]));
        assert_eq!(cfg.mode, Mode::Monitor);
        cfg.apply_args(&args(&["--once"]));
        assert_eq!(cfg.mode, Mode::Once);
        cfg.apply_args(&args(&["export"]));
        assert_eq!(cfg.mode, Mode::Export { follow: false });
        cfg.apply_args(&args(&["export", "--follow", "--spool", "/tmp/s"]));
        assert_eq!(cfg.mode, Mode::Export { follow: true });
        assert_eq!(cfg.spool_dir, PathBuf::from("/tmp/s"));
        cfg.apply_args(&args(&["logs", "--follow", "--source", "web"]));
        assert_eq!(cfg.mode, Mode::Logs { args: args(&["--follow", "--source", "web"]) });
    }

    #[test]
    fn defaults_are_usable() {
        let cfg = Config::default();
        assert!(cfg.interval >= MIN_INTERVAL);
        assert!(cfg.top_processes > 0);
    }
}
