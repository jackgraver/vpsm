package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"vpsm-collector/internal/pull"
	"vpsm-collector/internal/storage"
)

// Follow sessions that fail are retried with backoff between these bounds:
// quick enough that a blip is invisible, slow enough that a host that is
// down is not hammered with SSH handshakes.
const (
	followRetryMin = 2 * time.Second
	followRetryMax = 2 * time.Minute
)

// PullAll pulls every target once, concurrently, and writes one line per
// target to report. It returns an error only if every target failed: a
// fleet with one host unreachable is still worth displaying, and that host
// shows as stale in the views, which is the finding.
func PullAll(ctx context.Context, store *storage.Store, targets []string, o pull.Options, report io.Writer) error {
	results := make([]string, len(targets))
	failed := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := o
			opts.Target = target
			opts.Follow = false
			res, err := pull.Run(ctx, store, opts)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				results[i] = fmt.Sprintf("pull %s failed: %v", target, err)
				return
			}
			results[i] = "pulled " + summary(res)
		}()
	}
	wg.Wait()
	for _, line := range results {
		fmt.Fprintln(report, line)
	}
	if failed == len(targets) && failed > 0 {
		return fmt.Errorf("could not pull from any host")
	}
	return nil
}

func summary(r pull.Result) string {
	host := r.HostID
	if host == "" {
		host = "?"
	}
	s := fmt.Sprintf("%s from %s: %d new, %d already held", host, r.Target, r.Accepted, r.Duplicate)
	if r.Rejected > 0 {
		s += fmt.Sprintf(", %d invalid and discarded", r.Rejected)
	}
	return s
}

// Followers keeps a live pull session open to each target, reconnecting
// when one drops, and tracks a one-line state for each to show the user.
type Followers struct {
	mu    sync.Mutex
	state map[string]string
}

// StartFollowing opens a follow session per target. They run until ctx is
// cancelled.
func StartFollowing(ctx context.Context, store *storage.Store, targets []string, o pull.Options, log *slog.Logger) *Followers {
	f := &Followers{state: make(map[string]string)}
	for _, target := range targets {
		f.set(target, "connecting")
		go f.follow(ctx, store, target, o, log)
	}
	return f
}

func (f *Followers) follow(ctx context.Context, store *storage.Store, target string, o pull.Options, log *slog.Logger) {
	o.Target = target
	o.Follow = true
	o.OnProgress = func(r pull.Result) {
		if r.CaughtUp {
			f.set(target, "live")
		} else {
			f.set(target, fmt.Sprintf("catching up, %d samples", r.Accepted+r.Duplicate))
		}
	}

	wait := followRetryMin
	for ctx.Err() == nil {
		start := time.Now()
		res, err := pull.Run(ctx, store, o)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("session closed")
		}
		log.Warn("pull session ended", slog.String("target", target), slog.Any("err", err),
			slog.Int("accepted", res.Accepted))
		// A session that ran a while was healthy; start the backoff over.
		if time.Since(start) > followRetryMax {
			wait = followRetryMin
		}
		f.set(target, fmt.Sprintf("retrying in %s (%s)", wait, oneLine(err.Error())))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, followRetryMax)
	}
}

func (f *Followers) set(target, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[target] = state
}

// Status is a one-line summary of every session, for a view's header.
func (f *Followers) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := make([]string, 0, len(f.state))
	for target, state := range f.state {
		parts = append(parts, target+": "+state)
	}
	sort.Strings(parts)
	return "pulling " + strings.Join(parts, "  ·  ")
}

// oneLine trims an error to something that fits in a header.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:99] + "…"
	}
	return s
}

// Prune applies retention after a pull. `serve` sweeps on a timer; a
// collector that only ever pulls has no timer, so it sweeps here instead.
func Prune(ctx context.Context, store *storage.Store, retention time.Duration) error {
	if retention == 0 {
		return nil
	}
	_, err := store.Prune(ctx, time.Now().Add(-retention))
	return err
}
