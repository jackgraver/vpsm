package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vpsm-collector/internal/api"
	"vpsm-collector/internal/client"
	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
	"vpsm-collector/internal/storage"
)

const testToken = "s3cret"

// newServer wires the real storage layer behind the real handler, so these
// tests cover the path a mini-bot actually takes rather than a mock of it.
func newServer(t *testing.T, token string) (*httptest.Server, *storage.Store) {
	t.Helper()

	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := httptest.NewServer(api.Handler(api.Options{
		Store:  store,
		Source: store,
		Token:  token,
		Logger: slog.New(slog.DiscardHandler),
	}))
	t.Cleanup(srv.Close)
	return srv, store
}

// sampleJSON builds a reading whose tick counters advance by 1000 per step,
// so consecutive samples derive to round percentages.
func sampleJSON(at string, step int) string {
	// The eight states advance by exactly 1000 ticks per step, split so the
	// derived percentages land on round numbers: 40% busy, 10% iowait,
	// 5% steal.
	return fmt.Sprintf(`{"at":%q,"uptime_secs":%d,"elapsed_secs":10,
		"cpu":{"ticks":{"user":%d,"nice":0,"system":%d,"idle":%d,"iowait":%d,
			"irq":%d,"softirq":50,"steal":%d},"load1":1.5},
		"memory":{"total_bytes":8589934592,"used_bytes":4294967296,"available_bytes":4294967296},
		"filesystems":[{"device":"/dev/sda1","mount_point":"/","fs_type":"ext4",
			"total_bytes":1000,"used_bytes":500,"available_bytes":450,
			"inodes_total":1000,"inodes_used":100}],
		"net":[{"iface":"eth0","rx_bytes":%d,"tx_bytes":%d,"rx_errors":7}]}`,
		at, 1000+step*10,
		1000+200*step, 200+100*step, 8000+500*step, 500+100*step, 50+50*step, 200+50*step,
		5000+500*step, 2000+100*step)
}

func post(t *testing.T, srv *httptest.Server, token, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, srv.URL+protocol.ReportsPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func get(t *testing.T, srv *httptest.Server, token, path string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestIngestStoresReport(t *testing.T) {
	srv, store := newServer(t, testToken)

	body := fmt.Sprintf(`{"host":{"id":"web-01","hostname":"web-01.example.com","cores":4},
		"samples":[%s,%s]}`,
		sampleJSON("2026-11-14T12:00:00Z", 0),
		sampleJSON("2026-11-14T12:00:10Z", 1))

	code, out := post(t, srv, testToken, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, out)
	}

	var got protocol.Response
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Accepted != 2 || got.Duplicate != 0 {
		t.Errorf("response = %+v, want accepted 2", got)
	}

	hosts, err := store.Overview(context.Background(), 10)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Host.ID != "web-01" {
		t.Fatalf("stored hosts = %+v", hosts)
	}
	if n := len(hosts[0].Samples); n != 2 {
		t.Errorf("stored %d samples, want 2", n)
	}
}

func TestIngestIsIdempotent(t *testing.T) {
	srv, _ := newServer(t, testToken)
	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON("2026-11-14T12:00:00Z", 0))

	if code, out := post(t, srv, testToken, body); code != http.StatusOK {
		t.Fatalf("first POST: %d %s", code, out)
	}
	code, out := post(t, srv, testToken, body)
	if code != http.StatusOK {
		t.Fatalf("replay POST: %d %s", code, out)
	}

	var got protocol.Response
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Accepted != 0 || got.Duplicate != 1 {
		t.Errorf("replay = %+v, want duplicate 1", got)
	}
}

