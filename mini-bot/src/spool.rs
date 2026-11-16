//! The spool: samples the collector has not collected yet, kept on disk.
//!
//! The collector is not always running, so it pulls rather than the agent
//! pushing (see `export`). Between pulls every sample lands here, and the
//! collector acknowledges what it has stored so this end can delete it. The
//! spool is therefore a backlog, not a history: with a collector pulling
//! regularly it holds minutes of data, and it only grows while nobody pulls.
//!
//! # Layout
//!
//! ```text
//! <dir>/host.json                 the host block, written at startup
//! <dir>/latest                    "<seq>\t<sample with processes>"
//! <dir>/<seq>.seg                 "<seq>\t<at_ms>\t<sample>" per line
//! <dir>/<seq>.thin.seg            a segment that has been thinned
//! <dir>/cursor                    highest seq the collector has stored
//! <dir>/.lock                     flock serialising deletes and rewrites
//! ```
//!
//! Every line has a sequence number that only ever increases, and that is
//! what acknowledgements refer to. Not the timestamp: a clock stepped
//! backwards would otherwise make an ack swallow samples never delivered.
//!
//! Segment files are named by their first seq, so a segment is wholly
//! acknowledged once the *next* segment starts at or before `cursor + 1`.
//! The newest segment is never deleted by an ack, because the daemon may
//! still be appending to it; it goes once a newer one exists.
//!
//! # Staying small
//!
//! Spooled samples carry no process list. The collector keeps only the
//! newest process snapshot per host, so storing one per sample would be most
//! of the spool's size spent on data that is discarded on arrival. The latest
//! full sample is kept in `latest` instead, and attached on export.
//!
//! When the backlog passes half of its cap, the oldest segments are thinned
//! to one sample per `thin_secs`. That loses less than it sounds: the wire
//! carries counters, so the collector's rate over a thinned window is still
//! the exact average across it — what goes is the detail inside the window.
//! Only past the full cap are whole segments dropped, oldest first.
//!
//! # Sharing
//!
//! The daemon usually runs as root (per-process I/O needs it) and `export`
//! runs as whoever the collector SSHes in as. The install puts the directory
//! in a shared group with the setgid bit, and both ends run with umask 007,
//! so everything here is readable and replaceable by that group.

use std::fs::{self, File, OpenOptions};
use std::io::{self, BufRead, BufReader, Read, Seek, SeekFrom, Write};
use std::os::fd::AsRawFd;
use std::path::{Path, PathBuf};

/// Rotate to a new segment past this size. Small enough that acks free
/// space promptly and thinning works in modest steps; large enough that a
/// day's backlog is tens of files, not thousands.
const SEGMENT_BYTES: u64 = 1024 * 1024;

pub const HOST_FILE: &str = "host.json";
const LATEST_FILE: &str = "latest";
const CURSOR_FILE: &str = "cursor";
const LOCK_FILE: &str = ".lock";
const SEGMENT_EXT: &str = ".seg";
const THIN_MARK: &str = ".thin";

/// One segment file on disk.
#[derive(Clone, Debug, PartialEq)]
pub struct Segment {
    pub first_seq: u64,
    pub path: PathBuf,
    pub thinned: bool,
    pub bytes: u64,
}

/// One spooled sample.
#[derive(Clone, Debug, PartialEq)]
pub struct Line {
    pub seq: u64,
    pub at_ms: u64,
    pub json: String,
}

/// The writing end, owned by the daemon.
pub struct Writer {
    dir: PathBuf,
    max_bytes: u64,
    thin_ms: u64,
    next_seq: u64,
    active: Option<(File, u64)>,
}

impl Writer {
    /// Open (creating if needed) the spool and work out where numbering
    /// resumes. Always starts a new segment, so a line half-written when the
    /// previous run died is left at the end of a sealed file, where readers
    /// skip it, instead of being glued to the next one.
    pub fn open(dir: &Path, max_bytes: u64, thin_secs: u64) -> io::Result<Self> {
        fs::create_dir_all(dir)?;
        let mut last = read_cursor(dir)?;
        if let Some(newest) = segments(dir)?.last() {
            for line in read_lines(&newest.path, 0)?.0 {
                last = last.max(line.seq);
            }
            // A segment's name is its first seq even if every line in it
            // turned out to be unreadable.
            last = last.max(newest.first_seq);
        }
        if let Ok(latest) = read_latest(dir) {
            last = last.max(latest.0);
        }
        Ok(Self {
            dir: dir.to_path_buf(),
            max_bytes,
            thin_ms: thin_secs.max(1) * 1000,
            next_seq: last + 1,
            active: None,
        })
    }

