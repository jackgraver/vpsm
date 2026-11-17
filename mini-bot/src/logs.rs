//! `mini-bot logs`: read this host's logs out of journald, for the laptop.
//!
//! The laptop runs `ssh host mini-bot logs [...]` and gets one JSON line per
//! log entry on stdout. Nothing is copied, stored or deleted on the VPS:
//! journald already keeps the logs, with its own disk cap and retention, and
//! every service lands there — Docker containers through its journald log
//! driver, systemd units (Caddy) natively. Unlike the metrics spool this is a
//! history, not a backlog, so the laptop keeps its place with journald's
//! cursor instead of acknowledging deletes.
//!
//! Reading the journal needs only the `systemd-journal` group, a read-only
//! permission. The agent still has no network code: SSH is the transport and
//! the auth, as for `export`.
//!
//! # Usage
//!
//! ```text
//! mini-bot logs [--after-cursor C | --since TIME] [--lines N]
//!               [--source NAME]... [--follow]
//! ```
//!
//! - `--after-cursor` resumes after the last entry already received: pass
//!   the `cursor` of the last line. This is how repeated pulls stay cheap.
//! - `--since` takes anything journalctl does (`-2h`, `2026-11-16 10:00`).
//! - With neither, the newest `--lines` entries (default 1000), so running
//!   it by hand never dumps the whole journal.
//! - `--source web` matches the compose service `web`, a container named
//!   `web`, the unit `web.service`, or the syslog identifier `web`. Repeat
//!   to OR several.
//! - `--follow` keeps streaming new entries until stdin or stdout closes.
//!
//! # The line format
//!
//! ```text
//! {"cursor":"s=…","t":1763287200123456,"src":"web","stream":"stderr","pri":3,"msg":"…"}
//! ```
//!
//! - `cursor`: journald's, opaque; hand it back as `--after-cursor`.
//! - `t`: microseconds since the Unix epoch, when journald received it.
//! - `src`: the compose service, else the container name, else the systemd
//!   unit without `.service`, else the syslog identifier, else the command
//!   name.
//! - `stream`: `stdout`/`stderr`, for containers only.
//! - `pri`: syslog priority 0 (emerg) .. 7 (debug), present only when the
//!   program chose it. Output journald captured from a stream gets a fixed
//!   priority by stream, not by content — Docker marks every stderr line 3
//!   ("error") — so for those it is left out rather than mislead, and the
//!   level comes from the text.
//! - `msg`: the message. Docker's 16 KiB partial lines are rejoined.
//!
//! The authoritative description is `logs/CLAUDE.md`; the reader is
//! `logs/viewer/internal/logline`.

use std::collections::HashMap;
use std::io::{self, BufRead, BufReader, BufWriter, Read, Write};
use std::process::{self, Command, Stdio};

use crate::wire;

/// The journal field Docker writes the `com.docker.compose.service` label to.
const COMPOSE_SERVICE: &str = "COM_DOCKER_COMPOSE_SERVICE";

/// Entries returned when no starting point is given.
const DEFAULT_LINES: u32 = 1000;

/// The journal fields read. `__CURSOR` and `__REALTIME_TIMESTAMP` are always
/// included by journalctl's export format.
const FIELDS: &[&str] = &[
    "MESSAGE",
    "PRIORITY",
    "_TRANSPORT",
    "CONTAINER_NAME",
    "CONTAINER_PARTIAL_MESSAGE",
    // Present when Docker's daemon.json lists the label in `log-opts.labels`
    // (deploy/docker-daemon.json does): the compose service, `backend`
    // rather than `simple-track-backend-1`.
    COMPOSE_SERVICE,
    "_SYSTEMD_UNIT",
    "SYSLOG_IDENTIFIER",
    "_COMM",
];

#[derive(Debug, Default, PartialEq)]
pub struct Args {
    pub after_cursor: Option<String>,
    pub since: Option<String>,
    pub lines: Option<u32>,
    pub sources: Vec<String>,
    pub follow: bool,
}

impl Args {
    /// Parse the arguments after `logs`.
    pub fn parse(args: &[String]) -> Result<Self, String> {
        let mut out = Args::default();
        let mut it = args.iter();
        while let Some(arg) = it.next() {
            let mut value = |name: &str| {
                it.next().cloned().ok_or_else(|| format!("{name} needs a value"))
            };
            match arg.as_str() {
                "--after-cursor" => out.after_cursor = Some(value(arg)?),
                "--since" => out.since = Some(value(arg)?),
                "--lines" => {
                    let raw = value(arg)?;
                    out.lines = Some(raw.parse().map_err(|_| format!("--lines {raw:?}: expected a count"))?);
                }
                "--source" => out.sources.push(value(arg)?),
                "--follow" => out.follow = true,
                other => return Err(format!("unknown argument {other:?}")),
            }
        }
        Ok(out)
    }

