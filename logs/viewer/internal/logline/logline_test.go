package logline

import (
	"strings"
	"testing"
	"time"
)

func parseOne(t *testing.T, raw string) Entry {
	t.Helper()
	p := &Parser{Now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.Local)}
	e, ok := p.Next(raw)
	if !ok {
		t.Fatalf("Next(%q) produced no entry", raw)
	}
	return e
}

func TestDetectsLevelAcrossFormats(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Level
	}{
		{"plain uppercase", "2026-11-16 10:00:00 ERROR db: connection refused", LevelError},
		{"bracketed lowercase (nginx)", "2026/11/16 10:00:00 [warn] 123#0: upstream slow", LevelWarn},
		{"python logging", "WARNING:root:disk nearly full", LevelWarn},
		{"go slog text", `time=2026-11-16T10:00:00Z level=INFO msg="started"`, LevelInfo},
		{"logfmt quoted", `lvl="debug" msg=tick`, LevelDebug},
		{"json zap", `{"level":"error","ts":1763287200.5,"msg":"boom"}`, LevelError},
		{"json pino numeric", `{"level":40,"time":1763287200000,"msg":"slow"}`, LevelWarn},
		{"json journald", `{"PRIORITY":"3","MESSAGE":"failed","__REALTIME_TIMESTAMP":"1763287200000000"}`, LevelError},
		{"json without level", `{"msg":"hello"}`, LevelUnknown},
		{"access log 502", `1.2.3.4 - - [16/Nov/2026:10:00:00 +0000] "GET /api HTTP/1.1" 502 157 "-" "curl"`, LevelError},
		{"access log 404", `1.2.3.4 - - [16/Nov/2026:10:00:00 +0000] "GET /x HTTP/1.1" 404 0 "-" "bot"`, LevelWarn},
		{"access log 200", `1.2.3.4 - - [16/Nov/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 512 "-" "curl"`, LevelInfo},
		{"no level", "just some output", LevelUnknown},
		{"lowercase word is message text", "retrying after error from upstream", LevelUnknown},
		{"uppercase word inside identifier", "INFORMATION_SCHEMA query ran", LevelUnknown},
		{"level word far into the line", strings.Repeat("x", 130) + " ERROR", LevelUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseOne(t, c.raw).Level; got != c.want {
				t.Errorf("level = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDetectsTimeAcrossFormats(t *testing.T) {
	utc := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	local := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"rfc3339", "2026-11-16T10:00:00Z hello", utc},
		{"rfc3339 offset", "2026-11-16T11:00:00+01:00 hello", utc},
		{"space, no zone, is local", "2026-11-16 10:00:00 hello", local},
		{"comma millis (python)", "2026-11-16 10:00:00,000 INFO hello", local},
		{"go log slashes", "2026/11/16 10:00:00 hello", local},
		{"bracketed", "[2026-11-16 10:00:00] hello", local},
		{"syslog without year", "Nov 16 10:00:00 host sshd[1]: hello", local},
		{"clf", `1.2.3.4 - - [16/Nov/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 1 "-" "-"`, utc},
		{"logfmt", `level=info ts=2026-11-16T10:00:00Z msg=hi`, utc},
		{"json epoch seconds", `{"ts":1794823200}`, time.Unix(1794823200, 0)},
		{"json epoch millis", `{"time":1794823200000}`, time.UnixMilli(1794823200000)},
		{"json iso", `{"@timestamp":"2026-11-16T10:00:00Z"}`, utc},
		{"none", "hello", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseOne(t, c.raw).Time; !got.Equal(c.want) {
				t.Errorf("time = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSyslogYearDoesNotLandInTheFuture(t *testing.T) {
	p := &Parser{Now: time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local)}
	e, _ := p.Next("Dec 31 23:00:00 host cron: ran")
	if e.Time.Year() != 2025 {
		t.Errorf("year = %d, want 2025: a December line read in January is last year's", e.Time.Year())
	}
}

func TestUnwrapsDocker(t *testing.T) {
	p := &Parser{}
	e, ok := p.Next(`{"log":"\u001b[31mERROR\u001b[0m db down\n","stream":"stderr","time":"2026-11-16T10:00:00.123456789Z"}`)
	if !ok {
		t.Fatal("no entry")
	}
	if e.Text != "2026-11-16T10:00:00.123Z ERROR db down" {
		t.Errorf("text = %q", e.Text)
	}
	if e.Level != LevelError || e.Stream != "stderr" {
		t.Errorf("level = %v stream = %q", e.Level, e.Stream)
	}
	if want := time.Date(2026, 11, 16, 10, 0, 0, 123456789, time.UTC); !e.Time.Equal(want) {
		t.Errorf("time = %v, want %v", e.Time, want)
	}
}

func TestDockerPrefersTheAppsOwnTimestamp(t *testing.T) {
	e := parseOne(t, `{"log":"2026-11-16T09:59:59Z INFO hi\n","stream":"stdout","time":"2026-11-16T10:00:00Z"}`)
	if want := time.Date(2026, 11, 16, 9, 59, 59, 0, time.UTC); !e.Time.Equal(want) {
		t.Errorf("time = %v, want the app's %v", e.Time, want)
	}
}

func TestJoinsDockerPartialLines(t *testing.T) {
	p := &Parser{}
	if _, ok := p.Next(`{"log":"ERROR first half ","stream":"stdout","time":"2026-11-16T10:00:00Z"}`); ok {
		t.Fatal("partial line produced an entry")
	}
	e, ok := p.Next(`{"log":"second half\n","stream":"stdout","time":"2026-11-16T10:00:00Z"}`)
	if !ok || !strings.HasSuffix(e.Text, "ERROR first half second half") || e.Level != LevelError {
		t.Errorf("joined = %+v", e)
	}
	if e.Line != 2 {
		t.Errorf("line = %d, want 2 (the line that completed it)", e.Line)
	}
}

func TestFlushReturnsTrailingPartial(t *testing.T) {
	p := &Parser{}
	p.Next(`{"log":"cut off","stream":"stdout","time":"2026-11-16T10:00:00Z"}`)
	if e, ok := p.Flush(); !ok || e.Text != "cut off" {
		t.Errorf("flush = %+v, %v", e, ok)
	}
	if _, ok := p.Flush(); ok {
		t.Error("second flush returned an entry")
	}
}

func TestStackTraceInheritsLevel(t *testing.T) {
	p := &Parser{}
	lines := []string{
		"2026-11-16 10:00:00 ERROR request failed",
		"java.lang.IllegalStateException: nope",
		"    at com.example.Handler.run(Handler.java:42)",
		"Caused by: java.io.IOException",
		"2026-11-16 10:00:01 INFO recovered",
	}
	var got []Entry
	for _, l := range lines {
		e, _ := p.Next(l)
		got = append(got, e)
	}
	for _, i := range []int{1, 2, 3} {
		if !got[i].Continuation || got[i].Level != LevelError || got[i].Time.IsZero() {
			t.Errorf("line %d = %+v, want a continuation inheriting ERROR and the time", i+1, got[i])
		}
	}
	if got[4].Continuation || got[4].Level != LevelInfo {
		t.Errorf("line 5 = %+v", got[4])
	}
}

func TestUntimestampedLogOnlyJoinsIndentedLines(t *testing.T) {
	p := &Parser{}
	var got []Entry
	for _, l := range []string{"ERROR failed", "next message", "  detail of it"} {
		e, _ := p.Next(l)
		got = append(got, e)
	}
	if got[1].Continuation {
		t.Error("with no timestamps to go by, an unindented line is its own entry")
	}
	if !got[2].Continuation {
		t.Error("an indented line should still be a continuation")
	}
}

func TestDockerStackTraceInheritsLevel(t *testing.T) {
	p := &Parser{}
	p.Next(`{"log":"2026-11-16 10:00:00 ERROR boom\n","stream":"stderr","time":"2026-11-16T10:00:00Z"}`)
	e, _ := p.Next(`{"log":"panic: nil map\n","stream":"stderr","time":"2026-11-16T10:00:00.001Z"}`)
	if !e.Continuation || e.Level != LevelError {
		t.Errorf("entry = %+v, want a continuation of the ERROR despite Docker's own timestamp", e)
	}
}

func TestCleanStripsEscapesAndControls(t *testing.T) {
	cases := map[string]string{
		"plain":                    "plain",
		"\x1b[1;31mred\x1b[0m":     "red",
		"a\tb":                     "a    b",
		"10%\r50%\r100% done":      "100% done",
		"bell\x07 and nul\x00 too": "bell and nul too",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnwrapsJournalLines(t *testing.T) {
	p := &Parser{}
	e, ok := p.Next(`{"cursor":"s=1","t":1794823200123000,"src":"web","stream":"stderr","msg":"\u001b[33mWARN\u001b[0m slow"}`)
	if !ok {
		t.Fatal("no entry")
	}
	if e.Text != "2026-11-16T10:00:00.123Z WARN slow" || e.Origin != "web" || e.Stream != "stderr" || e.Level != LevelWarn {
		t.Errorf("entry = %+v", e)
	}
	if want := time.UnixMicro(1794823200123000); !e.Time.Equal(want) {
		t.Errorf("time = %v, want %v", e.Time, want)
	}
}

func TestJournalPriorityIsAFallbackForTheText(t *testing.T) {
	p := &Parser{}
	e, _ := p.Next(`{"cursor":"a","src":"sshd","pri":3,"msg":"Connection reset"}`)
	if e.Level != LevelError {
		t.Errorf("level = %v, want ERROR from the priority", e.Level)
	}
	e, _ = p.Next(`{"cursor":"b","src":"caddy","pri":3,"msg":"{\"level\":\"info\",\"msg\":\"served\"}"}`)
	if e.Level != LevelInfo {
		t.Errorf("level = %v, want the text's INFO over the priority", e.Level)
	}
}

func TestMultilineJournalMessageStaysOneEntry(t *testing.T) {
	e := parseOne(t, `{"cursor":"a","src":"app","msg":"first\nsecond\n"}`)
	if !strings.HasSuffix(e.Text, "first ↵ second") {
		t.Errorf("text = %q", e.Text)
	}
}

func TestContinuationStaysWithinItsService(t *testing.T) {
	p := &Parser{}
	lines := []string{
		`{"cursor":"1","src":"web","msg":"2026-11-16 10:00:00 ERROR boom"}`,
		`{"cursor":"2","src":"caddy","msg":"2026-11-16 10:00:00 INFO served"}`,
		`{"cursor":"3","src":"web","msg":"    at handler (app.js:1)"}`,
		`{"cursor":"4","src":"caddy","msg":"no timestamp here"}`,
	}
	var got []Entry
	for _, l := range lines {
		e, _ := p.Next(l)
		got = append(got, e)
	}
	if !got[2].Continuation || got[2].Level != LevelError {
		t.Errorf("web's stack frame = %+v, want it to continue web's ERROR, not caddy's INFO", got[2])
	}
	if !got[3].Continuation || got[3].Level != LevelInfo {
		t.Errorf("caddy's line = %+v, want it to continue caddy's INFO", got[3])
	}
}

func TestUnrecognisedCursorLineIsPlainJSON(t *testing.T) {
	e := parseOne(t, `{"cursor":"x","level":"warn"`) // truncated: not a frame
	if e.Origin != "" || e.Text != `{"cursor":"x","level":"warn"` {
		t.Errorf("entry = %+v", e)
	}
}

func TestIsProbe(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`[GIN] 2026/11/16 - 21:13:06 | 200 |    1.358552ms |       127.0.0.1 | GET      "/health"`, true},
		{`127.0.0.1 - - [17/Nov/2026:03:13:51 +0000] "GET / HTTP/1.1" 200 1438 "-" "Wget"`, true},
		{`10.0.0.5 - - [17/Nov/2026:03:13:51 +0000] "GET /healthz HTTP/1.1" 200 2 "-" "kube-probe"`, true},
		{`[GIN] 2026/11/16 - 21:12:45 | 200 |  83.1ms |      172.18.0.1 | POST     "/auth/login"`, false},
		{`172.18.0.1 - - [17/Nov/2026:03:12:45 +0000] "GET /assets/gym.js HTTP/1.1" 200 7521 "-" "Mozilla"`, false},
		{`[GIN] 2026/11/16 - 21:12:45 | 200 |  5ms |      172.18.0.1 | GET      "/healthy-recipes"`, false},
		{`time="..." level=error msg="healthcheck failed for container"`, false},
		{`INFO health insurance quote requested`, false},
	}
	for _, c := range cases {
		if got := IsProbe(c.line); got != c.want {
			t.Errorf("IsProbe(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestGinRequestLines(t *testing.T) {
	p := &Parser{}
	lines := []string{
		`{"cursor":"1","src":"backend","msg":"2026/11/16 21:12:45 INFO authentication login attempt"}`,
		`{"cursor":"2","src":"backend","msg":"[GIN] 2026/11/16 - 21:12:45 | 200 |   83.1ms |  172.18.0.1 | POST     \"/auth/login\""}`,
		`{"cursor":"3","src":"backend","msg":"[GIN] 2026/11/16 - 21:12:42 | 401 |   24.6µs |  172.18.0.1 | GET      \"/auth/me\""}`,
		`{"cursor":"4","src":"backend","msg":"[GIN] 2026/11/16 - 21:12:46 | 502 |   1.2ms |  172.18.0.1 | GET      \"/money\""}`,
	}
	want := []Level{LevelInfo, LevelInfo, LevelWarn, LevelError}
	for i, l := range lines {
		e, _ := p.Next(l)
		if e.Level != want[i] || e.Continuation {
			t.Errorf("line %d: level %v continuation %v, want %v and its own entry", i+1, e.Level, e.Continuation, want[i])
		}
	}
	e := parseOne(t, `[GIN] 2026/11/16 - 21:12:45 | 200 | 1ms | 1.2.3.4 | GET "/x"`)
	if want := time.Date(2026, 11, 16, 21, 12, 45, 0, time.Local); !e.Time.Equal(want) {
		t.Errorf("time = %v, want %v", e.Time, want)
	}
}
