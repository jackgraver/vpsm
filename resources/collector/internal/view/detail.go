package view

import (
	"fmt"
	"io"
	"strings"
	"time"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

// detailSparkCells is wider than the overview's: with one host on screen
// there is room for history that actually shows a trend.
const detailSparkCells = 48

// Detail renders everything known about one host.
func Detail(w io.Writer, h *query.HostStatus, o Options) error {
	o = o.normalise()
	p := palette{enabled: o.Color}

	if err := detailHeader(w, h, o, p); err != nil {
		return err
	}

	latest := h.Latest()
	if latest == nil {
		_, err := fmt.Fprintf(w, "\n%s\n", p.dim("This host is registered but has never reported a sample."))
		return err
	}

	for _, section := range []func() error{
		func() error { return detailGauges(w, h, latest, p) },
		func() error { return detailTrend(w, h, o, p) },
		func() error { return detailFilesystems(w, latest, p) },
		func() error { return detailNet(w, latest, p) },
		func() error { return detailProcesses(w, h, o, p) },
	} {
		if err := section(); err != nil {
			return err
		}
	}
	return nil
}

func detailHeader(w io.Writer, h *query.HostStatus, o Options, p palette) error {
	id := p.bold(h.Host.ID)

	var facts []string
	for _, f := range []string{
		h.Host.Hostname,
		strings.TrimSpace(h.Host.OS + " " + h.Host.Kernel),
		coresLabel(h.Host.Cores),
		agentLabel(h.Host.AgentVersion),
	} {
		if f != "" {
			facts = append(facts, f)
		}
	}
	line := id
	if len(facts) > 0 {
		line += p.dim("  ·  " + strings.Join(facts, "  ·  "))
	}
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}

	age := o.Now.Sub(h.LastSeen)
	seen := fmt.Sprintf("last seen %s ago", relAge(age))
	if age > o.StaleAfter {
		seen = p.forLevel(levelCrit, seen+" (stale)")
	}

	meta := []string{seen}
	if latest := h.Latest(); latest != nil && latest.UptimeSecs > 0 {
		meta = append(meta, "up "+humanUptime(latest.UptimeSecs))
	}
	meta = append(meta,
		fmt.Sprintf("first seen %s", h.FirstSeen.Format(time.DateTime)+" UTC"),
		fmt.Sprintf("%s held", plural(len(h.Samples), "sample")),
	)
	_, err := fmt.Fprintln(w, p.dim(strings.Join(meta, "  ·  ")))
	return err
}

func coresLabel(n int) string {
	if n <= 0 {
		return ""
	}
	return plural(n, "core")
}

func agentLabel(v string) string {
	if v == "" {
		return ""
	}
	return "agent " + v
}

// detailGauges is the headline block: current CPU, memory and swap.
func detailGauges(w io.Writer, h *query.HostStatus, s *query.Snapshot, p palette) error {
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	t := newTable(2,
		col{"", alignLeft},
		col{"", alignLeft},
		col{"", alignLeft},
	)

	if s.CPU.Derived {
		t.add(p.dim("CPU"), gauge(p, s.CPU.BusyPct), p.dim(loadLabel(s, h.Host.Cores)))
		// The split is the point of carrying tick counters rather than one
		// percentage: steal and iowait each point at a different fix.
		t.add("", "", cpuBreakdown(p, s))
	} else {
		t.add(p.dim("CPU"), p.dim(placeholder), p.dim("needs two samples to derive"))
	}

	t.add(p.dim("MEM"), gauge(p, s.MemPct()), p.dim(memLabel(s)))

	if s.SwapTotalBytes > 0 {
		t.add(p.dim("SWAP"), gauge(p, s.SwapPct()),
			p.dim(fmt.Sprintf("%s / %s", humanBytes(s.SwapUsedBytes), humanBytes(s.SwapTotalBytes))))
	} else {
		t.add(p.dim("SWAP"), p.dim(placeholder), p.dim("none configured"))
	}
	return t.render(w, p)
}

// cpuBreakdown spells out where the CPU time went. Steal and iowait are
// coloured on their own terms because a few percent of either means
// something that a few percent of user time does not.
func cpuBreakdown(p palette, s *query.Snapshot) string {
	parts := []string{
		p.dim("user ") + humanPct(s.CPU.UserPct),
		p.dim("sys ") + humanPct(s.CPU.SystemPct),
		p.dim("iowait ") + p.forLevel(levelFor(s.CPU.IowaitPct), humanPct(s.CPU.IowaitPct)),
		p.dim("steal ") + p.forLevel(stealLevel(s.CPU.StealPct), humanPct(s.CPU.StealPct)),
	}
	// The run queue belongs next to these: iowait says the CPU is idle
	// waiting on storage, and procs_blocked says how many tasks are stuck
	// in uninterruptible sleep doing the waiting. Either alone is
	// ambiguous; together they are a diagnosis.
	if s.ProcsRunning > 0 || s.ProcsBlocked > 0 {
		run := fmt.Sprintf("%d", s.ProcsRunning)
		blocked := fmt.Sprintf("%d", s.ProcsBlocked)
		if s.ProcsBlocked > 0 {
			blocked = p.forLevel(levelWarn, blocked)
		}
		parts = append(parts, p.dim("run ")+run+p.dim("  blocked ")+blocked)
	}

	line := strings.Join(parts, p.dim("  ·  "))

	if stealLevel(s.CPU.StealPct) != levelOK {
		// Worth spelling out: the fix for stolen time is not on this box.
		line += p.dim("   (stolen by the hypervisor — this host is oversold)")
	}
	return line
}

