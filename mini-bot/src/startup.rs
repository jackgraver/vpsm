//! Agent startup: load configuration, then run the sample loop.

use std::process;
use std::thread;
use std::time::Duration;

use crate::bench;
use crate::config::Config;
use crate::metrics::host::Identity;
use crate::metrics::{Sampler, cpu};
use crate::render;

/// A one-off run still needs a window to measure rates over, but it doesn't
/// need a whole interval of it — a second produces real numbers without
/// making whoever typed `--once` wait around for them.
const ONCE_WINDOW: Duration = Duration::from_secs(1);

pub fn run() {
    let config = Config::from_env();

    if config.bench {
        // Self-measurement instead of monitoring: answers what the agent
        // costs this particular box, and what sample interval suits it.
        bench::run(&config);
        return;
    }

    // Counted from /proc/stat's per-CPU lines rather than from a separate
    // source, so the CPU count can never disagree with the per-CPU figures.
    let cpus = cpu::read().map(|stat| stat.per_cpu.len()).unwrap_or(0);
    let identity = Identity::read(cpus);

    let mut sampler = match Sampler::new(config.top_processes, config.process_io) {
        Ok(sampler) => sampler,
        Err(error) => {
            // /proc/stat is the one source with no fallback. Exit non-zero so
            // systemd reports a failed unit instead of a running agent that
            // silently measures nothing.
            eprintln!("mini-bot: cannot read /proc/stat ({error}); nothing to monitor");
            process::exit(1);
        }
    };

    let window = if config.once {
        config.interval.min(ONCE_WINDOW)
    } else {
        config.interval
    };

    eprintln!(
        "mini-bot: watching {} ({} CPUs), sampling every {}s, top {} processes{}",
        identity.hostname,
        identity.cpus,
        window.as_secs(),
        config.top_processes,
        if config.process_io { " with disk I/O" } else { "" },
    );
    match &config.collector_url {
        // TODO: report samples to the collector. Deliberately not built yet:
        // the transport, payload shape and auth between mini-bot and the
        // collector aren't decided, and both ends have to agree (see the root
        // CLAUDE.md). Until then the agent samples and prints, which is
        // enough to validate the sampling half on a real box.
        Some(url) => eprintln!("mini-bot: collector {url} configured, but reporting isn't built yet"),
        None => eprintln!("mini-bot: no collector configured; printing samples locally"),
    }

    loop {
        // Sleep first: the sampler was primed at construction, and rates need
        // a window to have elapsed before there's anything to report.
        thread::sleep(window);

        match sampler.sample() {
            Ok(sample) => println!("{}", render::sample(&identity, &sample)),
            // Keep going. A single failed sample is usually something
            // transient, and an agent that gives up is worse than one with a
            // gap in its history.
            Err(error) => eprintln!("mini-bot: sample failed: {error}"),
        }

        if config.once {
            break;
        }
    }
}
