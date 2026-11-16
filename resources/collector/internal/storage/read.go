package storage

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

// Store implements query.Source.
var _ query.Source = (*Store)(nil)

// maxParams chunks IN-clause lookups. SQLite's bound-parameter ceiling is
// high but finite, and a wide fleet with deep history can produce a lot of
// sample ids.
const maxParams = 500

// recentSamples selects the newest `history` samples per host. The window
// function does the per-host limiting in one pass over samples_host_at,
// rather than issuing a query per host.
const recentSamples = `
SELECT host_id, id, at_ms, received_ms, uptime_secs, elapsed_secs,
       procs_running, procs_blocked,
       cpu_user, cpu_nice, cpu_system, cpu_idle, cpu_iowait, cpu_irq, cpu_softirq, cpu_steal,
       load1, load5, load15,
       mem_total_bytes, mem_used_bytes, mem_available_bytes, swap_total_bytes, swap_used_bytes
FROM (
	SELECT host_id, id, at_ms, received_ms, uptime_secs, elapsed_secs,
	       procs_running, procs_blocked,
	       cpu_user, cpu_nice, cpu_system, cpu_idle, cpu_iowait, cpu_irq, cpu_softirq, cpu_steal,
	       load1, load5, load15,
	       mem_total_bytes, mem_used_bytes, mem_available_bytes, swap_total_bytes, swap_used_bytes,
	       ROW_NUMBER() OVER (PARTITION BY host_id ORDER BY at_ms DESC) AS rn
	FROM samples
	%s
)
WHERE rn <= ?
ORDER BY host_id ASC, at_ms DESC`

// Overview returns every known host with up to history recent samples each.
// The per-process list is omitted: a fleet view has no room to show it, and
// loading it for every host would dominate the query for nothing.
func (s *Store) Overview(ctx context.Context, history int) ([]query.HostStatus, error) {
	return s.read(ctx, "", history, false)
}

// Host returns one host with up to history recent samples, including its
// process list.
func (s *Store) Host(ctx context.Context, id string, history int) (*query.HostStatus, error) {
	hosts, err := s.read(ctx, id, history, true)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%q: %w", id, query.ErrNoHost)
	}
	return &hosts[0], nil
}

func (s *Store) read(ctx context.Context, hostID string, history int, withTop bool) ([]query.HostStatus, error) {
	if history < 1 {
		history = 1
	}

	hosts, err := s.loadHosts(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, nil
	}

	samples, byID, err := s.loadSamples(ctx, hostID, history)
	if err != nil {
		return nil, err
	}
	if err := s.loadFilesystems(ctx, byID); err != nil {
		return nil, err
	}
	if err := s.loadNet(ctx, byID); err != nil {
		return nil, err
	}

	for i := range hosts {
		hosts[i].Samples = samples[hosts[i].Host.ID]
		derive(hosts[i].Samples)
	}
	return hosts, s.loadProcesses(ctx, hosts, withTop)
}

