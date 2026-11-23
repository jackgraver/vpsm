// Package config resolves collector settings from defaults and the
// environment. Callers layer command-line flags on top, so precedence is
// flag > environment > default.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Defaults for every setting.
const (
	DefaultAddr          = "127.0.0.1:8777"
	DefaultRetention     = 14 * 24 * time.Hour
	DefaultPruneInterval = time.Hour
	DefaultHistory       = 30
)

// DefaultDBPath is where the database lives when neither -db nor VPSM_DB
// says otherwise: $XDG_DATA_HOME/vpsm/vpsm.db, or ~/.local/share/vpsm/vpsm.db.
//
// It is deliberately not relative to the working directory. A pull acks
// what it stores, and the agent then deletes it, so a pull that quietly
// wrote to a stray ./vpsm.db in whatever directory the command ran from
// would lose that history from the real database for good.
func DefaultDBPath() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "vpsm", "vpsm.db")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "vpsm", "vpsm.db")
	}
	return "vpsm.db" // no home directory at all: nothing better to offer
}

// Config is everything the collector needs to run.
type Config struct {
	// Addr is the listen address for `serve`. It defaults to loopback: a
	// collector should be deliberately exposed (behind a reverse proxy with
	// TLS, or on a private network), not by accident.
	Addr string

	// DBPath is the SQLite file. Agents' reports land here and the CLI reads
	// from here.
	DBPath string

	// Token is the shared bearer token agents authenticate with, and that
	// the CLI sends when reading over HTTP.
	Token string

	// AllowAnonymous serves without authentication. Required to be explicit
	// because the alternative — silently accepting unauthenticated reports —
	// means anyone who can reach the port can forge or flood your metrics.
	AllowAnonymous bool

	// Retention is how long samples are kept; zero keeps them forever.
	Retention time.Duration

	// PruneInterval is how often expired samples are swept.
	PruneInterval time.Duration

	// Remote, when set, makes the display commands read from another
	// collector's HTTP API instead of DBPath.
	Remote string

	// Pull is the agents to fetch from over SSH (VPSM_PULL): ssh
	// destinations such as user@203.0.113.7 or a ~/.ssh/config alias. When
	// set, the display commands pull before showing anything, and `watch`
	// keeps a live session open to each.
	Pull []string

	// AgentCommand is what runs on the far end of a pull (VPSM_AGENT_CMD).
	// Empty means pull.DefaultAgentCommand.
	AgentCommand string
}

// Load returns a Config from the environment, falling back to defaults.
func Load() (Config, error) {
	c := Config{
		Addr:          env("VPSM_ADDR", DefaultAddr),
		DBPath:        env("VPSM_DB", DefaultDBPath()),
		Token:         os.Getenv("VPSM_TOKEN"),
		Retention:     DefaultRetention,
		PruneInterval: DefaultPruneInterval,
		Remote:        os.Getenv("VPSM_REMOTE"),
		Pull:          SplitTargets(os.Getenv("VPSM_PULL")),
		AgentCommand:  os.Getenv("VPSM_AGENT_CMD"),
	}
	if v := os.Getenv("VPSM_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("VPSM_RETENTION %q: %w", v, err)
		}
		if d < 0 {
			return c, fmt.Errorf("VPSM_RETENTION %q is negative", v)
		}
		c.Retention = d
	}
	return c, nil
}

// Validate checks the config is coherent for serving.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("listen address is empty")
	}
	if c.DBPath == "" {
		return fmt.Errorf("database path is empty")
	}
	if c.Token == "" && !c.AllowAnonymous {
		return fmt.Errorf("no auth token set: pass -token, set VPSM_TOKEN, " +
			"or pass -allow-anonymous to serve without authentication")
	}
	if c.Token != "" && c.AllowAnonymous {
		return fmt.Errorf("-allow-anonymous conflicts with a configured token")
	}
	if c.Retention < 0 {
		return fmt.Errorf("retention is negative")
	}
	if c.PruneInterval <= 0 {
		return fmt.Errorf("prune interval must be positive")
	}
	return nil
}

// SplitTargets parses a comma- or space-separated list of pull targets.
func SplitTargets(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
