//! Network throughput, from `/proc/net/dev`.
//!
//! Per interface, and per second: the file holds byte and packet counters
//! since boot, which are useless on their own and become traffic once
//! differenced against the previous sample.
//!
//! Error and drop counters are carried through as raw totals rather than
//! rates. They're normally flat at some small number from years ago, and what
//! matters is whether they're *moving* — a rate would smear a burst of drops
//! across the window and round it to something that looks like nothing.

use std::io;

use crate::metrics::per_sec;
use crate::procfs;

const NET_DEV: &str = "/proc/net/dev";

/// Monotonic counters for one interface.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Counters {
    pub name: String,
    pub rx_bytes: u64,
    pub rx_packets: u64,
    pub rx_errors: u64,
    pub rx_dropped: u64,
    pub tx_bytes: u64,
    pub tx_packets: u64,
    pub tx_errors: u64,
    pub tx_dropped: u64,
}

/// Traffic on one interface over the window between two samples.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Rates {
    pub name: String,
    pub rx_bytes_per_sec: f64,
    pub tx_bytes_per_sec: f64,
    pub rx_packets_per_sec: f64,
    pub tx_packets_per_sec: f64,
    pub rx_errors: u64,
    pub tx_errors: u64,
    pub rx_dropped: u64,
    pub tx_dropped: u64,
}

impl Counters {
    pub fn since(&self, earlier: &Self, elapsed_secs: f64) -> Rates {
        Rates {
            name: self.name.clone(),
            rx_bytes_per_sec: per_sec(self.rx_bytes, earlier.rx_bytes, elapsed_secs),
            tx_bytes_per_sec: per_sec(self.tx_bytes, earlier.tx_bytes, elapsed_secs),
            rx_packets_per_sec: per_sec(self.rx_packets, earlier.rx_packets, elapsed_secs),
            tx_packets_per_sec: per_sec(self.tx_packets, earlier.tx_packets, elapsed_secs),
            rx_errors: self.rx_errors,
            tx_errors: self.tx_errors,
            rx_dropped: self.rx_dropped,
            tx_dropped: self.tx_dropped,
        }
    }
}

pub fn counters() -> io::Result<Vec<Counters>> {
    Ok(parse(&procfs::read(NET_DEV)?))
}

/// Parse `/proc/net/dev`.
///
/// Loopback is dropped: it's real traffic but it never leaves the box, so
/// including it makes a chatty local service look like bandwidth use. Docker
/// bridges and veth pairs are kept — on a VPS that *is* where the interesting
/// traffic goes, and an operator can tell them apart by name.
pub fn parse(text: &str) -> Vec<Counters> {
    let mut interfaces = Vec::new();

    for line in text.lines() {
        // Split on the colon rather than on whitespace: when a counter grows
        // wide enough the kernel's column padding runs out and the line
        // arrives as "eth0:1234567890", with no space after the name.
        let Some((name, stats)) = line.split_once(':') else {
            // The two header lines have no colon, which is also what filters
            // them out.
            continue;
        };
        let name = name.trim();
        if name.is_empty() || name == "lo" {
            continue;
        }

        let columns: Vec<u64> = stats
            .split_ascii_whitespace()
            .map(|f| f.parse().unwrap_or(0))
            .collect();
        // Eight receive columns then eight transmit ones. Anything shorter
        // isn't a layout we know how to read.
        if columns.len() < 16 {
            continue;
        }

        interfaces.push(Counters {
            name: name.to_string(),
            rx_bytes: columns[0],
            rx_packets: columns[1],
            rx_errors: columns[2],
            rx_dropped: columns[3],
            tx_bytes: columns[8],
            tx_packets: columns[9],
            tx_errors: columns[10],
            tx_dropped: columns[11],
        });
    }

    interfaces
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = "\
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 5000 50 0 0 0 0 0 0 5000 50 0 0 0 0 0 0
  eth0: 1000 10 1 2 0 0 0 0 2000 20 3 4 0 0 0 0
docker0: 100 1 0 0 0 0 0 0 200 2 0 0 0 0 0 0
";

    #[test]
    fn parses_receive_and_transmit_columns() {
        let interfaces = parse(SAMPLE);
        let eth0 = interfaces.iter().find(|i| i.name == "eth0").unwrap();
        assert_eq!(eth0.rx_bytes, 1000);
        assert_eq!(eth0.rx_packets, 10);
        assert_eq!(eth0.rx_errors, 1);
        assert_eq!(eth0.rx_dropped, 2);
        assert_eq!(eth0.tx_bytes, 2000);
        assert_eq!(eth0.tx_packets, 20);
        assert_eq!(eth0.tx_errors, 3);
        assert_eq!(eth0.tx_dropped, 4);
    }

    #[test]
    fn skips_headers_and_loopback_but_keeps_bridges() {
        let names: Vec<String> = parse(SAMPLE).into_iter().map(|i| i.name).collect();
        assert_eq!(names, vec!["eth0", "docker0"]);
    }

    #[test]
    fn handles_counters_wide_enough_to_eat_the_column_padding() {
        // No space after the colon once the byte count gets long enough.
        let line = "  eth0:99999999999999 10 0 0 0 0 0 0 1 2 0 0 0 0 0 0\n";
        let interfaces = parse(line);
        assert_eq!(interfaces.len(), 1);
        assert_eq!(interfaces[0].name, "eth0");
        assert_eq!(interfaces[0].rx_bytes, 99_999_999_999_999);
    }

    #[test]
    fn short_lines_are_skipped() {
        assert!(parse("eth0: 1 2 3\n").is_empty());
    }

    #[test]
    fn rates_divide_the_delta_by_the_window() {
        let earlier = Counters { name: "eth0".into(), rx_bytes: 1_000, tx_bytes: 500, rx_packets: 10, tx_packets: 5, ..Counters::default() };
        let now = Counters { name: "eth0".into(), rx_bytes: 11_000, tx_bytes: 2_500, rx_packets: 30, tx_packets: 25, rx_dropped: 7, ..Counters::default() };
        let rates = now.since(&earlier, 5.0);
        assert_eq!(rates.rx_bytes_per_sec, 2_000.0);
        assert_eq!(rates.tx_bytes_per_sec, 400.0);
        assert_eq!(rates.rx_packets_per_sec, 4.0);
        assert_eq!(rates.tx_packets_per_sec, 4.0);
        // Drops travel as the raw total, not a rate.
        assert_eq!(rates.rx_dropped, 7);
    }

    #[test]
    fn interface_reset_does_not_wrap() {
        let earlier = Counters { name: "eth0".into(), rx_bytes: 9_000, ..Counters::default() };
        let now = Counters { name: "eth0".into(), rx_bytes: 10, ..Counters::default() };
        assert_eq!(now.since(&earlier, 1.0).rx_bytes_per_sec, 0.0);
    }
}