    /// Record the host block that `export` sends with every report.
    pub fn write_host(&self, host_json: &str) -> io::Result<()> {
        replace(&self.dir.join(HOST_FILE), host_json.as_bytes())
    }

    /// Append one sample. `full_json` is the same sample with its process
    /// list, which replaces `latest`.
    pub fn append(&mut self, at_ms: u64, json: &str, full_json: &str) -> io::Result<()> {
        let seq = self.next_seq;

        // `latest` first: once the line is visible a follower may look for
        // its process list, and it should already be there.
        replace(&self.dir.join(LATEST_FILE), format!("{seq}\t{full_json}").as_bytes())?;

        let rotate = match &self.active {
            None => true,
            Some((_, size)) => *size >= SEGMENT_BYTES,
        };
        if rotate {
            let path = self.dir.join(format!("{seq:020}{SEGMENT_EXT}"));
            let file = OpenOptions::new().append(true).create_new(true).open(path)?;
            self.active = Some((file, 0));
            // The previous segment just sealed: the moment to check the cap.
            if let Err(error) = self.maintain() {
                eprintln!("mini-bot: spool maintenance failed: {error}");
            }
        }

        let line = format!("{seq}\t{at_ms}\t{json}\n");
        let (file, size) = self.active.as_mut().expect("rotated above");
        // One write per line, so a reader sees either all of it or none.
        if let Err(error) = file.write_all(line.as_bytes()) {
            // Whatever was half written stays at the end of this segment;
            // start a fresh one rather than appending after it.
            self.active = None;
            return Err(error);
        }
        *size += line.len() as u64;
        self.next_seq += 1;
        Ok(())
    }

    /// Thin, then drop, the oldest sealed segments until the spool is within
    /// its cap. Never touches the active segment.
    fn maintain(&mut self) -> io::Result<()> {
        let _lock = Lock::acquire(&self.dir)?;
        let mut all = segments(&self.dir)?;
        // The newest is the one just opened for writing.
        all.pop();
        let mut total: u64 = all.iter().map(|s| s.bytes).sum();

        let soft = self.max_bytes / 2;
        let mut thinned = 0;
        for seg in all.iter_mut() {
            if total <= soft {
                break;
            }
            if seg.thinned {
                continue;
            }
            let before = seg.bytes;
            *seg = thin(seg, self.thin_ms)?;
            total = total - before + seg.bytes;
            thinned += 1;
        }
        if thinned > 0 {
            eprintln!(
                "mini-bot: spool past {} MiB uncollected; thinned {thinned} oldest segment(s) to one sample per {}s",
                soft / (1024 * 1024),
                self.thin_ms / 1000,
            );
        }

        let mut dropped = 0;
        for seg in &all {
            if total <= self.max_bytes {
                break;
            }
            remove(&seg.path)?;
            total -= seg.bytes;
            dropped += 1;
        }
        if dropped > 0 {
            eprintln!(
                "mini-bot: spool full ({} MiB cap); dropped {dropped} oldest segment(s) nobody collected",
                self.max_bytes / (1024 * 1024),
            );
        }
        Ok(())
    }
}

/// Rewrite a segment keeping the first sample in each `thin_ms` bucket.
fn thin(seg: &Segment, thin_ms: u64) -> io::Result<Segment> {
    let (lines, _) = read_lines(&seg.path, 0)?;
    let mut kept = String::new();
    let mut bucket = None;
    for line in lines {
        let b = line.at_ms / thin_ms;
        if bucket == Some(b) {
            continue;
        }
        bucket = Some(b);
        kept.push_str(&format!("{}\t{}\t{}\n", line.seq, line.at_ms, line.json));
    }

    let path = seg.path.with_file_name(format!("{:020}{THIN_MARK}{SEGMENT_EXT}", seg.first_seq));
    replace(&path, kept.as_bytes())?;
    remove(&seg.path)?;
    Ok(Segment {
        first_seq: seg.first_seq,
        path,
        thinned: true,
        bytes: kept.len() as u64,
    })
}

