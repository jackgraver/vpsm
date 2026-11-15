// Package storage persists host statistics in SQLite and reads them back.
//
// SQLite suits this workload: write volume is one small row per host per
// sampling interval, the data is naturally a single-writer time series, and
// the whole store is one file an operator can copy, back up, or inspect with
// the sqlite3 CLI. The driver is pure Go (modernc.org/sqlite), so the
// collector still cross-compiles to a static binary with no C toolchain and
// nothing to install on the host.
//
// Timestamps are stored as milliseconds since the Unix epoch — compact,
// integer-sortable, and free of timezone ambiguity. All times handed back
// are UTC.
package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is written to PRAGMA user_version. Bump it whenever the
// shape below changes incompatibly; Open refuses to touch a file written by
// a different version rather than failing later on a missing column.
const SchemaVersion = 3

// schema is applied on every Open. Every statement is idempotent.
//
// Counter columns hold what the agent sent, unmodified. Rates are derived on
// read rather than stored, which is what lets the collector bridge a gap in
// delivery: if a report is lost, the next diff spans the gap instead of
// leaving a hole.
const schema = `
CREATE TABLE IF NOT EXISTS hosts (
	id            TEXT PRIMARY KEY,
	hostname      TEXT    NOT NULL DEFAULT '',
	os            TEXT    NOT NULL DEFAULT '',
	kernel        TEXT    NOT NULL DEFAULT '',
	cores         INTEGER NOT NULL DEFAULT 0,
	agent_version TEXT    NOT NULL DEFAULT '',
	first_seen_ms INTEGER NOT NULL,
	last_seen_ms  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS samples (
	id                  INTEGER PRIMARY KEY,
	host_id             TEXT    NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
	at_ms               INTEGER NOT NULL,
	received_ms         INTEGER NOT NULL,
	uptime_secs         INTEGER NOT NULL DEFAULT 0,
	elapsed_secs        REAL    NOT NULL DEFAULT 0,

	-- Host-level gauges, not part of the process snapshot: paired with
	-- cpu_iowait they separate "stuck on storage" from "busy", and that
	-- only reads as an answer over time.
	procs_running       INTEGER NOT NULL DEFAULT 0,
	procs_blocked       INTEGER NOT NULL DEFAULT 0,

	-- Cumulative CPU time by state, straight from /proc/stat.
	cpu_user            INTEGER NOT NULL DEFAULT 0,
	cpu_nice            INTEGER NOT NULL DEFAULT 0,
	cpu_system          INTEGER NOT NULL DEFAULT 0,
	cpu_idle            INTEGER NOT NULL DEFAULT 0,
	cpu_iowait          INTEGER NOT NULL DEFAULT 0,
	cpu_irq             INTEGER NOT NULL DEFAULT 0,
	cpu_softirq         INTEGER NOT NULL DEFAULT 0,
	cpu_steal           INTEGER NOT NULL DEFAULT 0,

	load1               REAL    NOT NULL DEFAULT 0,
	load5               REAL    NOT NULL DEFAULT 0,
	load15              REAL    NOT NULL DEFAULT 0,

	mem_total_bytes     INTEGER NOT NULL DEFAULT 0,
	mem_used_bytes      INTEGER NOT NULL DEFAULT 0,
	mem_available_bytes INTEGER NOT NULL DEFAULT 0,
	swap_total_bytes    INTEGER NOT NULL DEFAULT 0,
	swap_used_bytes     INTEGER NOT NULL DEFAULT 0
);

-- Serves both duplicate rejection on ingest and the "newest N samples for
-- this host" scan every read does.
CREATE UNIQUE INDEX IF NOT EXISTS samples_host_at ON samples(host_id, at_ms);

CREATE TABLE IF NOT EXISTS filesystems (
	sample_id       INTEGER NOT NULL REFERENCES samples(id) ON DELETE CASCADE,
	mount_point     TEXT    NOT NULL,
	device          TEXT    NOT NULL DEFAULT '',
	fs_type         TEXT    NOT NULL DEFAULT '',
	total_bytes     INTEGER NOT NULL DEFAULT 0,
	used_bytes      INTEGER NOT NULL DEFAULT 0,
	available_bytes INTEGER NOT NULL DEFAULT 0,
	inodes_total    INTEGER NOT NULL DEFAULT 0,
	inodes_used     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (sample_id, mount_point)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS net_usage (
	sample_id  INTEGER NOT NULL REFERENCES samples(id) ON DELETE CASCADE,
	iface      TEXT    NOT NULL,
	rx_bytes   INTEGER NOT NULL DEFAULT 0,
	tx_bytes   INTEGER NOT NULL DEFAULT 0,
	rx_packets INTEGER NOT NULL DEFAULT 0,
	tx_packets INTEGER NOT NULL DEFAULT 0,
	rx_errors  INTEGER NOT NULL DEFAULT 0,
	tx_errors  INTEGER NOT NULL DEFAULT 0,
	rx_dropped INTEGER NOT NULL DEFAULT 0,
	tx_dropped INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (sample_id, iface)
) WITHOUT ROWID;

-- Process data is kept as a snapshot: one row per host, replaced on each
-- report, rather than a row per sample. A busy host has 500+ processes and
-- high-cardinality command strings; storing every interval would dominate
-- the database to answer a question ("what is eating this box?") that is
-- almost always about right now. Widening this to full history later is an
-- added table, not a reshape of these.
CREATE TABLE IF NOT EXISTS host_processes (
	host_id  TEXT    PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
	at_ms    INTEGER NOT NULL,
	total    INTEGER NOT NULL DEFAULT 0,
	threads  INTEGER NOT NULL DEFAULT 0,
	running  INTEGER NOT NULL DEFAULT 0,
	sleeping INTEGER NOT NULL DEFAULT 0,
	blocked  INTEGER NOT NULL DEFAULT 0,
	zombie   INTEGER NOT NULL DEFAULT 0,
	stopped  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS host_process_top (
	host_id       TEXT    NOT NULL REFERENCES host_processes(host_id) ON DELETE CASCADE,
	idx           INTEGER NOT NULL,
	pid           INTEGER NOT NULL,
	ppid          INTEGER NOT NULL DEFAULT 0,
	name          TEXT    NOT NULL DEFAULT '',
	command       TEXT    NOT NULL DEFAULT '',
	state         TEXT    NOT NULL DEFAULT '',
	uid           INTEGER NOT NULL DEFAULT 0,
	-- NULL when the uid could not be resolved to a name.
	user          TEXT,
	threads       INTEGER NOT NULL DEFAULT 0,
	-- A share of one CPU: legitimately exceeds 100 on a multi-core box.
	cpu_pct       REAL    NOT NULL DEFAULT 0,
	rss_bytes     INTEGER NOT NULL DEFAULT 0,
	virtual_bytes INTEGER NOT NULL DEFAULT 0,
	mem_pct       REAL    NOT NULL DEFAULT 0,
	-- NULL means "not measured" (ptrace denied), never zero.
	io_read_per_sec  REAL,
	io_write_per_sec REAL,
	PRIMARY KEY (host_id, idx)
) WITHOUT ROWID;
`

