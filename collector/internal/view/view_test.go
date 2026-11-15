package view

import (
	"strings"
	"testing"
	"time"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func testOptions() Options {
	return Options{Now: now, Color: false, StaleAfter: 2 * time.Minute, SparkCells: 6}
}

// host builds a host whose latest sample sits `age` in the past. The disk
// figure is expressed as a used percentage; available is set so the
// df-style calculation reproduces it exactly.
func host(id string, age time.Duration, cpu, mem, disk float64) query.HostStatus {
	at := now.Add(-age)
	const diskTotal = 100 << 30
	used := uint64(float64(diskTotal) * disk / 100)

	snap := query.Snapshot{
		At:          at,
		UptimeSecs:  100000,
		ElapsedSecs: 10,
		CPU: query.CPUUsage{
			Derived: true,
			BusyPct: cpu,
			UserPct: cpu * 0.7,
			// Low enough not to trip the steal thresholds unless a test
			// raises it deliberately.
			SystemPct: cpu * 0.3,
			IowaitPct: 1,
			StealPct:  0,
		},
		Load1:             1.5,
		MemTotalBytes:     8 << 30,
		MemUsedBytes:      uint64(float64(8<<30) * mem / 100),
		MemAvailableBytes: uint64(float64(8<<30) * (100 - mem) / 100),
		SwapTotalBytes:    2 << 30,
		SwapUsedBytes:     1 << 28,
		Filesystems: []query.Filesystem{{
			Device: "/dev/sda1", MountPoint: "/", FSType: "ext4",
			TotalBytes: diskTotal, UsedBytes: used, AvailableBytes: diskTotal - used,
			InodesTotal: 1000, InodesUsed: 100,
		}},
		Net: []query.Iface{
			{Name: "eth0", RxBytes: 5 << 30, TxBytes: 1 << 30, RxPerSec: 51200, TxPerSec: 1024, Rates: true},
		},
	}
	return query.HostStatus{
		Host:      protocol.Host{ID: id, Hostname: id + ".example.com", OS: "linux", Kernel: "6.1.0", Cores: 4, AgentVersion: "0.1.0"},
		FirstSeen: at.Add(-time.Hour),
		LastSeen:  at,
		Samples:   []query.Snapshot{snap},
	}
}

func render(t *testing.T, f func(*strings.Builder) error) string {
	t.Helper()
	var b strings.Builder
	if err := f(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestMeter(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, "░░░░░░░░"},
		{100, "████████"},
		{50, "████░░░░"},
		{25, "██░░░░░░"},
		// Clamped rather than overflowing the column.
		{-10, "░░░░░░░░"},
		{150, "████████"},
	}
	for _, tc := range cases {
		if got := meter(tc.pct, 8); got != tc.want {
			t.Errorf("meter(%g) = %q, want %q", tc.pct, got, tc.want)
		}
	}

	// Every gauge must occupy exactly its column width, or the table's
	// alignment drifts from row to row.
	for pct := 0.0; pct <= 100; pct += 0.37 {
		if n := visibleLen(meter(pct, 8)); n != 8 {
			t.Fatalf("meter(%g) is %d cells wide, want 8", pct, n)
		}
	}
}

func TestSparkUsesFixedScale(t *testing.T) {
	// An idle host and a saturated one must not look alike: with an
	// auto-fitted scale both would render as full-range noise.
	idle := spark([]float64{1, 2, 3, 2, 1, 2}, 6)
	busy := spark([]float64{95, 96, 97, 96, 95, 96}, 6)
	if idle == busy {
		t.Fatalf("idle and busy sparklines are identical: %q", idle)
	}
	if !strings.Contains(idle, "\u2581") {
		t.Errorf("idle sparkline = %q, want the lowest glyph", idle)
	}
	if !strings.Contains(busy, "\u2588") {
		t.Errorf("busy sparkline = %q, want the highest glyph", busy)
	}
}

func TestSparkWidthIsFixed(t *testing.T) {
	// Short history pads on the left so the newest reading stays in the
	// same column across rows.
	short := spark([]float64{50, 60}, 6)
	if n := visibleLen(short); n != 6 {
		t.Errorf("width = %d, want 6 (%q)", n, short)
	}
	if !strings.HasPrefix(short, "    ") {
		t.Errorf("short history = %q, want left padding", short)
	}

	// Excess history is trimmed to the newest values, not the oldest.
	long := spark([]float64{0, 0, 0, 0, 0, 0, 0, 0, 100, 100}, 2)
	if long != "\u2588\u2588" {
		t.Errorf("long history = %q, want the two newest values", long)
	}
	if n := visibleLen(spark(nil, 6)); n != 6 {
		t.Error("empty history must still fill the column")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{10 * 1024, "10K"},
		{8 << 30, "8.0G"},
		{450 << 30, "450G"},
	}
	for _, tc := range cases {
		got := humanBytes(tc.in)
		if got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
		// The overview budgets five characters for a byte count.
		if n := visibleLen(got); n > 5 {
			t.Errorf("humanBytes(%d) = %q is %d chars, want at most 5", tc.in, got, n)
		}
	}
}

func TestRelAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{3 * time.Second, "3s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
		// Clock skew between agent and collector should be visible, not
		// rounded away to "0s".
		{-5 * time.Second, "+5s"},
	}
	for _, tc := range cases {
		if got := relAge(tc.in); got != tc.want {
			t.Errorf("relAge(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVisibleLenIgnoresANSI(t *testing.T) {
	p := palette{enabled: true}
	plain := "94%"
	styled := p.forLevel(levelCrit, plain)

	if styled == plain {
		t.Fatal("palette did not style the string")
	}
	if got := visibleLen(styled); got != len(plain) {
		t.Errorf("visibleLen(%q) = %d, want %d", styled, got, len(plain))
	}
	// Multi-byte glyphs count as one cell each.
	if got := visibleLen("████"); got != 4 {
		t.Errorf("visibleLen of four blocks = %d, want 4", got)
	}
}

func TestPadToAccountsForANSI(t *testing.T) {
	p := palette{enabled: true}
	styled := p.forLevel(levelWarn, "78%")

	padded := padTo(styled, 10, alignRight)
	if n := visibleLen(padded); n != 10 {
		t.Errorf("visible width = %d, want 10", n)
	}
	if !strings.HasPrefix(padded, "       ") {
		t.Errorf("right-aligned cell = %q, want leading spaces", padded)
	}

	// Over-wide cells are left alone: truncating would cut escape codes in
	// half and leak styling into the rest of the line.
	long := strings.Repeat("x", 20)
	if got := padTo(long, 10, alignLeft); got != long {
		t.Errorf("padTo widened/truncated an over-wide cell: %q", got)
	}
}

func TestPaletteDisabledEmitsNoEscapes(t *testing.T) {
	p := palette{enabled: false}
	for _, s := range []string{p.dim("x"), p.bold("x"), p.header("x"), p.forLevel(levelCrit, "x")} {
		if s != "x" {
			t.Errorf("styled %q with colour disabled", s)
		}
	}
}

func TestOverviewPlainTextHasNoEscapes(t *testing.T) {
	hosts := []query.HostStatus{
		host("web-01", 4*time.Second, 12, 62, 48),
		host("db-01", 6*time.Second, 94, 91, 93),
	}
	out := render(t, func(b *strings.Builder) error { return Overview(b, hosts, testOptions()) })

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("plain output contains ANSI escapes:\n%s", out)
	}
	for _, want := range []string{"HOST", "web-01", "db-01", "94%", "12%", "/"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "1 critical") {
		t.Errorf("summary missing the critical count:\n%s", out)
	}
}

func TestOverviewColumnsAlign(t *testing.T) {
	// Colour on, which is where naive width maths breaks.
	o := testOptions()
	o.Color = true
	hosts := []query.HostStatus{
		host("a", 4*time.Second, 1, 2, 3),
		host("a-much-longer-host-id", 6*time.Second, 99, 99, 99),
		host("mid-01", 8*time.Second, 50, 50, 50),
	}
	out := render(t, func(b *strings.Builder) error { return Overview(b, hosts, o) })

	// Every data row must put its percentages in the same columns. Styled
	// cells carry invisible escape bytes, so this only holds if the layout
	// measured visible width rather than string length.
	var rows [][]int
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "%") || strings.Contains(line, "reporting") {
			continue // the summary footer also contains a %
		}
		plain := stripANSI(line)
		var at []int
		for i, r := range plain {
			if r == '%' {
				at = append(at, i)
			}
		}
		rows = append(rows, at)
	}

	if len(rows) != len(hosts) {
		t.Fatalf("found %d data rows, want %d:\n%s", len(rows), len(hosts), out)
	}
	for i, at := range rows[1:] {
		if len(at) != len(rows[0]) {
			t.Fatalf("row %d has %d percentages, row 0 has %d:\n%s", i+1, len(at), len(rows[0]), out)
		}
		for j := range at {
			if at[j] != rows[0][j] {
				t.Errorf("row %d percentage %d at column %d, row 0 has it at %d:\n%s",
					i+1, j, at[j], rows[0][j], out)
			}
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
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
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestOverviewFlagsStaleHost(t *testing.T) {
	// Numbers from an hour ago are history, not status: a quiet agent must
	// not read as a healthy one.
	hosts := []query.HostStatus{host("gone-01", time.Hour, 5, 5, 5)}
	out := render(t, func(b *strings.Builder) error { return Overview(b, hosts, testOptions()) })

	if !strings.Contains(out, "1 stale") {
		t.Errorf("summary missing the stale count:\n%s", out)
	}
	if !strings.Contains(out, "\u25cb") {
		t.Errorf("stale host not marked with a hollow glyph:\n%s", out)
	}
	if strings.Contains(out, "all healthy") {
		t.Errorf("stale host reported as healthy:\n%s", out)
	}
}

func TestOverviewHostWithNoSamples(t *testing.T) {
	// A registered host that has never reported: it must appear, not vanish.
	hosts := []query.HostStatus{{Host: protocol.Host{ID: "new-01"}}}
	out := render(t, func(b *strings.Builder) error { return Overview(b, hosts, testOptions()) })

	if !strings.Contains(out, "new-01") || !strings.Contains(out, "never reported") {
		t.Errorf("output = %s", out)
	}
}

func TestOverviewEmpty(t *testing.T) {
	out := render(t, func(b *strings.Builder) error { return Overview(b, nil, testOptions()) })
	if !strings.Contains(out, "No hosts") {
		t.Errorf("output = %q", out)
	}
}

func TestOverviewAllHealthy(t *testing.T) {
	hosts := []query.HostStatus{host("web-01", time.Second, 5, 10, 15)}
	out := render(t, func(b *strings.Builder) error { return Overview(b, hosts, testOptions()) })
	if !strings.Contains(out, "all healthy") {
		t.Errorf("output missing the healthy summary:\n%s", out)
	}
}

func TestOverviewMissingRates(t *testing.T) {
	// One sample means nothing to diff against, so throughput is unknown
	// rather than zero.
	h := host("web-01", time.Second, 5, 10, 15)
	h.Samples[0].Net[0].Rates = false
	out := render(t, func(b *strings.Builder) error { return Overview(b, []query.HostStatus{h}, testOptions()) })

	if strings.Contains(out, "/s") {
		t.Errorf("output shows a rate it cannot know:\n%s", out)
	}
	if !strings.Contains(out, placeholder) {
		t.Errorf("output missing the unknown placeholder:\n%s", out)
	}
}

func TestDetail(t *testing.T) {
	h := host("db-01", 6*time.Second, 94, 91, 93)
	h.Samples = append(h.Samples, h.Samples[0], h.Samples[0])
	h.Samples[1].CPU.BusyPct, h.Samples[2].CPU.BusyPct = 50, 20
	h.Samples[1].At = h.Samples[0].At.Add(-10 * time.Second)
	h.Samples[2].At = h.Samples[0].At.Add(-20 * time.Second)

	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })

	for _, want := range []string{
		"db-01", "db-01.example.com", "linux 6.1.0", "4 cores", "agent 0.1.0",
		"CPU", "MEM", "SWAP", "94%", "TREND", "FILESYSTEMS", "/", "ext4",
		"INTERFACES", "eth0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("plain detail contains ANSI escapes:\n%s", out)
	}
	// Load relative to core count is the number that means something.
	if !strings.Contains(out, "of 4 cores") {
		t.Errorf("detail missing load-per-core context:\n%s", out)
	}
}

func TestDetailNoSamples(t *testing.T) {
	h := query.HostStatus{Host: protocol.Host{ID: "new-01"}}
	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })
	if !strings.Contains(out, "never reported") {
		t.Errorf("output = %s", out)
	}
}

