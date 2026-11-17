package startup

import (
	"reflect"
	"testing"
)

func TestRemoteArgs(t *testing.T) {
	got := remoteArgs("-1h", []string{"backend", "caddy"}, true)
	want := []string{"mini-bot", "logs", "--since", "-1h", "--source", "backend", "--source", "caddy", "--follow"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("remoteArgs = %q", got)
	}
	if got := remoteArgs("", nil, false); !reflect.DeepEqual(got, []string{"mini-bot", "logs"}) {
		t.Errorf("bare = %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"-1h":              "-1h",
		"backend":          "backend",
		"2026-11-16 10:00": "'2026-11-16 10:00'",
		"it's":             `'it'\''s'`,
		"":                 "''",
		"$(reboot)":        "'$(reboot)'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