func (s *Store) loadHosts(ctx context.Context, hostID string) ([]query.HostStatus, error) {
	q := `SELECT id, hostname, os, kernel, cores, agent_version, first_seen_ms, last_seen_ms
	      FROM hosts`
	var args []any
	if hostID != "" {
		q += ` WHERE id = ?`
		args = append(args, hostID)
	}
	q += ` ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("select hosts: %w", err)
	}
	defer rows.Close()

	var out []query.HostStatus
	for rows.Next() {
		var h query.HostStatus
		var firstMs, lastMs int64
		if err := rows.Scan(
			&h.Host.ID, &h.Host.Hostname, &h.Host.OS, &h.Host.Kernel,
			&h.Host.Cores, &h.Host.AgentVersion, &firstMs, &lastMs,
		); err != nil {
			return nil, fmt.Errorf("scan host: %w", err)
		}
		h.FirstSeen, h.LastSeen = timeOf(firstMs), timeOf(lastMs)
		out = append(out, h)
	}
	return out, rows.Err()
}

// loadSamples returns samples grouped by host (newest first), plus an index
// from sample id to the snapshot it became, which the child-table loads use
// to attach filesystem and interface rows.
func (s *Store) loadSamples(ctx context.Context, hostID string, history int) (map[string][]query.Snapshot, map[int64]*query.Snapshot, error) {
	where := ""
	var args []any
	if hostID != "" {
		where = `WHERE host_id = ?`
		args = append(args, hostID)
	}
	args = append(args, history)

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(recentSamples, where), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("select samples: %w", err)
	}
	defer rows.Close()

	// Collect first and group second: the index below hands out pointers
	// into the grouped slices, which must not be reallocated afterwards.
	type record struct {
		hostID string
		id     int64
		snap   query.Snapshot
	}
	var recs []record
	for rows.Next() {
		var r record
		var atMs, receivedMs int64
		t := &r.snap.Ticks
		if err := rows.Scan(
			&r.hostID, &r.id, &atMs, &receivedMs, &r.snap.UptimeSecs, &r.snap.ElapsedSecs,
			&r.snap.ProcsRunning, &r.snap.ProcsBlocked,
			&t.User, &t.Nice, &t.System, &t.Idle, &t.Iowait, &t.IRQ, &t.SoftIRQ, &t.Steal,
			&r.snap.Load1, &r.snap.Load5, &r.snap.Load15,
			&r.snap.MemTotalBytes, &r.snap.MemUsedBytes, &r.snap.MemAvailableBytes,
			&r.snap.SwapTotalBytes, &r.snap.SwapUsedBytes,
		); err != nil {
			return nil, nil, fmt.Errorf("scan sample: %w", err)
		}
		r.snap.At, r.snap.ReceivedAt = timeOf(atMs), timeOf(receivedMs)
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	grouped := make(map[string][]query.Snapshot)
	ids := make(map[string][]int64)
	for _, r := range recs {
		grouped[r.hostID] = append(grouped[r.hostID], r.snap)
		ids[r.hostID] = append(ids[r.hostID], r.id)
	}

	byID := make(map[int64]*query.Snapshot, len(recs))
	for host, snaps := range grouped {
		for i := range snaps {
			byID[ids[host][i]] = &grouped[host][i]
		}
	}
	return grouped, byID, nil
}

func (s *Store) loadFilesystems(ctx context.Context, byID map[int64]*query.Snapshot) error {
	return s.loadChildren(ctx, byID,
		`SELECT sample_id, mount_point, device, fs_type,
		        total_bytes, used_bytes, available_bytes, inodes_total, inodes_used
		 FROM filesystems WHERE sample_id IN (%s) ORDER BY mount_point ASC`,
		func(scan func(...any) error, into func(int64) *query.Snapshot) error {
			var id int64
			var f query.Filesystem
			if err := scan(&id, &f.MountPoint, &f.Device, &f.FSType,
				&f.TotalBytes, &f.UsedBytes, &f.AvailableBytes, &f.InodesTotal, &f.InodesUsed,
			); err != nil {
				return err
			}
			if snap := into(id); snap != nil {
				snap.Filesystems = append(snap.Filesystems, f)
			}
			return nil
		})
}

func (s *Store) loadNet(ctx context.Context, byID map[int64]*query.Snapshot) error {
	return s.loadChildren(ctx, byID,
		`SELECT sample_id, iface, rx_bytes, tx_bytes, rx_packets, tx_packets,
		        rx_errors, tx_errors, rx_dropped, tx_dropped
		 FROM net_usage WHERE sample_id IN (%s) ORDER BY iface ASC`,
		func(scan func(...any) error, into func(int64) *query.Snapshot) error {
			var id int64
			var n query.Iface
			if err := scan(&id, &n.Name, &n.RxBytes, &n.TxBytes, &n.RxPackets, &n.TxPackets,
				&n.RxErrors, &n.TxErrors, &n.RxDropped, &n.TxDropped,
			); err != nil {
				return err
			}
			if snap := into(id); snap != nil {
				snap.Net = append(snap.Net, n)
			}
			return nil
		})
}

// loadChildren runs a chunked IN-clause query over the loaded sample ids and
// hands each row to attach, which appends it to the owning snapshot.
func (s *Store) loadChildren(
	ctx context.Context,
	byID map[int64]*query.Snapshot,
	queryFmt string,
	attach func(scan func(...any) error, into func(int64) *query.Snapshot) error,
) error {
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	into := func(id int64) *query.Snapshot { return byID[id] }

	for chunk := range slices.Chunk(ids, maxParams) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(queryFmt, placeholders(len(chunk))), args...)
		if err != nil {
			return fmt.Errorf("select sample detail: %w", err)
		}
		for rows.Next() {
			if err := attach(rows.Scan, into); err != nil {
				rows.Close()
				return fmt.Errorf("scan sample detail: %w", err)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// loadProcesses attaches each host's process snapshot. The per-process list
// is only loaded when withTop is set, which is the single-host view.
func (s *Store) loadProcesses(ctx context.Context, hosts []query.HostStatus, withTop bool) error {
	byHost := make(map[string]*query.HostStatus, len(hosts))
	ids := make([]any, 0, len(hosts))
	for i := range hosts {
		byHost[hosts[i].Host.ID] = &hosts[i]
		ids = append(ids, hosts[i].Host.ID)
	}

	for chunk := range slices.Chunk(ids, maxParams) {
		q := fmt.Sprintf(
			`SELECT host_id, at_ms, total, threads, running, sleeping, blocked, zombie, stopped
			 FROM host_processes WHERE host_id IN (%s)`, placeholders(len(chunk)))
		rows, err := s.db.QueryContext(ctx, q, chunk...)
		if err != nil {
			return fmt.Errorf("select process snapshots: %w", err)
		}
		for rows.Next() {
			var hostID string
			var atMs int64
			var p query.ProcessSnapshot
			if err := rows.Scan(&hostID, &atMs, &p.Total, &p.Threads,
				&p.Running, &p.Sleeping, &p.Blocked, &p.Zombie, &p.Stopped); err != nil {
				rows.Close()
				return fmt.Errorf("scan process snapshot: %w", err)
			}
			p.At = timeOf(atMs)
			if h := byHost[hostID]; h != nil {
				h.Processes = &p
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}

	if !withTop {
		return nil
	}
	return s.loadProcessTop(ctx, byHost)
}

func (s *Store) loadProcessTop(ctx context.Context, byHost map[string]*query.HostStatus) error {
	ids := make([]any, 0, len(byHost))
	for id, h := range byHost {
		if h.Processes != nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	for chunk := range slices.Chunk(ids, maxParams) {
		q := fmt.Sprintf(
			`SELECT host_id, pid, ppid, name, command, state, uid, user, threads,
			        cpu_pct, rss_bytes, virtual_bytes, mem_pct, io_read_per_sec, io_write_per_sec
			 FROM host_process_top WHERE host_id IN (%s) ORDER BY host_id ASC, idx ASC`,
			placeholders(len(chunk)))
		rows, err := s.db.QueryContext(ctx, q, chunk...)
		if err != nil {
			return fmt.Errorf("select process list: %w", err)
		}
		for rows.Next() {
			var hostID string
			var p protocol.Process
			// A null user, or null io, means the agent could not measure
			// it. That is not zero, and the distinction has to survive
			// storage as well as the wire.
			var user sql.NullString
			var ioRead, ioWrite sql.NullFloat64

			if err := rows.Scan(&hostID, &p.PID, &p.PPID, &p.Name, &p.Command, &p.State,
				&p.UID, &user, &p.Threads,
				&p.CPUPct, &p.RSSBytes, &p.VirtualBytes, &p.MemPct, &ioRead, &ioWrite,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan process: %w", err)
			}
			if user.Valid {
				p.User = &user.String
			}
			if ioRead.Valid && ioWrite.Valid {
				p.IO = &protocol.ProcessIO{
					ReadBytesPerSec:  ioRead.Float64,
					WriteBytesPerSec: ioWrite.Float64,
				}
			}
			if h := byHost[hostID]; h != nil && h.Processes != nil {
				h.Processes.Top = append(h.Processes.Top, p)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// derive turns the counters the agent sent into the rates the display wants,
// by diffing each sample against the one before it. Samples arrive newest
// first, so a sample's predecessor is the next element along.
//
// A value is left unset rather than reported as zero whenever the diff
// cannot be trusted: nothing to compare against, a host that rebooted
// (uptime went backwards, so every kernel counter restarted), or a counter
// that moved backwards on its own. Zero and "unknown" mean very different
// things when you are looking at a quiet machine.
func derive(samples []query.Snapshot) {
	for i := 0; i+1 < len(samples); i++ {
		cur, prev := &samples[i], &samples[i+1]

		elapsed := cur.At.Sub(prev.At).Seconds()
		if elapsed <= 0 {
			continue
		}
		if rebooted(cur, prev) {
			continue
		}

		deriveCPU(cur, prev)
		deriveNet(cur, prev, elapsed)
	}
}

// rebooted reports whether the host restarted between the two samples, in
// which case every cumulative counter began again from zero.
func rebooted(cur, prev *query.Snapshot) bool {
	return cur.UptimeSecs != 0 && prev.UptimeSecs != 0 && cur.UptimeSecs < prev.UptimeSecs
}

func deriveCPU(cur, prev *query.Snapshot) {
	total := cur.Ticks.Total()
	before := prev.Ticks.Total()
	if total <= before {
		// No movement, or the counters reset under us.
		return
	}
	window := total - before

	cur.CPU = query.CPUUsage{
		Derived:   true,
		BusyPct:   tickPct(cur.Ticks.Busy(), prev.Ticks.Busy(), window),
		UserPct:   tickPct(cur.Ticks.User+cur.Ticks.Nice, prev.Ticks.User+prev.Ticks.Nice, window),
		SystemPct: tickPct(cur.Ticks.System+cur.Ticks.IRQ+cur.Ticks.SoftIRQ, prev.Ticks.System+prev.Ticks.IRQ+prev.Ticks.SoftIRQ, window),
		IowaitPct: tickPct(cur.Ticks.Iowait, prev.Ticks.Iowait, window),
		StealPct:  tickPct(cur.Ticks.Steal, prev.Ticks.Steal, window),
	}
}

// tickPct is one state's share of the ticks that elapsed. Because it is a
// ratio of deltas, the collector never needs to know USER_HZ.
func tickPct(cur, prev, window uint64) float64 {
	if cur < prev || window == 0 {
		return 0
	}
	return float64(cur-prev) / float64(window) * 100
}

func deriveNet(cur, prev *query.Snapshot, elapsed float64) {
	before := make(map[string]query.Iface, len(prev.Net))
	for _, n := range prev.Net {
		before[n.Name] = n
	}

	for j := range cur.Net {
		n := &cur.Net[j]
		was, ok := before[n.Name]
		if !ok || n.RxBytes < was.RxBytes || n.TxBytes < was.TxBytes {
			continue
		}
		n.RxPerSec = float64(n.RxBytes-was.RxBytes) / elapsed
		n.TxPerSec = float64(n.TxBytes-was.TxBytes) / elapsed
		n.RxPacketsPerSec = perSec(n.RxPackets, was.RxPackets, elapsed)
		n.TxPacketsPerSec = perSec(n.TxPackets, was.TxPackets, elapsed)

		// Errors and drops stay as counts, not rates: they are usually
		// flat for years and then jump, and a rate would smear a burst
		// across the window into something that looks like nothing.
		n.ErrorsDelta = delta(n.RxErrors, was.RxErrors) + delta(n.TxErrors, was.TxErrors)
		n.DroppedDelta = delta(n.RxDropped, was.RxDropped) + delta(n.TxDropped, was.TxDropped)
		n.Rates = true
	}
}

func perSec(cur, prev uint64, elapsed float64) float64 {
	if cur < prev {
		return 0
	}
	return float64(cur-prev) / elapsed
}

func delta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimPrefix(strings.Repeat(",?", n), ",")
}
