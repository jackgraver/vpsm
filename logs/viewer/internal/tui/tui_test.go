package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"vpsm-logs/internal/filter"
	"vpsm-logs/internal/load"
	"vpsm-logs/internal/logline"
)

func newTestModel(t *testing.T, lines ...string) *model {
	t.Helper()
	set, err := load.Load([]string{load.StdinName}, strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(Options{Set: set, Version: "test"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	return m
}

// press sends keys: a single character as typed text, anything longer
// by its Bubble Tea name.
func press(m *model, keys ...string) {
	names := map[string]tea.KeyType{
		"enter": tea.KeyEnter, "esc": tea.KeyEsc, "backspace": tea.KeyBackspace,
		"up": tea.KeyUp, "down": tea.KeyDown, "space": tea.KeySpace,
	}
	for _, k := range keys {
		if t, ok := names[k]; ok {
			m.Update(tea.KeyMsg{Type: t})
		} else {
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
	}
}

func selectedText(m *model) string {
	if e, ok := m.selected(); ok {
		return e.Text
	}
	return ""
}

var sample = []string{
	"10:00 INFO starting",
	"10:01 WARN disk 91%",
	"10:02 ERROR db timeout",
	"10:03 INFO request ok",
	"10:04 ERROR cache Timeout",
	"10:05 INFO done",
}

func TestStartsAtNewestLine(t *testing.T) {
	m := newTestModel(t, sample...)
	if got := selectedText(m); got != "10:05 INFO done" {
		t.Errorf("selected = %q", got)
	}
}

func TestSearchFiltersLiveAsYouType(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "/", "t", "i", "m")
	if len(m.visible) != 2 {
		t.Errorf("visible = %d after typing, want 2 (smart-case matches both)", len(m.visible))
	}
	if !m.editing {
		t.Error("should still be editing")
	}
	press(m, "enter")
	if m.editing || m.filter.Query != "tim" {
		t.Errorf("editing = %v query = %q, want the search kept", m.editing, m.filter.Query)
	}
	view := m.View()
	if !strings.Contains(view, "2 of 6 lines") {
		t.Errorf("header does not count matches:\n%s", view)
	}
	if strings.Contains(view, "request ok") {
		t.Errorf("non-matching line shown:\n%s", view)
	}
}

func TestBurstOfTypedKeysIsSplit(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "/timeout") // one event, as fast typing and tmux deliver it
	if !m.editing || m.filter.Query != "timeout" {
		t.Errorf("editing = %v query = %q, want search mode with \"timeout\"", m.editing, m.filter.Query)
	}
	press(m, "enter", "ll")
	if m.filter.MinLevel != logline.LevelDebug+1 {
		t.Errorf("level = %v, want two steps (INFO)", m.filter.MinLevel)
	}
}

func TestUppercaseSearchIsExact(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "/", "T", "i", "m", "e", "enter")
	if len(m.visible) != 1 || selectedText(m) != "10:04 ERROR cache Timeout" {
		t.Errorf("visible = %d, selected %q", len(m.visible), selectedText(m))
	}
}

func TestBackspaceWidensAndEscClears(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "/", "o", "k", "backspace")
	if m.filter.Query != "o" {
		t.Errorf("query = %q", m.filter.Query)
	}
	press(m, "esc")
	if m.editing || m.filter.Query != "" || len(m.visible) != len(sample) {
		t.Errorf("esc while typing should clear: editing=%v query=%q visible=%d", m.editing, m.filter.Query, len(m.visible))
	}
}

func TestLevelCycleAndEscPeelsFilters(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "l", "l", "l") // all -> debug -> info -> warn
	if m.filter.MinLevel != logline.LevelWarn || len(m.visible) != 3 {
		t.Fatalf("level = %v visible = %d, want WARN and 3", m.filter.MinLevel, len(m.visible))
	}
	if !strings.Contains(m.View(), "≥WARN") {
		t.Error("header does not show the level filter")
	}
	press(m, "/", "d", "b", "enter")
	if len(m.visible) != 1 {
		t.Fatalf("visible = %d, want the one ERROR mentioning db", len(m.visible))
	}
	press(m, "esc")
	if m.filter.Query != "" || m.filter.MinLevel != logline.LevelWarn {
		t.Errorf("first esc should drop the search only: %+v", m.filter)
	}
	press(m, "esc")
	if m.filter.Active() {
		t.Errorf("second esc should drop the level: %+v", m.filter)
	}
	press(m, "L")
	if m.filter.MinLevel != logline.LevelError {
		t.Errorf("L from all should wrap to ERROR, got %v", m.filter.MinLevel)
	}
}

func TestCursorStaysOnItsLineAcrossFilterChanges(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "/", "e", "r", "r", "o", "r", "enter", "k") // up to the first ERROR
	if selectedText(m) != "10:02 ERROR db timeout" {
		t.Fatalf("selected = %q", selectedText(m))
	}
	press(m, "esc")
	if selectedText(m) != "10:02 ERROR db timeout" {
		t.Errorf("after clearing, selected = %q, want the same line", selectedText(m))
	}
}

func TestNoMatchesSaysHowToRecover(t *testing.T) {
	m := newTestModel(t, sample...)
	press(m, "/", "z", "z", "z", "enter")
	if !strings.Contains(m.View(), "No lines match") {
		t.Errorf("no empty-state message:\n%s", m.View())
	}
	press(m, "enter") // opening detail with nothing selected must not panic
	if m.detail {
		t.Error("opened detail with no selection")
	}
}

func TestDetailPrettyPrintsJSON(t *testing.T) {
	m := newTestModel(t, `{"level":"error","msg":"boom","ctx":{"user":42}}`)
	press(m, "enter")
	view := m.View()
	for _, want := range []string{"stdin:1", "ERROR", `  "ctx": {`, `    "user": 42`} {
		if !strings.Contains(view, want) {
			t.Errorf("detail lacks %q:\n%s", want, view)
		}
	}
	press(m, "esc")
	if m.detail {
		t.Error("esc should close detail")
	}
}

func TestLongLinesAreTruncatedToWidth(t *testing.T) {
	m := newTestModel(t, strings.Repeat("y", 500))
	for _, line := range strings.Split(m.View(), "\n") {
		if w := len([]rune(stripANSI(line))); w > 100 {
			t.Errorf("line is %d columns wide, terminal is 100", w)
		}
	}
}

func TestInitialFilterFromOptions(t *testing.T) {
	set, _ := load.Load([]string{load.StdinName}, strings.NewReader(strings.Join(sample, "\n")))
	m := newModel(Options{Set: set, Filter: filter.Filter{MinLevel: logline.LevelError}})
	if len(m.visible) != 2 {
		t.Errorf("visible = %d, want the 2 ERROR lines", len(m.visible))
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case in:
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				in = false
			}
		case r == 0x1b:
			in = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
