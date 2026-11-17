package startup

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vpsm-logs/internal/filter"
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

func TestHealthFlagIncludesProbes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(path, []byte("127.0.0.1 - - [x] \"GET /health HTTP/1.1\" 200 2\nreal\n"), 0o644)
	var hidden, shown bytes.Buffer
	run([]string{path}, nil, &hidden, &bytes.Buffer{})
	run([]string{"-health", path}, nil, &shown, &bytes.Buffer{})
	if hidden.String() != "real\n" || strings.Count(shown.String(), "\n") != 2 {
		t.Errorf("hidden = %q, shown = %q", hidden.String(), shown.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("reader went away") }

func TestLivePrintStopsWhenItsReaderGoesAway(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte("first line\n")) // and then the pipe stays open, like --follow
	done := make(chan error, 1)
	go func() { done <- printLive(r, failingWriter{}, filter.Filter{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("want the write error back")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("printLive kept reading after its output failed")
	}
}

func TestLivePrintPrefixesServicesFromTheFirstLine(t *testing.T) {
	in := strings.NewReader(`{"cursor":"1","src":"backend","msg":"a"}` + "\n" + `{"cursor":"2","src":"caddy","msg":"b"}` + "\n")
	var out bytes.Buffer
	if err := printLive(in, &out, filter.Filter{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "backend:a\ncaddy:b\n" {
		t.Errorf("output = %q", got)
	}
}
