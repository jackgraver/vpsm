package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// t.Setenv isolates the environment per test.
	for _, k := range []string{"VPSM_ADDR", "VPSM_DB", "VPSM_TOKEN", "VPSM_REMOTE", "VPSM_RETENTION"} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, DefaultAddr)
	}
	// Loopback by default: a collector should be exposed deliberately.
	if !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Errorf("default Addr = %q, want a loopback address", cfg.Addr)
	}
	if cfg.DBPath != DefaultDBPath {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, DefaultDBPath)
	}
	if cfg.Retention != DefaultRetention {
		t.Errorf("Retention = %s, want %s", cfg.Retention, DefaultRetention)
	}
}

func TestLoadFromEnvironment(t *testing.T) {
	t.Setenv("VPSM_ADDR", "0.0.0.0:9000")
	t.Setenv("VPSM_DB", "/srv/vpsm.db")
	t.Setenv("VPSM_TOKEN", "abc")
	t.Setenv("VPSM_REMOTE", "https://vpsm.example.com")
	t.Setenv("VPSM_RETENTION", "48h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" || cfg.DBPath != "/srv/vpsm.db" || cfg.Token != "abc" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Remote != "https://vpsm.example.com" {
		t.Errorf("Remote = %q", cfg.Remote)
	}
	if cfg.Retention != 48*time.Hour {
		t.Errorf("Retention = %s, want 48h", cfg.Retention)
	}
}

func TestLoadRejectsBadRetention(t *testing.T) {
	for _, v := range []string{"nonsense", "-1h"} {
		t.Setenv("VPSM_RETENTION", v)
		if _, err := Load(); err == nil {
			t.Errorf("Load accepted VPSM_RETENTION=%q", v)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Config{Addr: "127.0.0.1:8777", DBPath: "vpsm.db", Token: "abc", PruneInterval: time.Hour}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			// Serving open by default would let anyone who can reach the
			// port forge or flood metrics, so it must be asked for.
			"no token and no opt-in",
			Config{Addr: "a", DBPath: "b", PruneInterval: time.Hour},
			"no auth token set",
		},
		{
			"token with anonymous",
			Config{Addr: "a", DBPath: "b", Token: "x", AllowAnonymous: true, PruneInterval: time.Hour},
			"conflicts",
		},
		{"no addr", Config{DBPath: "b", Token: "x", PruneInterval: time.Hour}, "listen address"},
		{"no db", Config{Addr: "a", Token: "x", PruneInterval: time.Hour}, "database path"},
		{"negative retention", Config{Addr: "a", DBPath: "b", Token: "x", Retention: -1, PruneInterval: time.Hour}, "retention"},
		{"zero prune interval", Config{Addr: "a", DBPath: "b", Token: "x"}, "prune interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAnonymousIsAllowedWhenAskedFor(t *testing.T) {
	cfg := Config{Addr: "a", DBPath: "b", AllowAnonymous: true, PruneInterval: time.Hour}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestZeroRetentionIsAllowed(t *testing.T) {
	// Zero means "keep forever", not "keep nothing".
	cfg := Config{Addr: "a", DBPath: "b", Token: "x", Retention: 0, PruneInterval: time.Hour}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