func memLabel(s *query.Snapshot) string {
	label := fmt.Sprintf("%s / %s", humanBytes(s.MemUsedBytes), humanBytes(s.MemTotalBytes))
	if s.MemAvailableBytes > 0 {
		// "used" here is total minus available, not minus free, so page
		// cache does not read as exhaustion. Showing available makes that
		// visible rather than something you have to know.
		label += fmt.Sprintf("  ·  %s available", humanBytes(s.MemAvailableBytes))
	}
	return label
}

func loadLabel(s *query.Snapshot, cores int) string {
	if s.Load1 == 0 && s.Load5 == 0 && s.Load15 == 0 {
		return ""
	}
	label := fmt.Sprintf("load %.2f %.2f %.2f", s.Load1, s.Load5, s.Load15)
	if cores > 0 {
		// Load relative to core count is the number that means something;
		// 4.0 is saturation on 4 cores and idle-ish on 32.
		label += fmt.Sprintf("  (%.0f%% of %s)", s.Load1/float64(cores)*100, plural(cores, "core"))
	}
	return label
}

// detailTrend draws CPU and memory history as sparklines over the window the
// caller asked for.
func detailTrend(w io.Writer, h *query.HostStatus, o Options, p palette) error {
	if len(h.Samples) < 2 {
		return nil
	}

	oldest := h.Samples[len(h.Samples)-1].At
	newest := h.Samples[0].At
	span := fmt.Sprintf("last %s  ·  %s → %s UTC",
		plural(len(h.Samples), "sample"),
		oldest.Format(time.TimeOnly),
		newest.Format(time.TimeOnly))

	if _, err := fmt.Fprintf(w, "\n%s %s\n", p.header("TREND"), p.dim(span)); err != nil {
		return err
	}

	var cpu, mem, steal []float64
	for i := len(h.Samples) - 1; i >= 0; i-- {
		s := &h.Samples[i]
		mem = append(mem, s.MemPct())
		if s.CPU.Derived {
			cpu = append(cpu, s.CPU.BusyPct)
			steal = append(steal, s.CPU.StealPct)
		}
	}

	// Size the lines to the data. spark pads on the left to keep the newest
	// reading in a fixed column, which is what the overview wants; here a
	// 48-cell line holding 12 samples would just render as leading blank.
	cells := min(detailSparkCells, max(len(cpu), len(mem)))

	t := newTable(2,
		col{"", alignLeft},
		col{"", alignLeft},
		col{"", alignRight},
	)
	if len(cpu) > 0 {
		t.add(p.dim("CPU"), p.forLevel(levelFor(cpu[len(cpu)-1]), spark(cpu, cells)), p.dim("0-100%"))
	}
	t.add(p.dim("MEM"), p.forLevel(levelFor(mem[len(mem)-1]), spark(mem, cells)), p.dim("0-100%"))

	// Only worth the line when there is something to see: on dedicated
	// hardware this is flat zero forever.
	if anyAbove(steal, 0) {
		t.add(p.dim("STEAL"), p.forLevel(stealLevel(steal[len(steal)-1]), spark(steal, cells)), p.dim("0-100%"))
	}
	return t.render(w, p)
}

func anyAbove(values []float64, threshold float64) bool {
	for _, v := range values {
		if v > threshold {
			return true
		}
	}
	return false
}

func detailFilesystems(w io.Writer, s *query.Snapshot, p palette) error {
	if len(s.Filesystems) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", p.header("FILESYSTEMS")); err != nil {
		return err
	}

	// Inodes get a column of their own rather than being folded into the
	// usage bar: a filesystem out of inodes refuses writes with bytes to
	// spare, and that has to be readable as a separate fact.
	t := newTable(2,
		col{"MOUNT", alignLeft},
		col{"FS", alignLeft},
		col{"USED", alignLeft},
		col{"SIZE", alignRight},
		col{"INODES", alignRight},
		col{"DEVICE", alignLeft},
	)
	for _, f := range s.Filesystems {
		inodes := p.dim(placeholder)
		if f.InodesTotal > 0 {
			inodes = p.forLevel(levelFor(f.InodesPct()), humanPct(f.InodesPct()))
		}
		t.add(
			f.MountPoint,
			p.dim(f.FSType),
			gauge(p, f.BytesPct()),
			padTo(humanBytes(f.UsedBytes), 5, alignRight)+p.dim(" / "+humanBytes(f.TotalBytes)),
			inodes,
			p.dim(f.Device),
		)
	}
	return t.render(w, p)
}