func TestIngestRejectsBadInput(t *testing.T) {
	srv, _ := newServer(t, testToken)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"malformed", `{`, http.StatusBadRequest},
		{"no host id", `{"samples":[{"at":"2026-11-14T12:00:00Z"}]}`, http.StatusBadRequest},
		{"no samples", `{"host":{"id":"web-01"}}`, http.StatusBadRequest},
		{"negative elapsed", `{"host":{"id":"web-01"},"samples":[{"at":"2026-11-14T12:00:00Z","elapsed_secs":-1}]}`, http.StatusBadRequest},
		{"no timestamp", `{"host":{"id":"web-01"},"samples":[{"cpu":{"pct":1}}]}`, http.StatusBadRequest},
		{"memory over total", `{"host":{"id":"web-01"},"samples":[{"at":"2026-11-14T12:00:00Z","memory":{"total_bytes":10,"used_bytes":11}}]}`, http.StatusBadRequest},
		{"filesystem without mount", `{"host":{"id":"web-01"},"samples":[{"at":"2026-11-14T12:00:00Z","filesystems":[{"total_bytes":10,"used_bytes":1}]}]}`, http.StatusBadRequest},
		{"inodes over total", `{"host":{"id":"web-01"},"samples":[{"at":"2026-11-14T12:00:00Z","filesystems":[{"mount_point":"/","inodes_total":10,"inodes_used":11}]}]}`, http.StatusBadRequest},
		{"process cpu negative", `{"host":{"id":"web-01"},"samples":[{"at":"2026-11-14T12:00:00Z","processes":{"total":1,"top":[{"pid":1,"cpu_pct":-5}]}}]}`, http.StatusBadRequest},
		// A host id carrying an escape sequence would be echoed straight
		// into an operator's terminal by the display layer.
		{"control char in id", `{"host":{"id":"web\u001b[2J01"},"samples":[{"at":"2026-11-14T12:00:00Z"}]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := post(t, srv, testToken, tc.body); code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", code, tc.want, out)
			}
		})
	}
}

func TestIngestRejectsOversizedBody(t *testing.T) {
	srv, _ := newServer(t, testToken)

	// Valid JSON, just far too much of it: the limit has to trip during
	// decoding rather than after buffering the whole thing.
	var b strings.Builder
	b.WriteString(`{"host":{"id":"web-01","hostname":"`)
	b.WriteString(strings.Repeat("x", api.MaxBodyBytes+1))
	b.WriteString(`"},"samples":[]}`)

	if code, out := post(t, srv, testToken, b.String()); code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body %s)", code, out)
	}
}

