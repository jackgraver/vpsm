//! Memory and swap, from `/proc/meminfo`.
//!
//! The one judgement call here is what "used" means. `MemTotal - MemFree` is
//! the naive answer and it's wrong in a way that matters: Linux deliberately
//! spends all otherwise-free memory on page cache, so by that definition a
//! perfectly healthy box reads as ~100% full and the number tells you
//! nothing. We use `MemTotal - MemAvailable` instead — `MemAvailable` is the
//! kernel's own estimate of what a new workload could claim without pushing
//! the machine into swap, which is the question an operator is really asking.

use std::io;

const MEMINFO: &str = "/proc/meminfo";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Memory {
    pub total_bytes: u64,
    /// `total - available`: memory genuinely committed to running work,
    /// excluding reclaimable cache.
    pub used_bytes: u64,
    /// The kernel's estimate of what's claimable without swapping.
    pub available_bytes: u64,
    /// Completely untouched memory. Expected to be small and uninteresting on
    /// a long-running box; kept because its absence confuses people reading
    /// the output next to `free`.
    pub free_bytes: u64,
    pub buffers_bytes: u64,
    /// Page cache, with reclaimable slab added and tmpfs subtracted — the
    /// same arithmetic `free` and `htop` use. Shared memory lives in the
    /// `Cached` figure but is not reclaimable, so leaving it in would
    /// overstate how much cache the kernel could hand back under pressure.
    pub cached_bytes: u64,
    pub swap_total_bytes: u64,
    pub swap_used_bytes: u64,
}

impl Memory {
    pub fn used_pct(&self) -> f64 {
        pct(self.used_bytes, self.total_bytes)
    }

    /// Swap in use, as a percentage. Worth watching independently of its
    /// size: a box that has started swapping is already in trouble, however
    /// little of the swap file it's touched.
    pub fn swap_used_pct(&self) -> f64 {
        pct(self.swap_used_bytes, self.swap_total_bytes)
    }
}

fn pct(part: u64, whole: u64) -> f64 {
    if whole == 0 {
        // No swap configured is the common case here, not an error.
        return 0.0;
    }
    100.0 * part as f64 / whole as f64
}

pub fn read() -> io::Result<Memory> {
    Ok(parse(&crate::procfs::read(MEMINFO)?))
}

pub fn parse(text: &str) -> Memory {
    let mut total = 0;
    let mut free = 0;
    let mut available = None;
    let mut buffers = 0;
    let mut cached = 0;
    let mut reclaimable = 0;
    let mut shmem = 0;
    let mut swap_total = 0;
    let mut swap_free = 0;

    for line in text.lines() {
        let Some((key, value)) = line.split_once(':') else {
            continue;
        };
        // Every field we read is reported in kB. Fields that aren't (the
        // hugepage counts) are ones we never look at, so a blanket
        // conversion is safe here.
        let Some(bytes) = value
            .split_ascii_whitespace()
            .next()
            .and_then(|v| v.parse::<u64>().ok())
            .map(|kb| kb * 1024)
        else {
            continue;
        };

        match key {
            "MemTotal" => total = bytes,
            "MemFree" => free = bytes,
            "MemAvailable" => available = Some(bytes),
            "Buffers" => buffers = bytes,
            "Cached" => cached = bytes,
            "SReclaimable" => reclaimable = bytes,
            "Shmem" => shmem = bytes,
            "SwapTotal" => swap_total = bytes,
            "SwapFree" => swap_free = bytes,
            _ => {}
        }
    }

    // MemAvailable arrived in Linux 3.14. On anything older, fall back to the
    // estimate that predated it: free plus the cache we'd expect to reclaim.
    let available = available.unwrap_or_else(|| free + buffers + cached);

    Memory {
        total_bytes: total,
        used_bytes: total.saturating_sub(available),
        available_bytes: available,
        free_bytes: free,
        buffers_bytes: buffers,
        cached_bytes: (cached + reclaimable).saturating_sub(shmem),
        swap_total_bytes: swap_total,
        swap_used_bytes: swap_total.saturating_sub(swap_free),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = "\
MemTotal:        4000000 kB
MemFree:          200000 kB
MemAvailable:    2500000 kB
Buffers:          100000 kB
Cached:          1500000 kB
SwapCached:            0 kB
Active:          1000000 kB
Shmem:            300000 kB
SReclaimable:     200000 kB
SwapTotal:       1000000 kB
SwapFree:         750000 kB
Hugepagesize:       2048 kB
HugePages_Total:       0
";

    fn sample() -> Memory {
        parse(SAMPLE)
    }

    #[test]
    fn converts_kilobytes_to_bytes() {
        assert_eq!(sample().total_bytes, 4_000_000 * 1024);
    }

    #[test]
    fn used_is_total_minus_available_not_minus_free() {
        let mem = sample();
        assert_eq!(mem.used_bytes, 1_500_000 * 1024);
        assert_eq!(mem.used_pct(), 37.5);
    }

    #[test]
    fn cached_adds_reclaimable_slab_and_drops_shmem() {
        // 1500000 + 200000 - 300000
        assert_eq!(sample().cached_bytes, 1_400_000 * 1024);
    }

    #[test]
    fn swap_is_reported_as_used_not_free() {
        let mem = sample();
        assert_eq!(mem.swap_used_bytes, 250_000 * 1024);
        assert_eq!(mem.swap_used_pct(), 25.0);
    }

    #[test]
    fn missing_available_falls_back_to_free_plus_cache() {
        let mem = parse("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 200 kB\n");
        assert_eq!(mem.available_bytes, 350 * 1024);
        assert_eq!(mem.used_bytes, 650 * 1024);
    }

    #[test]
    fn no_swap_configured_reports_zero_rather_than_dividing_by_zero() {
        let mem = parse("MemTotal: 1000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n");
        assert_eq!(mem.swap_used_pct(), 0.0);
    }

    #[test]
    fn garbage_lines_are_skipped() {
        let mem = parse("not a field\nMemTotal: lots kB\nMemFree: 100 kB\n");
        assert_eq!(mem.total_bytes, 0);
        assert_eq!(mem.free_bytes, 100 * 1024);
    }
}
