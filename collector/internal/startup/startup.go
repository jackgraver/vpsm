// Package startup wires up the collector: configuration, storage, and the
// HTTP server that receives reports from mini-bot agents.
//
// It is also where the command line is parsed. The binary is both the server
// and the tool for looking at what the server collected, so Run dispatches
// to a subcommand and hands each one an already-resolved config.
package startup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vpsm-collector/internal/cli"
	"vpsm-collector/internal/client"
	"vpsm-collector/internal/config"
	"vpsm-collector/internal/query"
	"vpsm-collector/internal/storage"
)

// Version is the collector's own version, distinct from protocol.Version.
const Version = "0.1.0"

// DefaultWatchInterval matches a typical agent sampling interval: redrawing
// much faster than reports arrive just burns cycles on identical frames.
const DefaultWatchInterval = 5 * time.Second

// Run executes the command line and exits the process.
func Run() {
	// Ctrl-C and SIGTERM cancel the context: `serve` drains and shuts down,
	// `watch` stops redrawing. A second signal kills it outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
		return
	case errors.Is(err, flag.ErrHelp):
		// The flag package already printed the usage text.
		return
	default:
		fmt.Fprintf(os.Stderr, "vpsm-collector: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return serve(ctx, rest, stderr)
	case "status":
		return status(ctx, rest, stdout, stderr)
	case "host":
		return host(ctx, rest, stdout, stderr)
	case "watch":
		return watch(ctx, rest, stdout, stderr)
	case "version", "-version", "--version":
		_, err := fmt.Fprintf(stdout, "vpsm-collector %s\n", Version)
		return err
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `vpsm-collector %s — the VPSM collector and its terminal views.

Usage:
  vpsm-collector <command> [flags]

Commands:
  serve      Receive reports from mini-bot agents over HTTP.
  status     Print a one-line-per-host overview of the fleet.
  watch      Redraw the overview on an interval (live view).
  host <id>  Print everything known about one host.
  version    Print the collector version.

The display commands read the SQLite file directly by default, or a remote
collector's API with -remote.

Environment:
  VPSM_ADDR       Listen address for serve (default %s)
  VPSM_DB         SQLite path (default %s)
  VPSM_TOKEN      Shared bearer token for agents and readers
  VPSM_REMOTE     Collector URL for the display commands
  VPSM_RETENTION  How long to keep samples, e.g. 720h (default %s)

Run 'vpsm-collector <command> -h' for a command's flags.
`, Version, config.DefaultAddr, config.DefaultDBPath, config.DefaultRetention)
}

// serve runs the API server.
func serve(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "path to the SQLite database")
	flags.StringVar(&cfg.Token, "token", cfg.Token, "shared bearer token agents must present")
	flags.BoolVar(&cfg.AllowAnonymous, "allow-anonymous", false, "serve without authentication (not for public networks)")
	flags.DurationVar(&cfg.Retention, "retention", cfg.Retention, "how long to keep samples; 0 keeps them forever")
	flags.DurationVar(&cfg.PruneInterval, "prune-interval", cfg.PruneInterval, "how often to sweep expired samples")
	verbose := flags.Bool("v", false, "log at debug level")

	extra, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("serve takes no arguments, got %q", extra[0])
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	return cli.Serve(ctx, cfg, log)
}

// status prints the fleet overview once.
func status(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, history, extra, err := displayFlags("status", args, stderr)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("status takes no arguments (did you mean `host %s`?)", extra[0])
	}

	src, closeSrc, err := openSource(cfg)
	if err != nil {
		return err
	}
	defer closeSrc()

	return cli.Status(ctx, src, stdout, history)
}

// host prints one host in detail.
func host(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, history, extra, err := displayFlags("host", args, stderr)
	if err != nil {
		return err
	}
	if len(extra) != 1 {
		return errors.New("host takes exactly one host id, e.g. `host web-01`")
	}

	src, closeSrc, err := openSource(cfg)
	if err != nil {
		return err
	}
	defer closeSrc()

	return cli.Host(ctx, src, stdout, extra[0], history)
}

// watch redraws the overview until interrupted.
func watch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("watch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "path to the SQLite database")
	flags.StringVar(&cfg.Remote, "remote", cfg.Remote, "read a remote collector's API instead of -db")
	flags.StringVar(&cfg.Token, "token", cfg.Token, "bearer token for -remote")
	history := flags.Int("history", config.DefaultHistory, "samples of history to read per host")
	interval := flags.Duration("interval", DefaultWatchInterval, "how often to redraw")

	extra, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("watch takes no arguments, got %q", extra[0])
	}

	src, closeSrc, err := openSource(cfg)
	if err != nil {
		return err
	}
	defer closeSrc()

	return cli.Watch(ctx, src, stdout, *history, *interval)
}

// displayFlags registers the flags the read-only commands share and returns
// the leftover positional arguments.
func displayFlags(name string, args []string, stderr io.Writer) (config.Config, int, []string, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, 0, nil, err
	}

	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DBPath, "db", cfg.DBPath, "path to the SQLite database")
	flags.StringVar(&cfg.Remote, "remote", cfg.Remote, "read a remote collector's API instead of -db")
	flags.StringVar(&cfg.Token, "token", cfg.Token, "bearer token for -remote")
	history := flags.Int("history", config.DefaultHistory, "samples of history to read per host")

	extra, err := parse(flags, args)
	if err != nil {
		return cfg, 0, nil, err
	}
	return cfg, *history, extra, nil
}

// parse is flag.FlagSet.Parse with argument permutation, so
// `host db-01 -db ./vpsm.db` works as readily as `host -db ./vpsm.db db-01`.
//
// The standard Parse stops at the first non-flag argument, which for a
// command taking a host id means every flag after it is silently collected
// as a positional rather than applied.
func parse(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		// Set the non-flag aside and keep parsing what follows it.
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// openSource resolves where the display commands read from: a remote
// collector if asked, otherwise the local database.
func openSource(cfg config.Config) (query.Source, func(), error) {
	if cfg.Remote != "" {
		c, err := client.New(cfg.Remote, cfg.Token)
		if err != nil {
			return nil, nil, err
		}
		return c, func() {}, nil
	}

	// storage.Open would happily create an empty database here, which would
	// report "no hosts" for what is really a wrong path.
	if _, err := os.Stat(cfg.DBPath); errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("no database at %s: start the collector with `serve`, "+
			"or point somewhere else with -db or -remote", cfg.DBPath)
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	return store, func() { store.Close() }, nil
}