func TestIngestRejectsOversizedBatch(t *testing.T) {
	srv, _ := newServer(t, testToken)

	var b strings.Builder
	b.WriteString(`{"host":{"id":"web-01"},"samples":[`)
	for i := range protocol.MaxSamplesPerReport + 1 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"at":"2026-11-14T12:%02d:%02dZ","cpu":{"pct":1}}`, i/60, i%60)
	}
	b.WriteString(`]}`)

	if code, _ := post(t, srv, testToken, b.String()); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestAuth(t *testing.T) {
	srv, _ := newServer(t, testToken)
	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON("2026-11-14T12:00:00Z", 0))

	if code, _ := post(t, srv, "", body); code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", code)
	}
	if code, _ := post(t, srv, "wrong", body); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", code)
	}
	if code, _ := get(t, srv, "", "/v1/hosts"); code != http.StatusUnauthorized {
		t.Errorf("read without token: status = %d, want 401", code)
	}

	// Health has to answer unauthenticated for load balancers to use it.
	if code, _ := get(t, srv, "", "/healthz"); code != http.StatusOK {
		t.Errorf("healthz: status = %d, want 200", code)
	}
}

func TestAnonymousServerAcceptsAnything(t *testing.T) {
	srv, _ := newServer(t, "")
	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON("2026-11-14T12:00:00Z", 0))

	if code, out := post(t, srv, "", body); code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", code, out)
	}
}

func TestReadEndpoints(t *testing.T) {
	srv, _ := newServer(t, testToken)
	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON("2026-11-14T12:00:00Z", 0))
	if code, out := post(t, srv, testToken, body); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, out)
	}

	code, out := get(t, srv, testToken, "/v1/hosts")
	if code != http.StatusOK {
		t.Fatalf("hosts: %d %s", code, out)
	}
	if !strings.Contains(out, `"web-01"`) {
		t.Errorf("hosts body missing the host: %s", out)
	}

	if code, _ = get(t, srv, testToken, "/v1/hosts/web-01"); code != http.StatusOK {
		t.Errorf("host: status = %d, want 200", code)
	}
	if code, _ = get(t, srv, testToken, "/v1/hosts/nope"); code != http.StatusNotFound {
		t.Errorf("unknown host: status = %d, want 404", code)
	}
	if code, _ = get(t, srv, testToken, "/v1/hosts?history=abc"); code != http.StatusBadRequest {
		t.Errorf("bad history: status = %d, want 400", code)
	}

	// Method is part of the route pattern.
	if code, _ = get(t, srv, testToken, protocol.ReportsPath); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on ingest: status = %d, want 405", code)
	}
}

func TestHistoryIsClamped(t *testing.T) {
	srv, _ := newServer(t, testToken)

	var b strings.Builder
	b.WriteString(`{"host":{"id":"web-01"},"samples":[`)
	for i := range 5 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(sampleJSON(fmt.Sprintf("2026-11-14T12:00:%02dZ", i*10), i))
	}
	b.WriteString(`]}`)
	if code, out := post(t, srv, testToken, b.String()); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, out)
	}

	// Zero and negative are floors to 1 rather than errors, so a caller
	// asking for "just the latest" gets it.
	for _, raw := range []string{"0", "-5"} {
		code, out := get(t, srv, testToken, "/v1/hosts?history="+raw)
		if code != http.StatusOK {
			t.Fatalf("history=%s: status %d", raw, code)
		}
		var body struct {
			Hosts []query.HostStatus `json:"hosts"`
		}
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if n := len(body.Hosts[0].Samples); n != 1 {
			t.Errorf("history=%s returned %d samples, want 1", raw, n)
		}
	}

	// An absurd request is capped instead of attempting to load everything.
	if code, _ := get(t, srv, testToken, "/v1/hosts?history=999999999"); code != http.StatusOK {
		t.Errorf("huge history: status = %d, want 200", code)
	}
}

func TestEmptyOverviewIsAnArray(t *testing.T) {
	srv, _ := newServer(t, testToken)

	// A nil slice would marshal to null and break clients that iterate.
	code, out := get(t, srv, testToken, "/v1/hosts")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(out, `"hosts":[]`) {
		t.Errorf("empty overview = %s, want an empty array", out)
	}
}

// TestClientMatchesStorage is the contract test for the read API: the same
// query.Source read locally and over HTTP must produce identical data, since
// the display layer is written against the interface and cannot tell which
// one it was handed.
func TestClientMatchesStorage(t *testing.T) {
	srv, store := newServer(t, testToken)

	body := fmt.Sprintf(`{"host":{"id":"web-01","hostname":"web-01.example.com","os":"linux","kernel":"6.1.0","cores":4,"agent_version":"0.1.0"},
		"samples":[%s,%s,%s]}`,
		sampleJSON("2026-11-14T12:00:00Z", 0),
		sampleJSON("2026-11-14T12:00:10Z", 1),
		sampleJSON("2026-11-14T12:00:20Z", 2))
	if code, out := post(t, srv, testToken, body); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, out)
	}

	remote, err := client.New(srv.URL, testToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx := context.Background()

	reads := []struct {
		name string
		fn   func(query.Source) (any, error)
	}{
		{"Overview", func(s query.Source) (any, error) { return s.Overview(ctx, 10) }},
		{"Host", func(s query.Source) (any, error) { return s.Host(ctx, "web-01", 10) }},
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			local, err := read.fn(store)
			if err != nil {
				t.Fatalf("local: %v", err)
			}
			over, err := read.fn(remote)
			if err != nil {
				t.Fatalf("remote: %v", err)
			}
			if a, b := mustJSON(t, local), mustJSON(t, over); !bytes.Equal(a, b) {
				t.Errorf("local and remote disagree:\n local: %s\nremote: %s", a, b)
			}
		})
	}
}

func TestClientReportsUnknownHost(t *testing.T) {
	srv, _ := newServer(t, testToken)
	remote, err := client.New(srv.URL, testToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = remote.Host(context.Background(), "nope", 1)
	if !errors.Is(err, query.ErrNoHost) {
		t.Errorf("err = %v, want ErrNoHost", err)
	}
}

func TestClientRejectedToken(t *testing.T) {
	srv, _ := newServer(t, testToken)
	remote, err := client.New(srv.URL, "wrong")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = remote.Overview(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v, want a message naming the token", err)
	}
}

func TestClientAcceptsBareHostPort(t *testing.T) {
	srv, _ := newServer(t, testToken)

	// Operators type host:port far more often than a full URL.
	bare := strings.TrimPrefix(srv.URL, "http://")
	remote, err := client.New(bare, testToken)
	if err != nil {
		t.Fatalf("client.New(%q): %v", bare, err)
	}
	if _, err := remote.Overview(context.Background(), 1); err != nil {
		t.Errorf("Overview: %v", err)
	}
}

func TestClientHonoursContext(t *testing.T) {
	// A dead collector must not hang the CLI.
	remote, err := client.New("http://127.0.0.1:1", testToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := remote.Overview(ctx, 1); err == nil {
		t.Error("want an error contacting a dead port")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// postRaw sends a body with explicit headers, for the encoding tests.
func postRaw(t *testing.T, srv *httptest.Server, body []byte, headers map[string]string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, srv.URL+protocol.ReportsPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestGzipBody covers the optional compression an agent may use for a long
// buffered flush, which is exactly when a report is at its biggest.
func TestGzipBody(t *testing.T) {
	srv, store := newServer(t, testToken)

	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s,%s]}`,
		sampleJSON("2026-11-14T12:00:00Z", 0),
		sampleJSON("2026-11-14T12:00:10Z", 1))

	code, out := postRaw(t, srv, gzipped(t, body), map[string]string{
		"Content-Encoding": "gzip",
		"Content-Type":     "application/json",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, out)
	}

	hosts, err := store.Overview(context.Background(), 10)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(hosts) != 1 || len(hosts[0].Samples) != 2 {
		t.Errorf("gzipped report did not land: %+v", hosts)
	}
}

