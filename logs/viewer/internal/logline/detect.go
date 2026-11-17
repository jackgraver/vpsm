package logline

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Keys searched, in order, in a JSON line. They cover zap, zerolog, pino,
// bunyan, slog, logrus, Serilog, ECS and journald's JSON output.
var (
	jsonLevelKeys = []string{"level", "lvl", "severity", "log.level", "loglevel", "@l", "PRIORITY"}
	jsonTimeKeys  = []string{"time", "ts", "timestamp", "@timestamp", "t", "@t", "__REALTIME_TIMESTAMP"}
)

func (p *Parser) fromJSON(s string) (Level, time.Time, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return LevelUnknown, time.Time{}, false
	}
	lvl := LevelUnknown
	for _, k := range jsonLevelKeys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		switch v := v.(type) {
		case string:
			if k == "PRIORITY" {
				if n, err := strconv.Atoi(v); err == nil {
					lvl = syslogPriority(n)
				}
			} else {
				lvl = ParseLevel(v)
			}
		case float64:
			lvl = numericLevel(v)
		}
		if lvl != LevelUnknown {
			break
		}
	}
	var t time.Time
	for _, k := range jsonTimeKeys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		switch v := v.(type) {
		case string:
			if k == "__REALTIME_TIMESTAMP" { // microseconds since the epoch
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					t = time.UnixMicro(n)
				}
			} else {
				t = p.parseTime(v)
			}
		case float64:
			t = epoch(v)
		}
		if !t.IsZero() {
			break
		}
	}
	return lvl, t, true
}

// epoch reads a number as Unix seconds, or milliseconds when it is too
// large to be seconds (anything past the year 33658).
func epoch(n float64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n > 1e12 {
		return time.UnixMilli(int64(n))
	}
	sec := int64(n)
	return time.Unix(sec, int64((n-float64(sec))*1e9))
}

// headWindow is how far into a plain-text line a level word is looked
// for. Levels sit in the prefix; "ERROR" further in is usually message
// text ("retrying after ERROR from upstream") and not the line's level.
const headWindow = 120

var (
	// A bare uppercase level word, delimited the way loggers print them:
	// "ERROR", "[WARN]", "level:INFO", "|DEBUG|".
	reUpperLevel = regexp.MustCompile(`(?:^|[\s\[(|:<])(TRACE|DEBUG|INFO|NOTICE|WARN|WARNING|ERROR|ERR|FATAL|CRIT|CRITICAL|PANIC|ALERT|EMERG)(?:$|[\s\])|:>,])`)
	// Any case inside brackets: "[warn]", "<error>" (nginx error log uses "[error]").
	reBracketLevel = regexp.MustCompile(`(?i)[\[<](trace|debug|info|notice|warn|warning|error|err|fatal|crit|critical|panic|alert|emerg)[\]>]`)
	// logfmt and key=value styles: level=warn, lvl="error", severity=ERROR.
	reKVLevel = regexp.MustCompile(`(?i)\b(?:level|lvl|severity)=("?)([a-z]+)`)
	reKVTime  = regexp.MustCompile(`\b(?:ts|time|timestamp)=("?)([^"\s]+(?: [0-9:.]+)?)`)
	// The request and status of an HTTP access log (combined/common format).
	reAccessStatus = regexp.MustCompile(`"[A-Z]+ \S+ HTTP/[0-9.]+" ([1-5][0-9]{2}) `)
	// Gin's request log: `[GIN] 2026/11/16 - 21:12:45 | 200 | 83ms | ...`.
	// Its timestamp is not at the start and not ISO, so it needs its own
	// pattern; without it, a Gin line after a timestamped one reads as that
	// line's continuation.
	reGin = regexp.MustCompile(`^\[GIN\] (\d{4}/\d{2}/\d{2} - \d{2}:\d{2}:\d{2}) \|\s*([1-5][0-9]{2}) \|`)

	// Timestamps at the start of a line, optionally bracketed.
	reISOPrefix    = regexp.MustCompile(`^\[?(\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z| ?[+-]\d{2}:?\d{2})?)`)
	reSyslogPrefix = regexp.MustCompile(`^([A-Z][a-z]{2} [ \d]\d \d{2}:\d{2}:\d{2})`)
	// Common log format's "[16/Nov/2026:10:00:00 +0000]", which follows the
	// client address rather than starting the line.
	reCLFTime = regexp.MustCompile(`\[(\d{2}/[A-Z][a-z]{2}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4})\]`)
)

