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

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	f := filter.Filter{Query: *query}
	if *level != "" {
		f.MinLevel = logline.ParseLevel(*level)
		if f.MinLevel == logline.LevelUnknown {
			return fmt.Errorf("unknown level %q (want debug, info, warn or error)", *level)
		}
	}

	paths := fs.Args()
	readsStdin := false
	if len(paths) == 0 {
		if isTerminal(stdin) {
			fs.Usage()
			return errors.New("no log paths given and nothing piped on stdin")
		}
		paths = []string{load.StdinName}
	}
	for _, p := range paths {
		readsStdin = readsStdin || p == load.StdinName
	}

	set, err := load.Load(paths, stdin)
	if err != nil {
		return err
	}

	out, isFile := stdout.(*os.File)
	if *print || !isFile || !isTerminal(out) {
		return printMatches(stdout, set, f)
	}
	return tui.Run(tui.Options{Set: set, Filter: f, Version: Version, TTYInput: readsStdin})
}

// printMatches writes the matching lines, prefixed with their source when
// there is more than one, as grep does.
func printMatches(w io.Writer, set *load.Set, f filter.Filter) error {
	multi := len(set.Sources) > 1
	for _, i := range f.Apply(set.Entries) {
		e := &set.Entries[i]
		var err error
		if multi {
			_, err = fmt.Fprintf(w, "%s:%s\n", set.Sources[e.Source], e.Text)
		} else {
			_, err = fmt.Fprintln(w, e.Text)
		}
		if err != nil {
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