func TestGzipRejectsGarbage(t *testing.T) {
	srv, _ := newServer(t, testToken)

	code, _ := postRaw(t, srv, []byte("not actually gzip"), map[string]string{
		"Content-Encoding": "gzip",
	})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestUnsupportedEncodingRejected(t *testing.T) {
	srv, _ := newServer(t, testToken)

	code, out := postRaw(t, srv, []byte("{}"), map[string]string{"Content-Encoding": "br"})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if !strings.Contains(out, "gzip") {
		t.Errorf("error should name what is supported: %s", out)
	}
}

// TestGzipBombRejected is why accepting compression is safe: the limit
// applies to what the body expands into, not just what arrived.
func TestGzipBombRejected(t *testing.T) {
	srv, _ := newServer(t, testToken)

	// Highly compressible JSON that inflates past the decoded cap.
	var b strings.Builder
	b.WriteString(`{"host":{"id":"web-01","hostname":"`)
	b.WriteString(strings.Repeat("x", api.MaxDecodedBytes+1))
	b.WriteString(`"},"samples":[]}`)

	compressed := gzipped(t, b.String())
	if len(compressed) > api.MaxBodyBytes {
		t.Fatalf("test payload is %d compressed bytes, which the wire limit would catch first", len(compressed))
	}

	code, _ := postRaw(t, srv, compressed, map[string]string{"Content-Encoding": "gzip"})
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", code)
	}
}

