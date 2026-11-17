// Package logline turns raw log lines of unknown format into entries with a
// best-guess level and timestamp.
//
// Nothing here requires a format to be declared. Every line is kept and
// displayed as text; the parser only adds what it can recognise:
//
//   - Lines from `mini-bot logs` (a VPS's journal) are unwrapped to the
//     message, filed under the service that wrote it, and stamped with
//     journald's time; a syslog priority the program set is used when the
//     text gives no level.
//   - Docker's json-file wrapper ({"log":..,"stream":..,"time":..}) is
//     unwrapped, partial lines it split at 16 KiB are rejoined, and its
//     timestamp is shown in front of the text, as `docker logs -t` does.
//   - JSON lines (zap, zerolog, pino, slog, logrus, journald -o json) give
//     their level and time from the usual keys.
//   - logfmt (level=warn ts=...) and plain text (2026-11-16 10:00:00 ERROR)
//     are matched by pattern near the start of the line.
//   - HTTP access logs grade by status: 5xx as error, 4xx as warn.
//   - Continuation lines inherit the level and time of the entry above, so
//     a level filter keeps a stack trace with the error that produced it.
//     In a log whose lines carry timestamps, any line without its own is a
//     continuation; otherwise only indented lines, stack frames and
//     "Caused by:" are.
//
// A wrong guess costs a mis-coloured line; a missing guess leaves
// LevelUnknown. The text itself is never altered beyond removing terminal
// escape sequences.
package logline

import (
	"encoding/json"
	"strings"
	"time"
)

// Entry is one log line as displayed.
type Entry struct {
	Source int // index into the loaded set's source names
	Line   int // 1-based line number within its source

	// Text is the line as shown and searched: the application's own output,
	// with terminal escapes and control characters removed.
	Text  string
	Level Level
	// Time is zero when no timestamp was recognised.
	Time time.Time
	// Stream is "stdout" or "stderr" when the line came through Docker.
	Stream string
	// Origin is the service a line names itself as coming from, when one
	// stream carries several (a journal pulled from a VPS). Empty for a
	// plain file, whose name is the source.
	Origin string
	// Continuation marks a line whose level and time were inherited from
	// the line before it.
	Continuation bool
	// Noise marks a health check or local probe: a request to /health and
	// friends, or a GET from the container's own loopback. Hidden unless
	// asked for, since a service probed every 10s drowns its real traffic.
	Noise bool
}

// Parser parses the lines of one source in order. It is stateful: Docker
// partial lines are joined across calls, and continuation lines inherit
// from their predecessor. Use one Parser per source.
type Parser struct {
	Source int
	// Now anchors timestamps that omit the year (syslog's "Nov 16 10:00:00").
	Now time.Time

	line int
	// Continuation state per origin: lines pulled from a VPS journal
	// interleave services, and a line only continues its own service's.
	chains  map[string]*chain
	partial strings.Builder
	partOK  bool
}

type chain struct {
	prev Entry
	// ownTime is whether the entry before carried (or inherited) a
	// timestamp in its own text. In a log whose lines are timestamped, a
	// line without one is the tail of the entry above it.
	ownTime bool
}

// wrapper is what an envelope around the application's text said: Docker's
// json-file format, or a line pulled from a VPS by `mini-bot logs`.
type wrapper struct {
	stamp    time.Time
	stream   string
	origin   string
	priority Level // LevelUnknown unless the program set one
}

// Next parses one raw line (without its newline). It returns false when the
// line produced no entry yet: a Docker partial line waiting for the rest.
func (p *Parser) Next(raw string) (Entry, bool) {
	p.line++
	text, w, ok := p.unwrapJournal(raw)
	if !ok {
		var pending bool
		text, w, ok, pending = p.unwrapDocker(raw)
		if pending {
			return Entry{}, false // held until the final piece arrives
		}
	}

	text = Clean(text)
	e := Entry{Source: p.Source, Line: p.line, Text: text, Stream: w.stream, Origin: w.origin, Noise: IsProbe(text)}
	e.Level, e.Time = p.detect(text)
	ownTime := !e.Time.IsZero()

	if p.chains == nil {
		p.chains = map[string]*chain{}
	}
	c, seen := p.chains[e.Origin]
	// Judged on the application's own text, before the envelope's timestamp
	// is added: Docker and journald stamp every line, continuation or not.
	if e.Level == LevelUnknown && !ownTime && seen && (c.ownTime || isContinuation(text)) {
		e.Level, e.Time, e.Continuation = c.prev.Level, c.prev.Time, true
	}
	if e.Level == LevelUnknown {
		e.Level = w.priority
	}
	if ok && !w.stamp.IsZero() {
		if !ownTime {
			e.Time = w.stamp
		}
		e.Text = w.stamp.UTC().Format("2006-01-02T15:04:05.000Z") + " " + text
	}
	if !seen {
		c = &chain{}
		p.chains[e.Origin] = c
	}
	c.ownTime = ownTime || (e.Continuation && c.ownTime)
	c.prev = e
	return e, true
}