func TestDetailNoSwap(t *testing.T) {
	h := host("web-01", time.Second, 5, 10, 15)
	h.Samples[0].SwapTotalBytes, h.Samples[0].SwapUsedBytes = 0, 0
	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })

	if !strings.Contains(out, "none configured") {
		t.Errorf("a swapless host should say so, not show 0%%:\n%s", out)
	}
}

func TestWorstIgnoresUnknown(t *testing.T) {
	// A host with no disks reported is not thereby in a worse state than
	// its CPU and memory say.
	if got := worst(levelOK, levelUnknown); got != levelOK {
		t.Errorf("worst(ok, unknown) = %v, want ok", got)
	}
	if got := worst(levelWarn, levelCrit); got != levelCrit {
		t.Errorf("worst(warn, crit) = %v, want crit", got)
	}
	if got := worst(levelUnknown, levelWarn); got != levelWarn {
		t.Errorf("worst(unknown, warn) = %v, want warn", got)
	}
}

func TestLevelForThresholds(t *testing.T) {
	cases := []struct {
		pct  float64
		want level
	}{
		{0, levelOK},
		{74.9, levelOK},
		{WarnPct, levelWarn},
		{89.9, levelWarn},
		{CritPct, levelCrit},
		{100, levelCrit},
	}
	for _, tc := range cases {
		if got := levelFor(tc.pct); got != tc.want {
			t.Errorf("levelFor(%g) = %v, want %v", tc.pct, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("/var/lib/something/deep", 8); visibleLen(got) != 8 {
		t.Errorf("truncate = %q, width %d, want 8", got, visibleLen(got))
	}
	if got := truncate("/", 8); got != "/" {
		t.Errorf("truncate widened a short string: %q", got)
	}
}

func TestNoTrailingWhitespace(t *testing.T) {
	// Padding must be applied outside a cell's colour wrapper, or the
	// trailing spaces hide after the reset code where they cannot be
	// trimmed — invisible until someone diffs or greps the output.
	hosts := []query.HostStatus{
		host("a", 4*time.Second, 1, 2, 3),
		host("a-much-longer-host-id", 6*time.Second, 99, 99, 99),
	}

	for _, colour := range []bool{false, true} {
		o := testOptions()
		o.Color = colour

		out := render(t, func(b *strings.Builder) error { return Overview(b, hosts, o) })
		detail := render(t, func(b *strings.Builder) error { return Detail(b, &hosts[1], o) })

		for _, line := range strings.Split(out+detail, "\n") {
			if plain := stripANSI(line); plain != strings.TrimRight(plain, " \t") {
				t.Errorf("colour=%v: line has trailing whitespace: %q", colour, plain)
			}
		}
	}
}

// TestOverviewFlagsSteal is the signal the agent argued hardest for: a host
// losing CPU to other tenants looks idle by every other measure, so the
// overview has to say so explicitly.
func TestOverviewFlagsSteal(t *testing.T) {
	h := host("vps-01", time.Second, 20, 30, 40)
	h.Samples[0].CPU.StealPct = 22

	out := render(t, func(b *strings.Builder) error {
		return Overview(b, []query.HostStatus{h}, testOptions())
	})

	if !strings.Contains(out, "steal 22%") {
		t.Errorf("overview does not name the stolen time:\n%s", out)
	}
	// Every other number is healthy, so without steal grading this host
	// would show as fine.
	if strings.Contains(out, "all healthy") {
		t.Errorf("host with 22%% steal reported as healthy:\n%s", out)
	}
	if !strings.Contains(out, "1 critical") {
		t.Errorf("steal did not reach the summary:\n%s", out)
	}
}

func TestStealThresholds(t *testing.T) {
	cases := []struct {
		pct  float64
		want level
	}{
		{0, levelOK},
		{4.9, levelOK},
		{StealWarnPct, levelWarn},
		{StealCritPct, levelCrit},
	}
	for _, tc := range cases {
		if got := stealLevel(tc.pct); got != tc.want {
			t.Errorf("stealLevel(%g) = %v, want %v", tc.pct, got, tc.want)
		}
	}
	// Stolen time is graded far more harshly than ordinary use: 10% CPU is
	// nothing, 10% steal is capacity you are paying for and not getting.
	if levelFor(10) == stealLevel(10) {
		t.Error("steal is graded on the same scale as ordinary CPU use")
	}
}

// TestOverviewFlagsInodeExhaustion covers the other invisible-outage case:
// bytes free, inodes gone, writes failing.
func TestOverviewFlagsInodeExhaustion(t *testing.T) {
	h := host("web-01", time.Second, 10, 20, 10)
	h.Samples[0].Filesystems = []query.Filesystem{{
		MountPoint: "/var", FSType: "ext4",
		TotalBytes: 100 << 30, UsedBytes: 10 << 30, AvailableBytes: 90 << 30,
		InodesTotal: 1000, InodesUsed: 995,
	}}

	out := render(t, func(b *strings.Builder) error {
		return Overview(b, []query.HostStatus{h}, testOptions())
	})

	if !strings.Contains(out, "inodes") {
		t.Errorf("overview does not mention inodes:\n%s", out)
	}
	// The disk gauge must show the inode pressure, not the 10% byte usage.
	if !strings.Contains(out, "100%") && !strings.Contains(out, "99%") {
		t.Errorf("disk column shows byte usage, hiding the real limit:\n%s", out)
	}
	if strings.Contains(out, "all healthy") {
		t.Errorf("host out of inodes reported as healthy:\n%s", out)
	}
}

func TestOverviewFlagsNetErrors(t *testing.T) {
	h := host("web-01", time.Second, 10, 20, 30)
	h.Samples[0].Net[0].ErrorsDelta = 12

	out := render(t, func(b *strings.Builder) error {
		return Overview(b, []query.HostStatus{h}, testOptions())
	})
	if !strings.Contains(out, "net errs +12") {
		t.Errorf("moving error counters not flagged:\n%s", out)
	}
}

func TestOverviewHealthyHostHasNoFlags(t *testing.T) {
	h := host("web-01", time.Second, 10, 20, 30)
	out := render(t, func(b *strings.Builder) error {
		return Overview(b, []query.HostStatus{h}, testOptions())
	})

	for _, unwanted := range []string{"steal", "inodes", "net errs", "swap"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("healthy host flagged with %q:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "all healthy") {
		t.Errorf("output = %s", out)
	}
}

// TestUnderivedCPUShowsUnknown guards the "unknown is not zero" rule for
// the one case the counter protocol introduced: the first sample from a
// host has no predecessor to diff against.
func TestUnderivedCPUShowsUnknown(t *testing.T) {
	h := host("web-01", time.Second, 90, 20, 30)
	h.Samples[0].CPU = query.CPUUsage{Derived: false}

	out := render(t, func(b *strings.Builder) error {
		return Overview(b, []query.HostStatus{h}, testOptions())
	})
	if !strings.Contains(out, placeholder) {
		t.Errorf("undervied CPU not shown as unknown:\n%s", out)
	}

	detail := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })
	if !strings.Contains(detail, "needs two samples") {
		t.Errorf("detail does not explain the missing CPU figure:\n%s", detail)
	}
}

func TestCPUHistorySkipsUnderivedSamples(t *testing.T) {
	// A sample with no derived percentage must be left out of the
	// sparkline, not drawn as a zero, which would show a false trough.
	h := host("web-01", time.Second, 80, 20, 30)
	h.Samples = append(h.Samples, query.Snapshot{At: now.Add(-10 * time.Second)})

	got := cpuHistory(&h, 6)
	if len(got) != 1 || got[0] != 80 {
		t.Errorf("cpuHistory = %v, want just the derived value", got)
	}
}

func TestDetailShowsCPUBreakdown(t *testing.T) {
	h := host("db-01", time.Second, 60, 40, 30)
	h.Samples[0].CPU = query.CPUUsage{
		Derived: true, BusyPct: 60, UserPct: 35, SystemPct: 25, IowaitPct: 18, StealPct: 12,
	}

	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })

	for _, want := range []string{"user 35%", "sys 25%", "iowait 18%", "steal 12%"} {
		if !strings.Contains(out, want) {
			t.Errorf("breakdown missing %q:\n%s", want, out)
		}
	}
	// Worth saying out loud: the fix for stolen time is not on this box.
	if !strings.Contains(out, "oversold") {
		t.Errorf("detail does not explain what steal means:\n%s", out)
	}
}

