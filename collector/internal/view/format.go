package view

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Thresholds a utilisation percentage is judged against. They drive both
// colour and the per-host status glyph, so one host at 94% CPU is visible in
// a list of fifty without reading any numbers.
const (
	WarnPct = 75.0
	CritPct = 90.0
)

// level is how alarming a reading is.
type level int

const (
	levelOK level = iota
	levelWarn
	levelCrit
	levelUnknown // no data: a host that has never reported, or has gone quiet
)

// levelFor grades a percentage.
func levelFor(pct float64) level {
	switch {
	case pct >= CritPct:
		return levelCrit
	case pct >= WarnPct:
		return levelWarn
	default:
		return levelOK
	}
}

// worst returns the most alarming of the given levels.
func worst(levels ...level) level {
	out := levelOK
	for _, l := range levels {
		// levelUnknown sorts above levelCrit numerically but is less
		// alarming than it, so compare deliberately rather than by value.
		if l == levelUnknown {
			continue
		}
		if l > out {
			out = l
		}
	}
	return out
}

// ANSI SGR codes. Limited to the basic set, which every terminal worth
// supporting renders the same way.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "1"
	ansiDim    = "2"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
	ansiCyan   = "36"
)

// palette applies colour, or doesn't. Every styled string goes through it so
// that piping output to a file produces clean text with no escape codes.
type palette struct{ enabled bool }

func (p palette) style(codes, s string) string {
	if !p.enabled || codes == "" || s == "" {
		return s
	}
	return "\x1b[" + codes + "m" + s + ansiReset
}

func (p palette) dim(s string) string    { return p.style(ansiDim, s) }
func (p palette) bold(s string) string   { return p.style(ansiBold, s) }
func (p palette) header(s string) string { return p.style(ansiDim+";"+ansiCyan, s) }

// forLevel colours by severity: green reads as fine, so it is reserved for
// values that are, and anything unknown is dimmed rather than coloured.
func (p palette) forLevel(l level, s string) string {
	switch l {
	case levelCrit:
		return p.style(ansiBold+";"+ansiRed, s)
	case levelWarn:
		return p.style(ansiYellow, s)
	case levelUnknown:
		return p.dim(s)
	default:
		return p.style(ansiGreen, s)
	}
}

// meterCells is the width of a gauge bar.
const meterCells = 8

// eighths are the partial block glyphs, giving a gauge sub-cell resolution:
// an 8-cell bar resolves to roughly 1.5%, so small changes still move it.
var eighths = [...]rune{'▏', '▎', '▍', '▌', '▋', '▊', '▉'}

const (
	fullBlock  = '█'
	emptyBlock = '░'
)

// meter renders pct as a fixed-width gauge. Fixed width matters more than it
// sounds: it makes the bars across a column directly comparable by eye.
func meter(pct float64, cells int) string {
	if cells <= 0 {
		return ""
	}
	pct = math.Max(0, math.Min(100, pct))

	exact := pct / 100 * float64(cells)
	full := int(exact)
	var b strings.Builder
	b.Grow(cells * 4)
	for range full {
		b.WriteRune(fullBlock)
	}
	if full < cells {
		// Round the remainder to the nearest eighth. Zero means there is
		// nothing worth drawing, so the cell stays empty; eight means it
		// rounded up to a whole cell, which has its own glyph rather than
		// an index into the partials.
		switch e := int(math.Round((exact - float64(full)) * 8)); {
		case e >= 8:
			b.WriteRune(fullBlock)
			full++
		case e > 0:
			b.WriteRune(eighths[e-1])
			full++
		}
	}
	for i := full; i < cells; i++ {
		b.WriteRune(emptyBlock)
	}
	return b.String()
}

// sparkRunes are the eight heights a sparkline can draw.
var sparkRunes = [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// spark renders values as a sparkline on a fixed 0-100 scale, oldest to
// newest, left to right.
//
// The scale is deliberately fixed rather than fitted to the data: an
// auto-scaled sparkline makes an idle host's 2-3% jitter look identical to a
// saturated one's, which is precisely backwards for a fleet overview.
func spark(values []float64, cells int) string {
	if cells <= 0 || len(values) == 0 {
		return strings.Repeat(" ", max(cells, 0))
	}

	var b strings.Builder
	b.Grow(cells * 4)
	// Right-align: pad on the left when there is less history than room, so
	// the newest reading is always in the same column.
	for range cells - len(values) {
		b.WriteRune(' ')
	}
	if len(values) > cells {
		values = values[len(values)-cells:]
	}
	for _, v := range values {
		v = math.Max(0, math.Min(100, v))
		i := int(v / 100 * float64(len(sparkRunes)))
		if i >= len(sparkRunes) {
			i = len(sparkRunes) - 1
		}
		b.WriteRune(sparkRunes[i])
	}
	return b.String()
}

// byteUnits are 1024-based, matching what `df -h` and `free -h` print, since
// that is what anyone reading this output will be comparing against.
var byteUnits = [...]string{"B", "K", "M", "G", "T", "P"}

// humanBytes renders a byte count in at most 5 visible characters.
func humanBytes(n uint64) string {
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(byteUnits)-1 {
		v /= 1024
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d%s", n, byteUnits[i])
	case v < 10:
		return fmt.Sprintf("%.1f%s", v, byteUnits[i])
	default:
		return fmt.Sprintf("%.0f%s", v, byteUnits[i])
	}
}

// humanRate renders bytes/second.
func humanRate(perSec float64) string {
	if perSec < 0 {
		perSec = 0
	}
	return humanBytes(uint64(perSec)) + "/s"
}

// humanPct renders a percentage in at most 4 visible characters.
func humanPct(pct float64) string { return fmt.Sprintf("%.0f%%", pct) }

// relAge renders a duration compactly: how long ago, at a glance.
func relAge(d time.Duration) string {
	switch {
	case d < 0:
		// A sample stamped in the future: clock skew between agent and
		// collector. Worth seeing rather than hiding behind "0s".
		return "+" + relAge(-d)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// humanUptime renders uptime the way `uptime` does, coarsely.
func humanUptime(secs uint64) string {
	if secs == 0 {
		return "unknown"
	}
	d := time.Duration(secs) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// visibleLen is the rendered width of s, ignoring ANSI escape sequences.
// Column alignment has to be computed on this rather than len() or even rune
// count, because styled cells carry invisible escape bytes.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// Skip a CSI sequence: ESC [ ... final-byte in @-~.
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < '@' || s[j] > '~') {
					j++
				}
				if j < len(s) {
					j++
				}
			}
			i = j
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// padTo pads s with spaces to width w, aligning as asked. Strings already
// wider than w are returned unchanged: silently truncating a long hostname
// would corrupt the escape codes inside it.
func padTo(s string, w int, a align) string {
	gap := w - visibleLen(s)
	if gap <= 0 {
		return s
	}
	if a == alignRight {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

// truncate shortens s to at most w visible characters, marking the cut. It is
// only for plain, unstyled text such as a mount point or hostname.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:w-1]) + "…"
}

// Steal thresholds are far lower than the general ones. A host losing 5% of
// its CPU to other tenants is a real finding — it is capacity you are
// paying for and not receiving — where 5% ordinary CPU use is nothing.
const (
	StealWarnPct = 5.0
	StealCritPct = 15.0
)

// stealLevel grades stolen CPU time on its own scale.
func stealLevel(pct float64) level {
	switch {
	case pct >= StealCritPct:
		return levelCrit
	case pct >= StealWarnPct:
		return levelWarn
	default:
		return levelOK
	}
}
