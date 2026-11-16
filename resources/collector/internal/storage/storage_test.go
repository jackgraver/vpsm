package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

var base = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ticksAt builds a tick counter `step` windows along from a fixed origin.
// Each step advances the counters by exactly 1000 ticks, split so the
// derived percentages come out at round numbers: 40% busy, 20% user,
// 15% system, 10% iowait, 5% steal.
func ticksAt(step uint64) protocol.CPUTicks {
	return protocol.CPUTicks{
		User:    1000 + 200*step,
		Nice:    0,
		System:  200 + 100*step,
		Idle:    8000 + 500*step,
		Iowait:  500 + 100*step,
		IRQ:     50 + 50*step,
		SoftIRQ: 50,
		Steal:   200 + 50*step,
	}
}

// sample builds a reading at the given step: timestamps 10s apart, counters
// advanced by one window each.
func sample(step uint64) protocol.Sample {
	return protocol.Sample{
		At:          base.Add(time.Duration(step) * 10 * time.Second),
		ElapsedSecs: 10,
		UptimeSecs:  1000 + step*10,
		CPU:         protocol.CPU{Ticks: ticksAt(step), Load1: 1.5, Load5: 1.2, Load15: 1.0},
		Memory: protocol.Memory{
			TotalBytes: 8 << 30, UsedBytes: 4 << 30, AvailableBytes: 4 << 30,
			SwapTotalBytes: 2 << 30, SwapUsedBytes: 1 << 28,
		},
		Filesystems: []protocol.Filesystem{
			{
				Device: "/dev/sda1", MountPoint: "/", FSType: "ext4",
				TotalBytes: 100 << 30, UsedBytes: 48 << 30, AvailableBytes: 47 << 30,
				InodesTotal: 1000, InodesUsed: 100,
			},
			{
				Device: "/dev/sdb1", MountPoint: "/data", FSType: "xfs",
				TotalBytes: 500 << 30, UsedBytes: 450 << 30, AvailableBytes: 50 << 30,
				InodesTotal: 2000, InodesUsed: 200,
			},
		},
		Net: []protocol.Net{{
			Iface:    "eth0",
			RxBytes:  1_000_000 + 500_000*step,
			TxBytes:  500_000 + 100_000*step,
			RxErrors: 7, TxErrors: 0,
			RxDropped: 2, TxDropped: 0,
		}},
	}
}

func report(id string, steps ...uint64) *protocol.Report {
	r := &protocol.Report{Host: protocol.Host{ID: id, Cores: 4}}
	for _, step := range steps {
		r.Samples = append(r.Samples, sample(step))
	}
	return r
}

func save(t *testing.T, s *Store, r *protocol.Report) protocol.Response {
	t.Helper()
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got, err := s.SaveReport(context.Background(), r, base)
	if err != nil {
		t.Fatalf("SaveReport: %v", err)
	}
	return got
}

func TestSaveAndRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	r := report("web-01", 0, 1)
	r.Host.Hostname = "web-01.example.com"
	r.Host.AgentVersion = "0.1.0"
	if got := save(t, s, r); got.Accepted != 2 || got.Duplicate != 0 {
		t.Fatalf("SaveReport = %+v, want accepted 2", got)
	}

	hosts, err := s.Overview(ctx, 10)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("Overview returned %d hosts, want 1", len(hosts))
	}
	h := hosts[0]
	if h.Host.Hostname != "web-01.example.com" || h.Host.Cores != 4 {
		t.Errorf("host metadata = %+v", h.Host)
	}
	if !h.FirstSeen.Equal(base) || !h.LastSeen.Equal(base.Add(10*time.Second)) {
		t.Errorf("first/last seen = %s / %s", h.FirstSeen, h.LastSeen)
	}
	if len(h.Samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(h.Samples))
	}

	latest := h.Latest()
	if latest.Load1 != 1.5 {
		t.Errorf("Load1 = %v, want 1.5 (samples out of order?)", latest.Load1)
	}
	if got, want := latest.MemPct(), 50.0; got != want {
		t.Errorf("MemPct = %v, want %v", got, want)
	}
	if latest.ElapsedSecs != 10 {
		t.Errorf("ElapsedSecs = %v, want 10", latest.ElapsedSecs)
	}
}