/// Record that the collector has stored everything up to `seq`, and delete
/// the segments that leaves fully collected.
pub fn ack(dir: &Path, seq: u64) -> io::Result<()> {
    let _lock = Lock::acquire(dir)?;
    // Monotonic: a late or repeated ack never un-acknowledges anything.
    let cursor = read_cursor(dir)?.max(seq);
    replace(&dir.join(CURSOR_FILE), cursor.to_string().as_bytes())?;

    let all = segments(dir)?;
    for pair in all.windows(2) {
        // Every line in pair[0] is below pair[1].first_seq.
        if pair[1].first_seq <= cursor + 1 {
            remove(&pair[0].path)?;
        }
    }
    Ok(())
}

/// The highest seq the collector has acknowledged, or 0.
pub fn read_cursor(dir: &Path) -> io::Result<u64> {
    match fs::read_to_string(dir.join(CURSOR_FILE)) {
        Ok(text) => Ok(text.trim().parse().unwrap_or(0)),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(0),
        Err(error) => Err(error),
    }
}

/// The newest sample with its process list: `(seq, json)`.
pub fn read_latest(dir: &Path) -> io::Result<(u64, String)> {
    let text = fs::read_to_string(dir.join(LATEST_FILE))?;
    let (seq, json) = text
        .split_once('\t')
        .and_then(|(seq, json)| Some((seq.parse().ok()?, json.trim_end().to_string())))
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "latest is malformed"))?;
    Ok((seq, json))
}

/// Segments on disk, oldest first.
pub fn segments(dir: &Path) -> io::Result<Vec<Segment>> {
    let mut out = Vec::new();
    for entry in fs::read_dir(dir)? {
        let entry = entry?;
        let name = entry.file_name();
        let Some(name) = name.to_str() else { continue };
        let Some(stem) = name.strip_suffix(SEGMENT_EXT) else { continue };
        let (digits, thinned) = match stem.strip_suffix(THIN_MARK) {
            Some(digits) => (digits, true),
            None => (stem, false),
        };
        let Ok(first_seq) = digits.parse() else { continue };
        // Raced with a delete: it is gone, which is fine.
        let Ok(meta) = entry.metadata() else { continue };
        out.push(Segment { first_seq, path: entry.path(), thinned, bytes: meta.len() });
    }
    out.sort_by_key(|s| (s.first_seq, !s.thinned));
    // Mid-thin, a segment briefly exists in both forms. The thinned one is
    // complete (it is renamed into place), so prefer it.
    out.dedup_by_key(|s| s.first_seq);
    Ok(out)
}

/// Read complete lines from `path` starting at byte `offset`. Returns them
/// with the offset just past the last complete line, so a caller tailing the
/// file can resume there. A trailing line with no newline is still being
/// written (or was cut off by a crash) and is left for later.
pub fn read_lines(path: &Path, offset: u64) -> io::Result<(Vec<Line>, u64)> {
    let mut file = File::open(path)?;
    file.seek(SeekFrom::Start(offset))?;
    let mut reader = BufReader::new(file);
    let mut lines = Vec::new();
    let mut pos = offset;
    let mut buf = String::new();
    loop {
        buf.clear();
        let n = match reader.read_line(&mut buf) {
            Ok(n) => n,
            // Not UTF-8: a torn write. Skip to the end of what is readable.
            Err(error) if error.kind() == io::ErrorKind::InvalidData => break,
            Err(error) => return Err(error),
        };
        if n == 0 || !buf.ends_with('\n') {
            break;
        }
        pos += n as u64;
        if let Some(line) = parse_line(&buf) {
            lines.push(line);
        }
    }
    Ok((lines, pos))
}

fn parse_line(text: &str) -> Option<Line> {
    let mut parts = text.trim_end_matches('\n').splitn(3, '\t');
    let seq = parts.next()?.parse().ok()?;
    let at_ms = parts.next()?.parse().ok()?;
    let json = parts.next()?;
    // Cheap sanity check against a line torn mid-write and then followed by
    // a newline from somewhere else; the collector validates properly.
    if !(json.starts_with('{') && json.ends_with('}')) {
        return None;
    }
    Some(Line { seq, at_ms, json: json.to_string() })
}

