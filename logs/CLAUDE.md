# VPSM logs

The log half of VPSM: reading the logs services on a VPS produce, and
finding what matters in them.

## Status

`viewer/` is a Go program, `vpsm-logs`, that reads log files or stdin and
shows them in a Bubble Tea TUI with live search. On the VPS,
`mini-bot logs` reads the journal and emits one JSON line per entry, so the
two already work together over SSH:

```
ssh host mini-bot logs --since -1h | vpsm-logs
ssh host mini-bot logs --source web --source caddy | vpsm-logs -level warn
```

Not built yet: `vpsm-logs pull <host>` keeping a local cache and the last
cursor, so repeated pulls fetch only what is new (see Next).

## On the VPS: journald is the log store

Decided: logs stay where the host already keeps them, in journald, and
mini-bot reads them in place. The metrics spool is a backlog that is
deleted once collected; logs are a history you search later and are far
larger, so copying them into a mini-bot spool would store everything twice
and compete with the workload for disk I/O.

- **Docker uses the journald log driver** (`mini-bot/deploy/docker-daemon.json`,
  `tag` set to the container name). Everything on the target VPS except
  Caddy is Docker; Caddy is a systemd unit and logs to the journal
  natively. `docker logs` keeps working with this driver.
- **journald is capped at 2G and 30 days, persistent, with no rate limit**
  (`mini-bot/deploy/journald-vpsm.conf`). The VPS has 25 GB.
- **The pull user is in `systemd-journal`**, which grants reading the
  journal and nothing else. Reading Docker's json-file logs directly would
  have needed root or the `docker` group, which is root in all but name.
- `install.sh` sets up the group and journald. It does not switch Docker's
  driver: that restarts every container, so it prints the steps instead.

Why journald over reading Docker's json files: a resumable cursor
(`--after-cursor`) instead of tracking inode + offset across rotations; one
disk cap for the host; time and per-service filtering done on the VPS; and
non-Docker services covered by the same path.

## Line format (`mini-bot logs` → viewer)

One JSON object per line. Written by `../mini-bot/src/logs.rs`, read by
`viewer/internal/logline` (`unwrapJournal`); change both together.

```
{"cursor":"s=…","t":1763287200123456,"src":"web","stream":"stderr","pri":3,"msg":"…"}
```

| field | meaning |
|---|---|
| `cursor` | journald's, opaque. Pass it back as `--after-cursor` to resume. |
| `t` | microseconds since the epoch, when journald received it |
| `src` | container name, else systemd service without `.service`, else syslog identifier, else command |
| `stream` | `stdout`/`stderr`, containers only |
| `pri` | syslog priority 0–7, **only when the program chose it** |
| `msg` | the message; Docker's 16 KiB partial lines rejoined |

`pri` is left out for output journald captured from a stream, because that
priority is set by stream, not content: Docker marks every stderr line 3
("error"), and a unit's stdout gets 6 regardless. Many apps log routine
output to stderr. So the viewer takes the level from the text, and uses
`pri` only when the text gives none.

In the viewer, each `src` becomes its own source (label, header count,
`-print` prefix), and continuation lines only continue their own
service's entries, since a pulled journal interleaves services.

## Using the viewer

```
vpsm-logs [-q search] [-level warn] [-print] [path ...]
```

- Paths are files (plain or gzip, detected by content), or directories,
  whose regular files are read with rotations oldest first
  (`app.log.2.gz`, `app.log.1`, `app.log`). `-` or no path reads stdin, in
  which case keys are read from `/dev/tty`.
- Several sources are interleaved into one timeline by timestamp.
- When stdout is not a terminal (or with `-print`) the matching lines are
  printed instead, so the same flags work in a pipeline.

Keys: `/` search (live, as you type), `l`/`L` step the minimum level,
`esc` removes the search and then the level, `enter` shows one line in full
(JSON pretty-printed), `g`/`G` top/bottom, `q` quit.

## Layout

`viewer/` follows the collector's shape: `main.go` is a doc comment and
`startup.Run()`; nothing else goes there.

- `internal/startup` — flags, stdin-vs-files, viewer-vs-print.
- `internal/load` — opening, gunzipping, directory expansion, line reading,
  interleaving several sources.
- `internal/logline` — per-line level and timestamp detection. Stateful per
  source (Docker partial lines, continuation lines), so one `Parser` per
  source.
- `internal/filter` — the search and level filter, and match spans for
  highlighting.
- `internal/tui` — the viewer. Tests drive it through `Update` with key
  messages and read `View()`, as in j-cicd.

## Decisions worth not re-litigating

- **No declared format.** Logs are whatever the services print, so every
  line is kept as text and parsing only *adds* a level and a time when it
  can recognise them. A wrong guess costs a mis-coloured line; a line
  nobody recognised still shows up and is still searchable. Recognised:
  Docker's json-file wrapper, JSON (zap, zerolog, pino numeric levels, slog,
  logrus, ECS, journald `-o json`), logfmt, plain-text level words and
  ISO/syslog/CLF timestamps, and HTTP access logs by status (5xx error, 4xx
  warn).
- **Docker's json-file format is unwrapped** too, for logs copied off a
  host that still uses the default driver. Its 16 KiB partial lines are
  rejoined, and its timestamp is put in front of the text as `docker logs
  -t` does — as is journald's for pulled lines.
- **Plain-text level words must be uppercase or bracketed, and near the
  start** (first 120 bytes). "retrying after error" is message text, not
  the line's level; "ERROR" or "[error]" at the front is.
- **Continuation lines inherit level and time** from the entry above, so a
  level filter keeps a stack trace with its error. In a log whose lines
  carry timestamps, any line without one is a continuation; otherwise only
  indented lines and stack-frame shapes are. This is judged on the
  application's text, before Docker's own stamp, which every line has.
- **A level filter hides lines with no level.** "Warnings and up" should not
  be padded with unclassified noise; `esc` brings them back.
- **Search is a smart-case substring**, as in ripgrep and vim: lowercase
  ignores case, any uppercase makes it exact. Not a regex, on purpose, for
  v1.
- **Everything is loaded into memory.** Fine for the logs one VPS keeps;
  lines over 1 MiB are cut, not dropped.

## Next

- `vpsm-logs pull <host>`: run `ssh host mini-bot logs --after-cursor C`,
  append to `~/.local/share/vpsm/logs/<host>/YYYY-MM-DD.jsonl.gz`, store the
  last cursor; first pull uses `--since`. `vpsm-logs host <host>` opens the
  cache. The cache files are the line format above, so the viewer already
  reads them.
- Remote search for what is older than the cache (`journalctl --grep` via
  `mini-bot logs`), live follow in the TUI, regex search.
