package startup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests run with stdout captured, so run takes the print path: the same
// flags filter the same way outside the viewer.

func TestPrintsMatchingLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(path, []byte("INFO up\nERROR db timeout\nWARN slow\nERROR other\n"), 0o644)

	var out, errOut bytes.Buffer
	if err := run([]string{"-level", "error", "-q", "db", path}, nil, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "ERROR db timeout\n" {
		t.Errorf("output = %q", got)
	}
}

func TestSeveralSourcesArePrefixed(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")
	os.WriteFile(a, []byte("from a\n"), 0o644)
	os.WriteFile(b, []byte("from b\n"), 0o644)

	var out bytes.Buffer
	if err := run([]string{a, b}, nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != a+":from a\n"+b+":from b\n" {
		t.Errorf("output = %q", got)
	}
}

func TestReadsPipedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("ERROR piped\nINFO quiet\n")
	w.Close()
	defer r.Close()

	var out bytes.Buffer
	if err := run([]string{"-level", "warn"}, r, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "ERROR piped\n" {
		t.Errorf("output = %q", got)
	}
}

func TestRejectsUnknownLevel(t *testing.T) {
	err := run([]string{"-level", "loud", "x.log"}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown level") {
		t.Errorf("err = %v", err)
	}
}