/// Read a small text file whole.
pub fn read_file(path: &Path) -> io::Result<String> {
    let mut text = String::new();
    File::open(path)?.read_to_string(&mut text)?;
    Ok(text)
}

/// Atomically replace `path`: readers see the old contents or the new, never
/// a partial write.
fn replace(path: &Path, contents: &[u8]) -> io::Result<()> {
    let mut tmp = path.as_os_str().to_owned();
    tmp.push(format!(".tmp{}", std::process::id()));
    let tmp = PathBuf::from(tmp);
    fs::write(&tmp, contents)?;
    fs::rename(&tmp, path)
}

/// Delete, treating "already gone" as done.
fn remove(path: &Path) -> io::Result<()> {
    match fs::remove_file(path) {
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        other => other,
    }
}

/// An exclusive `flock` on the spool, held for the life of the value.
///
/// Appending needs no lock. Deleting and rewriting do: without it, an ack
/// deleting a segment while the daemon thins it would have the thinned copy
/// renamed back into existence.
struct Lock {
    _file: File,
}

impl Lock {
    fn acquire(dir: &Path) -> io::Result<Self> {
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(dir.join(LOCK_FILE))
            // Another user created it and we may only read it; that is
            // still enough to lock.
            .or_else(|_| File::open(dir.join(LOCK_FILE)))?;
        // SAFETY: flock on a descriptor we own; it is released when the
        // file is closed, which is when `Lock` drops.
        if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX) } != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(Self { _file: file })
    }
}

