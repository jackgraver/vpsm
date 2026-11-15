package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validSample() Sample {
	return Sample{
		At:          time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC),
		ElapsedSecs: 5,
		UptimeSecs:  1000,
		CPU: CPU{
			Ticks: CPUTicks{User: 1000, System: 200, Idle: 8000, Iowait: 500, Steal: 100},
			Load1: 1.5,
		},
		Memory: Memory{TotalBytes: 100, UsedBytes: 50, AvailableBytes: 50},
		Filesystems: []Filesystem{{
			MountPoint: "/", FSType: "ext4",
			TotalBytes: 100, UsedBytes: 10, AvailableBytes: 85,
			InodesTotal: 1000, InodesUsed: 100,
		}},
		Net: []Net{{Iface: "eth0", RxBytes: 1, TxBytes: 2, RxErrors: 7}},
	}
}

func TestValidateAcceptsAGoodReport(t *testing.T) {
	r := Report{Host: Host{ID: "web-01", Cores: 4}, Samples: []Sample{validSample()}}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want string
	}{
		{"no host id", Report{Samples: []Sample{validSample()}}, "host.id is required"},
		{"no samples", Report{Host: Host{ID: "a"}}, "no samples"},
		{
			"oversized host id",
			Report{Host: Host{ID: strings.Repeat("x", MaxHostIDLen+1)}, Samples: []Sample{validSample()}},
			"limit is",
		},
		{
			"control char in host id",
			Report{Host: Host{ID: "web\x1b[2J01"}, Samples: []Sample{validSample()}},
			"control characters",
		},
		{"negative cores", Report{Host: Host{ID: "a", Cores: -1}, Samples: []Sample{validSample()}}, "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsBadSamples(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Sample)
		want   string
	}{
		{"no timestamp", func(s *Sample) { s.At = time.Time{} }, "at is required"},
		{"negative elapsed", func(s *Sample) { s.ElapsedSecs = -1 }, "elapsed_secs"},
		{"memory over total", func(s *Sample) { s.Memory.UsedBytes = 101 }, "exceeds total_bytes"},
		{"available over total", func(s *Sample) { s.Memory.AvailableBytes = 101 }, "available_bytes"},
		{"swap over total", func(s *Sample) { s.Memory.SwapUsedBytes = 1 }, "swap_used_bytes"},
		{"filesystem without mount", func(s *Sample) { s.Filesystems[0].MountPoint = "" }, "mount_point is required"},
		{"filesystem over total", func(s *Sample) { s.Filesystems[0].UsedBytes = 1000 }, "exceeds total_bytes"},
		{"inodes over total", func(s *Sample) { s.Filesystems[0].InodesUsed = 9999 }, "inodes_used"},
		{"iface without name", func(s *Sample) { s.Net[0].Iface = "" }, "net.iface is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSample()
			tc.mutate(&s)
			r := Report{Host: Host{ID: "a"}, Samples: []Sample{s}}
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
			// The index tells an agent operator which reading was at fault.
			if !strings.Contains(err.Error(), "sample 0") {
				t.Errorf("err = %q, want it to name the sample index", err)
			}
		})
	}
}

// TestCPUTicksAreNotRangeChecked guards the decision to carry counters: a
// tick total is unbounded and means nothing on its own, so there is no
// range to validate and no reason to reject a sample for its value.
func TestCPUTicksAreNotRangeChecked(t *testing.T) {
	s := validSample()
	s.CPU.Ticks = CPUTicks{User: 1 << 62, Idle: 1 << 62}
	r := Report{Host: Host{ID: "a"}, Samples: []Sample{s}}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate rejected large tick counters: %v", err)
	}

	// A sample with no ticks at all is also fine: the collector renders a
	// percentage it cannot derive as unknown rather than refusing the data.
	s.CPU.Ticks = CPUTicks{}
	r = Report{Host: Host{ID: "a"}, Samples: []Sample{s}}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate rejected a sample with no ticks: %v", err)
	}
}

func TestCPUTicksArithmetic(t *testing.T) {
	ticks := CPUTicks{User: 100, Nice: 10, System: 50, Idle: 700, Iowait: 100, IRQ: 20, SoftIRQ: 10, Steal: 10}
	if got, want := ticks.Total(), uint64(1000); got != want {
		t.Errorf("Total = %d, want %d", got, want)
	}
	// Busy excludes iowait deliberately, so a disk-bound box does not read
	// as CPU-bound.
	if got, want := ticks.Busy(), uint64(200); got != want {
		t.Errorf("Busy = %d, want %d", got, want)
	}
}

// TestProcessCPUIsNotClampedTo100 is the case the agent warned about: a
// process's CPU share is per single core, so a multi-threaded process on a
// many-core box legitimately reports several hundred percent. Validating it
// against 100 would reject real data.
func TestProcessCPUIsNotClampedTo100(t *testing.T) {
	s := validSample()
	s.Processes = &Processes{
		Total: 10,
		Top:   []Process{{PID: 1, Name: "builder", CPUPct: 780}},
	}
	r := Report{Host: Host{ID: "a"}, Samples: []Sample{s}}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate rejected a 780%% process: %v", err)
	}
}

