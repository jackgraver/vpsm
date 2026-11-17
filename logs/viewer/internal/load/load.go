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
type Set struct {
	Sources []string
	Entries []logline.Entry
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
	now := time.Now()
	origins := map[string]int{}
	for _, path := range files {
		r, closeFn, err := open(path, stdin)
		if err != nil {
			return nil, err
		}
		name := path
		if path == StdinName {
			name = "stdin"
		}
		p := &logline.Parser{Source: len(set.Sources), Now: now}
		set.Sources = append(set.Sources, name)
		err = readLines(r, func(line string) {
			if e, ok := p.Next(line); ok {
				set.Entries = append(set.Entries, set.file(e, origins))
			}
		})
		closeFn()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		if e, ok := p.Flush(); ok {
			set.Entries = append(set.Entries, set.file(e, origins))
		}
	}
	set.dropEmpty()
	if len(set.Sources) > 1 {
		interleave(set.Entries)
	}
	return set, nil
}

// file files an entry that names its own origin (a service, in a journal
// pulled from a VPS) under that name rather than under the file it came in.
func (s *Set) file(e logline.Entry, origins map[string]int) logline.Entry {
	if e.Origin == "" {
		return e
	}
	i, ok := origins[e.Origin]
	if !ok {
		i = len(s.Sources)
		s.Sources = append(s.Sources, e.Origin)
		origins[e.Origin] = i
	}
	e.Source = i
	return e
}

// dropEmpty removes sources left with no entries — a pulled journal file,
// once its lines are filed under their services — and renumbers the rest.
func (s *Set) dropEmpty() {
	used := make([]bool, len(s.Sources))
	for _, e := range s.Entries {
		used[e.Source] = true
	}
	remap := make([]int, len(s.Sources))
	var kept []string
	for i, name := range s.Sources {
		if used[i] {
			remap[i] = len(kept)
			kept = append(kept, name)
		}
	}
	if len(kept) == len(s.Sources) || len(kept) == 0 {
		return
	}
	for i := range s.Entries {
		s.Entries[i].Source = remap[s.Entries[i].Source]
	}
	s.Sources = kept
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

// readLines calls fn for each line of r, without its line ending.
func readLines(r io.Reader, fn func(string)) error {
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
			fn(s)
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