    /// The journalctl command line this asks for.
    fn journalctl_args(&self) -> Vec<String> {
        let mut a: Vec<String> = vec![
            "--output=export".into(),
            format!("--output-fields={}", FIELDS.join(",")),
            "--no-pager".into(),
            "--quiet".into(),
        ];
        if let Some(c) = &self.after_cursor {
            a.push(format!("--after-cursor={c}"));
        }
        if let Some(s) = &self.since {
            a.push(format!("--since={s}"));
        }
        if let Some(n) = self.lines.or((self.after_cursor.is_none() && self.since.is_none()).then_some(DEFAULT_LINES)) {
            a.push(format!("--lines={n}"));
        }
        if self.follow {
            a.push("--follow".into());
        }
        // Matches on different fields AND together in journalctl; `+` makes
        // each alternative its own OR group.
        for (i, name) in self.sources.iter().enumerate() {
            if i > 0 {
                a.push("+".into());
            }
            let base = name.strip_suffix(".service").unwrap_or(name);
            a.push(format!("{COMPOSE_SERVICE}={base}"));
            a.push("+".into());
            a.push(format!("CONTAINER_NAME={base}"));
            a.push("+".into());
            a.push(format!("_SYSTEMD_UNIT={base}.service"));
            a.push("+".into());
            a.push(format!("SYSLOG_IDENTIFIER={base}"));
        }
        a
    }
}

pub fn run(raw: &[String]) {
    let args = match Args::parse(raw) {
        Ok(args) => args,
        Err(error) => {
            eprintln!("mini-bot logs: {error}");
            eprintln!(
                "usage: mini-bot logs [--after-cursor C | --since TIME] [--lines N] [--source NAME]... [--follow]"
            );
            process::exit(2);
        }
    };
    let mut child = match Command::new("journalctl")
        .args(args.journalctl_args())
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
    {
        Ok(child) => child,
        Err(error) => {
            eprintln!("mini-bot logs: cannot run journalctl ({error}); is this a systemd host?");
            process::exit(1);
        }
    };
    let journal = BufReader::new(child.stdout.take().expect("piped"));
    let stdout = io::stdout();
    let result = convert(journal, BufWriter::new(stdout.lock()), args.follow);
    // On a closed stdout (the laptop went away) stop journalctl too, rather
    // than leaving a --follow running on the VPS.
    let _ = child.kill();
    let status = child.wait();
    match result {
        Err(error) if error.kind() != io::ErrorKind::BrokenPipe => {
            eprintln!("mini-bot logs: {error}");
            process::exit(1);
        }
        _ => {}
    }
    if let Ok(status) = status
        && !status.success()
        && status.code().is_some()
    {
        process::exit(status.code().unwrap_or(1));
    }
}

/// Read journalctl's export stream and write one line per entry. With
/// `flush_each`, every line is flushed as written, for a live follow.
pub fn convert(journal: impl BufRead, mut out: impl Write, flush_each: bool) -> io::Result<()> {
    let mut partial: HashMap<String, String> = HashMap::new();
    let mut reader = ExportReader { inner: journal };
    while let Some(fields) = reader.next_entry()? {
        if let Some(line) = frame(&fields, &mut partial) {
            out.write_all(line.as_bytes())?;
            out.write_all(b"\n")?;
            if flush_each {
                out.flush()?;
            }
        }
    }
    out.flush()
}

/// One journal entry's fields.
type Fields = HashMap<String, String>;

/// Reader for journalctl's export format: entries separated by a blank
/// line, each field `NAME=value`, or for values containing newlines or
/// binary data, `NAME` then a little-endian u64 length, the bytes, and a
/// newline.
struct ExportReader<R> {
    inner: R,
}

impl<R: BufRead> ExportReader<R> {
    fn next_entry(&mut self) -> io::Result<Option<Fields>> {
        let mut fields = Fields::new();
        let mut line = Vec::new();
        loop {
            line.clear();
            if self.inner.read_until(b'\n', &mut line)? == 0 {
                // End of stream: whatever was gathered is the last entry.
                return Ok((!fields.is_empty()).then_some(fields));
            }
            if line.last() == Some(&b'\n') {
                line.pop();
            }
            if line.is_empty() {
                if fields.is_empty() {
                    continue;
                }
                return Ok(Some(fields));
            }
            match line.iter().position(|&b| b == b'=') {
                Some(eq) => {
                    let name = String::from_utf8_lossy(&line[..eq]).into_owned();
                    let value = String::from_utf8_lossy(&line[eq + 1..]).into_owned();
                    fields.insert(name, value);
                }
                None => {
                    let name = String::from_utf8_lossy(&line).into_owned();
                    let mut len = [0u8; 8];
                    self.inner.read_exact(&mut len)?;
                    let len = u64::from_le_bytes(len);
                    let mut value = Vec::new();
                    (&mut self.inner).take(len).read_to_end(&mut value)?;
                    let mut newline = [0u8; 1];
                    self.inner.read_exact(&mut newline)?;
                    fields.insert(name, String::from_utf8_lossy(&value).into_owned());
                }
            }
        }
    }
}