func TestCPUDerivedFromTicks(t *testing.T) {
	s := testStore(t)
	save(t, s, report("web-01", 0, 1))

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	cpu := h.Latest().CPU
	if !cpu.Derived {
		t.Fatal("CPU not derived from two samples of ticks")
	}

	cases := []struct {
		name string
		got  float64
		want float64
	}{
		// Busy excludes iowait on purpose: a disk-bound box must not read
		// as CPU-bound.
		{"busy", cpu.BusyPct, 40},
		{"user", cpu.UserPct, 20},
		{"system", cpu.SystemPct, 15},
		{"iowait", cpu.IowaitPct, 10},
		{"steal", cpu.StealPct, 5},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// The oldest sample has nothing to diff against, so its percentages
	// are unknown rather than zero.
	if h.Samples[1].CPU.Derived {
		t.Error("oldest sample reports derived CPU, want unset")
	}
}

func TestCPUDerivationSpansAGap(t *testing.T) {
	s := testStore(t)

	// Steps 0 and 2: the report for step 1 never arrived. Diffing counters
	// across the gap still yields the correct average over the wider
	// window, which is the whole reason counters travel instead of rates.
	save(t, s, report("web-01", 0, 2))

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	cpu := h.Latest().CPU
	if !cpu.Derived {
		t.Fatal("CPU not derived across a delivery gap")
	}
	if cpu.BusyPct != 40 {
		t.Errorf("BusyPct across gap = %v, want 40", cpu.BusyPct)
	}
	if cpu.StealPct != 5 {
		t.Errorf("StealPct across gap = %v, want 5", cpu.StealPct)
	}
}

func TestNetRatesAndErrorDeltas(t *testing.T) {
	s := testStore(t)

	r := report("web-01", 0, 1)
	// Errors move between the two samples; drops do not.
	r.Samples[1].Net[0].RxErrors = 9
	save(t, s, r)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	n := h.Latest().Net[0]
	if !n.Rates {
		t.Fatal("no rates derived")
	}
	// 500_000 bytes over 10s.
	if n.RxPerSec != 50_000 {
		t.Errorf("RxPerSec = %v, want 50000", n.RxPerSec)
	}
	if n.TxPerSec != 10_000 {
		t.Errorf("TxPerSec = %v, want 10000", n.TxPerSec)
	}
	if n.ErrorsDelta != 2 {
		t.Errorf("ErrorsDelta = %d, want 2", n.ErrorsDelta)
	}
	if n.DroppedDelta != 0 {
		t.Errorf("DroppedDelta = %d, want 0", n.DroppedDelta)
	}
	if got := h.Latest().NetProblems(); got != 2 {
		t.Errorf("NetProblems = %d, want 2", got)
	}
}

func TestRebootSuppressesEverythingDerived(t *testing.T) {
	s := testStore(t)

	r := report("web-01", 0, 1)
	// Uptime drops and every kernel counter restarts: the host rebooted,
	// so no diff across this boundary means anything.
	r.Samples[1].UptimeSecs = 12
	r.Samples[1].CPU.Ticks = protocol.CPUTicks{User: 10, Idle: 90}
	r.Samples[1].Net[0].RxBytes = 4_000
	r.Samples[1].Net[0].TxBytes = 1_000
	save(t, s, r)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Latest().CPU.Derived {
		t.Error("CPU derived across a reboot, want unset")
	}
	if h.Latest().Net[0].Rates {
		t.Error("net rates derived across a reboot, want unset")
	}
}

func TestDuplicateSamplesDropped(t *testing.T) {
	s := testStore(t)
	save(t, s, report("web-01", 0))

	// An agent retrying a flush it was unsure landed, with one new sample.
	got := save(t, s, report("web-01", 0, 1))
	if got.Accepted != 1 || got.Duplicate != 1 {
		t.Errorf("retry = %+v, want accepted 1 duplicate 1", got)
	}

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if len(h.Samples) != 2 {
		t.Errorf("stored %d samples, want 2", len(h.Samples))
	}
}

func TestFilesystemPercentages(t *testing.T) {
	s := testStore(t)
	save(t, s, report("web-01", 0))

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	fs := h.Latest().Filesystems
	if len(fs) != 2 {
		t.Fatalf("got %d filesystems, want 2", len(fs))
	}

	// df-style: used / (used + available), which excludes root's reserve.
	// 450 / (450 + 50) = 90%, not 450/500 = 90%... pick the root mount,
	// where the reserve actually shows: 48 / (48 + 47) = 50.5%, where
	// used/total would say 48%.
	root := fs[0]
	if root.MountPoint != "/" {
		t.Fatalf("filesystems not ordered by mount point: %+v", fs)
	}
	if got := root.BytesPct(); got < 50.5 || got > 50.6 {
		t.Errorf("BytesPct = %v, want ~50.53 (df-style, excluding reserved blocks)", got)
	}
	if got := root.InodesPct(); got != 10 {
		t.Errorf("InodesPct = %v, want 10", got)
	}
	if root.InodeBound() {
		t.Error("root reported as inode-bound, but bytes are the limit here")
	}
}

func TestInodeExhaustionIsVisible(t *testing.T) {
	s := testStore(t)

	// A filesystem with plenty of bytes free and no inodes left still
	// fails writes, and nothing in the byte figures hints at why.
	r := report("web-01", 0)
	r.Samples[0].Filesystems = []protocol.Filesystem{{
		MountPoint: "/var", TotalBytes: 100 << 30, UsedBytes: 10 << 30, AvailableBytes: 90 << 30,
		InodesTotal: 1000, InodesUsed: 990,
	}}
	save(t, s, r)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	f, ok := h.Latest().Busiest()
	if !ok {
		t.Fatal("no filesystem returned")
	}
	if !f.InodeBound() {
		t.Error("filesystem not reported as inode-bound")
	}
	if got := f.Pressure(); got != 99 {
		t.Errorf("Pressure = %v, want 99 (inodes, not bytes)", got)
	}
}

func TestBusiestPicksWorstPressure(t *testing.T) {
	s := testStore(t)
	save(t, s, report("web-01", 0))

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	f, ok := h.Latest().Busiest()
	if !ok || f.MountPoint != "/data" {
		t.Errorf("Busiest = %+v (%v), want /data", f, ok)
	}
}

func TestProcessSnapshot(t *testing.T) {
	s := testStore(t)

	user := "www-data"
	r := report("web-01", 0, 1)
	r.Samples[1].Processes = &protocol.Processes{
		Total: 284, Threads: 1104, Running: 3, Sleeping: 280, Blocked: 1,
		Top: []protocol.Process{
			{
				PID: 1234, PPID: 1, Name: "nginx", Command: "nginx: worker process",
				State: "S", UID: 33, User: &user, Threads: 1,
				// A share of one CPU: 780 on a multi-core box is real.
				CPUPct: 780, RSSBytes: 545259520, MemPct: 3.2,
				IO: &protocol.ProcessIO{ReadBytesPerSec: 1048576, WriteBytesPerSec: 0},
			},
			{
				// Unresolvable uid and unmeasurable io both stay null.
				PID: 99, Name: "kworker", State: "I", UID: 0,
				CPUPct: 0.1, RSSBytes: 4096,
			},
		},
	}
	save(t, s, r)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes == nil {
		t.Fatal("no process snapshot stored")
	}
	if h.Processes.Total != 284 || h.Processes.Running != 3 {
		t.Errorf("counts = %+v", h.Processes)
	}
	if !h.Processes.At.Equal(base.Add(10 * time.Second)) {
		t.Errorf("snapshot at = %s, want the newest sample's time", h.Processes.At)
	}
	if len(h.Processes.Top) != 2 {
		t.Fatalf("got %d processes, want 2", len(h.Processes.Top))
	}

	// Order is preserved: the agent sends it worst-CPU-first.
	first := h.Processes.Top[0]
	if first.PID != 1234 {
		t.Errorf("first process = %d, want 1234", first.PID)
	}
	if first.CPUPct != 780 {
		t.Errorf("CPUPct = %v, want 780 — a per-CPU share must not be clamped", first.CPUPct)
	}
	if first.User == nil || *first.User != "www-data" {
		t.Errorf("User = %v, want www-data", first.User)
	}
	if first.IO == nil || first.IO.ReadBytesPerSec != 1048576 {
		t.Errorf("IO = %+v", first.IO)
	}

	// Null means "not measured", and must not arrive as zero.
	second := h.Processes.Top[1]
	if second.User != nil {
		t.Errorf("User = %v, want nil for an unresolved uid", *second.User)
	}
	if second.IO != nil {
		t.Errorf("IO = %+v, want nil when ptrace denied it", second.IO)
	}
}

func TestProcessSnapshotReplaced(t *testing.T) {
	s := testStore(t)

	first := report("web-01", 0)
	first.Samples[0].Processes = &protocol.Processes{
		Total: 100,
		Top:   []protocol.Process{{PID: 1, Name: "old"}, {PID: 2, Name: "older"}},
	}
	save(t, s, first)

	second := report("web-01", 1)
	second.Samples[0].Processes = &protocol.Processes{
		Total: 200,
		Top:   []protocol.Process{{PID: 3, Name: "new"}},
	}
	save(t, s, second)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes.Total != 200 {
		t.Errorf("Total = %d, want 200", h.Processes.Total)
	}
	// Replaced wholesale, not merged: process identity does not survive
	// between samples, so there is nothing to merge against.
	if len(h.Processes.Top) != 1 || h.Processes.Top[0].Name != "new" {
		t.Errorf("Top = %+v, want just the new list", h.Processes.Top)
	}
}

func TestBackfillDoesNotOverwriteNewerProcesses(t *testing.T) {
	s := testStore(t)

	current := report("web-01", 5)
	current.Samples[0].Processes = &protocol.Processes{Total: 200, Top: []protocol.Process{{PID: 3, Name: "current"}}}
	save(t, s, current)

	// An agent flushing a buffer after an outage delivers older samples.
	// They must not clobber the current process list.
	stale := report("web-01", 0, 1)
	stale.Samples[0].Processes = &protocol.Processes{Total: 100, Top: []protocol.Process{{PID: 1, Name: "stale"}}}
	save(t, s, stale)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes.Total != 200 {
		t.Errorf("Total = %d, want 200 — a backfill overwrote the current snapshot", h.Processes.Total)
	}
	if len(h.Processes.Top) != 1 || h.Processes.Top[0].Name != "current" {
		t.Errorf("Top = %+v, want the current list", h.Processes.Top)
	}
}

func TestOverviewOmitsProcessList(t *testing.T) {
	s := testStore(t)

	r := report("web-01", 0)
	r.Samples[0].Processes = &protocol.Processes{
		Total: 284,
		Top:   []protocol.Process{{PID: 1, Name: "nginx"}},
	}
	save(t, s, r)

	hosts, err := s.Overview(context.Background(), 10)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	p := hosts[0].Processes
	if p == nil {
		t.Fatal("overview dropped the process counts")
	}
	if p.Total != 284 {
		t.Errorf("Total = %d, want 284", p.Total)
	}
	// The counts are cheap and useful in a fleet view; the list is not.
	if len(p.Top) != 0 {
		t.Errorf("overview loaded %d process rows, want 0", len(p.Top))
	}
}

func TestHostWithoutProcessesHasNilSnapshot(t *testing.T) {
	s := testStore(t)
	save(t, s, report("web-01", 0))

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes != nil {
		t.Errorf("Processes = %+v, want nil for an agent that sent none", h.Processes)
	}
}

func TestHistoryLimitIsPerHost(t *testing.T) {
	s := testStore(t)

	for _, id := range []string{"web-01", "db-01"} {
		save(t, s, report(id, 0, 1, 2, 3, 4))
	}

	hosts, err := s.Overview(context.Background(), 2)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2", len(hosts))
	}
	if hosts[0].Host.ID != "db-01" {
		t.Errorf("hosts[0] = %q, want db-01 (ordered by id)", hosts[0].Host.ID)
	}
	for _, h := range hosts {
		if len(h.Samples) != 2 {
			t.Errorf("%s: got %d samples, want 2", h.Host.ID, len(h.Samples))
		}
		if !h.Latest().At.Equal(base.Add(40 * time.Second)) {
			t.Errorf("%s: latest is %s, want the newest", h.Host.ID, h.Latest().At)
		}
		if len(h.Latest().Filesystems) != 2 {
			t.Errorf("%s: latest has %d filesystems, want 2", h.Host.ID, len(h.Latest().Filesystems))
		}
	}
}