func TestValidateRejectsBadProcesses(t *testing.T) {
	cases := []struct {
		name string
		p    Processes
		want string
	}{
		{"negative cpu", Processes{Top: []Process{{PID: 1, CPUPct: -1}}}, "negative"},
		{"negative pid", Processes{Top: []Process{{PID: -1}}}, "pid is negative"},
		{
			"oversized list",
			Processes{Top: make([]Process, MaxTopProcesses+1)},
			"limit is",
		},
		{
			"oversized command",
			Processes{Top: []Process{{PID: 1, Command: strings.Repeat("x", MaxCommandLen+1)}}},
			"limit is",
		},
		{
			// A command line comes from the monitored box and is printed
			// into an operator's terminal.
			"escape sequence in command",
			Processes{Top: []Process{{PID: 1, Command: "evil\x1b[2Jthing"}}},
			"control characters",
		},
		{
			"escape sequence in name",
			Processes{Top: []Process{{PID: 1, Name: "evil\x1b[2J"}}},
			"control characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSample()
			s.Processes = &tc.p
			r := Report{Host: Host{ID: "a"}, Samples: []Sample{s}}
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsOversizedBatch(t *testing.T) {
	r := Report{Host: Host{ID: "a"}, Samples: make([]Sample, MaxSamplesPerReport+1)}
	for i := range r.Samples {
		r.Samples[i] = validSample()
	}
	if err := r.Validate(); err == nil {
		t.Fatal("Validate accepted an oversized batch")
	}
}

// TestWireFormatIsStable guards the field names on the wire. Renaming one is
// a protocol change that breaks every deployed mini-bot, so it should take a
// deliberate test update rather than slipping through a refactor.
func TestWireFormatIsStable(t *testing.T) {
	s := validSample()
	user := "www-data"
	s.Processes = &Processes{
		Total: 1,
		Top: []Process{{
			PID: 1, Name: "nginx", User: &user, CPUPct: 1, RSSBytes: 2,
			IO: &ProcessIO{ReadBytesPerSec: 1},
		}},
	}
	raw, err := json.Marshal(Report{Host: Host{ID: "web-01", Cores: 4}, Samples: []Sample{s}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	for _, want := range []string{
		`"host"`, `"id"`, `"cores"`, `"samples"`, `"at"`, `"elapsed_secs"`, `"uptime_secs"`,
		`"cpu"`, `"ticks"`, `"user"`, `"system"`, `"idle"`, `"iowait"`, `"steal"`,
		`"memory"`, `"total_bytes"`, `"used_bytes"`, `"available_bytes"`,
		`"filesystems"`, `"mount_point"`, `"fs_type"`, `"inodes_total"`, `"inodes_used"`,
		`"net"`, `"iface"`, `"rx_bytes"`, `"tx_bytes"`, `"rx_errors"`,
		`"processes"`, `"top"`, `"pid"`, `"cpu_pct"`, `"rss_bytes"`, `"io"`, `"read_bytes_per_sec"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire format missing %s:\n%s", want, raw)
		}
	}
}

// TestNullsSurviveTheWire protects the distinction the agent was explicit
// about: a null user or null io means "could not measure", and must not
// round-trip into a zero.
func TestNullsSurviveTheWire(t *testing.T) {
	body := `{"host":{"id":"web-01"},"samples":[{
		"at":"2026-11-14T12:00:00Z",
		"processes":{"total":2,"top":[
			{"pid":1,"name":"a","user":"root","io":{"read_bytes_per_sec":5,"write_bytes_per_sec":6}},
			{"pid":2,"name":"b"}
		]}
	}]}`

	var r Report
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	top := r.Samples[0].Processes.Top
	if top[0].User == nil || *top[0].User != "root" {
		t.Errorf("User = %v, want root", top[0].User)
	}
	if top[0].IO == nil || top[0].IO.ReadBytesPerSec != 5 {
		t.Errorf("IO = %+v", top[0].IO)
	}
	if top[1].User != nil {
		t.Errorf("absent user decoded to %q, want nil", *top[1].User)
	}
	if top[1].IO != nil {
		t.Errorf("absent io decoded to %+v, want nil", top[1].IO)
	}
}

// TestUnknownFieldsAreIgnored keeps a newer agent compatible with an older
// collector: extra fields must not fail the whole report.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	body := `{"host":{"id":"web-01","future_field":"x"},
		"samples":[{"at":"2026-11-14T12:00:00Z","cpu":{"ticks":{"user":1}},"gpu":{"pct":50}}]}`

	var r Report
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if r.Host.ID != "web-01" || len(r.Samples) != 1 {
		t.Errorf("decoded = %+v", r)
	}
}

func TestReportsPath(t *testing.T) {
	if ReportsPath != "/v1/reports" {
		t.Errorf("ReportsPath = %q, want /v1/reports", ReportsPath)
	}
}
