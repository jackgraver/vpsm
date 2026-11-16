# VPSM logs

The log half of VPSM: reading the logs services on a VPS produce, and
finding what matters in them.

## Status

Local only, for now. `viewer/` is a Go program, `vpsm-logs`, that reads log
files on this machine (or stdin) and shows them in a Bubble Tea TUI with
live search. Getting logs off each VPS is the next step, and is meant to
go through `../mini-bot/` the way resource samples do (see below).

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
- **Docker's json-file format is unwrapped**, because that is where a
  container's stdout ends up on the VPS
  (`/var/lib/docker/containers/<id>/<id>-json.log`). Its 16 KiB partial
  lines are rejoined, and its timestamp is put in front of the text as
  `docker logs -t` does.
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

- Pull from a VPS: a `mini-bot` subcommand that reads the host's log files
  (or the Docker log directory) and streams them over the same `ssh <host>
  mini-bot ...` channel the collector uses, so the agent still needs no
  network code.
- Follow mode (`tail -f`), and regex search.