func TestHostMetadataNotErasedByOmission(t *testing.T) {
	s := testStore(t)

	full := protocol.Host{ID: "web-01", Hostname: "web-01.example.com", OS: "linux", Kernel: "6.1.0", Cores: 4, AgentVersion: "0.1.0"}
	r := report("web-01", 0)
	r.Host = full
	save(t, s, r)

	// A later report that only carries the id must not wipe what we know.
	sparse := report("web-01", 1)
	sparse.Host = protocol.Host{ID: "web-01"}
	save(t, s, sparse)

	h, err := s.Host(context.Background(), "web-01", 1)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Host != full {
		t.Errorf("host metadata = %+v, want %+v", h.Host, full)
	}
}

func TestBackfillWidensFirstSeen(t *testing.T) {
	s := testStore(t)
	save(t, s, report("web-01", 5))

	// An agent flushing a buffer of older readings after an outage.
	older := report("web-01", 0)
	older.Samples[0].At = base.Add(-time.Hour)
	save(t, s, older)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if !h.FirstSeen.Equal(base.Add(-time.Hour)) {
		t.Errorf("FirstSeen = %s, want an hour before base", h.FirstSeen)
	}
	if !h.LastSeen.Equal(base.Add(50 * time.Second)) {
		t.Errorf("LastSeen = %s, want the newest sample", h.LastSeen)
	}
}

