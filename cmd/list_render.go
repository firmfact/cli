package cmd

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// A list prints for a person as a titled table:
//
//	Vendors in Demo (sample data)                        costs in EUR
//
//	NAME                    CODE    13-MONTH COST   THIS MONTH
//	Bloomberg Finance L.P.  BBG      1,262,160.00    97,089.23
//
//	13-month cost: cash basis, this month and six months either side.
//	This month: accrual basis.
//
// The title, columns, labels and footnotes come from the list's display
// block (see list_display.go); from a service that sends none, the table
// has no title, makes its headers from the field names, and has the
// CLI's own footnotes for the costs it shows. Under the
// table, on stderr, the count says how many there are in all and how to
// get the next page, as psql's "(37 rows)" does; after the footnotes a
// quiet line says when the data is sample data (see printToolOutput).

// isListAnswer reports whether out is a list's rows, which print as
// printList has them: a list tool's answer, or one with a display block.
func isListAnswer(tool mcp.Tool, out toolOutput) bool {
	if _, isList := out.Data.([]any); !isList {
		return false
	}
	_, named := listNoun(tool.Name, 0)
	return named || readListDisplay(out.Meta) != nil
}

// printList prints rows, the rows of a list, as a table under the list's
// title, then count, when there is one, on stderr, and the footnotes.
func printList(app *App, out toolOutput, rows []map[string]any, opts renderOptions, count string) error {
	d := readListDisplay(out.Meta)
	width := opts.table.width
	t := layTable(rows, listFields(rows, d, opts.table), width)
	t.noteDropped(app.Err)
	if d != nil && d.heading() != "" {
		fmt.Fprintln(app.Out, titleLine(d, t, width))
		fmt.Fprintln(app.Out)
	}
	if err := t.write(app.Out); err != nil {
		return err
	}
	if count != "" {
		fmt.Fprintln(app.Err, count)
	}
	notes := fallbackFootnotes(t)
	if d != nil {
		notes = d.footnotes
	}
	if len(notes) > 0 {
		fmt.Fprintln(app.Out)
		for _, note := range notes {
			for _, line := range wrapWords(note, width) {
				fmt.Fprintln(app.Out, line)
			}
		}
	}
	return nil
}

// listFields are the columns of a list's table. --columns names them
// outright. Otherwise the display block's columns, in its order; with
// --wide, then the fields it keeps back, and any other field the rows
// have. Without a block, every field as columnsFor orders them, less the
// ids that are UUIDs unless --wide asks for everything. Each is headed by
// its label in upper case.
func listFields(rows []map[string]any, d *listDisplay, style tableStyle) []tableField {
	keys := style.columns
	if len(keys) == 0 && d != nil {
		keys = displayKeys(rows, d, style.wide)
	}
	if len(keys) == 0 {
		for _, k := range columnsFor(rows) {
			if style.wide || !holdsUUIDs(k, rows) {
				keys = append(keys, k)
			}
		}
	}
	fields := make([]tableField, len(keys))
	for i, k := range keys {
		f := tableField{key: k, header: ui.SafeLine(strings.ToUpper(d.label(k))), money: isMoneyKey(k)}
		if c, ok := d.column(k); ok && c.kind == "money" {
			f.money, f.unit = true, c.unit
		}
		fields[i] = f
	}
	return fields
}

// displayKeys are the fields d shows of rows, each once: its columns that
// some row has and, when wide, the rest of the rows' fields, those d keeps
// back first. None when no row has any of d's columns, and the table is
// then made as for a service that sends no block.
func displayKeys(rows []map[string]any, d *listDisplay, wide bool) []string {
	present := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			present[k] = true
		}
	}
	var keys []string
	taken := map[string]bool{}
	add := func(k string) {
		if present[k] && !taken[k] {
			keys = append(keys, k)
			taken[k] = true
		}
	}
	for _, c := range d.columns {
		add(c.key)
	}
	if len(keys) == 0 || !wide {
		return keys
	}
	for _, k := range d.hidden {
		add(k)
	}
	for _, k := range columnsFor(rows) {
		add(k)
	}
	return keys
}

// titleLine is the list's title and, aligned to the right edge of the
// table, the currency its amounts are in, when the table shows amounts in
// the workspace's base currency. Fitted to a terminal, the title is cut
// before the currency, and the currency left out when the title would
// have to be cut to less than minCutWidth.
func titleLine(d *listDisplay, t laidOut, width int) string {
	title := d.heading()
	right := ""
	if d.baseCurrency != "" && t.showsUnit("base_currency") {
		right = "costs in " + d.baseCurrency
	}
	titleWidth := utf8.RuneCountInString(title)
	if right == "" {
		if width > 0 {
			return cutCell(title, width)
		}
		return title
	}
	rightWidth := utf8.RuneCountInString(right)
	span := max(t.width(), titleWidth+columnGap+rightWidth)
	if width > 0 && span > width {
		room := width - columnGap - rightWidth
		if room < minCutWidth {
			return cutCell(title, width)
		}
		span = width
		title = cutCell(title, room)
		titleWidth = utf8.RuneCountInString(title)
	}
	return title + strings.Repeat(" ", span-titleWidth-rightWidth) + right
}

// showsUnit reports whether the table shows a column of amounts in unit.
func (t laidOut) showsUnit(unit string) bool {
	for _, c := range t.cols {
		if c.field.money && c.field.unit == unit {
			return true
		}
	}
	return false
}
