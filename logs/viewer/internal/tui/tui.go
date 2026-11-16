// Package tui is the interactive log viewer: every loaded line with its
// guessed level, narrowed live by a search and a minimum level.
//
// The model holds the full set and a list of the indices that pass the
// filter; changing the filter recomputes that list and keeps the cursor on
// the same line, or the nearest one after it, so narrowing a search never
// throws away where you were looking.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"vpsm-logs/internal/filter"
	"vpsm-logs/internal/load"
	"vpsm-logs/internal/logline"
)

// Options configure the viewer.
type Options struct {
	Set *load.Set
	// Filter is applied from the start, e.g. from command-line flags.
	Filter  filter.Filter
	Version string
	// TTYInput reads keys from /dev/tty rather than stdin, for when stdin
	// is the log being viewed.
	TTYInput bool
}

// Run shows the viewer until the user quits.
func Run(opts Options) error {
	progOpts := []tea.ProgramOption{tea.WithAltScreen()}
	if opts.TTYInput {
		progOpts = append(progOpts, tea.WithInputTTY())
	}
	_, err := tea.NewProgram(newModel(opts), progOpts...).Run()
	return err
}

// levelSteps is what `l` cycles the minimum level through.
var levelSteps = []logline.Level{
	logline.LevelUnknown, logline.LevelDebug, logline.LevelInfo, logline.LevelWarn, logline.LevelError,
}

type model struct {
	opts    Options
	set     *load.Set
	filter  filter.Filter
	visible []int // indices into set.Entries that pass the filter

	cursor int // index into visible
	offset int // first visible row on screen

	editing      bool // typing into the search
	detail       bool // showing the selected entry in full
	detailOffset int

	labels []string // short per-source name, when there is more than one
	labelW int

	width, height int
}

func newModel(opts Options) *model {
	m := &model{opts: opts, set: opts.Set, filter: opts.Filter}
	if len(m.set.Sources) > 1 {
		m.labels = shortNames(m.set.Sources)
		for _, l := range m.labels {
			m.labelW = max(m.labelW, runewidth.StringWidth(l))
		}
	}
	m.refilter()
	// Start at the newest line, as tail does.
	m.cursor = max(0, len(m.visible)-1)
	return m
}

func (m *model) Init() tea.Cmd { return nil }

// refilter recomputes the visible lines, keeping the cursor on the entry
// it was on, or else the first visible one after it.
func (m *model) refilter() {
	anchor := -1
	if m.cursor >= 0 && m.cursor < len(m.visible) {
		anchor = m.visible[m.cursor]
	}
	m.visible = m.filter.Apply(m.set.Entries)
	if anchor < 0 {
		m.cursor = 0
		return
	}
	i := sort.SearchInts(m.visible, anchor)
	m.cursor = min(i, max(0, len(m.visible)-1))
}

func (m *model) selected() (*logline.Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return nil, false
	}
	return &m.set.Entries[m.visible[m.cursor]], true
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	// Fast typing and pastes arrive as one event carrying several runes.
	// Outside the search box each is a separate command ("/err" is "/"
	// then "err" typed into the search), so replay them one at a time.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !m.editing {
		var cmd tea.Cmd
		for i, r := range k.Runes {
			if m.editing {
				m.handleEditKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: k.Runes[i:]})
				break
			}
			if c := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}); c != nil {
				cmd = c
			}
		}
		return cmd
	}
	switch {
	case m.editing:
		m.handleEditKey(k)
	case m.detail:
		m.handleDetailKey(key)
	default:
		return m.handleListKey(key)
	}
	return nil
}

func (m *model) handleListKey(key string) tea.Cmd {
	page := max(1, m.rows()-1)
	switch key {
	case "q":
		return tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup", "ctrl+u", "b":
		m.move(-page)
	case "pgdown", "ctrl+d", " ", "f":
		m.move(page)
	case "home", "g":
		m.move(-1 << 30)
	case "end", "G":
		m.move(1 << 30)
	case "/":
		m.editing = true
	case "l":
		m.stepLevel(1)
	case "L":
		m.stepLevel(-1)
	case "esc":
		// Peel the filter back one layer: the search, then the level.
		if m.filter.Query != "" {
			m.filter.Query = ""
		} else {
			m.filter.MinLevel = logline.LevelUnknown
		}
		m.refilter()
	case "enter":
		if _, ok := m.selected(); ok {
			m.detail, m.detailOffset = true, 0
		}
	}
	return nil
}