func TestDetailShowsInodesAndAvailable(t *testing.T) {
	h := host("web-01", time.Second, 10, 50, 40)
	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })

	if !strings.Contains(out, "INODES") {
		t.Errorf("filesystem table has no inode column:\n%s", out)
	}
	if !strings.Contains(out, "available") {
		t.Errorf("memory line does not show available:\n%s", out)
	}
}

func TestDetailProcesses(t *testing.T) {
	h := host("web-01", time.Second, 10, 50, 40)
	user := "www-data"
	h.Processes = &query.ProcessSnapshot{
		At: now, Total: 284, Running: 3, Sleeping: 280, Blocked: 1,
		Top: []protocol.Process{
			{
				PID: 1234, Name: "nginx", Command: "nginx: worker process", State: "S",
				User: &user, CPUPct: 780, RSSBytes: 545259520, MemPct: 3.2,
				IO: &protocol.ProcessIO{ReadBytesPerSec: 1048576},
			},
			{PID: 99, Name: "kworker", State: "I", CPUPct: 0.1, RSSBytes: 4096},
		},
	}

	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })

	for _, want := range []string{"PROCESSES", "284 total", "1234", "www-data", "nginx: worker", "780.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("process table missing %q:\n%s", want, out)
		}
	}
	// "Not measured" must read differently from zero.
	if !strings.Contains(out, "n/a") {
		t.Errorf("unmeasured process io not distinguished from zero:\n%s", out)
	}
	if !strings.Contains(out, "?") {
		t.Errorf("unresolved user not distinguished from root:\n%s", out)
	}
	// The two traps anyone reading these numbers will otherwise hit.
	if !strings.Contains(out, "may exceed 100") || !strings.Contains(out, "double-counts") {
		t.Errorf("process table does not caveat CPU%% and RSS:\n%s", out)
	}
}

