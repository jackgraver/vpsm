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
use std::time::Duration;

/// Shortest interval we'll honour. Sampling costs a `/proc` walk, and below
/// about a second the agent starts showing up in its own measurements.
pub(crate) const MIN_INTERVAL: Duration = Duration::from_secs(1);

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
    /// Where the collector lives. Unused so far — the wire protocol between
    /// the two halves isn't decided yet (see the root `CLAUDE.md`), so this
    /// is read and validated but not yet dialled.
    pub collector_url: Option<String>,
    /// Print one sample and exit, instead of looping. For eyeballing output
    /// on a new box and for `cargo run`.
    pub once: bool,
    /// Measure what the agent costs this host and exit, instead of
    /// monitoring it. See `bench`.
    pub bench: bool,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            interval: Duration::from_secs(5),
            top_processes: 10,
            process_io: true,
            collector_url: None,
            once: false,
            bench: false,
        }
    }
}

impl Config {
    /// Build a config from the environment, falling back to defaults.
    ///
    /// Also accepts a bare `--once` argument, which is the one thing that's
    /// awkward as an env var because it's about this single invocation rather
    /// than about how the agent is deployed.
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

        cfg.collector_url = var("VPSM_COLLECTOR_URL");
        let args: Vec<String> = env::args().skip(1).collect();
        cfg.once = args.iter().any(|arg| arg == "--once");
        cfg.bench = args.iter().any(|arg| arg == "--bench");

        cfg
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

    #[test]
    fn defaults_are_usable() {
        let cfg = Config::default();
        assert!(cfg.interval >= MIN_INTERVAL);
        assert!(cfg.top_processes > 0);
    }
}