func TestUnknownHost(t *testing.T) {
	s := testStore(t)
	if _, err := s.Host(context.Background(), "nope", 1); !errors.Is(err, query.ErrNoHost) {
		t.Errorf("Host(nope) error = %v, want ErrNoHost", err)
	}
}

func TestPruneCascades(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	old := report("web-01", 0)
	old.Samples[0].At = base.Add(-48 * time.Hour)
	save(t, s, old)
	save(t, s, report("web-01", 1))

	n, err := s.Prune(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("Prune removed %d, want 1", n)
	}

	// The host itself stays listed even once its samples age out.
	h, err := s.Host(ctx, "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if len(h.Samples) != 1 {
		t.Errorf("got %d samples after prune, want 1", len(h.Samples))
	}

	var orphans int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM filesystems WHERE sample_id NOT IN (SELECT id FROM samples)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned filesystem rows after prune, want 0 (cascade not firing?)", orphans)
	}
}

func TestPruneKeepsProcessSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	r := report("web-01", 0)
	r.Samples[0].At = base.Add(-48 * time.Hour)
	r.Samples[0].Processes = &protocol.Processes{Total: 100, Top: []protocol.Process{{PID: 1, Name: "nginx"}}}
	save(t, s, r)

	if _, err := s.Prune(ctx, base.Add(-24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// A quiet host's last known process list is exactly what an operator
	// wants to still be able to look at.
	h, err := s.Host(ctx, "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes == nil || len(h.Processes.Top) != 1 {
		t.Errorf("process snapshot = %+v, want it kept across a prune", h.Processes)
	}
}

func TestSchemaVersionGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vpsm.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	s.Close()

	// Reopening a database this collector wrote is fine.
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s.Close()

	// One written by a different version is refused, rather than failing
	// later on a column that moved.
	bump, err := Open(path)
	if err != nil {
		t.Fatalf("open to bump: %v", err)
	}
	if _, err := bump.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	bump.Close()

	if _, err := Open(path); err == nil {
		t.Error("Open accepted a database from a different schema version")
	}
}