// TestRejectsFutureTimestamps covers the clock-skew rule. A sample dated
// next year would otherwise sit at the top of every "latest" query until it
// aged out, hiding everything real behind it.
func TestRejectsFutureTimestamps(t *testing.T) {
	srv, _ := newServer(t, testToken)

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON(future, 0))

	code, out := postRaw(t, srv, []byte(body), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(out, "time sync") {
		t.Errorf("error should point at the agent's clock: %s", out)
	}
}

func TestAcceptsPastTimestamps(t *testing.T) {
	srv, _ := newServer(t, testToken)

	// Backfill from a buffered outage is indistinguishable from an old
	// clock, and is the common case. It must not be rejected.
	past := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON(past, 0))

	if code, out := postRaw(t, srv, []byte(body), nil); code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", code, out)
	}
}

// TestProcessDataSurvivesTheRoundTrip is the end-to-end check on the
// nullable fields: "not measured" must still be null after crossing the
// wire, the database, and the read API.
func TestProcessDataSurvivesTheRoundTrip(t *testing.T) {
	srv, _ := newServer(t, testToken)

	body := `{"host":{"id":"web-01"},"samples":[{
		"at":"2026-11-14T12:00:00Z",
		"processes":{
			"total":284,"threads":1104,"running":3,"sleeping":280,"blocked":1,
			"top":[
				{"pid":1234,"ppid":1,"name":"nginx","command":"nginx: worker process",
				 "state":"S","uid":33,"user":"www-data","threads":1,
				 "cpu_pct":780,"rss_bytes":545259520,"mem_pct":3.2,
				 "io":{"read_bytes_per_sec":1048576,"write_bytes_per_sec":0}},
				{"pid":99,"name":"kworker","state":"I","cpu_pct":0.1,"rss_bytes":4096}
			]
		}}]}`

	if code, out := postRaw(t, srv, []byte(body), nil); code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, out)
	}

	remote, err := client.New(srv.URL, testToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	h, err := remote.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}
	if h.Processes == nil || len(h.Processes.Top) != 2 {
		t.Fatalf("processes = %+v", h.Processes)
	}

	first := h.Processes.Top[0]
	if first.CPUPct != 780 {
		t.Errorf("CPUPct = %v, want 780 — a per-CPU share must survive unclamped", first.CPUPct)
	}
	if first.User == nil || *first.User != "www-data" {
		t.Errorf("User = %v, want www-data", first.User)
	}
	if first.IO == nil || first.IO.ReadBytesPerSec != 1048576 {
		t.Errorf("IO = %+v", first.IO)
	}

	second := h.Processes.Top[1]
	if second.User != nil {
		t.Errorf("unresolved user came back as %q, want null", *second.User)
	}
	if second.IO != nil {
		t.Errorf("unmeasured io came back as %+v, want null — null is not zero", second.IO)
	}
}

