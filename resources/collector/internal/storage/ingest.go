package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vpsm-collector/internal/protocol"
)

// hostUpsert records the reporting host. Optional metadata only overwrites
// what we already have when the agent actually sent a value, so a report
// that omits, say, the kernel version does not erase the one we know.
// first_seen/last_seen widen rather than jump, which keeps them correct when
// an agent flushes a buffer of older samples after an outage.
const hostUpsert = `
INSERT INTO hosts (id, hostname, os, kernel, cores, agent_version, first_seen_ms, last_seen_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	hostname      = COALESCE(NULLIF(excluded.hostname, ''), hosts.hostname),
	os            = COALESCE(NULLIF(excluded.os, ''), hosts.os),
	kernel        = COALESCE(NULLIF(excluded.kernel, ''), hosts.kernel),
	cores         = COALESCE(NULLIF(excluded.cores, 0), hosts.cores),
	agent_version = COALESCE(NULLIF(excluded.agent_version, ''), hosts.agent_version),
	first_seen_ms = MIN(hosts.first_seen_ms, excluded.first_seen_ms),
	last_seen_ms  = MAX(hosts.last_seen_ms, excluded.last_seen_ms)`

// sampleInsert drops a sample the collector already holds rather than
// failing the batch, so an agent can safely retry a flush it is unsure
// landed. See the idempotency note in package protocol.
const sampleInsert = `
INSERT INTO samples (
	host_id, at_ms, received_ms, uptime_secs, elapsed_secs,
	procs_running, procs_blocked,
	cpu_user, cpu_nice, cpu_system, cpu_idle, cpu_iowait, cpu_irq, cpu_softirq, cpu_steal,
	load1, load5, load15,
	mem_total_bytes, mem_used_bytes, mem_available_bytes, swap_total_bytes, swap_used_bytes
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(host_id, at_ms) DO NOTHING`

const filesystemInsert = `
INSERT INTO filesystems (
	sample_id, mount_point, device, fs_type,
	total_bytes, used_bytes, available_bytes, inodes_total, inodes_used
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(sample_id, mount_point) DO NOTHING`

const netInsert = `
INSERT INTO net_usage (
	sample_id, iface, rx_bytes, tx_bytes, rx_packets, tx_packets,
	rx_errors, tx_errors, rx_dropped, tx_dropped
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(sample_id, iface) DO NOTHING`

// processUpsert replaces the host's process snapshot, but only when the
// incoming one is newer. The guard matters because an agent flushing a
// buffer after an outage delivers old samples: without it, a backfill would
// overwrite a current process list with a stale one.
const processUpsert = `
INSERT INTO host_processes (host_id, at_ms, total, threads, running, sleeping, blocked, zombie, stopped)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(host_id) DO UPDATE SET
	at_ms    = excluded.at_ms,
	total    = excluded.total,
	threads  = excluded.threads,
	running  = excluded.running,
	sleeping = excluded.sleeping,
	blocked  = excluded.blocked,
	zombie   = excluded.zombie,
	stopped  = excluded.stopped
WHERE excluded.at_ms > host_processes.at_ms`

