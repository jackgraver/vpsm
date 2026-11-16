package view

import (
	"bufio"
	"io"
	"slices"
	"strings"
)

// align is a column's horizontal alignment.
type align int

const (
	alignLeft align = iota
	alignRight
)

// col describes one table column.
type col struct {
	head string
	a    align
}

// table lays out fixed-width columns sized to their widest cell.
//
// text/tabwriter would normally do this, but it measures cells by their byte
// or rune content, and these cells carry ANSI colour codes that take up no
// visible space. Measuring with visibleLen is the whole reason this exists.
type table struct {
	cols []col
	rows [][]string
	gap  int
}

func newTable(gap int, cols ...col) *table {
	return &table{cols: cols, gap: gap}
}

// add appends a row. Short rows are padded with empty cells.
func (t *table) add(cells ...string) {
	row := make([]string, len(t.cols))
	copy(row, cells)
	t.rows = append(t.rows, row)
}

// render writes the table, header included, one line per row.
func (t *table) render(w io.Writer, p palette) error {
	widths := make([]int, len(t.cols))
	for i, c := range t.cols {
		widths[i] = visibleLen(c.head)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if n := visibleLen(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	bw := bufio.NewWriter(w)
	gap := strings.Repeat(" ", t.gap)

	// A table with no column names at all — the detail view uses them for
	// label/value blocks — must not emit a blank line where its header
	// would have gone.
	if slices.ContainsFunc(t.cols, func(c col) bool { return c.head != "" }) {
		head := make([]string, len(t.cols))
		for i, c := range t.cols {
			// Style first, then pad: padding inside the escape wrapper would
			// sit before the reset code, where writeLine cannot trim it.
			head[i] = padTo(p.header(c.head), widths[i], c.a)
		}
		if err := writeLine(bw, head, gap); err != nil {
			return err
		}
	}
	for _, row := range t.rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = padTo(cell, widths[i], t.cols[i].a)
		}
		if err := writeLine(bw, cells, gap); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// writeLine joins cells and trims the trailing padding, so output has no
// stray whitespace at end of line.
func writeLine(w io.Writer, cells []string, gap string) error {
	line := strings.TrimRight(strings.Join(cells, gap), " ")
	_, err := io.WriteString(w, line+"\n")
	return err
}
