// Package protocol defines the wire format between mini-bot agents and the
// collector. It is the contract both ends compile against: a change here is a
// change to the protocol, and the mini-bot side must be updated to match.
//
// # Transport
//
// Agents POST JSON to the collector over HTTP:
//
//	POST /v1/reports
//	Authorization: Bearer <token>
//	Content-Type: application/json
//	Content-Encoding: gzip        (optional)
//
// JSON over HTTP is deliberate at this stage: it is debuggable with curl,
// needs no schema compiler or extra dependency on either end, and the agent
// side is a few lines of hand-rolled request building. The envelope leaves
// room to move to a compact binary encoding later without reshaping the data.
//
// # Shape
//
// One Report carries the identity of a single host plus one or more Samples.
// Batching is the point: an agent that cannot reach the collector buffers
// its samples and flushes them on the next successful call, so a short
// outage leaves a gap in delivery but not in data.
//
// # Counters, not rates
//
// Where the kernel exposes a cumulative counter, the agent forwards the
// counter and the collector derives the rate by diffing consecutive samples.
// CPU time, network bytes and packets, errors and drops all travel as raw
// totals.
//
// The reason is that the collector is the only end that can see across a gap
// in delivery. If a report is lost, diffing counters costs one interval of
// resolution; pre-computed rates would leave a hole the collector cannot
// reason about, and would put reboot and counter-reset handling on the agent
// where it has less context.
//
// The exception is anything whose identity does not survive between samples.
// Per-process figures are rates computed by the agent, because PIDs are
// reused and short-lived processes appear and vanish within one interval, so
// there is frequently no previous value to diff against. ElapsedSecs travels
// alongside so those rates can be interpreted.
//
// # Idempotency
//
// A (host id, sample timestamp) pair identifies a sample. Re-delivering a
// sample the collector already stored is not an error — it is counted as a
// duplicate and dropped. That lets an agent retry a flush it is unsure
// landed without writing the same reading twice.
package protocol

import (
	"errors"
	"fmt"
	"time"
)

// Version is the protocol version, matching the URL prefix agents post to.
const Version = "v1"

// ReportsPath is the endpoint agents send reports to.
const ReportsPath = "/" + Version + "/reports"

// MaxSamplesPerReport caps how many samples one report may carry, so a
// misbehaving or malicious agent cannot push an unbounded batch. At a 5s
// sampling interval this is roughly 85 minutes of buffered outage.
const MaxSamplesPerReport = 1024

// MaxTopProcesses caps the per-sample process list. The agent sends the top
// N by CPU unioned with the top N by memory, so the ceiling is twice the
// agent's configured N, with headroom.
const MaxTopProcesses = 64

// MaxHostIDLen bounds host identifiers, which end up in storage keys and
// terminal output.
const MaxHostIDLen = 128

// MaxCommandLen bounds a process command line. The agent already truncates
// to 256; this is the collector refusing to be the one that stores more.
const MaxCommandLen = 512

// Report is the body of a POST to ReportsPath: who is reporting, and the
// readings they have for us.
type Report struct {
	Host    Host     `json:"host"`
	Samples []Sample `json:"samples"`
}

