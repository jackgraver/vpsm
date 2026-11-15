// Package query defines the read side of the collector: the shapes the
// display layer consumes, and the Source interface it reads them from.
//
// Two things implement Source — the local SQLite store and an HTTP client
// pointed at a remote collector — so the CLI renders the same views whether
// it is run on the collector box or from a laptop. Keeping these types here
// rather than in storage is what keeps that split free of import cycles.
//
// Everything the agent sends as a counter has already been turned into a
// rate by the time it reaches these types; see storage.derive.
package query

import (
	"context"
	"time"

	"vpsm-collector/internal/protocol"
)

// Source is somewhere host statistics can be read from.
type Source interface {
	// Overview returns every known host, each carrying up to history of its
	// most recent samples (newest first). Hosts with no samples at all are
	// included, with an empty Samples slice. Process counts are included;
	// the per-process list is not, since a fleet view has no room for it.
	Overview(ctx context.Context, history int) ([]HostStatus, error)

	// Host returns one host by id, including its per-process list. It
	// returns ErrNoHost if the id is unknown.
	Host(ctx context.Context, id string, history int) (*HostStatus, error)
}

// HostStatus is one host and its recent readings.
type HostStatus struct {
	Host      protocol.Host `json:"host"`
	FirstSeen time.Time     `json:"first_seen"`
	LastSeen  time.Time     `json:"last_seen"`

	// Samples are ordered newest first, so Samples[0] is current state.
	Samples []Snapshot `json:"samples"`

	// Processes is the newest process list the host reported, or nil if it
	// has never sent one. It hangs off the host rather than off a sample
	// because the collector keeps only the latest — see storage.
	Processes *ProcessSnapshot `json:"processes,omitempty"`
}

// Latest is the most recent reading, or nil if the host has reported none.
func (h *HostStatus) Latest() *Snapshot {
	if len(h.Samples) == 0 {
		return nil
	}
	return &h.Samples[0]
}

// Snapshot is a stored sample, with every rate the collector could derive
// from the sample before it already applied.
type Snapshot struct {
	At         time.Time `json:"at"`
	ReceivedAt time.Time `json:"received_at"`
	UptimeSecs uint64    `json:"uptime_secs"`

	// ElapsedSecs is the window the agent measured over, which is not
	// necessarily the interval it was configured with.
	ElapsedSecs float64 `json:"elapsed_secs,omitempty"`

	// ProcsRunning and ProcsBlocked are host gauges, kept per sample so the
	// run queue and the uninterruptible-sleep count have history.
	ProcsRunning int `json:"procs_running,omitempty"`
	ProcsBlocked int `json:"procs_blocked,omitempty"`

	CPU    CPUUsage `json:"cpu"`
	Load1  float64  `json:"load1"`
	Load5  float64  `json:"load5"`
	Load15 float64  `json:"load15"`

	MemTotalBytes     uint64 `json:"mem_total_bytes"`
	MemUsedBytes      uint64 `json:"mem_used_bytes"`
	MemAvailableBytes uint64 `json:"mem_available_bytes,omitempty"`
	SwapTotalBytes    uint64 `json:"swap_total_bytes"`
	SwapUsedBytes     uint64 `json:"swap_used_bytes"`

	Filesystems []Filesystem `json:"filesystems,omitempty"`
	Net         []Iface      `json:"net,omitempty"`

	// Ticks are the raw counters this snapshot's percentages were derived
	// from. They stay out of the JSON: derivation happens before anything
	// crosses the wire, so a remote reader has no use for them.
	Ticks protocol.CPUTicks `json:"-"`
}

// CPUUsage is CPU time as percentages, derived by diffing tick counters
// against the previous sample.
type CPUUsage struct {
	// Derived is false when there was nothing to diff against: the oldest
	// sample in the window, a reboot, or an agent that sent no ticks. The
	// percentages below are meaningless unless this is true, and the
	// display shows them as unknown rather than as zero.
	Derived bool `json:"derived"`

	// BusyPct excludes iowait, so a disk-bound box does not read as
	// CPU-bound.
	BusyPct   float64 `json:"busy_pct"`
	UserPct   float64 `json:"user_pct"`
	SystemPct float64 `json:"system_pct"`
	IowaitPct float64 `json:"iowait_pct"`

	// StealPct is time the hypervisor gave to another guest. On rented
	// hardware this is frequently the only evidence that the box is
	// oversold and the fix is to move provider, not to optimise anything.
	StealPct float64 `json:"steal_pct"`
}

// Filesystem is one mounted filesystem's usage.
type Filesystem struct {
	Device     string `json:"device,omitempty"`
	MountPoint string `json:"mount_point"`
	FSType     string `json:"fs_type,omitempty"`

	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes,omitempty"`

	InodesTotal uint64 `json:"inodes_total,omitempty"`
	InodesUsed  uint64 `json:"inodes_used,omitempty"`
}

