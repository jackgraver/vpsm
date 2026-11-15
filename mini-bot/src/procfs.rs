//! Shared plumbing for reading Linux `/proc`.
//!
//! Two things live here. First, I/O helpers: `/proc` files all report a size
//! of zero, so the usual read-the-whole-file paths do a grow-and-retry dance.
//! The per-process walk opens a couple of hundred files every interval, so
//! [`read_into`] lets callers reuse one buffer across all of them instead of
//! allocating per file.
//!
//! Second, the two `sysconf` values every `/proc` consumer needs in order to
//! turn kernel units into real ones: pages into bytes, and clock ticks into
//! seconds. Both are fixed for the life of the process, so they're read once.
//!
//! Parsing itself deliberately lives next to each metric (`cpu`, `memory`,
//! ...) rather than here, and is split so that the parsers take `&str` and
//! can be unit-tested against captured fixtures without touching the host.

use std::fs::File;
use std::io::{self, Read};
use std::sync::OnceLock;

/// Read a file into `buf`, replacing its previous contents.
///
/// Prefer this over `fs::read_to_string` on hot paths: it keeps the caller's
/// allocation alive across calls.
pub fn read_into(path: &str, buf: &mut String) -> io::Result<()> {
    buf.clear();
    File::open(path)?.read_to_string(buf)?;
    Ok(())
}

/// Read a file whose contents are small and read exactly once per sample.
pub fn read(path: &str) -> io::Result<String> {
    let mut buf = String::new();
    read_into(path, &mut buf)?;
    Ok(buf)
}

/// Read a file expected to hold a single value, trimmed of its newline.
pub fn read_trimmed(path: &str) -> io::Result<String> {
    Ok(read(path)?.trim().to_string())
}

/// Parse the `n`th (0-indexed) whitespace-separated field of `line`.
///
/// Returns `None` for both "no such field" and "that field didn't parse",
/// which collapses two cases that callers treat identically: `/proc` layouts
/// gain columns between kernel versions, and a field we can't make sense of
/// is no more useful than a missing one.
pub fn field<T: std::str::FromStr>(line: &str, n: usize) -> Option<T> {
    line.split_ascii_whitespace().nth(n)?.parse().ok()
}

/// Bytes per page, for the page-denominated fields in `/proc/<pid>/stat`.
pub fn page_size() -> u64 {
    static PAGE_SIZE: OnceLock<u64> = OnceLock::new();
    *PAGE_SIZE.get_or_init(|| sysconf(libc::_SC_PAGESIZE, 4096))
}

/// Kernel clock ticks per second, for the jiffie counters in `/proc/stat`
/// and `/proc/<pid>/stat`.
pub fn clock_ticks() -> u64 {
    static CLOCK_TICKS: OnceLock<u64> = OnceLock::new();
    *CLOCK_TICKS.get_or_init(|| sysconf(libc::_SC_CLK_TCK, 100))
}

/// `sysconf` for values that must be positive, falling back to the near
/// universal default on the "indeterminate" (-1) answer.
fn sysconf(name: libc::c_int, fallback: u64) -> u64 {
    // SAFETY: sysconf takes an int name and returns a long; no pointers and
    // no state involved. We only pass the two names declared by libc.
    let value = unsafe { libc::sysconf(name) };
    if value > 0 { value as u64 } else { fallback }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn field_picks_out_columns() {
        let line = "  10 20  thirty 40";
        assert_eq!(field::<u64>(line, 0), Some(10));
        assert_eq!(field::<u64>(line, 1), Some(20));
        assert_eq!(field::<String>(line, 2).as_deref(), Some("thirty"));
        assert_eq!(field::<u64>(line, 3), Some(40));
    }

    #[test]
    fn field_rejects_missing_and_unparsable() {
        let line = "10 twenty";
        assert_eq!(field::<u64>(line, 1), None, "unparsable");
        assert_eq!(field::<u64>(line, 9), None, "past the end");
    }

    #[test]
    fn sysconf_values_are_sane() {
        assert!(page_size().is_power_of_two());
        assert!(clock_ticks() > 0);
    }
}