func (m *model) handleEditKey(k tea.KeyMsg) {
	switch k.Type {
	case tea.KeyEnter:
		m.editing = false
		return
	case tea.KeyEsc:
		m.editing = false
		m.filter.Query = ""
	case tea.KeyBackspace:
		r := []rune(m.filter.Query)
		if len(r) > 0 {
			m.filter.Query = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		m.filter.Query = ""
	case tea.KeySpace:
		m.filter.Query += " "
	case tea.KeyRunes:
		m.filter.Query += string(k.Runes)
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		// Let the list move while typing, to check what the search hit.
		m.handleListKey(k.String())
		return
	default:
		return
	}
	m.refilter()
}

func (m *model) handleDetailKey(key string) {
	switch key {
	case "q", "esc", "enter", "backspace", "left", "h":
		m.detail = false
	case "up", "k":
		m.detailOffset = max(0, m.detailOffset-1)
	case "down", "j":
		m.detailOffset++
	case "pgup", "ctrl+u", "b":
		m.detailOffset = max(0, m.detailOffset-m.rows())
	case "pgdown", "ctrl+d", " ", "f":
		m.detailOffset += m.rows()
	case "home", "g":
		m.detailOffset = 0
	}
}

func (m *model) move(delta int) {
	m.cursor = min(max(m.cursor+delta, 0), max(0, len(m.visible)-1))
}

func (m *model) stepLevel(dir int) {
	i := 0
	for j, l := range levelSteps {
		if l == m.filter.MinLevel {
			i = j
		}
	}
	i = (i + dir + len(levelSteps)) % len(levelSteps)
	m.filter.MinLevel = levelSteps[i]
	m.refilter()
}

// shortNames gives each source the shortest name that tells it apart: the
// base name, or the full path where base names collide.
func shortNames(sources []string) []string {
	count := map[string]int{}
	for _, s := range sources {
		count[filepath.Base(s)]++
	}
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = filepath.Base(s)
		if count[out[i]] > 1 {
			out[i] = s
		}
		out[i] = runewidth.Truncate(out[i], 20, "…")
	}
	return out
}

// ---- view ----

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCursor = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleMatch  = lipgloss.NewStyle().Background(lipgloss.Color("3")).Foreground(lipgloss.Color("0"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleInput  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))

	levelStyles = map[logline.Level]lipgloss.Style{
		logline.LevelTrace: styleDim,
		logline.LevelDebug: lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
		logline.LevelInfo:  lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		logline.LevelWarn:  lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true),
		logline.LevelError: lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
		logline.LevelFatal: lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true).Reverse(true),
	}
	levelBadges = map[logline.Level]string{
		logline.LevelTrace: "TRC", logline.LevelDebug: "DBG", logline.LevelInfo: "INF",
		logline.LevelWarn: "WRN", logline.LevelError: "ERR", logline.LevelFatal: "FTL",
	}
)

// rows is the number of log lines that fit between header and footer.
func (m *model) rows() int { return max(1, m.height-3) }

func (m *model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	var body []string
	if m.detail {
		body = m.viewDetail()
	} else {
		body = m.viewList()
	}
	for len(body) < m.rows() {
		body = append(body, "")
	}
	return m.viewHeader() + "\n" + strings.Join(body, "\n") + "\n" + m.viewFooter()
}

func (m *model) viewHeader() string {
	left := styleTitle.Render("vpsm-logs")
	if m.opts.Version != "" {
		left += styleDim.Render(" " + m.opts.Version)
	}
	src := m.set.Sources[0]
	if n := len(m.set.Sources); n > 1 {
		src = fmt.Sprintf("%d sources", n)
	}
	left += styleDim.Render("  " + src)

	right := fmt.Sprintf("%s lines", commas(len(m.set.Entries)))
	if m.filter.Active() {
		right = fmt.Sprintf("%s of %s", commas(len(m.visible)), right)
	}
	if m.filter.MinLevel != logline.LevelUnknown {
		right = levelStyles[m.filter.MinLevel].Render("≥"+m.filter.MinLevel.String()) + "  " + right
	}
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) viewFooter() string {
	var line string
	switch {
	case m.editing:
		line = styleInput.Render("/"+m.filter.Query) + styleCursor.Render("█")
	case m.detail:
		line = ""
	case m.filter.Query != "":
		how := "any case"
		if m.filter.CaseSensitive() {
			how = "exact case"
		}
		line = styleInput.Render("/"+m.filter.Query) + styleDim.Render("  ("+how+")")
	}
	var keys string
	switch {
	case m.editing:
		keys = "type to filter · enter keep · esc clear · ↑↓ move"
	case m.detail:
		keys = "↑↓/pgup/pgdn scroll · esc back"
	default:
		keys = "↑↓/pgup/pgdn move · g/G top/bottom · / search · l/L level · esc clear · enter detail · q quit"
	}
	return truncateStyled(line, m.width) + "\n" + styleDim.Render(runewidth.Truncate(keys, m.width, "…"))
}