func TestDetailWithoutProcesses(t *testing.T) {
	h := host("web-01", time.Second, 10, 50, 40)
	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })
	if strings.Contains(out, "PROCESSES") {
		t.Errorf("process section rendered for an agent that sent none:\n%s", out)
	}
}

// TestMeterNeverEmptyForNonZero is a regression test: a value whose
// remainder rounds up to a whole cell must draw that cell, not fall through
// to empty. 12% of an 8-cell bar is 0.96 cells, which used to render as a
// completely empty gauge next to the text "12%".
func TestMeterNeverEmptyForNonZero(t *testing.T) {
	// An 8-cell bar with eighth-cell partials resolves to 1/64 of its
	// width, so anything above about 0.8% must leave a mark. Below that
	// there is genuinely no glyph to draw.
	empty := meter(0, 8)
	for pct := 1.0; pct <= 100; pct += 0.1 {
		if got := meter(pct, 8); got == empty {
			t.Fatalf("meter(%.1f) = %q, identical to the zero bar", pct, got)
		}
	}
	// And the inverse: zero really is empty.
	if empty != "░░░░░░░░" {
		t.Errorf("meter(0) = %q", empty)
	}
}

func TestMeterFillsCompletelyAt100(t *testing.T) {
	if got := meter(100, 8); got != "████████" {
		t.Errorf("meter(100) = %q, want a full bar", got)
	}
	// Just under full must still show some empty, or 99% and 100% look alike.
	if meter(99, 8) == meter(100, 8) {
		t.Error("99% and 100% render identically")
	}
}

// TestDetailShowsRunQueue covers the pairing the agent pointed at: iowait
// says the CPU is idle waiting on storage, procs_blocked says how many
// tasks are stuck doing the waiting.
func TestDetailShowsRunQueue(t *testing.T) {
	h := host("db-01", time.Second, 20, 40, 30)
	h.Samples[0].CPU.IowaitPct = 45
	h.Samples[0].ProcsRunning, h.Samples[0].ProcsBlocked = 2, 11

	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })

	if !strings.Contains(out, "blocked 11") {
		t.Errorf("detail does not show the blocked count:\n%s", out)
	}
	if !strings.Contains(out, "run 2") {
		t.Errorf("detail does not show the run queue:\n%s", out)
	}
	if !strings.Contains(out, "iowait 45%") {
		t.Errorf("detail does not show iowait beside it:\n%s", out)
	}
}

func TestDetailOmitsRunQueueWhenAbsent(t *testing.T) {
	h := host("web-01", time.Second, 20, 40, 30)
	out := render(t, func(b *strings.Builder) error { return Detail(b, &h, testOptions()) })
	if strings.Contains(out, "blocked") {
		t.Errorf("run queue rendered for an agent that sent none:\n%s", out)
	}
}
