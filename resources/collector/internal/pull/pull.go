// Package pull fetches spooled samples from an agent over SSH and stores
// them. It is the collector's half of the pull transport described in
// package protocol: run `mini-bot export` on the host, store each report it
// streams, and ack each one so the agent can free its spool.
//
// The ordering is the whole safety argument. A report is acked only after it
// is committed, so a crash, a dropped connection or a storage error at any
// point leaves the agent holding everything not yet stored, and the next
// pull sends it again. Repeats are harmless: they are counted as duplicates.
package pull

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"vpsm-collector/internal/protocol"
)

// DefaultAgentCommand is what runs on the far end of the SSH session.
const DefaultAgentCommand = "mini-bot export"

// Local is a target that runs the agent command on this machine rather than
// over SSH, for a collector on the same box as its agent.
const Local = "local"

// maxFrameBytes bounds one line of agent output. The agent sends at most a
// few hundred samples per frame, comfortably under this.
const maxFrameBytes = 64 << 20

// DefaultSSH is the ssh invocation the target and agent command are
// appended to. BatchMode makes a missing key fail rather than prompt into a
// terminal the user cannot see; -C compresses, which JSON takes to well.
var DefaultSSH = []string{
	"ssh",
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
	"-C",
}

// Ingester stores a report; storage.Store satisfies it.
type Ingester interface {
	SaveReport(ctx context.Context, r *protocol.Report, receivedAt time.Time) (protocol.Response, error)
}

// Options configures one pull.
type Options struct {
	// Target is an ssh destination (user@host, or a Host alias from
	// ~/.ssh/config), or Local.
	Target string

	// AgentCommand defaults to DefaultAgentCommand.
	AgentCommand string

	// SSH defaults to DefaultSSH.
	SSH []string

	// Follow keeps the session open after the backlog, storing each new
	// sample as the agent takes it, until ctx is cancelled.
	Follow bool

	// MaxClockSkew is how far into the future a sample may be stamped.
	// Defaults to 24h, matching the HTTP ingest.
	MaxClockSkew time.Duration

	// Now is overridable for tests.
	Now func() time.Time

	// Log receives per-report detail. Defaults to discarding it.
	Log *slog.Logger

	// OnProgress, if set, is called after each stored report with the
	// running totals. A follow uses it to know when to redraw.
	OnProgress func(Result)
}

// Result totals one pull.
type Result struct {
	Target string
	HostID string

	// Accepted and Duplicate are as in protocol.Response.
	Accepted  int
	Duplicate int

	// Rejected counts samples that failed validation and were discarded.
	// The HTTP equivalent is a 4xx the agent must not retry: a payload that
	// is invalid now will be invalid next time, and holding it would block
	// every sample behind it.
	Rejected int

	// CaughtUp is set once the agent's backlog has been fully received.
	CaughtUp bool
}

