package load

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGzip(t *testing.T, path, content string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(content))
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func texts(s *Set) []string {
	var out []string
	for _, e := range s.Entries {
		out = append(out, e.Text)
	}
	return out
}

func TestReadsOneFileInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	write(t, path, "one\r\ntwo\n\nfour") // CRLF, a blank line, no final newline
	set, err := Load([]string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(texts(set), "|"); got != "one|two||four" {
		t.Errorf("lines = %q", got)
	}
	if set.Entries[3].Line != 4 {
		t.Errorf("line number = %d, want 4", set.Entries[3].Line)
	}
}

func TestDirectoryReadsRotationsOldestFirstAndGunzips(t *testing.T) {
	dir := t.TempDir()
	// No timestamps, so order comes from file order alone.
	write(t, filepath.Join(dir, "app.log"), "live\n")
	write(t, filepath.Join(dir, "app.log.1"), "yesterday\n")
	writeGzip(t, filepath.Join(dir, "app.log.2.gz"), "two days ago\n")
	write(t, filepath.Join(dir, ".hidden"), "skipped\n")

	set, err := Load([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(texts(set), "|"); got != "two days ago|yesterday|live" {
		t.Errorf("lines = %q", got)
	}
}

func TestSeveralSourcesInterleaveByTime(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")
	write(t, a, "2026-11-16T10:00:00Z a1\n2026-11-16T10:00:02Z a2\n  a2 detail\n")
	write(t, b, "2026-11-16T10:00:01Z b1\n2026-11-16T10:00:03Z b2\n")

	set, err := Load([]string{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-11-16T10:00:00Z a1|2026-11-16T10:00:01Z b1|2026-11-16T10:00:02Z a2|  a2 detail|2026-11-16T10:00:03Z b2"
	if got := strings.Join(texts(set), "|"); got != want {
		t.Errorf("lines =\n  %q\nwant\n  %q", got, want)
	}
}

func TestStdin(t *testing.T) {
	set, err := Load([]string{StdinName}, strings.NewReader("piped\n"))
	if err != nil {
		t.Fatal(err)
	}
	if set.Sources[0] != "stdin" || len(set.Entries) != 1 || set.Entries[0].Text != "piped" {
		t.Errorf("set = %+v", set)
	}
}

func TestOverlongLineIsCutNotDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	write(t, path, strings.Repeat("x", maxLine+500)+"\nafter\n")
	set, err := Load([]string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Entries) != 2 || len(set.Entries[0].Text) != maxLine || set.Entries[1].Text != "after" {
		t.Errorf("got %d entries, first %d bytes", len(set.Entries), len(set.Entries[0].Text))
	}
}

func TestMissingPathIsAnError(t *testing.T) {
	if _, err := Load([]string{filepath.Join(t.TempDir(), "nope.log")}, nil); err == nil {
		t.Error("want an error for a missing file")
	}
	if _, err := Load([]string{t.TempDir()}, nil); err == nil {
		t.Error("want an error for an empty directory")
	}
}
