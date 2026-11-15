// Package view renders host statistics for a terminal.
//
// The output is built to survive its own future: every view is a pure
// function from query data to text, with colour and width passed in rather
// than discovered. Dropping these renderers into a TUI event loop means
// calling them on a tick instead of once — no rewrite — and the same
// functions degrade to clean, escape-free text when piped to a file.
package view

import (
	"fmt"
	"io"
	"strings"
	"time"

	"vpsm-collector/internal/query"
)

// Options controls rendering.
type Options struct {
	// Now is the clock the "last seen" ages are measured against.
	Now time.Time

	// Color enables ANSI styling. Callers set this from a TTY check.
	Color bool

	// StaleAfter is how long a host can go unheard-from before it is
	// flagged. A silent agent is itself a finding — often a more urgent one
	// than a busy disk — so it gets the same visual weight as a threshold
	// breach.
	StaleAfter time.Duration

	// SparkCells is how many samples of CPU history the overview draws.
	SparkCells int
}

// DefaultStaleAfter is a few times a typical sampling interval: long enough
// not to flag a host for one missed report, short enough to notice.
const DefaultStaleAfter = 2 * time.Minute

// Default returns sensible rendering options.
func Default() Options {
	return Options{
		Now:        time.Now(),
		StaleAfter: DefaultStaleAfter,
		SparkCells: 6,
	}
}

func (o Options) normalise() Options {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	if o.SparkCells < 0 {
		o.SparkCells = 0
	}
	return o
}

// placeholder stands in for a reading we do not have.
const placeholder = "—"

// Overview renders one line per host: the "is anything on fire?" view.
func Overview(w io.Writer, hosts []query.HostStatus, o Options) error {
	o = o.normalise()
	p := palette{enabled: o.Color}

	if len(hosts) == 0 {
		_, err := fmt.Fprintf(w, "%s\n", p.dim("No hosts have reported yet."))
		return err
	}

	t := newTable(2,
		col{"", alignLeft}, // status glyph
		col{"HOST", alignLeft},
		col{"SEEN", alignRight},
		col{"CPU", alignLeft},
		col{"MEM", alignLeft},
		col{"DISK", alignLeft},
		col{"NET ↓/↑", alignLeft},
		col{"", alignLeft}, // flags, empty on a healthy host
	)

	var warn, crit, stale int
	for i := range hosts {
		h := &hosts[i]
		latest := h.Latest()
		age := o.Now.Sub(h.LastSeen)
		isStale := latest == nil || age > o.StaleAfter

		if latest == nil {
			stale++
			t.add(p.dim("○"), h.Host.ID, p.dim(placeholder),
				p.dim("never reported"), "", "", "", "")
			continue
		}

		status := grade(latest)

		// A stale host's numbers are history, not status, so it is reported
		// as unknown regardless of what those numbers said.
		glyph := p.forLevel(status, "●")
		seen := relAge(age)
		switch {
		case isStale:
			stale++
			glyph = p.dim("○")
			seen = p.forLevel(levelCrit, seen)
		case status == levelCrit:
			crit++
		case status == levelWarn:
			warn++
		}

		t.add(
			glyph,
			h.Host.ID,
			seen,
			spark(cpuHistory(h, o.SparkCells), o.SparkCells)+" "+cpuCell(p, latest),
			gauge(p, latest.MemPct()),
			diskCell(p, latest),
			netCell(p, latest),
			flags(p, latest),
		)
	}

	if err := t.render(w, p); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n%s\n", p.dim(summary(len(hosts), warn, crit, stale)))
	return err
}

// grade is how alarming a host's latest reading is, across every signal
// that can independently ruin a machine.
func grade(s *query.Snapshot) level {
	levels := []level{levelFor(s.MemPct())}

	if s.CPU.Derived {
		// Steal is graded separately and on its own scale: a few percent of
		// stolen time is a real finding on rented hardware, where the same
		// few percent of ordinary CPU use is nothing.
		levels = append(levels, levelFor(s.CPU.BusyPct), stealLevel(s.CPU.StealPct))
	}
	if f, ok := s.Busiest(); ok {
		levels = append(levels, levelFor(f.Pressure()))
	}
	if s.SwapTotalBytes > 0 {
		levels = append(levels, levelFor(s.SwapPct()))
	}
	if s.NetProblems() > 0 {
		levels = append(levels, levelWarn)
	}
	return worst(levels...)
}

