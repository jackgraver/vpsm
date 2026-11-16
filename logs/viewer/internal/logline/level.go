package logline

import "strings"

// Level is a line's severity as far as it could be guessed. The zero value
// is LevelUnknown: most plain-text lines carry no level at all, and that is
// not the same as "info".
type Level uint8

const (
	LevelUnknown Level = iota
	LevelTrace
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

var levelNames = [...]string{"", "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"}

func (l Level) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return ""
}

// ParseLevel maps the many spellings loggers use onto a Level, ignoring
// case. It returns LevelUnknown for anything it does not recognise.
func ParseLevel(word string) Level {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "trace", "trc", "verbose":
		return LevelTrace
	case "debug", "dbg":
		return LevelDebug
	case "info", "inf", "information", "informational", "notice":
		return LevelInfo
	case "warn", "warning", "wrn":
		return LevelWarn
	case "error", "err", "eror":
		return LevelError
	case "fatal", "ftl", "crit", "critical", "panic", "alert", "emerg", "emergency":
		return LevelFatal
	}
	return LevelUnknown
}

// numericLevel maps pino/bunyan numeric levels (10 trace .. 60 fatal).
func numericLevel(n float64) Level {
	switch {
	case n >= 60:
		return LevelFatal
	case n >= 50:
		return LevelError
	case n >= 40:
		return LevelWarn
	case n >= 30:
		return LevelInfo
	case n >= 20:
		return LevelDebug
	case n >= 10:
		return LevelTrace
	}
	return LevelUnknown
}

// syslogPriority maps a syslog/journald PRIORITY (0 emerg .. 7 debug).
func syslogPriority(p int) Level {
	switch {
	case p < 0 || p > 7:
		return LevelUnknown
	case p <= 2:
		return LevelFatal
	case p == 3:
		return LevelError
	case p == 4:
		return LevelWarn
	case p <= 6:
		return LevelInfo
	}
	return LevelDebug
}
