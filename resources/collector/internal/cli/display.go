// Package cli implements the collector's commands: serving the API, and the
// terminal views over the data it collects.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"vpsm-collector/internal/query"
	"vpsm-collector/internal/view"
)

// Status prints the fleet overview once and returns.
func Status(ctx context.Context, src query.Source, out io.Writer, history int) error {
	hosts, err := src.Overview(ctx, history)
	if err != nil {
		return err
	}
	return view.Overview(out, hosts, displayOptions(out))
}

// Host prints everything known about one host.
func Host(ctx context.Context, src query.Source, out io.Writer, id string, history int) error {
	host, err := src.Host(ctx, id, history)
	if err != nil {
		if errors.Is(err, query.ErrNoHost) {
			return fmt.Errorf("no host %q is known to this collector", id)
		}
		return err
	}
	return view.Detail(out, host, displayOptions(out))
}

// Watch redraws the overview on an interval until the context is cancelled.
// header, if non-nil, supplies an extra status line drawn above the table
// each frame (the state of live pull sessions, for one).
//
// This is the cheapest useful version of a live display, and deliberately
// the shape a TUI would take: the renderer is already a pure function of the
// data, so moving to a full TUI framework replaces this loop and keeps
// everything below it.
func Watch(ctx context.Context, src query.Source, out io.Writer, history int, interval time.Duration, header func() string) error {
	if interval <= 0 {
		return errors.New("watch interval must be positive")
	}

	// Redraw into a buffer and write once per frame: two writes (clear, then
	// draw) let the terminal present a blank screen in between, which reads
	// as a flicker.
	frame := func() error {
		hosts, err := src.Overview(ctx, history)
		if err != nil {
			return err
		}

		o := displayOptions(out)
		var buf bytes.Buffer
		if o.Color {
			// Home the cursor and clear from there down, rather than
			// clearing the whole screen, so unchanged rows are overwritten
			// in place instead of blanked first.
			buf.WriteString("\x1b[H\x1b[J")
		}
		fmt.Fprintf(&buf, "%s  ·  refreshing every %s  ·  ctrl-c to exit\n",
			o.Now.Format(time.TimeOnly), interval)
		if header != nil {
			// Erase to end of line: the header changes length between
			// frames and is overwritten in place, not cleared first.
			buf.WriteString(header())
			if o.Color {
				buf.WriteString("\x1b[K")
			}
			buf.WriteString("\n")
		}
		buf.WriteString("\n")
		if err := view.Overview(&buf, hosts, o); err != nil {
			return err
		}
		_, err = out.Write(buf.Bytes())
		return err
	}

	if err := frame(); err != nil {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// A cancelled watch is how the command is meant to end.
			return nil
		case <-ticker.C:
			if err := frame(); err != nil {
				return err
			}
		}
	}
}

// displayOptions derives rendering options from the output destination.
func displayOptions(out io.Writer) view.Options {
	o := view.Default()
	if f, ok := out.(*os.File); ok {
		o.Color = useColor(f)
	}
	return o
}
