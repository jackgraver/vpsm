//! `mini-bot export`: hand the spool to a collector over stdin/stdout.
//!
//! The collector runs this over SSH (`ssh host mini-bot export`), so the
//! agent needs no network code, no listening port and no token of its own:
//! the SSH login the operator already has is the authentication.
//!
//! # The stream
//!
//! Stdout is one JSON object per line:
//!
//! ```text
//! {"cursor":N,"report":{"host":{...},"samples":[...]}}   zero or more
//! {"caught_up":true}                                      once the backlog is out
//! ```
//!
//! `cursor` is the highest spool seq in that report. Stdin takes lines of
//! `ack N` once the collector has *stored* everything up to N, and the spool
//! deletes what that covers. Nothing is deleted on read: a connection that
//! drops mid-transfer just means the next export sends it again, and the
//! collector counts the repeats as duplicates.
//!
//! Without `--follow`, export sends the backlog, then applies acks until
//! stdin closes. With `--follow` it keeps the session open and streams each
//! new sample as the daemon writes it, which is how a live view avoids a new
//! SSH handshake per refresh. Either way, closing stdin ends it.
//!
//! The newest sample goes out with its process list attached (see `spool`);
//! every other sample goes without.

use std::io::{self, BufRead, Write};
use std::path::{Path, PathBuf};
use std::process;
use std::thread;
use std::time::Duration;

use crate::spool;

/// Samples per report. The collector accepts up to 1024; staying well under
/// keeps each of its transactions short and each line a few hundred KB.
const BATCH: usize = 256;

/// How often a follower checks for new samples. A stat and occasionally a
/// small read; well below anything the agent's own sampling costs.
const FOLLOW_POLL: Duration = Duration::from_millis(500);

pub fn run(dir: &Path, follow: bool) {
    if let Err(error) = export(dir, follow) {
        // A closed stdout is the collector hanging up, not a failure.
        if error.kind() == io::ErrorKind::BrokenPipe {
            return;
        }
        eprintln!("mini-bot export: {error}");
        process::exit(1);
    }
}

fn export(dir: &Path, follow: bool) -> io::Result<()> {
    let host = spool::read_file(&dir.join(spool::HOST_FILE)).map_err(|error| {
        io::Error::new(
            error.kind(),
            format!(
                "no spool at {} ({error}); is the mini-bot service running, and is this user in its group?",
                dir.display()
            ),
        )
    })?;
    let host = host.trim().to_string();
    spool::set_shared_umask();

    let mut out = io::BufWriter::new(io::stdout().lock());
    let mut tail = Tail::new(dir.to_path_buf(), spool::read_cursor(dir)?);

    let sent = tail.send_new(&host, &mut out)?;
    if !sent {
        // Nothing new since the last pull. Send the latest reading anyway, as
        // a duplicate, so the collector's process snapshot is current.
        tail.send_latest(&host, &mut out, true)?;
    }
    out.write_all(b"{\"caught_up\":true}\n")?;
    out.flush()?;

    if !follow {
        return apply_acks(dir);
    }

    let ack_dir = dir.to_path_buf();
    thread::spawn(move || {
        if let Err(error) = apply_acks(&ack_dir) {
            eprintln!("mini-bot export: {error}");
        }
        // Stdin closed: the collector is done with us.
        process::exit(0);
    });
    loop {
        thread::sleep(FOLLOW_POLL);
        if tail.send_new(&host, &mut out)? {
            out.flush()?;
        }
    }
}

/// Apply `ack N` lines from stdin until it closes.
fn apply_acks(dir: &Path) -> io::Result<()> {
    for line in io::stdin().lock().lines() {
        let line = line?;
        let Some(seq) = line.trim().strip_prefix("ack ").and_then(|n| n.trim().parse().ok())
        else {
            eprintln!("mini-bot export: ignoring unrecognised input {line:?}");
            continue;
        };
        spool::ack(dir, seq)?;
    }
    Ok(())
}

/// Reads spooled samples past a seq, remembering where it got to so a
/// follower only reads what is new.
struct Tail {
    dir: PathBuf,
    /// Highest seq already sent.
    sent: u64,
    /// The segment being read and the byte offset reached in it.
    position: Option<(u64, u64)>,
}

impl Tail {
    fn new(dir: PathBuf, cursor: u64) -> Self {
        Self { dir, sent: cursor, position: None }
    }

    /// Send every sample past `self.sent`, then the latest reading if it is
    /// newer still. Returns whether anything went out.
    fn send_new(&mut self, host: &str, out: &mut impl Write) -> io::Result<bool> {
        let latest = spool::read_latest(&self.dir).ok();
        let mut any = false;
        let mut batch: Vec<spool::Line> = Vec::new();

        let segments = spool::segments(&self.dir)?;
        for (i, seg) in segments.iter().enumerate() {
            // Skip segments wholly sent: every seq in one is below the next
            // segment's first, so if that is at or before sent+1 there is
            // nothing new here. This keeps a follower from re-reading the
            // whole backlog every poll.
            if segments.get(i + 1).is_some_and(|next| next.first_seq <= self.sent + 1) {
                continue;
            }
            let offset = match self.position {
                Some((first, offset)) if first == seg.first_seq => offset,
                _ => 0,
            };
            let (lines, end) = match spool::read_lines(&seg.path, offset) {
                Ok(read) => read,
                // Thinned or acked away between listing and opening.
                Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error),
            };
            self.position = Some((seg.first_seq, end));
            for line in lines {
                if line.seq <= self.sent {
                    continue;
                }
                batch.push(line);
                if batch.len() == BATCH {
                    self.send_batch(host, &mut batch, latest.as_ref(), out)?;
                    any = true;
                }
            }
        }
        if !batch.is_empty() {
            self.send_batch(host, &mut batch, latest.as_ref(), out)?;
            any = true;
        }

