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
	//
	// It matches a line's text or the name of its source, so "backend"
	// finds everything the backend service logged.
	Query string
	// MinLevel hides entries below it. LevelUnknown shows everything;
	// anything above it also hides lines whose level could not be guessed,
	// since "show me warnings" should not be padded with unclassified noise.
	MinLevel logline.Level
	// ShowNoise shows health checks and local probes, which are hidden by
	// default (see logline.Entry.Noise).
	ShowNoise bool
}

// CaseSensitive reports whether Query is matched exactly.
func (f Filter) CaseSensitive() bool {
	return strings.IndexFunc(f.Query, unicode.IsUpper) >= 0
}

// Apply returns the indices of the entries that match. sources are the
// names Entry.Source indexes, searched alongside the text; nil skips them.
func (f Filter) Apply(entries []logline.Entry, sources []string) []int {
	return f.ApplyFrom(entries, sources, 0, make([]int, 0, len(entries)))
}

// ApplyFrom appends to out the indices, from start on, of the entries that
// match: for filtering only what arrived since the last call.
func (f Filter) ApplyFrom(entries []logline.Entry, sources []string, start int, out []int) []int {
	sensitive := f.CaseSensitive()
	// Which sources' names match is the same for every entry: decide once.
	var named []bool
	if f.Query != "" && sources != nil {
		named = make([]bool, len(sources))
		for i, name := range sources {
			named[i] = f.contains(name, sensitive)
		}
	}
	for i := start; i < len(entries); i++ {
		e := &entries[i]
		if f.match(e, sensitive, e.Source < len(named) && named[e.Source]) {
			out = append(out, i)
		}
	}
	return out
}

func (f Filter) contains(s string, sensitive bool) bool {
	if !sensitive {
		// ToLower returns its argument unchanged, without allocating,
		// when there is nothing to lower.
		s = strings.ToLower(s)
	}
	return strings.Contains(s, f.Query)
}

func (f Filter) match(e *logline.Entry, sensitive, sourceNamed bool) bool {
	if e.Noise && !f.ShowNoise {
		return false
	}
	if f.MinLevel != logline.LevelUnknown && e.Level < f.MinLevel {
		return false
	}
	if f.Query == "" || sourceNamed {
		return true
	}
	return f.contains(e.Text, sensitive)
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
