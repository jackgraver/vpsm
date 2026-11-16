package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/storage"
)

var now = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// fakeAgent writes frames to a file and returns a local agent command that
// plays them back, then records whatever the collector writes to its stdin.
func fakeAgent(t *testing.T, lines ...string) (cmd string, acks func() string) {
	t.Helper()
	dir := t.TempDir()
	frames := filepath.Join(dir, "frames")
	ackFile := filepath.Join(dir, "acks")
	if err := os.WriteFile(frames, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = fmt.Sprintf("cat %q; cat > %q; true", frames, ackFile)
	return cmd, func() string {
		b, _ := os.ReadFile(ackFile)
		return string(b)
	}
}

func sample(at time.Time) protocol.Sample {
	return protocol.Sample{
		At:         at,
		UptimeSecs: 1000,
		CPU:        protocol.CPU{Ticks: protocol.CPUTicks{User: 10, Idle: 90}},
		Memory:     protocol.Memory{TotalBytes: 100, UsedBytes: 40},
	}
}

func frame(t *testing.T, cursor uint64, samples ...protocol.Sample) string {
	t.Helper()
	b, err := json.Marshal(protocol.PullFrame{
		Cursor: cursor,
		Report: &protocol.Report{Host: protocol.Host{ID: "web-01"}, Samples: samples},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const caughtUp = `{"caught_up":true}`

func openStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "vpsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func run(t *testing.T, store Ingester, cmd string) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Run(ctx, store, Options{Target: Local, AgentCommand: cmd, Now: func() time.Time { return now }})
}

func TestBacklogIsStoredThenAcked(t *testing.T) {
	store := openStore(t)
	cmd, acks := fakeAgent(t,
		frame(t, 2, sample(now.Add(-10*time.Second)), sample(now.Add(-5*time.Second))),
		frame(t, 3, sample(now)),
		caughtUp,
	)

	res, err := run(t, store, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 3 || res.Duplicate != 0 || !res.CaughtUp || res.HostID != "web-01" {
		t.Fatalf("result = %+v", res)
	}
	if got := acks(); got != "ack 2\nack 3\n" {
		t.Fatalf("acks = %q", got)
	}

	// The same backlog again — an ack that never landed — is duplicates.
	res, err = run(t, store, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 0 || res.Duplicate != 3 {
		t.Fatalf("re-pull result = %+v", res)
	}
}

func TestInvalidSamplesCostOnlyThemselves(t *testing.T) {
	bad := sample(now)
	bad.Memory.UsedBytes = 500 // more than total
	future := sample(now.Add(48 * time.Hour))

	cmd, acks := fakeAgent(t, frame(t, 4, sample(now.Add(-time.Second)), bad, future, sample(now)), caughtUp)
	res, err := run(t, openStore(t), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Rejected != 2 {
		t.Fatalf("result = %+v", res)
	}
	// Discarded, not held: retrying them would only block what follows.
	if got := acks(); got != "ack 4\n" {
		t.Fatalf("acks = %q", got)
	}
}

type failingStore struct{}

func (failingStore) SaveReport(context.Context, *protocol.Report, time.Time) (protocol.Response, error) {
	return protocol.Response{}, errors.New("disk full")
}

func TestNothingIsAckedThatWasNotStored(t *testing.T) {
	cmd, acks := fakeAgent(t, frame(t, 1, sample(now)), caughtUp)
	_, err := run(t, failingStore{}, cmd)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if got := acks(); got != "" {
		t.Fatalf("acked %q after a storage failure", got)
	}
}

func TestNonProtocolOutputStopsThePull(t *testing.T) {
	cmd, acks := fakeAgent(t, "Welcome to Ubuntu!", frame(t, 1, sample(now)), caughtUp)
	_, err := run(t, openStore(t), cmd)
	if err == nil || !strings.Contains(err.Error(), "not a pull frame") {
		t.Fatalf("err = %v", err)
	}
	if acks() != "" {
		t.Fatal("acked past garbage")
	}
}

func TestAgentFailureSurfacesItsStderr(t *testing.T) {
	_, err := run(t, openStore(t), "echo 'mini-bot export: no spool at /var/lib/vpsm/spool' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "no spool at") {
		t.Fatalf("err = %v", err)
	}
}

func TestFollowStoresAsItArrivesAndEndsCleanlyOnCancel(t *testing.T) {
	store := openStore(t)
	dir := t.TempDir()
	// Backlog, caught up, then one more sample a moment later, then wait
	// for stdin to close the way the real agent does.
	script := fmt.Sprintf("echo '%s'; echo '%s'; sleep 0.2; echo '%s'; cat > %q; true",
		frame(t, 1, sample(now.Add(-time.Second))), caughtUp, frame(t, 2, sample(now)),
		filepath.Join(dir, "acks"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var final Result
	go func() {
		defer close(done)
		var err error
		final, err = Run(ctx, store, Options{
			Target: Local, AgentCommand: script, Follow: true,
			Now: func() time.Time { return now },
			OnProgress: func(r Result) {
				if r.Accepted == 2 {
					cancel()
				}
			},
		})
		if err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("follow did not end on cancel")
	}
	if final.Accepted != 2 {
		t.Fatalf("result = %+v", final)
	}
	acks, _ := os.ReadFile(filepath.Join(dir, "acks"))
	if string(acks) != "ack 1\nack 2\n" {
		t.Fatalf("acks = %q", acks)
	}
}

func TestTargetsCannotSmuggleSSHOptions(t *testing.T) {
	_, err := Run(context.Background(), openStore(t), Options{Target: "-oProxyCommand=touch /tmp/x"})
	if err == nil {
		t.Fatal("accepted a target starting with a dash")
	}
}