// Store is a handle on the statistics database. It is safe for concurrent
// use: the serving process writes while a CLI reader on the same box reads,
// which is why WAL mode below is not optional.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the
// schema. Pass ":memory:" for an ephemeral store, which is what the tests
// use.
func Open(path string) (*Store, error) {
	dsn, err := dsn(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// SQLite permits many readers but only one writer. Capping the pool
	// keeps us from parking a pile of connections against a file that can
	// only take one write at a time; busy_timeout in the DSN absorbs the
	// contention that remains.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if path == ":memory:" {
		// Each pooled connection to ":memory:" would otherwise get its own
		// private, empty database.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxIdleTime(0)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect %s: %w", path, err)
	}
	if err := migrate(db, path); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies the schema to a new database, or verifies that an
// existing one was written by this version.
//
// There is no upgrade path yet, on purpose: while the wire protocol is
// still settling, silently reinterpreting old columns is a worse outcome
// than telling the operator to start a fresh file. That changes the first
// time this holds data anyone minds losing.
func migrate(db *sql.DB, path string) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if version == 0 {
		// Either brand new, or written before versioning existed.
		var tables int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'samples'`,
		).Scan(&tables); err != nil {
			return fmt.Errorf("inspect database: %w", err)
		}
		if tables > 0 {
			return fmt.Errorf("%s predates schema versioning and cannot be read; "+
				"delete it and let the collector create a new one", path)
		}
	} else if version != SchemaVersion {
		return fmt.Errorf("%s has schema version %d, this collector speaks %d; "+
			"delete it and let the collector create a new one", path, version, SchemaVersion)
	}

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// PRAGMA does not accept bound parameters; SchemaVersion is a constant.
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
}

func dsn(path string) (string, error) {
	pragmas := "_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		// Take the write lock when a write transaction begins rather than
		// on its first write, so two writers queue instead of deadlocking
		// mid-transaction.
		"&_txlock=immediate"

	if path == ":memory:" {
		return "file::memory:?" + pragmas, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return "file:" + abs + "?" + pragmas, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func msOf(t time.Time) int64    { return t.UTC().UnixMilli() }
func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
