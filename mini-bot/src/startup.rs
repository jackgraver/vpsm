//! Agent startup: load configuration, then do what this invocation is for —
//! monitor into the spool, print one sample, benchmark, export the spool
//! to a collector, or read the journal for the log viewer.

use std::process;
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::bench;
use crate::config::{Config, Mode};
use crate::export;
use crate::logs;
use crate::metrics::host::Identity;
use crate::metrics::{Sampler, cpu};
use crate::render;
use crate::spool;
use crate::wire;

/// A one-off run still needs a window to measure rates over, but it doesn't
/// need a whole interval of it — a second produces real numbers without
/// making whoever typed `--once` wait around for them.
const ONCE_WINDOW: Duration = Duration::from_secs(1);

pub fn run() {
    let config = Config::from_env();

    match config.mode {
        // Self-measurement instead of monitoring: answers what the agent
        // costs this particular box, and what sample interval suits it.
        Mode::Bench => bench::run(&config),
        Mode::Export { follow } => export::run(&config.spool_dir, follow),
        Mode::Logs { ref args } => logs::run(args),
        Mode::Once => once(&config),
        Mode::Monitor => monitor(&config),
    }
}

/// Prime a sampler, or exit: `/proc/stat` is the one source with no
/// fallback. Exiting non-zero makes systemd report a failed unit instead of
/// a running agent that silently measures nothing.
fn sampler(config: &Config) -> (Identity, Sampler) {
    // Counted from /proc/stat's per-CPU lines rather than from a separate
    // source, so the CPU count can never disagree with the per-CPU figures.
    let cpus = cpu::read().map(|stat| stat.per_cpu.len()).unwrap_or(0);
    let identity = Identity::read(cpus);
    match Sampler::new(config.top_processes, config.process_io) {
        Ok(sampler) => (identity, sampler),
        Err(error) => {
            eprintln!("mini-bot: cannot read /proc/stat ({error}); nothing to monitor");
            process::exit(1);
        }
    }
}

fn once(config: &Config) {
    let (identity, mut sampler) = sampler(config);
    thread::sleep(config.interval.min(ONCE_WINDOW));
    match sampler.sample() {
        Ok(sample) => println!("{}", render::sample(&identity, &sample)),
        Err(error) => {
            eprintln!("mini-bot: sample failed: {error}");
            process::exit(1);
        }
    }
}

fn monitor(config: &Config) {
    let Some(host_id) = &config.host_id else {
        eprintln!(
            "mini-bot: VPSM_HOST_ID is not set. It is the key every stored sample is filed \
             under and must be unique across your hosts, so there is no default; \
             set it in the service's environment file."
        );
        process::exit(1);
    };

    spool::set_shared_umask();
    let mut spool = match spool::Writer::open(
        &config.spool_dir,
        config.spool_max_bytes,
        config.spool_thin_secs,
    ) {
        Ok(spool) => spool,
        Err(error) => {
            eprintln!("mini-bot: cannot open spool {}: {error}", config.spool_dir.display());
            process::exit(1);
        }
    };

    let (identity, mut sampler) = sampler(config);
    if let Err(error) = spool.write_host(&wire::host(host_id, &identity)) {
        eprintln!("mini-bot: cannot write to spool {}: {error}", config.spool_dir.display());
        process::exit(1);
    }

    eprintln!(
        "mini-bot: watching {} as {host_id:?} ({} CPUs), sampling every {}s, top {} processes{}",
        identity.hostname,
        identity.cpus,
        config.interval.as_secs(),
        config.top_processes,
        if config.process_io { " with disk I/O" } else { "" },
    );
    eprintln!(
        "mini-bot: spooling to {} (cap {} MiB) until a collector pulls it",
        config.spool_dir.display(),
        config.spool_max_bytes / (1024 * 1024),
    );

    let mut write_failing = false;
    loop {
        // Sleep first: the sampler was primed at construction, and rates need
        // a window to have elapsed before there's anything to report.
        thread::sleep(config.interval);

        let sample = match sampler.sample() {
            Ok(sample) => sample,
            // Keep going. A single failed sample is usually something
            // transient, and an agent that gives up is worse than one with a
            // gap in its history.
            Err(error) => {
                eprintln!("mini-bot: sample failed: {error}");
                continue;
            }
        };

        let at_ms = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0);
        let result = spool.append(
            at_ms,
            &wire::sample(at_ms, &sample, false),
            &wire::sample(at_ms, &sample, true),
        );
        // Report a failing disk once, and its recovery, not every interval.
        match result {
            Ok(()) if write_failing => {
                eprintln!("mini-bot: spool writable again");
                write_failing = false;
            }
            Err(error) if !write_failing => {
                eprintln!("mini-bot: cannot write to spool: {error}; samples are being lost");
                write_failing = true;
            }
            _ => {}
        }
    }
}