// cpuCell is the CPU gauge, or a placeholder when there was no previous
// sample to diff the tick counters against.
func cpuCell(p palette, s *query.Snapshot) string {
	if !s.CPU.Derived {
		return padTo(p.dim(placeholder), meterCells+5, alignLeft)
	}
	return gauge(p, s.CPU.BusyPct)
}

// diskCell shows the filesystem under the most pressure, naming inodes when
// they, rather than bytes, are what is about to fill it.
func diskCell(p palette, s *query.Snapshot) string {
	f, ok := s.Busiest()
	if !ok {
		return p.dim(placeholder)
	}
	label := truncate(f.MountPoint, 12)
	if f.InodeBound() {
		label += " inodes"
	}
	return gauge(p, f.Pressure()) + " " + p.dim(label)
}

// gauge is a bar and its percentage, coloured together by severity.
func gauge(p palette, pct float64) string {
	l := levelFor(pct)
	return p.forLevel(l, meter(pct, meterCells)) + " " + p.forLevel(l, padTo(humanPct(pct), 4, alignRight))
}

// netCell renders throughput, or a dash when it cannot be derived (see
// storage.derive: a single sample, a reboot, or a counter reset).
func netCell(p palette, s *query.Snapshot) string {
	rx, tx, ok := s.Totals()
	if !ok {
		return p.dim(placeholder)
	}
	return padTo(humanRate(rx), 8, alignRight) + p.dim(" / ") + padTo(humanRate(tx), 8, alignLeft)
}

// flags names what is wrong when the cause would not otherwise be visible
// in the columns. A red dot with no explanation is worse than no dot: these
// are the signals — stolen CPU, exhausted inodes, moving error counters —
// that a percentage column hides.
func flags(p palette, s *query.Snapshot) string {
	var out []string

	if s.CPU.Derived {
		if l := stealLevel(s.CPU.StealPct); l != levelOK {
			out = append(out, p.forLevel(l, fmt.Sprintf("steal %s", humanPct(s.CPU.StealPct))))
		}
	}
	if f, ok := s.Busiest(); ok && f.InodeBound() {
		if l := levelFor(f.InodesPct()); l != levelOK {
			out = append(out, p.forLevel(l, fmt.Sprintf("inodes %s", humanPct(f.InodesPct()))))
		}
	}
	if s.SwapTotalBytes > 0 {
		if l := levelFor(s.SwapPct()); l != levelOK {
			out = append(out, p.forLevel(l, fmt.Sprintf("swap %s", humanPct(s.SwapPct()))))
		}
	}
	if n := s.NetProblems(); n > 0 {
		out = append(out, p.forLevel(levelWarn, fmt.Sprintf("net errs +%d", n)))
	}
	return strings.Join(out, p.dim("  "))
}

// cpuHistory returns up to n CPU readings oldest-first, ready for spark.
// Samples whose percentages could not be derived are skipped rather than
// drawn as zero, which would show a false trough.
func cpuHistory(h *query.HostStatus, n int) []float64 {
	if n <= 0 {
		return nil
	}
	out := make([]float64, 0, n)
	// Samples are newest-first; a sparkline reads left to right in time.
	for i := min(n, len(h.Samples)) - 1; i >= 0; i-- {
		if s := &h.Samples[i]; s.CPU.Derived {
			out = append(out, s.CPU.BusyPct)
		}
	}
	return out
}

func summary(hosts, warn, crit, stale int) string {
	parts := []string{fmt.Sprintf("%s reporting", plural(hosts, "host"))}
	if crit > 0 {
		parts = append(parts, fmt.Sprintf("%d critical", crit))
	}
	if warn > 0 {
		parts = append(parts, fmt.Sprintf("%d warning", warn))
	}
	if stale > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", stale))
	}
	if crit == 0 && warn == 0 && stale == 0 {
		parts = append(parts, "all healthy")
	}
	parts = append(parts, fmt.Sprintf("warn %.0f%% / crit %.0f%%", WarnPct, CritPct))
	return strings.Join(parts, "  ·  ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