/// Render one entry as an output line, or hold it if it is a Docker partial
/// line waiting for the rest.
fn frame(f: &Fields, partial: &mut HashMap<String, String>) -> Option<String> {
    let cursor = f.get("__CURSOR")?;
    let container = f.get("CONTAINER_NAME").filter(|s| !s.is_empty());
    let src = source(f);
    let mut msg = f.get("MESSAGE").cloned().unwrap_or_default();

    if container.is_some() {
        if f.get("CONTAINER_PARTIAL_MESSAGE").is_some_and(|v| v == "true") {
            partial.entry(src).or_default().push_str(&msg);
            return None;
        }
        if let Some(mut head) = partial.remove(&src) {
            head.push_str(&msg);
            msg = head;
        }
    }

    let priority: Option<u8> = f.get("PRIORITY").and_then(|p| p.parse().ok());
    let mut out = String::with_capacity(msg.len() + 160);
    out.push_str("{\"cursor\":");
    wire::string(&mut out, cursor);
    if let Some(t) = f.get("__REALTIME_TIMESTAMP").and_then(|t| t.parse::<u64>().ok()) {
        out.push_str(",\"t\":");
        out.push_str(&t.to_string());
    }
    out.push_str(",\"src\":");
    wire::string(&mut out, &src);
    if container.is_some() {
        // Docker's journald driver encodes the stream as the priority.
        let stream = if priority == Some(3) { "stderr" } else { "stdout" };
        out.push_str(",\"stream\":\"");
        out.push_str(stream);
        out.push('"');
    } else if let Some(p) = priority
        && f.get("_TRANSPORT").is_none_or(|t| t != "stdout")
    {
        out.push_str(",\"pri\":");
        out.push_str(&p.to_string());
    }
    out.push_str(",\"msg\":");
    wire::string(&mut out, &msg);
    out.push('}');
    Some(out)
}

