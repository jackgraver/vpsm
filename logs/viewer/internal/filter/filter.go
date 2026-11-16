// Package filter decides which entries are shown: a substring search and a
// minimum level, both of which must match.
package filter

import (
	"strings"
	"unicode"

	"vpsm-logs/internal/logline"
)

// Filter selects entries. The zero value matches everything.
type Filter struct {
	// Query is a plain substring, not a pattern. It is smart-case, as in
	// ripgrep and vim: all-lowercase matches any case, and any uppercase
	// letter makes it case-sensitive. So "timeout" finds "Timeout", while
	// "ERROR" finds only "ERROR".
	Query string
	// MinLevel hides entries below it. LevelUnknown shows everything;
	// anything above it also hides lines whose level could not be guessed,
	// since "show me warnings" should not be padded with unclassified noise.
	MinLevel logline.Level
}

// Active reports whether the filter hides anything.
func (f Filter) Active() bool { return f.Query != "" || f.MinLevel != logline.LevelUnknown }

// CaseSensitive reports whether Query is matched exactly.
func (f Filter) CaseSensitive() bool {
	return strings.IndexFunc(f.Query, unicode.IsUpper) >= 0
}

// Apply returns the indices of the entries that match.
func (f Filter) Apply(entries []logline.Entry) []int {
	out := make([]int, 0, len(entries))
	sensitive := f.CaseSensitive()
	for i := range entries {
		if f.match(&entries[i], sensitive) {
			out = append(out, i)
		}
	}
	return out
}

func (f Filter) match(e *logline.Entry, sensitive bool) bool {
	if f.MinLevel != logline.LevelUnknown && e.Level < f.MinLevel {
		return false
	}
	if f.Query == "" {
		return true
	}
	text := e.Text
	if !sensitive {
		// ToLower returns its argument unchanged, without allocating,
		// when there is nothing to lower.
		text = strings.ToLower(text)
	}
	return strings.Contains(text, f.Query)
}

// Span is a half-open byte range [Start, End) of a match within a string.
type Span struct{ Start, End int }

// Matches returns where Query occurs in s, for highlighting. Offsets are
// into s itself, also when matching ignores case.
func (f Filter) Matches(s string) []Span {
	if f.Query == "" {
		return nil
	}
	hay := s
	if !f.CaseSensitive() {
		hay = strings.ToLower(s)
		// Lowercasing can change byte lengths outside ASCII (e.g. 'İ'),
		// which would misplace offsets; skip highlighting rather than
		// paint the wrong text.
		if len(hay) != len(s) {
			return nil
		}
	}
	var spans []Span
	for start := 0; ; {
		i := strings.Index(hay[start:], f.Query)
		if i < 0 {
			return spans
		}
		s := start + i
		spans = append(spans, Span{s, s + len(f.Query)})
		start = s + len(f.Query)
	}
}