// TestCPUDerivedOverTheWire checks that percentages reach a remote reader
// already derived, since the raw tick counters deliberately do not travel.
func TestCPUDerivedOverTheWire(t *testing.T) {
	srv, _ := newServer(t, testToken)

	body := fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s,%s]}`,
		sampleJSON("2026-11-14T12:00:00Z", 0),
		sampleJSON("2026-11-14T12:00:10Z", 1))
	if code, out := postRaw(t, srv, []byte(body), nil); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, out)
	}

	remote, err := client.New(srv.URL, testToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	h, err := remote.Host(context.Background(), "web-01", 10)
	if err != nil {
		t.Fatalf("Host: %v", err)
	}

	cpu := h.Latest().CPU
	if !cpu.Derived {
		t.Fatal("CPU not derived for a remote reader")
	}
	if cpu.BusyPct != 40 {
		t.Errorf("BusyPct = %v, want 40", cpu.BusyPct)
	}
	if cpu.StealPct != 5 {
		t.Errorf("StealPct = %v, want 5", cpu.StealPct)
	}
}

// TestErrorCodes pins the machine-readable classification on every
// rejection. The agent branches on these to tell an operator-fixable
// problem (a skewed clock on the monitored box) from one of its own
// (a malformed payload), so the strings are contract, not cosmetics.
func TestErrorCodes(t *testing.T) {
	srv, _ := newServer(t, testToken)
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name   string
		body   string
		header map[string]string
		status int
		code   protocol.ErrorCode
	}{
		{
			name:   "clock skew",
			body:   fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON(future, 0)),
			status: http.StatusBadRequest,
			code:   protocol.CodeClockSkew,
		},
		{
			name:   "malformed",
			body:   `{not json`,
			status: http.StatusBadRequest,
			code:   protocol.CodeMalformed,
		},
		{
			name:   "invalid",
			body:   `{"host":{},"samples":[{"at":"2026-11-14T12:00:00Z"}]}`,
			status: http.StatusBadRequest,
			code:   protocol.CodeInvalid,
		},
		{
			name:   "unsupported encoding",
			body:   `{}`,
			header: map[string]string{"Content-Encoding": "br"},
			status: http.StatusBadRequest,
			code:   protocol.CodeUnsupportedEncoding,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out := postRaw(t, srv, []byte(tc.body), tc.header)
			if status != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.status, out)
			}
			var body protocol.Error
			if err := json.Unmarshal([]byte(out), &body); err != nil {
				t.Fatalf("decode error body: %v (%s)", err, out)
			}
			if body.Code != tc.code {
				t.Errorf("code = %q, want %q (body %s)", body.Code, tc.code, out)
			}
			if body.Error == "" {
				t.Error("error body carries a code but no message for the log")
			}
		})
	}
}

// TestClockSkewIsDistinguishableFromMalformed is the specific confusion the
// agent asked to be able to avoid: both are 400s it must not retry, but one
// is fixed by the operator and one by the agent's author.
func TestClockSkewIsDistinguishableFromMalformed(t *testing.T) {
	srv, _ := newServer(t, testToken)
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)

	_, skewed := postRaw(t, srv, []byte(
		fmt.Sprintf(`{"host":{"id":"web-01"},"samples":[%s]}`, sampleJSON(future, 0))), nil)
	_, broken := postRaw(t, srv, []byte(`{not json`), nil)

	var a, b protocol.Error
	if err := json.Unmarshal([]byte(skewed), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal([]byte(broken), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Code == b.Code {
		t.Fatalf("both rejections carry code %q; they must be distinguishable", a.Code)
	}
}

func TestTooLargeCarriesItsCode(t *testing.T) {
	srv, _ := newServer(t, testToken)

	var b strings.Builder
	b.WriteString(`{"host":{"id":"web-01","hostname":"`)
	b.WriteString(strings.Repeat("x", api.MaxBodyBytes+1))
	b.WriteString(`"},"samples":[]}`)

	status, out := postRaw(t, srv, []byte(b.String()), nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", status)
	}
	var body protocol.Error
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if body.Code != protocol.CodeTooLarge {
		t.Errorf("code = %q, want %q", body.Code, protocol.CodeTooLarge)
	}
	// 413 is the one 4xx the agent can act on unaided, so say how.
	if !strings.Contains(body.Error, "halve") {
		t.Errorf("413 message does not say what to do: %q", body.Error)
	}
}

func TestUnauthorizedCarriesItsCode(t *testing.T) {
	srv, _ := newServer(t, testToken)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/hosts", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var body protocol.Error
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if body.Code != protocol.CodeUnauthorized {
		t.Errorf("code = %q, want %q", body.Code, protocol.CodeUnauthorized)
	}
}