func (p *Parser) fromText(text string) (Level, time.Time) {
	head := text
	if len(head) > headWindow {
		head = head[:headWindow]
	}

	lvl := LevelUnknown
	if m := reKVLevel.FindStringSubmatch(text); m != nil {
		lvl = ParseLevel(m[2])
	}
	if lvl == LevelUnknown {
		if m := reUpperLevel.FindStringSubmatch(head); m != nil {
			lvl = ParseLevel(m[1])
		}
	}
	if lvl == LevelUnknown {
		if m := reBracketLevel.FindStringSubmatch(head); m != nil {
			lvl = ParseLevel(m[1])
		}
	}
	gin := reGin.FindStringSubmatch(text)
	if lvl == LevelUnknown {
		if gin != nil {
			lvl = statusLevel(gin[2])
		} else if m := reAccessStatus.FindStringSubmatch(text); m != nil {
			lvl = statusLevel(m[1])
		}
	}

	var t time.Time
	switch {
	case gin != nil:
		t, _ = time.ParseInLocation("2006/01/02 - 15:04:05", gin[1], time.Local)
	case reISOPrefix.MatchString(text):
		t = p.parseTime(reISOPrefix.FindStringSubmatch(text)[1])
	case reSyslogPrefix.MatchString(text):
		t = p.parseSyslog(reSyslogPrefix.FindStringSubmatch(text)[1])
	default:
		if m := reCLFTime.FindStringSubmatch(head); m != nil {
			t, _ = time.Parse("02/Jan/2006:15:04:05 -0700", m[1])
		} else if m := reKVTime.FindStringSubmatch(text); m != nil {
			t = p.parseTime(m[2])
		}
	}
	return lvl, t
}

// statusLevel grades an HTTP status: 5xx error, 4xx warn, else info.
func statusLevel(code string) Level {
	switch code[0] {
	case '5':
		return LevelError
	case '4':
		return LevelWarn
	}
	return LevelInfo
}

// isoLayouts are tried in order on a normalised timestamp: '/' date
// separators become '-', and a ',' before fractional seconds becomes '.'.
var isoLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z0700",
	"2006-01-02 15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
}

// parseTime reads a timestamp string in any of the common ISO-like forms,
// or as epoch seconds. Times without a zone are taken as local time.
func (p *Parser) parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return epoch(n)
	}
	if len(s) >= 10 {
		s = strings.ReplaceAll(s[:10], "/", "-") + strings.Replace(s[10:], ",", ".", 1)
	}
	for _, layout := range isoLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseSyslog reads "Nov 16 10:00:00", which has no year: it is taken as
// the most recent such date not in the future.
func (p *Parser) parseSyslog(s string) time.Time {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	t, err := time.ParseInLocation("Jan _2 15:04:05", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	t = t.AddDate(now.Year(), 0, 0)
	if t.After(now.Add(24 * time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t
}

var (
	// A request for a health endpoint, as access logs and GIN print it:
	// `"GET /health HTTP/1.1"`, `GET      "/healthz"`.
	reHealthPath = regexp.MustCompile(`(?:GET|HEAD)\s+"?/(?:health|healthz|healthcheck|ready|readyz|live|livez|ping)(?:[/?"\s]|$)`)
	// A request from loopback. Inside a container, real traffic arrives from
	// the bridge (172.x) through the reverse proxy; loopback is the
	// container's own HEALTHCHECK probing itself.
	reLoopbackRequest = regexp.MustCompile(`(?:^(?:127\.0\.0\.1|::1) - \S+ \[.*"(?:GET|HEAD) |\|\s+(?:127\.0\.0\.1|::1)\s+\|\s+(?:GET|HEAD)\s)`)
)

// IsProbe reports whether a line is a health check or a local probe rather
// than real traffic.
func IsProbe(text string) bool {
	return reHealthPath.MatchString(text) || reLoopbackRequest.MatchString(text)
}