const processTopInsert = `
INSERT INTO host_process_top (
	host_id, idx, pid, ppid, name, command, state, uid, user, threads,
	cpu_pct, rss_bytes, virtual_bytes, mem_pct, io_read_per_sec, io_write_per_sec
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// SaveReport stores a validated report in one transaction: either the whole
// batch lands or none of it does. The returned counts tell the agent how
// much of what it sent was new, so it knows its buffer is safe to drop.
func (s *Store) SaveReport(ctx context.Context, r *protocol.Report, receivedAt time.Time) (protocol.Response, error) {
	var out protocol.Response

	oldest, newest := r.Samples[0].At, r.Samples[0].At
	for _, smp := range r.Samples[1:] {
		if smp.At.Before(oldest) {
			oldest = smp.At
		}
		if smp.At.After(newest) {
			newest = smp.At
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	h := &r.Host
	if _, err := tx.ExecContext(ctx, hostUpsert,
		h.ID, h.Hostname, h.OS, h.Kernel, h.Cores, h.AgentVersion,
		msOf(oldest), msOf(newest),
	); err != nil {
		return out, fmt.Errorf("upsert host %q: %w", h.ID, err)
	}

	// Batches can hold up to protocol.MaxSamplesPerReport samples, each with
	// several child rows; preparing once beats re-parsing the SQL per row.
	stmts, err := prepareAll(ctx, tx, sampleInsert, filesystemInsert, netInsert)
	if err != nil {
		return out, err
	}
	defer stmts.close()
	sampleStmt, fsStmt, netStmt := stmts.s[0], stmts.s[1], stmts.s[2]

	for i := range r.Samples {
		smp := &r.Samples[i]
		t := smp.CPU.Ticks

		res, err := sampleStmt.ExecContext(ctx,
			h.ID, msOf(smp.At), msOf(receivedAt), smp.UptimeSecs, smp.ElapsedSecs,
			smp.ProcsRunning, smp.ProcsBlocked,
			t.User, t.Nice, t.System, t.Idle, t.Iowait, t.IRQ, t.SoftIRQ, t.Steal,
			smp.CPU.Load1, smp.CPU.Load5, smp.CPU.Load15,
			smp.Memory.TotalBytes, smp.Memory.UsedBytes, smp.Memory.AvailableBytes,
			smp.Memory.SwapTotalBytes, smp.Memory.SwapUsedBytes,
		)
		if err != nil {
			return out, fmt.Errorf("insert sample %d: %w", i, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return out, fmt.Errorf("insert sample %d: %w", i, err)
		}
		if affected == 0 {
			out.Duplicate++
			continue
		}
		sampleID, err := res.LastInsertId()
		if err != nil {
			return out, fmt.Errorf("insert sample %d: %w", i, err)
		}

		for _, f := range smp.Filesystems {
			if _, err := fsStmt.ExecContext(ctx, sampleID, f.MountPoint, f.Device, f.FSType,
				f.TotalBytes, f.UsedBytes, f.AvailableBytes, f.InodesTotal, f.InodesUsed,
			); err != nil {
				return out, fmt.Errorf("insert sample %d filesystem %q: %w", i, f.MountPoint, err)
			}
		}
		for _, n := range smp.Net {
			if _, err := netStmt.ExecContext(ctx, sampleID, n.Iface,
				n.RxBytes, n.TxBytes, n.RxPackets, n.TxPackets,
				n.RxErrors, n.TxErrors, n.RxDropped, n.TxDropped,
			); err != nil {
				return out, fmt.Errorf("insert sample %d iface %q: %w", i, n.Iface, err)
			}
		}
		out.Accepted++
	}

	if err := saveProcesses(ctx, tx, h.ID, r.Samples); err != nil {
		return out, err
	}

	if err := tx.Commit(); err != nil {
		return protocol.Response{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// saveProcesses replaces the host's process snapshot with the newest one in
// the batch, if the batch carries a newer one than we already hold.
func saveProcesses(ctx context.Context, tx *sql.Tx, hostID string, samples []protocol.Sample) error {
	var newest *protocol.Sample
	for i := range samples {
		if samples[i].Processes == nil {
			continue
		}
		if newest == nil || samples[i].At.After(newest.At) {
			newest = &samples[i]
		}
	}
	if newest == nil {
		return nil
	}

	p := newest.Processes
	res, err := tx.ExecContext(ctx, processUpsert,
		hostID, msOf(newest.At), p.Total, p.Threads,
		p.Running, p.Sleeping, p.Blocked, p.Zombie, p.Stopped)
	if err != nil {
		return fmt.Errorf("upsert process snapshot for %q: %w", hostID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert process snapshot for %q: %w", hostID, err)
	}
	if affected == 0 {
		// The stored snapshot is newer; leave its process list alone too.
		return nil
	}

	// The list is replaced wholesale rather than merged: process identity
	// is not stable between samples, so there is nothing to merge against.
	if _, err := tx.ExecContext(ctx, `DELETE FROM host_process_top WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("clear process list for %q: %w", hostID, err)
	}

	stmt, err := tx.PrepareContext(ctx, processTopInsert)
	if err != nil {
		return fmt.Errorf("prepare process insert: %w", err)
	}
	defer stmt.Close()

	for i, proc := range p.Top {
		// A null user or null io must stay null in the database: "not
		// measured" is a different answer from zero, and the display
		// renders them differently.
		var user any
		if proc.User != nil {
			user = *proc.User
		}
		var ioRead, ioWrite any
		if proc.IO != nil {
			ioRead, ioWrite = proc.IO.ReadBytesPerSec, proc.IO.WriteBytesPerSec
		}

		if _, err := stmt.ExecContext(ctx,
			hostID, i, proc.PID, proc.PPID, proc.Name, proc.Command, proc.State,
			proc.UID, user, proc.Threads,
			proc.CPUPct, proc.RSSBytes, proc.VirtualBytes, proc.MemPct, ioRead, ioWrite,
		); err != nil {
			return fmt.Errorf("insert process %d for %q: %w", proc.PID, hostID, err)
		}
	}
	return nil
}

// Prune deletes samples taken before cutoff and reports how many went. Child
// filesystem and interface rows follow via ON DELETE CASCADE. Hosts are left
// in place: a machine that has gone quiet is exactly what an operator wants
// to still see listed, and so is its last known process list.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM samples WHERE at_ms < ?`, msOf(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune samples before %s: %w", cutoff.Format(time.RFC3339), err)
	}
	return res.RowsAffected()
}

// stmtSet is a small bundle of prepared statements sharing a lifetime.
type stmtSet struct{ s []*sql.Stmt }

func prepareAll(ctx context.Context, tx *sql.Tx, queries ...string) (*stmtSet, error) {
	set := &stmtSet{}
	for _, q := range queries {
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			set.close()
			return nil, fmt.Errorf("prepare: %w", err)
		}
		set.s = append(set.s, stmt)
	}
	return set, nil
}

func (s *stmtSet) close() {
	for _, stmt := range s.s {
		stmt.Close()
	}
}