// journalLine is what `mini-bot logs` emits; see mini-bot/src/logs.rs.
type journalLine struct {
	Cursor *string `json:"cursor"`
	T      int64   `json:"t"` // microseconds since the epoch
	Src    string  `json:"src"`
	Stream string  `json:"stream"`
	Pri    *int    `json:"pri"`
	Msg    string  `json:"msg"`
}

func (p *Parser) unwrapJournal(raw string) (string, wrapper, bool) {
	if !strings.HasPrefix(raw, `{"cursor":`) {
		return raw, wrapper{}, false
	}
	var j journalLine
	if err := json.Unmarshal([]byte(raw), &j); err != nil || j.Cursor == nil {
		return raw, wrapper{}, false
	}
	w := wrapper{stream: j.Stream, origin: j.Src}
	if j.T > 0 {
		w.stamp = time.UnixMicro(j.T)
	}
	if j.Pri != nil {
		w.priority = syslogPriority(*j.Pri)
	}
	// A journal message can span lines; keep it one entry, visibly.
	return strings.ReplaceAll(strings.TrimRight(j.Msg, "\n"), "\n", " ↵ "), w, true
}

// dockerLine is Docker's json-file log driver format.
type dockerLine struct {
	Log    *string `json:"log"`
	Stream string  `json:"stream"`
	Time   string  `json:"time"`
}

// unwrapDocker recognises Docker's json-file wrapper. pending reports a
// split line that is still incomplete (Docker writes lines over 16 KiB as
// pieces without a trailing newline).
func (p *Parser) unwrapDocker(raw string) (text string, w wrapper, ok, pending bool) {
	if !strings.HasPrefix(raw, `{"log":`) {
		return raw, w, false, false
	}
	var d dockerLine
	if err := json.Unmarshal([]byte(raw), &d); err != nil || d.Log == nil {
		return raw, w, false, false
	}
	if !strings.HasSuffix(*d.Log, "\n") {
		p.partial.WriteString(*d.Log)
		p.partOK = true
		return "", w, true, true
	}
	text = strings.TrimSuffix(*d.Log, "\n")
	if p.partOK {
		p.partial.WriteString(text)
		text = p.partial.String()
		p.partial.Reset()
		p.partOK = false
	}
	w.stamp, _ = time.Parse(time.RFC3339Nano, d.Time)
	w.stream = d.Stream
	return text, w, true, false
}

// Flush returns a Docker partial line left over at the end of the source,
// so a file cut mid-line still shows what it has.
func (p *Parser) Flush() (Entry, bool) {
	if !p.partOK {
		return Entry{}, false
	}
	text := Clean(p.partial.String())
	p.partial.Reset()
	p.partOK = false
	lvl, t := p.detect(text)
	return Entry{Source: p.Source, Line: p.line, Text: text, Level: lvl, Time: t}, true
}

func (p *Parser) detect(text string) (Level, time.Time) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		if lvl, t, ok := p.fromJSON(trimmed); ok {
			return lvl, t
		}
	}
	return p.fromText(text)
}

// isContinuation reports whether a line reads as the tail of a multi-line
// entry: indented, a stack frame, or a chained exception.
func isContinuation(text string) bool {
	if text == "" {
		return false
	}
	if text[0] == ' ' || text[0] == '\t' {
		return true
	}
	for _, prefix := range []string{"at ", "Caused by", "Traceback", "goroutine ", "... "} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// Clean makes a line safe to draw in a terminal: tabs become spaces, a
// carriage-return-redrawn progress line keeps its final state, and escape
// sequences and other control characters are dropped so an application's
// colours cannot scramble the display.
func Clean(l string) string {
	if !needsClean(l) {
		return l
	}
	if i := strings.LastIndexByte(strings.TrimRight(l, "\r"), '\r'); i >= 0 {
		l = l[i+1:]
	}
	var b strings.Builder
	b.Grow(len(l))
	inEscape := false
	for _, r := range l {
		switch {
		case inEscape:
			// CSI sequences end on a letter.
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
		case r == 0x1b:
			inEscape = true
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func needsClean(l string) bool {
	for i := 0; i < len(l); i++ {
		if c := l[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
