// Package startup parses the command line, loads the logs, and hands them
// to the interactive viewer — or, when output is not a terminal, prints
// the matching lines so the same filters work in a pipeline.
package startup

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"vpsm-logs/internal/filter"
	"vpsm-logs/internal/load"
	"vpsm-logs/internal/logline"
	"vpsm-logs/internal/tui"
)

// Version is the viewer's own version.
const Version = "0.1.0"

// Run executes the command line and exits the process.
func Run() {
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
	default:
		fmt.Fprintf(os.Stderr, "vpsm-logs: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("vpsm-logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	query := fs.String("q", "", "start with this search (smart-case substring)")
	level := fs.String("level", "", "start with this minimum level: debug, info, warn, error")
	print := fs.Bool("print", false, "print matching lines instead of opening the viewer (default when stdout is not a terminal)")
	health := fs.Bool("health", false, "include health checks and local probes (hidden by default; h toggles them in the viewer)")
	host := fs.String("ssh", "", "read the logs of this host, by running `mini-bot logs` on it over ssh")
	since := fs.String("since", "-1h", "with -ssh: how far back to start (anything journalctl --since takes)")
	follow := fs.Bool("follow", true, "with -ssh: keep streaming new lines (in -print mode only when given)")
	var sources []string
	fs.Func("source", "with -ssh: only this service, container or unit (repeatable)", func(v string) error {
		sources = append(sources, v)
		return nil
	})
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: vpsm-logs [flags] [path ...]

Browse and search log files of any format. Paths may be files (plain or
gzip), or directories, whose files are read in rotation order. Several
sources are interleaved by timestamp. With no path, or a path of "-",
logs are read from stdin:

    vpsm-logs /var/log/nginx
    vpsm-logs -level warn app.log app.log.1.gz
    docker logs web 2>&1 | vpsm-logs
    vpsm-logs -print -level error -q timeout app.log
    vpsm-logs -ssh st-vps                       last hour of a VPS, then live
    vpsm-logs -ssh st-vps -since -6h -source backend

Stdin is read as it arrives, so a following pipe keeps the viewer live.
Prefer -ssh to piping from ssh: piped, ssh also reads the keyboard and
swallows keys meant for the viewer (or use ssh -n).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	f := filter.Filter{Query: *query, ShowNoise: *health}
	if *level != "" {
		f.MinLevel = logline.ParseLevel(*level)
		if f.MinLevel == logline.LevelUnknown {
			return fmt.Errorf("unknown level %q (want debug, info, warn or error)", *level)
		}
	}

	out, isFile := stdout.(*os.File)
	printing := *print || !isFile || !isTerminal(out)

	if *host != "" {
		if fs.NArg() > 0 {
			return errors.New("-ssh reads the host's journal; it takes no paths")
		}
		followSet := false
		fs.Visit(func(fl *flag.Flag) { followSet = followSet || fl.Name == "follow" })
		return viewRemote(*host, remoteArgs(*since, sources, *follow && (!printing || followSet)), printing, f, stdout, stderr)
	}

	paths := fs.Args()
	if len(paths) == 0 {
		if isTerminal(stdin) {
			fs.Usage()
			return errors.New("no log paths given and nothing piped on stdin")
		}
		paths = []string{load.StdinName}
	}
	readsStdin := false
	for _, p := range paths {
		readsStdin = readsStdin || p == load.StdinName
	}
	// Stdin alone is read as it arrives rather than to the end first: it may
	// be `mini-bot logs --follow`, which never ends.
	if len(paths) == 1 && readsStdin {
		if printing {
			return printLive(stdin, stdout, f)
		}
		return tui.Run(tui.Options{
			Set: &load.Set{}, Filter: f, Version: Version, TTYInput: true,
			Live: lines(stdin, nil), LiveName: "stdin",
		})
	}

	set, err := load.Load(paths, stdin)
	if err != nil {
		return err
	}
	if printing {
		return printMatches(stdout, set, f)
	}
	return tui.Run(tui.Options{Set: set, Filter: f, Version: Version, TTYInput: readsStdin})
}

// viewRemote shows (or prints) a host's journal, read over ssh.
func viewRemote(host string, args []string, printing bool, f filter.Filter, stdout, stderr io.Writer) error {
	r, err := startRemote(host, args)
	if err != nil {
		return fmt.Errorf("running ssh: %w", err)
	}
	var ended atomic.Bool
	if printing {
		// printLive returns nil only once ssh's output is exhausted.
		err = printLive(r.stdout, stdout, f)
		ended.Store(err == nil)
	} else {
		err = tui.Run(tui.Options{
			Set: &load.Set{}, Filter: f, Version: Version,
			Live: lines(r.stdout, &ended), LiveName: host,
		})
	}
	msg, failed := r.stop(ended.Load())
	if err != nil {
		return err
	}
	if failed {
		if msg == "" {
			msg = "exited with an error"
		}
		return fmt.Errorf("ssh %s: %s", host, msg)
	}
	if msg != "" {
		fmt.Fprintf(stderr, "ssh %s: %s\n", host, msg)
	}
	return nil
}

// lines reads r in the background and delivers its lines on the returned
// channel, closing it at the end of r. A read error ends it the same way:
// the viewer shows what arrived, marked as ended.
func lines(r io.Reader, ended *atomic.Bool) <-chan string {
	ch := make(chan string, 1024)
	go func() {
		defer close(ch)
		if ended != nil {
			defer ended.Store(true)
		}
		_ = load.ReadLines(r, func(line string) { ch <- line })
	}()
	return ch
}

// printLive filters stdin as it arrives and prints each match at once, so
// `-print` over a following pipe behaves like grep --line-buffered.
func printLive(r io.Reader, w io.Writer, f filter.Filter) error {
	set := &load.Set{}
	st := set.NewStream("stdin")
	emit := func(from int) error {
		for _, i := range f.ApplyFrom(set.Entries, set.Sources, from, nil) {
			if err := writeEntry(w, set, &set.Entries[i]); err != nil {
				return err
			}
		}
		return nil
	}
	// A failed write (the reader of our output went away) stops the read,
	// or a following pipe would be read forever with nowhere to print.
	if err := load.ScanLines(r, func(line string) error {
		n := len(set.Entries)
		st.Add(line)
		return emit(n)
	}); err != nil {
		return err
	}
	n := len(set.Entries)
	st.Flush()
	return emit(n)
}

// writeEntry prints one entry, prefixed with its source when there is
// more than one, as grep does — or when the line named its own service, so
// a live stream does not change format once a second service appears.
func writeEntry(w io.Writer, set *load.Set, e *logline.Entry) error {
	var err error
	if len(set.Sources) > 1 || e.Origin != "" {
		_, err = fmt.Fprintf(w, "%s:%s\n", set.Sources[e.Source], e.Text)
	} else {
		_, err = fmt.Fprintln(w, e.Text)
	}
	return err
}

// printMatches writes the matching lines, prefixed with their source when
// there is more than one, as grep does.
func printMatches(w io.Writer, set *load.Set, f filter.Filter) error {
	for _, i := range f.Apply(set.Entries, set.Sources) {
		if err := writeEntry(w, set, &set.Entries[i]); err != nil {
			return err
		}
	}
	return nil
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