// Host identifies the reporting machine. These fields change rarely (or
// never), so they travel once per report rather than once per sample, and
// the collector keeps only the newest copy.
type Host struct {
	// ID is the stable identity of this host, assigned by the operator when
	// the agent is deployed (VPSM_HOST_ID). It is the key everything else
	// hangs off, so it must not change for the lifetime of the machine, and
	// it must not be derived from the hostname — two VPSes can share one.
	ID string `json:"id"`

	Hostname     string `json:"hostname,omitempty"`
	OS           string `json:"os,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	Cores        int    `json:"cores,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// Sample is one moment of system usage on one host.
type Sample struct {
	// At is when the agent took the reading, in UTC. The collector records
	// its own receive time separately, so late-arriving buffered samples
	// still land in the right place on the timeline.
	At time.Time `json:"at"`

	// ElapsedSecs is the length of the window the agent measured over. It
	// is not the configured interval: sampling takes time, and a loaded box
	// is exactly when the scheduler is least punctual. Per-process rates
	// were divided by this.
	ElapsedSecs float64 `json:"elapsed_secs,omitempty"`

	// UptimeSecs is seconds since boot. A drop between consecutive samples
	// means the host rebooted, which is how the collector knows every
	// cumulative counter below restarted from zero.
	UptimeSecs uint64 `json:"uptime_secs,omitempty"`

	// ProcsRunning and ProcsBlocked are host-level gauges, carried on every
	// sample rather than inside Processes below. They belong here because
	// their value is as a time series: ProcsBlocked paired with CPU iowait
	// is what distinguishes "stuck on storage" from "busy", and that is a
	// question about a trend. Inside the process block they would be
	// snapshot-only, and absent entirely from an agent with process
	// sampling switched off.
	ProcsRunning int `json:"procs_running,omitempty"`
	ProcsBlocked int `json:"procs_blocked,omitempty"`

	CPU         CPU          `json:"cpu"`
	Memory      Memory       `json:"memory"`
	Filesystems []Filesystem `json:"filesystems,omitempty"`
	Net         []Net        `json:"net,omitempty"`

	// Processes is the "task manager without SSH" payload. It is optional:
	// an agent with process sampling disabled simply omits it. The
	// collector keeps only the newest list per host (see storage), so this
	// costs a fixed amount of space regardless of reporting frequency.
	Processes *Processes `json:"processes,omitempty"`
}

// CPU is processor time and load at sample time.
type CPU struct {
	// Ticks are cumulative since boot, straight from /proc/stat. Percentages
	// are ratios of deltas, so the collector never needs to know USER_HZ.
	Ticks CPUTicks `json:"ticks"`

	Load1  float64 `json:"load1,omitempty"`
	Load5  float64 `json:"load5,omitempty"`
	Load15 float64 `json:"load15,omitempty"`
}

// CPUTicks is cumulative CPU time by state, aggregated across all CPUs.
//
// The split is carried rather than flattened to one "busy" figure because
// two of these fields answer questions nothing else can. Steal is time the
// hypervisor gave to another guest: folded into system it looks like your
// own kernel being slow, and left inside idle it disappears entirely, when
// it is often the only evidence that a host is oversold. Iowait is the CPU
// idle because every runnable task is blocked on disk: counted as busy it
// makes a disk-bound box look CPU-bound and points at the wrong fix.
type CPUTicks struct {
	User    uint64 `json:"user"`
	Nice    uint64 `json:"nice"`
	System  uint64 `json:"system"`
	Idle    uint64 `json:"idle"`
	Iowait  uint64 `json:"iowait"`
	IRQ     uint64 `json:"irq"`
	SoftIRQ uint64 `json:"softirq"`
	Steal   uint64 `json:"steal"`
}

// Total is every tick in the sample, the denominator for each percentage.
func (t CPUTicks) Total() uint64 {
	return t.User + t.Nice + t.System + t.Idle + t.Iowait + t.IRQ + t.SoftIRQ + t.Steal
}

// Busy is time neither idle nor waiting on I/O. Iowait is excluded
// deliberately; see the type comment.
func (t CPUTicks) Busy() uint64 { return t.Total() - t.Idle - t.Iowait }

// Memory is RAM and swap usage at sample time, in bytes. These are gauges,
// not counters: there is nothing to diff.
type Memory struct {
	TotalBytes uint64 `json:"total_bytes"`

	// UsedBytes is total minus MemAvailable, NOT total minus MemFree. Linux
	// deliberately spends otherwise-idle memory on page cache, so by the
	// naive definition a perfectly healthy box reads as ~100% full and the
	// number carries no information. Anything recomputing "used" downstream
	// must not undo this.
	UsedBytes uint64 `json:"used_bytes"`

	// AvailableBytes is the kernel's estimate of what a new workload could
	// claim. It travels even though it is derivable from the two fields
	// above, because carrying it makes the definition of UsedBytes
	// unmistakable at the point of use.
	AvailableBytes uint64 `json:"available_bytes,omitempty"`

	SwapTotalBytes uint64 `json:"swap_total_bytes,omitempty"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes,omitempty"`
}

// Filesystem is usage of one mounted filesystem. The agent filters out
// pseudo, network and squashfs mounts and collapses bind mounts.
type Filesystem struct {
	Device     string `json:"device,omitempty"`
	MountPoint string `json:"mount_point"`
	FSType     string `json:"fs_type,omitempty"`

	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes,omitempty"`

	// Inode exhaustion fills a disk from the other direction: a filesystem
	// with free bytes and no free inodes fails writes, and nothing in the
	// byte figures hints at why. Two integers to carry, a baffling outage
	// to debug without them.
	InodesTotal uint64 `json:"inodes_total,omitempty"`
	InodesUsed  uint64 `json:"inodes_used,omitempty"`
}

// Net is one network interface's counters, cumulative since boot. The agent
// excludes loopback but keeps docker bridges and veth pairs.
type Net struct {
	Iface string `json:"iface"`

	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets,omitempty"`
	TxPackets uint64 `json:"tx_packets,omitempty"`

	// Errors and drops are usually flat at some small number from years
	// ago; the question is always whether they are moving now. They are
	// counters like everything else here, and the collector diffs them.
	RxErrors  uint64 `json:"rx_errors,omitempty"`
	TxErrors  uint64 `json:"tx_errors,omitempty"`
	RxDropped uint64 `json:"rx_dropped,omitempty"`
	TxDropped uint64 `json:"tx_dropped,omitempty"`
}

// Processes is the host's process table, summarised.
type Processes struct {
	Total   int `json:"total"`
	Threads int `json:"threads,omitempty"`

	Running  int `json:"running"`
	Sleeping int `json:"sleeping"`
	Blocked  int `json:"blocked"` // uninterruptible sleep
	Zombie   int `json:"zombie"`
	Stopped  int `json:"stopped"`

	// Top is the top N by CPU unioned with the top N by memory, so up to 2N
	// entries, worst-CPU-first. It is a union rather than one sorted list
	// because a process hogging RAM at 0% CPU is exactly what you are
	// hunting, and a CPU-sorted top-N would drop it.
	Top []Process `json:"top,omitempty"`
}

// Process is one entry in the top list.
type Process struct {
	PID  int `json:"pid"`
	PPID int `json:"ppid,omitempty"`

	Name    string `json:"name"`              // kernel's short comm
	Command string `json:"command,omitempty"` // full cmdline, truncated
	State   string `json:"state,omitempty"`   // R/S/D/Z/T/I

	UID int `json:"uid"`
	// User is null when the uid could not be resolved to a name.
	User    *string `json:"user,omitempty"`
	Threads int     `json:"threads,omitempty"`

	// CPUPct is a share of ONE CPU, like top: a busy 8-thread process on a
	// 24-CPU box legitimately reports 780. Divide by core count to compare
	// against the host aggregate, and never validate it against 100.
	CPUPct float64 `json:"cpu_pct"`

	// RSSBytes double-counts shared pages: shared libraries, and everything
	// a forking server shares with its children, are counted in full
	// against every process mapping them. It is the right number for
	// ranking processes and the wrong number for totalling them — the
	// honest host total is Memory.UsedBytes.
	RSSBytes     uint64  `json:"rss_bytes"`
	VirtualBytes uint64  `json:"virtual_bytes,omitempty"`
	MemPct       float64 `json:"mem_pct,omitempty"`

	// IO is null when it could not be measured, which is not the same as
	// zero. Reading another process's I/O counters needs PTRACE_MODE_READ,
	// and under yama/ptrace_scope=1 as a non-root user it is denied for
	// most processes. Rendering null as "0 B/s" would turn a permissions
	// problem into a silent wrong answer, so the distinction survives all
	// the way to the display.
	IO *ProcessIO `json:"io,omitempty"`
}

// ProcessIO is a process's I/O throughput, as measured by the agent.
type ProcessIO struct {
	ReadBytesPerSec  float64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec float64 `json:"write_bytes_per_sec"`
}

// Response is what the collector returns from a successful POST, telling the
// agent how much of its batch was new. An agent can drop its whole buffer
// once Accepted+Duplicate covers what it sent.
type Response struct {
	Accepted  int `json:"accepted"`
	Duplicate int `json:"duplicate"`
}

// Validate reports whether the report is well-formed enough to store.
func (r *Report) Validate() error {
	if err := r.Host.Validate(); err != nil {
		return err
	}
	if len(r.Samples) == 0 {
		return errors.New("report carries no samples")
	}
	if len(r.Samples) > MaxSamplesPerReport {
		return fmt.Errorf("report carries %d samples, limit is %d", len(r.Samples), MaxSamplesPerReport)
	}
	for i := range r.Samples {
		if err := r.Samples[i].Validate(); err != nil {
			return fmt.Errorf("sample %d: %w", i, err)
		}
	}
	return nil
}

// Validate checks the host identity.
func (h *Host) Validate() error {
	if h.ID == "" {
		return errors.New("host.id is required")
	}
	if len(h.ID) > MaxHostIDLen {
		return fmt.Errorf("host.id is %d bytes, limit is %d", len(h.ID), MaxHostIDLen)
	}
	if hasControlChars(h.ID) {
		return errors.New("host.id contains control characters")
	}
	if h.Cores < 0 {
		return errors.New("host.cores is negative")
	}
	return nil
}

// Validate checks a single reading.
func (s *Sample) Validate() error {
	if s.At.IsZero() {
		return errors.New("at is required")
	}
	if s.ElapsedSecs < 0 {
		return fmt.Errorf("elapsed_secs %g is negative", s.ElapsedSecs)
	}
	if s.ProcsRunning < 0 || s.ProcsBlocked < 0 {
		return errors.New("procs_running or procs_blocked is negative")
	}

	// CPU ticks are unbounded counters; there is no range to check. A
	// sample with no ticks at all is accepted: the collector renders a
	// percentage it cannot derive as unknown rather than rejecting the
	// whole reading.

	if s.Memory.UsedBytes > s.Memory.TotalBytes {
		return fmt.Errorf("memory.used_bytes %d exceeds total_bytes %d", s.Memory.UsedBytes, s.Memory.TotalBytes)
	}
	if s.Memory.AvailableBytes > s.Memory.TotalBytes {
		return fmt.Errorf("memory.available_bytes %d exceeds total_bytes %d", s.Memory.AvailableBytes, s.Memory.TotalBytes)
	}
	if s.Memory.SwapUsedBytes > s.Memory.SwapTotalBytes {
		return fmt.Errorf("memory.swap_used_bytes %d exceeds swap_total_bytes %d", s.Memory.SwapUsedBytes, s.Memory.SwapTotalBytes)
	}

	for _, f := range s.Filesystems {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	for _, n := range s.Net {
		if n.Iface == "" {
			return errors.New("net.iface is required")
		}
	}
	if s.Processes != nil {
		if err := s.Processes.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks one filesystem entry.
func (f *Filesystem) Validate() error {
	if f.MountPoint == "" {
		return errors.New("filesystem.mount_point is required")
	}
	if f.UsedBytes > f.TotalBytes {
		return fmt.Errorf("filesystem %q used_bytes %d exceeds total_bytes %d", f.MountPoint, f.UsedBytes, f.TotalBytes)
	}
	// used + available can legitimately fall short of total: the difference
	// is the blocks reserved for root. Only each part against the whole is
	// checkable.
	if f.AvailableBytes > f.TotalBytes {
		return fmt.Errorf("filesystem %q available_bytes %d exceeds total_bytes %d", f.MountPoint, f.AvailableBytes, f.TotalBytes)
	}
	if f.InodesUsed > f.InodesTotal {
		return fmt.Errorf("filesystem %q inodes_used %d exceeds inodes_total %d", f.MountPoint, f.InodesUsed, f.InodesTotal)
	}
	return nil
}

// Validate checks the process summary.
func (p *Processes) Validate() error {
	if p.Total < 0 {
		return errors.New("processes.total is negative")
	}
	if len(p.Top) > MaxTopProcesses {
		return fmt.Errorf("processes.top carries %d entries, limit is %d", len(p.Top), MaxTopProcesses)
	}
	for i := range p.Top {
		if err := p.Top[i].Validate(); err != nil {
			return fmt.Errorf("processes.top[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate checks one process entry.
func (p *Process) Validate() error {
	if p.PID < 0 {
		return errors.New("pid is negative")
	}
	// CPUPct is a share of one CPU and routinely exceeds 100 on a
	// multi-core box. Only nonsense is rejected.
	if p.CPUPct < 0 {
		return fmt.Errorf("cpu_pct %g is negative", p.CPUPct)
	}
	if len(p.Command) > MaxCommandLen {
		return fmt.Errorf("command is %d bytes, limit is %d", len(p.Command), MaxCommandLen)
	}
	// Process names and command lines come from the monitored box and are
	// printed into an operator's terminal, so they are the likeliest route
	// for an escape sequence to arrive.
	if hasControlChars(p.Name) || hasControlChars(p.Command) {
		return fmt.Errorf("process %d name or command contains control characters", p.PID)
	}
	if p.User != nil && hasControlChars(*p.User) {
		return fmt.Errorf("process %d user contains control characters", p.PID)
	}
	return nil
}

// hasControlChars reports whether s contains anything that could smuggle a
// terminal escape sequence into the display layer.
func hasControlChars(s string) bool {
	for _, c := range s {
		if c < ' ' || c == 0x7f {
			return true
		}
	}
	return false
}

// ErrorCode classifies a non-2xx response. It exists so an agent can tell
// an operator-fixable rejection from one of its own bugs: both are "stop
// and discard the batch", but a skewed clock needs a human on the monitored
// box, where a malformed payload needs a fix in the agent. Matching on
// prose would break the first time an error message is reworded.
type ErrorCode string

const (
	// CodeMalformed is a body that is not valid JSON. The agent's bug.
	CodeMalformed ErrorCode = "malformed"

	// CodeInvalid is well-formed JSON that fails validation. The agent's
	// bug, and the message names the offending sample and field.
	CodeInvalid ErrorCode = "invalid"

	// CodeClockSkew is a sample stamped implausibly far in the future.
	// Operator-fixable on the monitored host: its clock is wrong.
	CodeClockSkew ErrorCode = "clock_skew"

	// CodeTooLarge is a body over the size limit. Fixable by the agent
	// without help: halve the batch and retry.
	CodeTooLarge ErrorCode = "too_large"

	// CodeUnsupportedEncoding is a Content-Encoding the collector does not
	// speak.
	CodeUnsupportedEncoding ErrorCode = "unsupported_encoding"

	// CodeUnauthorized is a missing or wrong bearer token.
	CodeUnauthorized ErrorCode = "unauthorized"

	// CodeNotFound is an unknown host on a read endpoint.
	CodeNotFound ErrorCode = "not_found"

	// CodeInternal is the collector's fault. Keep the batch and retry.
	CodeInternal ErrorCode = "internal"
)

// Error is the body of every non-2xx response from the collector.
type Error struct {
	// Error is a human-readable message, for logs. Its wording is not
	// stable; branch on Code instead.
	Error string `json:"error"`

	// Code is stable and safe to match on.
	Code ErrorCode `json:"code"`
}