// TestMixedBatchProcessPresence is the case the agent flagged: to keep a
// buffered flush small it strips process lists from every sample except the
// newest, so a single batch arrives with the field present on some samples
// and absent on others. Storage must take the newest list present and not
// care about the gaps.
func TestMixedBatchProcessPresence(t *testing.T) {
	s := testStore(t)

	r := report("web-01", 0, 1, 2, 3)
	// Only the newest carries a list, which is what a stripped flush looks
	// like after an outage.
	r.Samples[3].Processes = &protocol.Processes{
		Total: 284,
		Top:   []protocol.Process{{PID: 1, Name: "nginx"}},
	}
	if got := save(t, s, r); got.Accepted != 4 {
		t.Fatalf("accepted %d samples, want 4", got.Accepted)
	}

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes == nil {
		t.Fatal("no process snapshot stored from a stripped batch")
	}
	if h.Processes.Total != 284 || len(h.Processes.Top) != 1 {
		t.Errorf("snapshot = %+v", h.Processes)
	}
	// Stamped with the sample it came from, not with the batch or the
	// receive time.
	if !h.Processes.At.Equal(base.Add(30 * time.Second)) {
		t.Errorf("snapshot at = %s, want the newest sample's time", h.Processes.At)
	}
}

// TestProcessesOnOlderSampleStillLands covers the inverse: if the newest
// sample happens to carry no list but an older one does, the newest list
// available is still the right thing to keep.
func TestProcessesOnOlderSampleStillLands(t *testing.T) {
	s := testStore(t)

	r := report("web-01", 0, 1, 2)
	r.Samples[1].Processes = &protocol.Processes{
		Total: 150,
		Top:   []protocol.Process{{PID: 7, Name: "redis"}},
	}
	save(t, s, r)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes == nil || h.Processes.Total != 150 {
		t.Fatalf("snapshot = %+v, want the one list in the batch", h.Processes)
	}
	if !h.Processes.At.Equal(base.Add(10 * time.Second)) {
		t.Errorf("snapshot at = %s, want the time of the sample carrying it", h.Processes.At)
	}
}