// BytesPct is how full the filesystem is, computed the way df does it:
// used / (used + available), which excludes the blocks reserved for root.
// That is why a filesystem can legitimately report 100% with bytes left.
//
// When the agent sent no available figure this falls back to used / total,
// which under-reports by the reserved fraction. The case where that matters
// least is the full disk, which reads as nearly full either way.
func (f Filesystem) BytesPct() float64 {
	if f.AvailableBytes > 0 {
		return pct(f.UsedBytes, f.UsedBytes+f.AvailableBytes)
	}
	return pct(f.UsedBytes, f.TotalBytes)
}

// InodesPct is how much of the inode table is consumed.
func (f Filesystem) InodesPct() float64 { return pct(f.InodesUsed, f.InodesTotal) }

// Pressure is how close the filesystem is to refusing writes, by whichever
// of its two limits is nearer. A filesystem out of inodes fails writes with
// bytes to spare, and nothing in the byte figures hints at why.
func (f Filesystem) Pressure() float64 {
	return max(f.BytesPct(), f.InodesPct())
}

// InodeBound reports whether inodes, rather than bytes, are what is about
// to fill this filesystem.
func (f Filesystem) InodeBound() bool {
	return f.InodesTotal > 0 && f.InodesPct() > f.BytesPct()
}

// Iface is one interface's counters with the rates derived from them.
type Iface struct {
	Name string `json:"name"`

	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets,omitempty"`
	TxPackets uint64 `json:"tx_packets,omitempty"`

	RxErrors  uint64 `json:"rx_errors,omitempty"`
	TxErrors  uint64 `json:"tx_errors,omitempty"`
	RxDropped uint64 `json:"rx_dropped,omitempty"`
	TxDropped uint64 `json:"tx_dropped,omitempty"`

	// Rates below are bytes (or packets) per second since the previous
	// sample. They are only meaningful when Rates is true: the oldest
	// sample in a series has nothing to diff against, and a reboot or
	// counter reset makes the diff meaningless rather than zero.
	RxPerSec        float64 `json:"rx_per_sec"`
	TxPerSec        float64 `json:"tx_per_sec"`
	RxPacketsPerSec float64 `json:"rx_packets_per_sec,omitempty"`
	TxPacketsPerSec float64 `json:"tx_packets_per_sec,omitempty"`

	// ErrorsDelta and DroppedDelta are how much those counters moved since
	// the previous sample. Errors are normally flat at some number from
	// years ago; whether they are moving now is the whole question.
	ErrorsDelta  uint64 `json:"errors_delta,omitempty"`
	DroppedDelta uint64 `json:"dropped_delta,omitempty"`

	Rates bool `json:"rates"`
}

// ProcessSnapshot is the newest process list a host reported.
type ProcessSnapshot struct {
	At time.Time `json:"at"`

	Total   int `json:"total"`
	Threads int `json:"threads,omitempty"`

	Running  int `json:"running"`
	Sleeping int `json:"sleeping"`
	Blocked  int `json:"blocked"`
	Zombie   int `json:"zombie"`
	Stopped  int `json:"stopped"`

	// Top is empty in the fleet overview, which does not load it.
	Top []protocol.Process `json:"top,omitempty"`
}

// MemPct is memory used as a percentage, or 0 when total is unknown.
func (s *Snapshot) MemPct() float64 { return pct(s.MemUsedBytes, s.MemTotalBytes) }

// SwapPct is swap used as a percentage, or 0 when no swap is configured.
func (s *Snapshot) SwapPct() float64 { return pct(s.SwapUsedBytes, s.SwapTotalBytes) }

// Totals sums throughput across every interface in the snapshot. ok is false
// if no interface had a usable rate.
func (s *Snapshot) Totals() (rx, tx float64, ok bool) {
	for _, n := range s.Net {
		if !n.Rates {
			continue
		}
		rx += n.RxPerSec
		tx += n.TxPerSec
		ok = true
	}
	return rx, tx, ok
}

// NetProblems sums errors and drops that moved since the previous sample.
func (s *Snapshot) NetProblems() uint64 {
	var n uint64
	for _, i := range s.Net {
		n += i.ErrorsDelta + i.DroppedDelta
	}
	return n
}

// Busiest returns the filesystem under the most pressure, which is the one
// worth putting in a one-line-per-host overview. ok is false if there are
// none.
func (s *Snapshot) Busiest() (f Filesystem, ok bool) {
	for _, c := range s.Filesystems {
		if !ok || c.Pressure() > f.Pressure() {
			f, ok = c, true
		}
	}
	return f, ok
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}
