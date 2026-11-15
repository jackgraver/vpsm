package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

// fakeSource is a query.Source that counts reads, so the watch loop's
// behaviour can be observed without a database. Watch reads from its own
// goroutine, so the counter is guarded.
type fakeSource struct {
	mu    sync.Mutex
	hosts []query.HostStatus
	reads int
	err   error
}

func (f *fakeSource) Overview(ctx context.Context, history int) ([]query.HostStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.hosts, f.err
}

// count is the number of reads so far.
func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeSource) Host(ctx context.Context, id string, history int) (*query.HostStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.hosts {
		if f.hosts[i].Host.ID == id {
			return &f.hosts[i], nil
		}
	}
	return nil, query.ErrNoHost
}

// syncBuffer is an io.Writer the test can safely read while Watch writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testHosts() []query.HostStatus {
	at := time.Now()
	return []query.HostStatus{{
		Host:     protocol.Host{ID: "web-01", Hostname: "web-01.example.com", Cores: 2},
		LastSeen: at,
		Samples: []query.Snapshot{{
			At:            at,
			CPU:           query.CPUUsage{Derived: true, BusyPct: 20},
			MemTotalBytes: 100,
			MemUsedBytes:  50,
		}},
	}}
}

func TestStatus(t *testing.T) {
	src := &fakeSource{hosts: testHosts()}
	var out bytes.Buffer
	if err := Status(context.Background(), src, &out, 10); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(out.String(), "web-01") {
		t.Errorf("output = %s", out.String())
	}
}

func TestHostDetail(t *testing.T) {
	src := &fakeSource{hosts: testHosts()}
	var out bytes.Buffer
	if err := Host(context.Background(), src, &out, "web-01", 10); err != nil {
		t.Fatalf("Host: %v", err)
	}
	if !strings.Contains(out.String(), "web-01.example.com") {
		t.Errorf("output = %s", out.String())
	}
}

func TestHostUnknownNamesTheHost(t *testing.T) {
	src := &fakeSource{hosts: testHosts()}
	var out bytes.Buffer

	err := Host(context.Background(), src, &out, "nope", 10)
	if err == nil {
		t.Fatal("want an error for an unknown host")
	}
	// The raw "no such host" is useless without saying which one.
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %q, want it to name the host", err)
	}
}

func TestWatchRendersThenStopsOnCancel(t *testing.T) {
	src := &fakeSource{hosts: testHosts()}
	var out syncBuffer

	// A long interval means only the initial frame should be drawn: Watch
	// must render immediately rather than waiting out the first tick.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, src, &out, 10, time.Hour) }()

	deadline := time.After(5 * time.Second)
	for src.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("Watch drew no frame")
		case err := <-done:
			t.Fatalf("Watch returned early: %v", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-done:
		// Cancellation is how watch is meant to end, not an error.
		if err != nil {
			t.Errorf("Watch returned %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not stop after cancel")
	}

	if !strings.Contains(out.String(), "web-01") {
		t.Errorf("frame = %s", out.String())
	}
	if !strings.Contains(out.String(), "ctrl-c") {
		t.Errorf("frame missing the exit hint: %s", out.String())
	}
}

func TestWatchRejectsNonPositiveInterval(t *testing.T) {
	src := &fakeSource{hosts: testHosts()}
	var out bytes.Buffer
	if err := Watch(context.Background(), src, &out, 10, 0); err == nil {
		t.Error("want an error for a zero interval")
	}
}

func TestWatchRedrawsOnTick(t *testing.T) {
	src := &fakeSource{hosts: testHosts()}
	var out syncBuffer

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, src, &out, 10, 5*time.Millisecond) }()

	deadline := time.After(5 * time.Second)
	for src.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("drew %d frames, want at least 3", src.count())
		case err := <-done:
			t.Fatalf("Watch returned early: %v", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}