// TestStrippedBatchDoesNotClearProcesses guards the steady-state case
// against the stripping change: an agent that sends no list at all in a
// later report must not wipe the one we already hold.
func TestStrippedBatchDoesNotClearProcesses(t *testing.T) {
	s := testStore(t)

	first := report("web-01", 0)
	first.Samples[0].Processes = &protocol.Processes{
		Total: 284,
		Top:   []protocol.Process{{PID: 1, Name: "nginx"}},
	}
	save(t, s, first)

	// A later report with process sampling off, or simply stripped.
	save(t, s, report("web-01", 1, 2))

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes == nil || len(h.Processes.Top) != 1 {
		t.Errorf("process snapshot = %+v, want it retained", h.Processes)
	}
}

// TestProcsGaugesAreASeries covers the fix prompted by the agent's review.
// procs_running / procs_blocked are host gauges carried on every sample,
// not part of the snapshot-only process block: paired with CPU iowait,
// blocked is what separates "stuck on storage" from "busy", and that only
// reads as an answer over time.
func TestProcsGaugesAreASeries(t *testing.T) {
	s := testStore(t)

	r := report("web-01", 0, 1, 2)
	r.Samples[0].ProcsRunning, r.Samples[0].ProcsBlocked = 2, 0
	r.Samples[1].ProcsRunning, r.Samples[1].ProcsBlocked = 3, 7
	r.Samples[2].ProcsRunning, r.Samples[2].ProcsBlocked = 1, 12
	// No process block at all: these must survive without it.
	save(t, s, r)

	h, err := s.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes != nil {
		t.Fatalf("process snapshot = %+v, want nil", h.Processes)
	}
	if len(h.Samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(h.Samples))
	}

	// Newest first.
	want := []struct{ running, blocked int }{{1, 12}, {3, 7}, {2, 0}}
	for i, w := range want {
		got := h.Samples[i]
		if got.ProcsRunning != w.running || got.ProcsBlocked != w.blocked {
			t.Errorf("sample %d = run %d blocked %d, want run %d blocked %d",
				i, got.ProcsRunning, got.ProcsBlocked, w.running, w.blocked)
		}
	}
}