func detailNet(w io.Writer, s *query.Snapshot, p palette) error {
	if len(s.Net) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", p.header("INTERFACES")); err != nil {
		return err
	}

	t := newTable(2,
		col{"IFACE", alignLeft},
		col{"↓ RATE", alignRight},
		col{"↑ RATE", alignRight},
		col{"↓ TOTAL", alignRight},
		col{"↑ TOTAL", alignRight},
		col{"ERRS", alignRight},
		col{"DROPS", alignRight},
	)
	for _, n := range s.Net {
		rx, tx := p.dim(placeholder), p.dim(placeholder)
		if n.Rates {
			rx, tx = humanRate(n.RxPerSec), humanRate(n.TxPerSec)
		}
		// Errors and drops show the cumulative total, with the movement
		// since the last sample called out: a counter sitting at 7 since
		// 2019 is noise, and the same counter gaining 7 a second is not.
		t.add(
			n.Name, rx, tx,
			p.dim(humanBytes(n.RxBytes)), p.dim(humanBytes(n.TxBytes)),
			counterCell(p, n.RxErrors+n.TxErrors, n.ErrorsDelta),
			counterCell(p, n.RxDropped+n.TxDropped, n.DroppedDelta),
		)
	}
	return t.render(w, p)
}

func counterCell(p palette, total, delta uint64) string {
	if delta > 0 {
		return p.forLevel(levelWarn, fmt.Sprintf("%d (+%d)", total, delta))
	}
	if total == 0 {
		return p.dim("0")
	}
	return p.dim(fmt.Sprintf("%d", total))
}

// detailProcesses renders the host's most recent process list.
func detailProcesses(w io.Writer, h *query.HostStatus, o Options, p palette) error {
	proc := h.Processes
	if proc == nil {
		return nil
	}

	age := o.Now.Sub(proc.At)
	header := fmt.Sprintf("\n%s %s\n", p.header("PROCESSES"), p.dim(fmt.Sprintf(
		"%d total  ·  %d running  ·  %d sleeping  ·  %d blocked  ·  %d zombie  ·  as of %s ago",
		proc.Total, proc.Running, proc.Sleeping, proc.Blocked, proc.Zombie, relAge(age))))
	if _, err := fmt.Fprint(w, header); err != nil {
		return err
	}

	if len(proc.Top) == 0 {
		_, err := fmt.Fprintf(w, "%s\n", p.dim("  (no process list — run `host` against this id to load it)"))
		return err
	}

	t := newTable(2,
		col{"PID", alignRight},
		col{"USER", alignLeft},
		col{"CPU%", alignRight},
		col{"MEM%", alignRight},
		col{"RSS", alignRight},
		col{"S", alignLeft},
		col{"IO ↓/↑", alignRight},
		col{"COMMAND", alignLeft},
	)
	for _, proc := range proc.Top {
		t.add(
			fmt.Sprintf("%d", proc.PID),
			userLabel(p, proc.User),
			// Not a percentage of the host: a share of one CPU, so it
			// exceeds 100 for anything multi-threaded. Left uncoloured
			// because there is no fixed scale to colour it against.
			fmt.Sprintf("%.1f", proc.CPUPct),
			fmt.Sprintf("%.1f", proc.MemPct),
			humanBytes(proc.RSSBytes),
			p.dim(proc.State),
			processIO(p, proc.IO),
			commandLabel(proc),
		)
	}
	if err := t.render(w, p); err != nil {
		return err
	}

	// RSS double-counts shared pages, so these will not add up to the
	// memory figure above. Saying so beats letting someone discover it by
	// trying to reconcile the two.
	_, err := fmt.Fprintf(w, "%s\n",
		p.dim("CPU% is a share of one core and may exceed 100. RSS double-counts shared pages; it ranks processes, it does not total them."))
	return err
}

func userLabel(p palette, user *string) string {
	if user == nil {
		// The uid could not be resolved, which is not the same as root.
		return p.dim("?")
	}
	return truncate(*user, 10)
}

func commandLabel(proc protocol.Process) string {
	if proc.Command != "" {
		return truncate(proc.Command, 48)
	}
	return truncate(proc.Name, 48)
}

// processIO renders a process's I/O, distinguishing "not measured" from
// zero. Reading another process's counters needs ptrace permission the
// agent often does not have, and showing that as 0 B/s would turn a
// permissions problem into a silent wrong answer.
func processIO(p palette, io *protocol.ProcessIO) string {
	if io == nil {
		return p.dim("n/a")
	}
	return humanRate(io.ReadBytesPerSec) + p.dim(" / ") + humanRate(io.WriteBytesPerSec)
}