/// The name an entry is filed under: what an operator would call it.
fn source(f: &Fields) -> String {
    let get = |k: &str| f.get(k).filter(|s| !s.is_empty());
    // Containers created before daemon.json listed the label lack it, and
    // fall back to their full name.
    if let Some(c) = get(COMPOSE_SERVICE).or_else(|| get("CONTAINER_NAME")) {
        return c.clone();
    }
    // Only a service unit names the program. A user session's unit
    // (user@1000.service) or a scope (init.scope, session-3.scope) holds
    // many; the identifier says which one logged.
    if let Some(u) = get("_SYSTEMD_UNIT")
        && let Some(name) = u.strip_suffix(".service")
        && !u.starts_with("user@")
    {
        return name.to_string();
    }
    get("SYSLOG_IDENTIFIER")
        .or_else(|| get("_COMM"))
        .cloned()
        .unwrap_or_else(|| "journal".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(list: &[&str]) -> Vec<String> {
        list.iter().map(|s| s.to_string()).collect()
    }

    fn convert_str(input: &[u8]) -> Vec<String> {
        let mut out = Vec::new();
        convert(input, &mut out, false).unwrap();
        String::from_utf8(out).unwrap().lines().map(str::to_string).collect()
    }

    #[test]
    fn parses_arguments() {
        let a = Args::parse(&args(&["--after-cursor", "s=1", "--source", "web", "--source", "caddy", "--follow"])).unwrap();
        assert_eq!(a.after_cursor.as_deref(), Some("s=1"));
        assert_eq!(a.sources, vec!["web", "caddy"]);
        assert!(a.follow);
        assert!(Args::parse(&args(&["--lines"])).is_err());
        assert!(Args::parse(&args(&["--lines", "many"])).is_err());
        assert!(Args::parse(&args(&["--bogus"])).is_err());
    }

    #[test]
    fn defaults_to_recent_lines_only_without_a_starting_point() {
        let plain = Args::default().journalctl_args();
        assert!(plain.contains(&"--lines=1000".to_string()));
        let resumed = Args { after_cursor: Some("s=1".into()), ..Args::default() }.journalctl_args();
        assert!(!resumed.iter().any(|a| a.starts_with("--lines")));
        assert!(resumed.contains(&"--after-cursor=s=1".to_string()));
    }

    #[test]
    fn sources_are_or_groups_across_fields() {
        let a = Args { sources: vec!["web".into(), "caddy.service".into()], ..Args::default() }.journalctl_args();
        let matches: Vec<&str> = a.iter().map(String::as_str).skip_while(|s| s.starts_with("--")).collect();
        assert_eq!(
            matches,
            vec![
                "COM_DOCKER_COMPOSE_SERVICE=web", "+", "CONTAINER_NAME=web", "+",
                "_SYSTEMD_UNIT=web.service", "+", "SYSLOG_IDENTIFIER=web", "+",
                "COM_DOCKER_COMPOSE_SERVICE=caddy", "+", "CONTAINER_NAME=caddy", "+",
                "_SYSTEMD_UNIT=caddy.service", "+", "SYSLOG_IDENTIFIER=caddy",
            ]
        );
    }

    #[test]
    fn container_lines_carry_stream_not_priority() {
        let input = b"__CURSOR=c1\n__REALTIME_TIMESTAMP=1763287200000000\nCONTAINER_NAME=web\nPRIORITY=3\n_TRANSPORT=journal\nMESSAGE=INFO listening\n\n";
        assert_eq!(
            convert_str(input),
            vec![r#"{"cursor":"c1","t":1763287200000000,"src":"web","stream":"stderr","msg":"INFO listening"}"#]
        );
    }

    #[test]
    fn priority_is_kept_only_when_the_program_chose_it() {
        let input = b"__CURSOR=a\n_SYSTEMD_UNIT=caddy.service\nPRIORITY=6\n_TRANSPORT=stdout\nMESSAGE=x\n\n\
__CURSOR=b\n_SYSTEMD_UNIT=sshd.service\nPRIORITY=4\n_TRANSPORT=syslog\nMESSAGE=y\n\n";
        assert_eq!(
            convert_str(input),
            vec![
                r#"{"cursor":"a","src":"caddy","msg":"x"}"#,
                r#"{"cursor":"b","src":"sshd","pri":4,"msg":"y"}"#,
            ]
        );
    }

    #[test]
    fn reads_binary_fields_with_newlines() {
        let mut input = b"__CURSOR=c\nSYSLOG_IDENTIFIER=app\nMESSAGE\n".to_vec();
        let msg = b"line one\nline \"two\"";
        input.extend_from_slice(&(msg.len() as u64).to_le_bytes());
        input.extend_from_slice(msg);
        input.extend_from_slice(b"\n\n");
        assert_eq!(convert_str(&input), vec![r#"{"cursor":"c","src":"app","msg":"line one\nline \"two\""}"#]);
    }

    #[test]
    fn rejoins_docker_partial_lines() {
        let input = b"__CURSOR=1\nCONTAINER_NAME=web\nCONTAINER_PARTIAL_MESSAGE=true\nPRIORITY=6\nMESSAGE=first \n\n\
__CURSOR=2\nCONTAINER_NAME=api\nPRIORITY=6\nMESSAGE=other\n\n\
__CURSOR=3\nCONTAINER_NAME=web\nPRIORITY=6\nMESSAGE=second\n\n";
        assert_eq!(
            convert_str(input),
            vec![
                r#"{"cursor":"2","src":"api","stream":"stdout","msg":"other"}"#,
                r#"{"cursor":"3","src":"web","stream":"stdout","msg":"first second"}"#,
            ]
        );
    }

    #[test]
    fn names_the_source_the_way_an_operator_would() {
        let f = |pairs: &[(&str, &str)]| pairs.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect::<Fields>();
        assert_eq!(source(&f(&[("CONTAINER_NAME", "web"), ("_SYSTEMD_UNIT", "docker.service")])), "web");
        assert_eq!(
            source(&f(&[("CONTAINER_NAME", "simple-track-backend-1"), ("COM_DOCKER_COMPOSE_SERVICE", "backend")])),
            "backend"
        );
        assert_eq!(source(&f(&[("_SYSTEMD_UNIT", "caddy.service"), ("SYSLOG_IDENTIFIER", "caddy")])), "caddy");
        assert_eq!(source(&f(&[("_SYSTEMD_UNIT", "user@1000.service"), ("SYSLOG_IDENTIFIER", "discord")])), "discord");
        assert_eq!(source(&f(&[("_SYSTEMD_UNIT", "init.scope"), ("SYSLOG_IDENTIFIER", "systemd")])), "systemd");
        assert_eq!(source(&f(&[("_COMM", "kthreadd")])), "kthreadd");
        assert_eq!(source(&f(&[])), "journal");
    }

    #[test]
    fn last_entry_without_trailing_blank_line_is_kept() {
        assert_eq!(convert_str(b"__CURSOR=z\nMESSAGE=end"), vec![r#"{"cursor":"z","src":"journal","msg":"end"}"#]);
    }
}