// Run pulls from one agent. Without Follow it returns once the backlog is
// stored and acked; with Follow it returns when ctx is cancelled (with a nil
// error) or the session fails.
func Run(ctx context.Context, store Ingester, o Options) (Result, error) {
	res := Result{Target: o.Target}
	if o.AgentCommand == "" {
		o.AgentCommand = DefaultAgentCommand
	}
	if o.SSH == nil {
		o.SSH = DefaultSSH
	}
	if o.MaxClockSkew <= 0 {
		o.MaxClockSkew = 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	remote := o.AgentCommand
	if o.Follow {
		remote += " --follow"
	}
	var cmd *exec.Cmd
	if o.Target == Local {
		cmd = exec.CommandContext(ctx, "sh", "-c", remote)
	} else {
		if o.Target == "" || strings.HasPrefix(o.Target, "-") {
			// A leading dash would be read by ssh as an option.
			return res, fmt.Errorf("invalid pull target %q", o.Target)
		}
		args := append(append([]string{}, o.SSH[1:]...), "--", o.Target, remote)
		cmd = exec.CommandContext(ctx, o.SSH[0], args...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return res, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr

	// Cancelling closes the agent's stdin, which is its signal to exit
	// cleanly; it is killed only if it ignores that.
	var closeOnce sync.Once
	closeStdin := func() { closeOnce.Do(func() { stdin.Close() }) }
	cmd.Cancel = func() error { closeStdin(); return nil }
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("%s: start: %w", o.Target, err)
	}

	readErr := receive(ctx, store, o, stdout, stdin, closeStdin, &res)
	closeStdin()
	// Drain whatever is left so Wait does not block on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if readErr != nil {
		return res, fmt.Errorf("%s: %w%s", o.Target, readErr, stderr.suffix())
	}
	if ctx.Err() != nil && o.Follow {
		// A follow ends by being cancelled, and the agent's exit status
		// then is just the cancellation; that is success.
		return res, nil
	}
	if waitErr != nil {
		return res, fmt.Errorf("%s: agent exited: %w%s", o.Target, waitErr, stderr.suffix())
	}
	if !res.CaughtUp {
		return res, fmt.Errorf("%s: session ended before the backlog was sent%s", o.Target, stderr.suffix())
	}
	return res, nil
}

// receive reads frames until the stream ends, storing and acking each.
func receive(
	ctx context.Context,
	store Ingester,
	o Options,
	stdout io.Reader,
	stdin io.Writer,
	closeStdin func(),
	res *Result,
) error {
	r := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := readLine(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var frame protocol.PullFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			// Not a protocol stream at all, most often a login banner or a
			// shell rc file printing to stdout. Nothing after it can be
			// trusted to line up, so stop rather than guess.
			return fmt.Errorf("agent sent something that is not a pull frame (%.80q): %w", line, err)
		}

		if frame.Report != nil {
			if err := storeReport(ctx, store, o, frame.Report, res); err != nil {
				return err
			}
			// Only now, with it committed, may the agent forget it.
			if frame.Cursor > 0 {
				if _, err := fmt.Fprintf(stdin, "ack %d\n", frame.Cursor); err != nil {
					return fmt.Errorf("ack: %w", err)
				}
			}
			if o.OnProgress != nil {
				o.OnProgress(*res)
			}
		}

		if frame.CaughtUp {
			res.CaughtUp = true
			if o.OnProgress != nil {
				o.OnProgress(*res)
			}
			if !o.Follow {
				// Closing stdin tells the agent the acks are all in; it
				// applies them and exits, which ends this loop with EOF.
				closeStdin()
			}
		}
	}
}

// storeReport validates sample by sample, so one bad reading costs itself
// and not the batch it arrived in, then stores what is left.
func storeReport(ctx context.Context, store Ingester, o Options, r *protocol.Report, res *Result) error {
	if err := r.Host.Validate(); err != nil {
		// Every sample hangs off the host id; without a valid one nothing
		// can be filed, and the agent's configuration needs fixing.
		return fmt.Errorf("agent reports an invalid host: %w", err)
	}
	if res.HostID == "" {
		res.HostID = r.Host.ID
	} else if res.HostID != r.Host.ID {
		return fmt.Errorf("host id changed mid-session from %q to %q", res.HostID, r.Host.ID)
	}

	horizon := o.Now().Add(o.MaxClockSkew)
	kept := r.Samples[:0]
	var firstErr error
	rejected := 0
	for i := range r.Samples {
		s := &r.Samples[i]
		err := s.Validate()
		if err == nil && s.At.After(horizon) {
			err = fmt.Errorf("stamped %s, more than %s ahead of the collector's clock; check the agent's time sync",
				s.At.UTC().Format(time.RFC3339), o.MaxClockSkew)
		}
		if err != nil {
			rejected++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		kept = append(kept, *s)
	}
	if rejected > 0 {
		res.Rejected += rejected
		o.Log.WarnContext(ctx, "discarded invalid samples",
			slog.String("host", r.Host.ID),
			slog.Int("count", rejected),
			slog.Any("first", firstErr))
	}

	// Chunk to the protocol's batch limit; the agent stays under it, but
	// nothing on this side should depend on that.
	for len(kept) > 0 {
		n := min(len(kept), protocol.MaxSamplesPerReport)
		batch := protocol.Report{Host: r.Host, Samples: kept[:n]}
		kept = kept[n:]
		resp, err := store.SaveReport(ctx, &batch, o.Now())
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		res.Accepted += resp.Accepted
		res.Duplicate += resp.Duplicate
		o.Log.DebugContext(ctx, "report stored",
			slog.String("host", r.Host.ID),
			slog.Int("accepted", resp.Accepted),
			slog.Int("duplicate", resp.Duplicate))
	}
	return nil
}

// readLine reads one newline-terminated line of any length up to
// maxFrameBytes. A final line without a newline is returned as is.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxFrameBytes {
			return nil, fmt.Errorf("frame exceeds %d bytes", maxFrameBytes)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

// tailBuffer keeps the last max bytes written to it: enough of ssh's or
// the agent's stderr to say why a session failed.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// suffix renders the captured stderr for an error message, if any.
func (t *tailBuffer) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := strings.TrimSpace(string(t.buf))
	if text == "" {
		return ""
	}
	return ": " + text
}