        // `latest` is written just before its spool line, so it can be one
        // ahead of the segments. Sending it now is what makes a follower's
        // view current; the spool line, when it lands, is then skipped.
        if latest.as_ref().is_some_and(|(seq, _)| *seq > self.sent) {
            any |= self.send_latest(host, out, false)?;
        }
        Ok(any)
    }

    fn send_batch(
        &mut self,
        host: &str,
        batch: &mut Vec<spool::Line>,
        latest: Option<&(u64, String)>,
        out: &mut impl Write,
    ) -> io::Result<()> {
        let cursor = batch.last().map_or(self.sent, |l| l.seq);
        let samples: Vec<&str> = batch
            .iter()
            .map(|line| match latest {
                // The same reading, with its process list.
                Some((seq, full)) if *seq == line.seq => full.as_str(),
                _ => line.json.as_str(),
            })
            .collect();
        write_report(out, host, cursor, &samples)?;
        self.sent = cursor;
        batch.clear();
        Ok(())
    }

    /// Send the latest reading on its own. `force` sends it even when it is
    /// not new, without moving the cursor past anything unsent.
    fn send_latest(&mut self, host: &str, out: &mut impl Write, force: bool) -> io::Result<bool> {
        let Ok((seq, full)) = spool::read_latest(&self.dir) else {
            return Ok(false);
        };
        if seq <= self.sent && !force {
            return Ok(false);
        }
        let cursor = seq.max(self.sent);
        write_report(out, host, cursor, &[full.as_str()])?;
        self.sent = cursor;
        Ok(true)
    }
}

fn write_report(out: &mut impl Write, host: &str, cursor: u64, samples: &[&str]) -> io::Result<()> {
    write!(out, "{{\"cursor\":{cursor},\"report\":{{\"host\":{host},\"samples\":[")?;
    for (i, sample) in samples.iter().enumerate() {
        if i > 0 {
            out.write_all(b",")?;
        }
        out.write_all(sample.as_bytes())?;
    }
    out.write_all(b"]}}\n")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    fn temp(name: &str) -> PathBuf {
        let path = std::env::temp_dir().join(format!("mini-bot-export-{name}-{}", process::id()));
        let _ = fs::remove_dir_all(&path);
        path
    }

    fn frames(out: Vec<u8>) -> Vec<String> {
        String::from_utf8(out).unwrap().lines().map(str::to_string).collect()
    }

    #[test]
    fn sends_the_backlog_past_the_cursor_with_processes_on_the_newest() {
        let dir = temp("backlog");
        let mut w = spool::Writer::open(&dir, 1 << 30, 60).unwrap();
        for i in 1..=3 {
            w.append(i, &format!("{{\"n\":{i}}}"), &format!("{{\"n\":{i},\"processes\":{{}}}}"))
                .unwrap();
        }
        spool::ack(&dir, 1).unwrap();

        let mut tail = Tail::new(dir.clone(), spool::read_cursor(&dir).unwrap());
        let mut out = Vec::new();
        assert!(tail.send_new("{\"id\":\"h\"}", &mut out).unwrap());
        assert_eq!(
            frames(out),
            [r#"{"cursor":3,"report":{"host":{"id":"h"},"samples":[{"n":2},{"n":3,"processes":{}}]}}"#]
        );

        // A follower sees nothing until the daemon writes again.
        let mut out = Vec::new();
        assert!(!tail.send_new("{}", &mut out).unwrap());
        w.append(4, "{\"n\":4}", "{\"n\":4,\"processes\":{}}").unwrap();
        assert!(tail.send_new("{}", &mut out).unwrap());
        assert_eq!(frames(out), [r#"{"cursor":4,"report":{"host":{},"samples":[{"n":4,"processes":{}}]}}"#]);
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn latest_ahead_of_the_spool_goes_out_once() {
        let dir = temp("ahead");
        let mut w = spool::Writer::open(&dir, 1 << 30, 60).unwrap();
        w.append(1, "{\"n\":1}", "{\"n\":1,\"p\":1}").unwrap();
        // Simulate the window between `latest` and the spool line.
        fs::write(dir.join("latest"), "2\t{\"n\":2,\"p\":1}").unwrap();

        let mut tail = Tail::new(dir.clone(), 0);
        let mut out = Vec::new();
        tail.send_new("{}", &mut out).unwrap();
        assert_eq!(
            frames(out),
            [
                r#"{"cursor":1,"report":{"host":{},"samples":[{"n":1}]}}"#,
                r#"{"cursor":2,"report":{"host":{},"samples":[{"n":2,"p":1}]}}"#,
            ]
        );

        // Its spool line then lands and is not sent again.
        w.append(2, "{\"n\":2}", "{\"n\":2,\"p\":1}").unwrap();
        let mut out = Vec::new();
        assert!(!tail.send_new("{}", &mut out).unwrap());
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn large_backlogs_are_split_into_batches() {
        let dir = temp("batches");
        let mut w = spool::Writer::open(&dir, 1 << 30, 60).unwrap();
        for i in 1..=(BATCH as u64 + 10) {
            w.append(i, "{}", "{}").unwrap();
        }
        let mut out = Vec::new();
        Tail::new(dir.clone(), 0).send_new("{}", &mut out).unwrap();
        let frames = frames(out);
        assert_eq!(frames.len(), 2);
        assert!(frames[0].starts_with(&format!("{{\"cursor\":{BATCH},")));
        fs::remove_dir_all(dir).unwrap();
    }
}