/// Make files this process creates group-readable and -writable, and closed
/// to everyone else. See "Sharing" above.
pub fn set_shared_umask() {
    // SAFETY: umask has no failure mode.
    unsafe {
        libc::umask(0o007);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct TempDir(PathBuf);

    impl TempDir {
        fn new(name: &str) -> Self {
            let path = std::env::temp_dir()
                .join(format!("mini-bot-spool-{name}-{}", std::process::id()));
            let _ = fs::remove_dir_all(&path);
            fs::create_dir_all(&path).unwrap();
            Self(path)
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn all_lines(dir: &Path) -> Vec<Line> {
        segments(dir)
            .unwrap()
            .iter()
            .flat_map(|s| read_lines(&s.path, 0).unwrap().0)
            .collect()
    }

    fn sample(i: u64) -> String {
        format!("{{\"n\":{i}}}")
    }

    #[test]
    fn appends_numbered_lines_and_keeps_latest() {
        let tmp = TempDir::new("append");
        let mut w = Writer::open(&tmp.0, 1 << 30, 60).unwrap();
        for i in 0..3 {
            w.append(1000 * i, &sample(i), "{\"full\":true}").unwrap();
        }
        let lines = all_lines(&tmp.0);
        assert_eq!(lines.iter().map(|l| l.seq).collect::<Vec<_>>(), [1, 2, 3]);
        assert_eq!(lines[2].json, "{\"n\":2}");
        assert_eq!(read_latest(&tmp.0).unwrap(), (3, "{\"full\":true}".into()));
    }

    #[test]
    fn numbering_resumes_after_restart_and_after_everything_was_acked() {
        let tmp = TempDir::new("resume");
        let mut w = Writer::open(&tmp.0, 1 << 30, 60).unwrap();
        w.append(0, &sample(0), "{}").unwrap();
        w.append(1, &sample(1), "{}").unwrap();
        drop(w);

        let mut w = Writer::open(&tmp.0, 1 << 30, 60).unwrap();
        w.append(2, &sample(2), "{}").unwrap();
        assert_eq!(all_lines(&tmp.0).last().unwrap().seq, 3);
        assert_eq!(segments(&tmp.0).unwrap().len(), 2, "a restart starts a new segment");

        // Everything acked and deleted, latest gone too: the cursor alone
        // must stop seqs being reused, or the next export would skip them.
        ack(&tmp.0, 3).unwrap();
        for seg in segments(&tmp.0).unwrap() {
            fs::remove_file(seg.path).unwrap();
        }
        fs::remove_file(tmp.0.join(LATEST_FILE)).unwrap();
        let mut w = Writer::open(&tmp.0, 1 << 30, 60).unwrap();
        w.append(3, &sample(3), "{}").unwrap();
        assert_eq!(all_lines(&tmp.0)[0].seq, 4);
    }

    #[test]
    fn readers_skip_a_torn_trailing_line_and_resume_from_offset() {
        let tmp = TempDir::new("torn");
        let path = tmp.0.join("00000000000000000001.seg");
        fs::write(&path, "1\t10\t{\"a\":1}\n2\t20\t{\"b\":").unwrap();
        let (lines, offset) = read_lines(&path, 0).unwrap();
        assert_eq!(lines.len(), 1);

        // The writer finishes the line; a tail picks up exactly that.
        let mut f = OpenOptions::new().append(true).open(&path).unwrap();
        f.write_all(b"2}\n").unwrap();
        let (lines, _) = read_lines(&path, offset).unwrap();
        assert_eq!(lines, [Line { seq: 2, at_ms: 20, json: "{\"b\":2}".into() }]);
    }

    #[test]
    fn ack_deletes_only_segments_wholly_collected_and_never_the_newest() {
        let tmp = TempDir::new("ack");
        for (first, last) in [(1, 3), (4, 6), (7, 9)] {
            let text: String = (first..=last).map(|s| format!("{s}\t{s}\t{{}}\n")).collect();
            fs::write(tmp.0.join(format!("{first:020}.seg")), text).unwrap();
        }

        ack(&tmp.0, 5).unwrap();
        let firsts: Vec<_> = segments(&tmp.0).unwrap().iter().map(|s| s.first_seq).collect();
        assert_eq!(firsts, [4, 7], "seg 4..6 still holds an uncollected 6");

        ack(&tmp.0, 9).unwrap();
        let firsts: Vec<_> = segments(&tmp.0).unwrap().iter().map(|s| s.first_seq).collect();
        assert_eq!(firsts, [7], "the newest may still be appended to");

        ack(&tmp.0, 2).unwrap();
        assert_eq!(read_cursor(&tmp.0).unwrap(), 9, "acks never move backwards");
    }

    #[test]
    fn past_half_the_cap_old_segments_are_thinned_then_past_the_cap_dropped() {
        let tmp = TempDir::new("thin");
        let filler = "x".repeat(200);
        // One sample a second for ~1.2 segments' worth per segment written.
        let mut w = Writer::open(&tmp.0, 3 * SEGMENT_BYTES, 60).unwrap();
        let per_line = format!("{}\t{}\t{{\"f\":\"{filler}\"}}\n", 100_000, 100_000_000).len() as u64;
        let lines_per_segment = SEGMENT_BYTES / per_line + 1;
        let json = format!("{{\"f\":\"{filler}\"}}");
        for i in 0..lines_per_segment * 3 {
            w.append(100_000_000 + i * 1000, &json, "{}").unwrap();
        }

        let segs = segments(&tmp.0).unwrap();
        assert!(segs[0].thinned, "{segs:?}");
        let thinned = read_lines(&segs[0].path, 0).unwrap().0;
        for pair in thinned.windows(2) {
            assert_ne!(pair[0].at_ms / 60_000, pair[1].at_ms / 60_000, "one per minute");
        }
        assert!(
            segs.iter().map(|s| s.bytes).sum::<u64>() <= 3 * SEGMENT_BYTES + SEGMENT_BYTES,
            "within the cap, give or take the segment being written"
        );
    }

    #[test]
    fn hard_cap_drops_oldest_segments() {
        let tmp = TempDir::new("drop");
        let mut w = Writer::open(&tmp.0, SEGMENT_BYTES, 1).unwrap();
        // Every sample in its own one-second bucket: thinning cannot help.
        let json = format!("{{\"f\":\"{}\"}}", "x".repeat(500));
        for i in 0..(SEGMENT_BYTES / 500) * 4 {
            w.append(i * 1000, &json, "{}").unwrap();
        }
        let segs = segments(&tmp.0).unwrap();
        let sealed: u64 = segs[..segs.len() - 1].iter().map(|s| s.bytes).sum();
        assert!(sealed <= SEGMENT_BYTES, "sealed {sealed}");
        assert!(segs[0].first_seq > 1, "the oldest went first");
    }
}