func (m *model) viewList() []string {
	rows := m.rows()
	if len(m.set.Entries) == 0 {
		return []string{styleDim.Render("(no lines)")}
	}
	if len(m.visible) == 0 {
		return []string{styleDim.Render("No lines match — esc clears the search, l/L changes the level.")}
	}
	// Scroll just enough to keep the cursor on screen.
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	m.offset = min(max(m.offset, 0), max(0, len(m.visible)-rows))

	out := make([]string, 0, rows)
	for i := m.offset; i < len(m.visible) && i < m.offset+rows; i++ {
		out = append(out, m.viewRow(&m.set.Entries[m.visible[i]], i == m.cursor))
	}
	return out
}

func (m *model) viewRow(e *logline.Entry, selected bool) string {
	gutter := "  "
	if selected {
		gutter = styleCursor.Render("▌ ")
	}
	badge := "   "
	if b, ok := levelBadges[e.Level]; ok {
		style := levelStyles[e.Level]
		if e.Continuation {
			style = styleDim
		}
		badge = style.Render(b)
	}
	prefix := gutter + badge + " "
	used := 2 + 3 + 1
	if m.labels != nil {
		label := m.labels[e.Source]
		prefix += styleDim.Render(label+strings.Repeat(" ", m.labelW-runewidth.StringWidth(label))) + " "
		used += m.labelW + 1
	}
	text := runewidth.Truncate(e.Text, max(0, m.width-used), "…")
	base := lipgloss.NewStyle()
	if selected {
		base = base.Bold(true)
	}
	return prefix + highlight(text, m.filter.Matches(text), base)
}

// highlight renders s with spans in the match style and the rest in base.
func highlight(s string, spans []filter.Span, base lipgloss.Style) string {
	var b strings.Builder
	at := 0
	for _, sp := range spans {
		if sp.Start > at {
			b.WriteString(base.Render(s[at:sp.Start]))
		}
		b.WriteString(styleMatch.Render(s[sp.Start:sp.End]))
		at = sp.End
	}
	if at < len(s) {
		b.WriteString(base.Render(s[at:]))
	}
	return b.String()
}

func (m *model) viewDetail() []string {
	e, ok := m.selected()
	if !ok {
		m.detail = false
		return m.viewList()
	}
	meta := []string{styleTitle.Render(fmt.Sprintf("%s:%d", m.set.Sources[e.Source], e.Line))}
	var facts []string
	if e.Level != logline.LevelUnknown {
		facts = append(facts, levelStyles[e.Level].Render(e.Level.String()))
	} else {
		facts = append(facts, styleDim.Render("no level detected"))
	}
	if !e.Time.IsZero() {
		facts = append(facts, e.Time.Local().Format("2006-01-02 15:04:05.000 MST"))
	} else {
		facts = append(facts, styleDim.Render("no time detected"))
	}
	if e.Stream != "" {
		facts = append(facts, e.Stream)
	}
	if e.Continuation {
		facts = append(facts, styleDim.Render("level and time inherited from the line above"))
	}
	meta = append(meta, strings.Join(facts, styleDim.Render(" · ")), "")

	var body []string
	for _, line := range strings.Split(prettyJSON(e.Text), "\n") {
		body = append(body, wrapHard(line, max(1, m.width))...)
	}
	rows := max(1, m.rows()-len(meta))
	m.detailOffset = min(m.detailOffset, max(0, len(body)-rows))
	end := min(len(body), m.detailOffset+rows)
	return append(meta, body[m.detailOffset:end]...)
}

// prettyJSON indents s if it is a JSON object, and returns it unchanged
// otherwise. A JSON payload after a timestamp prefix is indented too.
func prettyJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return s
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s[start:]), "", "  "); err != nil {
		return s
	}
	if prefix := strings.TrimSpace(s[:start]); prefix != "" {
		return prefix + "\n" + buf.String()
	}
	return buf.String()
}

// wrapHard breaks s into lines of at most w columns, mid-word if need be:
// log lines are often one long token (a URL, a base64 blob).
func wrapHard(s string, w int) []string {
	if s == "" {
		return []string{""}
	}
	var lines []string
	var cur strings.Builder
	cw := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if cw+rw > w && cw > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			cw = 0
		}
		cur.WriteRune(r)
		cw += rw
	}
	return append(lines, cur.String())
}

// truncateStyled cuts an already-styled line to w columns.
func truncateStyled(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// commas formats n with thousands separators.
func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
