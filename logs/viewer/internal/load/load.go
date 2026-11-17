// Package load reads log sources — files, gzip-rotated files, directories
// of either, and stdin — into one set of parsed entries.
//
// One source is shown in its own order. Several are interleaved by
// timestamp, so the moment an error appeared in one service sits next to
// what the others were doing; lines without a timestamp of their own keep
// their place behind the line before them in the same source.
package load

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vpsm-logs/internal/logline"
)

// StdinName is the source name for standard input, and the path that
// selects it explicitly.
const StdinName = "-"

// Set is everything loaded: the source names, and the entries from all of
// them in display order. Entry.Source indexes Sources.
//
// A source is a file, or a service named inside one: lines pulled from a
// VPS journal each say which service wrote them, and are filed under it.
// Sources are created as the first entry for them arrives, so a pulled
// journal file never appears as an empty source of its own.
type Set struct {
	Sources []string
	Entries []logline.Entry
	index   map[string]int
}

// intern returns the index of the named source, adding it if new.
func (s *Set) intern(name string) int {
	if s.index == nil {
		s.index = map[string]int{}
	}
	i, ok := s.index[name]
	if !ok {
		i = len(s.Sources)
		s.Sources = append(s.Sources, name)
		s.index[name] = i
	}
	return i
}

// Stream parses one input's lines, in order, into its Set. It is how both
// a whole file and a live pipe are read: Load feeds a file through one,
// and the viewer feeds stdin through one as lines arrive.
type Stream struct {
	set    *Set
	name   string
	parser *logline.Parser
}

// NewStream starts reading an input called name into s.
func (s *Set) NewStream(name string) *Stream {
	return &Stream{set: s, name: name, parser: &logline.Parser{Now: time.Now()}}
}

// Add parses one line, appending the entry it completes, if any.
func (st *Stream) Add(line string) {
	if e, ok := st.parser.Next(line); ok {
		st.append(e)
	}
}

// Flush appends a final partial line the input ended on.
func (st *Stream) Flush() {
	if e, ok := st.parser.Flush(); ok {
		st.append(e)
	}
}

func (st *Stream) append(e logline.Entry) {
	name := st.name
	if e.Origin != "" {
		name = e.Origin
	}
	e.Source = st.set.intern(name)
	st.set.Entries = append(st.set.Entries, e)
}

// maxLine caps one line's length. Longer lines are cut, not dropped, so a
// runaway line cannot exhaust memory and still shows where it was.
const maxLine = 1 << 20

// Load reads every path, expanding directories to the files directly in
// them. A path of "-" reads stdin.
func Load(paths []string, stdin io.Reader) (*Set, error) {
	files, err := expand(paths)
	if err != nil {
		return nil, err
	}
	set := &Set{}
	for _, path := range files {
		r, closeFn, err := open(path, stdin)
		if err != nil {
			return nil, err
		}
		name := path
		if path == StdinName {
			name = "stdin"
		}
		st := set.NewStream(name)
		err = ReadLines(r, st.Add)
		closeFn()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		st.Flush()
	}
	if len(set.Sources) > 1 && len(files) > 1 {
		interleave(set.Entries)
	}
	return set, nil
}

// expand turns directories into the regular files directly inside them,
// sorted, skipping hidden files. Rotated files sort before the live one
// (app.log.2.gz, app.log.1, app.log) so a directory reads oldest first.
func expand(paths []string) ([]string, error) {
	var out []string
	for _, path := range paths {
		if path == StdinName {
			out = append(out, path)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, path)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		var files []string
		for _, e := range entries {
			if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
		sort.SliceStable(files, func(i, j int) bool { return rotationKey(files[i]) < rotationKey(files[j]) })
		out = append(out, files...)
	}
	if len(out) == 0 {
		return nil, errors.New("no log files found")
	}
	return out, nil
}

// rotationKey orders "app.log.3.gz" < "app.log.2" < "app.log.1" < "app.log":
// same base name together, higher rotation number (older) first.
func rotationKey(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".gz")
	base, n := name, 0
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		var num int
		if _, err := fmt.Sscanf(name[i+1:], "%d", &num); err == nil && fmt.Sprint(num) == name[i+1:] {
			base, n = name[:i], num
		}
	}
	// Invert n so larger rotation numbers sort earlier; the live file (n=0)
	// sorts last.
	return fmt.Sprintf("%s\x00%09d", base, 999999999-n)
}

func open(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == StdinName {
		if stdin == nil {
			return nil, nil, errors.New("stdin is not available")
		}
		return stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(f)
	// Decide gzip by content, not name: rotation tools are not consistent
	// about the suffix.
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		return zr, func() { zr.Close(); f.Close() }, nil
	}
	return br, func() { f.Close() }, nil
}

// ReadLines calls fn for each line of r, without its line ending, as each
// arrives: on a pipe it does not wait for the end of the input.
func ReadLines(r io.Reader, fn func(string)) error {
	return ScanLines(r, func(line string) error { fn(line); return nil })
}

// ScanLines is ReadLines stopping at the first error fn returns, which it
// returns: a reader of a never-ending pipe has to be able to give up.
func ScanLines(r io.Reader, fn func(string) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			if len(long) < maxLine {
				long = append(long, chunk[:min(len(chunk), maxLine-len(long))]...)
			}
			continue
		}
		if len(chunk) > 0 || len(long) > 0 {
			line := chunk
			if long != nil {
				if len(long) < maxLine {
					long = append(long, chunk[:min(len(chunk), maxLine-len(long))]...)
				}
				line, long = long, nil
			}
			s := strings.TrimRight(string(line), "\r\n")
			if err := fn(s); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// interleave sorts entries from several sources into one timeline. Each
// entry is placed by its own timestamp or, lacking one, by the most recent
// timestamp before it in the same source, so untimed lines travel with the
// timed line they follow. Ties keep load order.
func interleave(entries []logline.Entry) {
	keys := make([]time.Time, len(entries))
	last := map[int]time.Time{}
	for i, e := range entries {
		if !e.Time.IsZero() {
			last[e.Source] = e.Time
		}
		keys[i] = last[e.Source]
	}
	idx := make([]int, len(entries))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return keys[idx[a]].Before(keys[idx[b]]) })
	sorted := make([]logline.Entry, len(entries))
	for i, j := range idx {
		sorted[i] = entries[j]
	}
	copy(entries, sorted)
}
