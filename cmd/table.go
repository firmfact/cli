package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/ui"
)

// Rows print as a table for a person, or as CSV or TSV for a spreadsheet
// or a script (--format). A table puts the columns a reader looks for first
// and every other field after them, aligns numbers on their last digit and
// groups amounts of money; on a terminal it fits the width, cutting long
// text with an ellipsis (--wide keeps it whole). CSV and TSV carry the
// values as the server sent them: no grouping, no rounding, nothing cut,
// but for the quote in front of text a spreadsheet would run as a formula.

// tableStyle is how printTable lays out its rows.
type tableStyle struct {
	// columns are --columns: these, in this order, instead of the ones
	// columnsFor picks.
	columns []string
	// width, when above zero, is the terminal's: the table is fitted to it.
	width int
	// wide is --wide: a list shows the fields it keeps out of its table by
	// default too (see listFields).
	wide bool
}

// leadingColumns come first when the rows have them, in this order: what a
// row is called and its ids (an analysis names its rows entity_name and
// entity_id), its state, and what it costs.
var leadingColumns = []string{"name", "entity_name", "id", "entity_id", "userdef_id", "status", "cost", "monthly_cost", "currency"}

// columnsFor puts the leading columns first, then every other field by
// name. A field that holds a list or an object in any row is left out: one
// cell cannot show it, and --json has it whole. Every row counts, as rows
// merged from several pages need not all have the same fields.
func columnsFor(rows []map[string]any) []string {
	nested := map[string]bool{}
	present := map[string]bool{}
	for _, row := range rows {
		for k, v := range row {
			present[k] = true
			switch v.(type) {
			case map[string]any, []any:
				nested[k] = true
			}
		}
	}
	var cols []string
	taken := map[string]bool{}
	for _, k := range leadingColumns {
		if present[k] && !nested[k] {
			cols = append(cols, k)
			taken[k] = true
		}
	}
	rest := make([]string, 0, len(present))
	for k := range present {
		if !taken[k] && !nested[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(cols, rest...)
}

// isMoneyKey tells an amount of money by its name, for grouping: cost,
// monthly_cost, average_cost_per_entity, total_amount, unit_price.
func isMoneyKey(key string) bool {
	return isCostKey(key) || strings.HasSuffix(key, "amount") || strings.HasSuffix(key, "price")
}

// tableField is a column as a table prints it: the field it shows, the
// header above it, and what its values are.
type tableField struct {
	key, header string
	// money formats the values as amounts: to the cent, their thousands
	// grouped.
	money bool
	// unit is what the amounts are in, as meta.display names it
	// ("base_currency"); empty when not known.
	unit string
}

// keyFields are the columns keys name, headed by the keys themselves, as
// the table of an answer that is not a list shows them. Amounts of money
// are told by their names (see isMoneyKey).
func keyFields(keys []string) []tableField {
	fields := make([]tableField, len(keys))
	for i, k := range keys {
		fields[i] = tableField{key: k, header: ui.SafeLine(strings.ToUpper(k)), money: isMoneyKey(k)}
	}
	return fields
}

// tableColumn is one column of a table as it prints.
type tableColumn struct {
	field  tableField
	header string
	cells  []string
	// right aligns the column on its last character: every value in it is
	// a number.
	right bool
	// natural is the width of the widest cell or the header; width is the
	// width it gets, which is less once it is cut to fit.
	natural, width int
}

// columnGap separates the columns of a table.
const columnGap = 2

// minCutWidth is the narrowest a column is cut to: seven characters and an
// ellipsis still say which row is which.
const minCutWidth = 8

// printTable prints rows as a table with the columns style names, or the
// ones columnsFor picks. Fitted to a terminal, columns that do not fit
// even when cut are left out from the right, and notes says which.
func printTable(w, notes io.Writer, rows []map[string]any, style tableStyle) error {
	keys := style.columns
	if len(keys) == 0 {
		keys = columnsFor(rows)
	}
	if len(keys) == 0 {
		return nil
	}
	t := layTable(rows, keyFields(keys), style.width)
	t.noteDropped(notes)
	return t.write(w)
}

// laidOut is a table with its columns built and, for a terminal, fitted
// to it: ready to print, and as wide as it will print.
type laidOut struct {
	cols []tableColumn
	rows int
	// dropped are the fields of the columns left out to fit.
	dropped []tableField
}

// layTable builds a column for each of fields and, when width is above
// zero, fits them into it (see fitColumns).
func layTable(rows []map[string]any, fields []tableField, width int) laidOut {
	cols := make([]tableColumn, len(fields))
	for i, f := range fields {
		cols[i] = buildColumn(f, rows)
	}
	t := laidOut{cols: cols, rows: len(rows)}
	if width > 0 && len(cols) > 0 {
		kept, dropped := fitColumns(cols, width)
		t.cols = kept
		for _, c := range dropped {
			t.dropped = append(t.dropped, c.field)
		}
	}
	return t
}

// width is how many columns the widest line of the table takes.
func (t laidOut) width() int {
	if len(t.cols) == 0 {
		return 0
	}
	sum := columnGap * (len(t.cols) - 1)
	for _, c := range t.cols {
		sum += c.width
	}
	return sum
}

// noteDropped says on notes which columns were left out to fit, by the
// names --columns takes.
func (t laidOut) noteDropped(notes io.Writer) {
	if len(t.dropped) == 0 {
		return
	}
	names := make([]string, len(t.dropped))
	for i, f := range t.dropped {
		names[i] = ui.SafeLine(f.key)
	}
	fmt.Fprintf(notes, "note: %d %s left out to fit the terminal (%s); --wide shows every column, --columns picks them.\n",
		len(t.dropped), plural(len(t.dropped), "column", "columns"), strings.Join(names, ", "))
}

// write prints the header and a line per row.
func (t laidOut) write(w io.Writer) error {
	if len(t.cols) == 0 {
		return nil
	}
	line := func(cell func(c tableColumn) string) string {
		var b strings.Builder
		for i, c := range t.cols {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", columnGap))
			}
			text := cutCell(cell(c), c.width)
			pad := strings.Repeat(" ", c.width-utf8.RuneCountInString(text))
			if c.right {
				b.WriteString(pad + text)
			} else {
				b.WriteString(text + pad)
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	if _, err := fmt.Fprintln(w, line(func(c tableColumn) string { return c.header })); err != nil {
		return err
	}
	for r := range t.rows {
		if _, err := fmt.Fprintln(w, line(func(c tableColumn) string { return c.cells[r] })); err != nil {
			return err
		}
	}
	return nil
}

// buildColumn formats one column of rows: amounts of money to the cent
// with their thousands grouped, anything else as cell shows it, and no
// value as a blank. A column is aligned right when all its values are
// numbers, whether the server sent them as JSON numbers or, as it does for
// exact decimals, as strings.
func buildColumn(f tableField, rows []map[string]any) tableColumn {
	c := tableColumn{field: f, header: f.header, cells: make([]string, len(rows))}
	money := f.money
	numbers, values := true, 0
	for i, row := range rows {
		v := row[f.key]
		if v == nil || v == "" {
			continue
		}
		values++
		n, isNumber := tableNumber(v)
		switch {
		case isNumber && money:
			c.cells[i] = groupThousands(strconv.FormatFloat(n, 'f', 2, 64))
		case isNumber:
			c.cells[i] = cell(v)
		default:
			numbers = false
			c.cells[i] = cell(v)
		}
	}
	c.right = numbers && values > 0
	c.natural = utf8.RuneCountInString(c.header)
	for _, s := range c.cells {
		c.natural = max(c.natural, utf8.RuneCountInString(s))
	}
	c.width = c.natural
	return c
}

// tableNumber is v as a number, when it is one: a JSON number, or a
// string of plain decimal digits such as the server sends exact amounts
// and quantities in ("1257170.693865", "13.0"). A string such as "1e5" or
// "0x10" is an id or a code, however Go would parse it.
func tableNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return amount(t)
	case string:
		if !isDecimal(t) {
			return 0, false
		}
		return amount(t)
	}
	return 0, false
}

// isDecimal reports whether s is an optional minus sign, digits, and
// optionally a point and more digits.
func isDecimal(s string) bool {
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasPoint := strings.Cut(s, ".")
	digits := func(p string) bool {
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
		return p != ""
	}
	return digits(whole) && (!hasPoint || digits(frac))
}

// fitColumns fits cols into width. Text is cut first, the widest columns
// before the others, to no less than minCutWidth; numbers are never cut,
// as a number cut short reads as another number. When that is not enough,
// columns are left out from the right; the first always stays, cut to the
// width if it must be.
func fitColumns(cols []tableColumn, width int) (kept, dropped []tableColumn) {
	for n := len(cols); n > 0; n-- {
		if cutToFit(cols[:n], width) {
			return cols[:n], cols[n:]
		}
	}
	cols[0].width = max(1, min(cols[0].natural, width))
	return cols[:1], cols[1:]
}

// cutToFit sets the widths of cols so that they fit width, cutting the
// text columns to one limit, the widest such limit that fits, and giving
// what that leaves over to the columns it cut, from the left. It reports
// false when they do not fit even at minCutWidth.
func cutToFit(cols []tableColumn, width int) bool {
	used := columnGap * (len(cols) - 1)
	for i := range cols {
		cols[i].width = cols[i].natural
		used += cols[i].natural
	}
	if used <= width {
		return true
	}
	// total is the table's width with the text columns cut to limit.
	total := func(limit int) int {
		sum := columnGap * (len(cols) - 1)
		for _, c := range cols {
			if c.right {
				sum += c.natural
			} else {
				sum += min(c.natural, max(limit, minCutWidth))
			}
		}
		return sum
	}
	if total(minCutWidth) > width {
		return false
	}
	// The widest limit that fits: total grows with limit.
	lo, hi := minCutWidth, 0
	for _, c := range cols {
		hi = max(hi, c.natural)
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if total(mid) <= width {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	spare := width - total(lo)
	for i := range cols {
		if !cols[i].right {
			cols[i].width = min(cols[i].natural, lo)
			if cols[i].width < cols[i].natural && spare > 0 {
				cols[i].width++
				spare--
			}
		}
	}
	return true
}

// cutCell shortens s to width characters, ending it in an ellipsis when
// anything was cut.
func cutCell(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:width-1]), " ") + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// writeDelimited writes rows as CSV or, with tsv, as TSV: a line of column
// names, then a line per row, with the columns picked as for a table. The
// values are the server's as it sent them: numbers in full, lists and
// objects as JSON. Terminal controls in them are escaped all the same (see
// ui.SafeText), as the output may well reach a terminal, and text that a
// spreadsheet would run as a formula is marked as text (see
// spreadsheetText), as the file may well be opened in one.
//
// CSV quotes a value with a comma, a quote or a line break in it, as RFC
// 4180 has it, and the empty value of a row with one column. TSV has no
// quoting, so a tab, a line break or a backslash in a value is written
// \t, \n or \\, as PostgreSQL's COPY and most TSV readers expect.
func writeDelimited(w io.Writer, rows []map[string]any, columns []string, tsv bool) error {
	if len(columns) == 0 {
		columns = columnsFor(rows)
	}
	if len(columns) == 0 {
		return nil
	}
	record := make([]string, len(columns))
	for i, k := range columns {
		record[i] = headerCell(k)
	}
	if tsv {
		if err := writeTSVLine(w, record); err != nil {
			return err
		}
		for _, row := range rows {
			for i, k := range columns {
				record[i] = rawCell(row[k])
			}
			if err := writeTSVLine(w, record); err != nil {
				return err
			}
		}
		return nil
	}
	cw := csv.NewWriter(w)
	write := func(record []string) error {
		// One empty field would be an empty line, which CSV readers skip:
		// with one column, every row without a value would go missing.
		// Quoted, it is a row with an empty value.
		if len(record) == 1 && record[0] == "" {
			cw.Flush()
			if err := cw.Error(); err != nil {
				return err
			}
			_, err := io.WriteString(w, "\"\"\n")
			return err
		}
		return cw.Write(record)
	}
	if err := write(record); err != nil {
		return err
	}
	for _, row := range rows {
		for i, k := range columns {
			record[i] = rawCell(row[k])
		}
		if err := write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

var tsvEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`)

func writeTSVLine(w io.Writer, record []string) error {
	escaped := make([]string, len(record))
	for i, s := range record {
		escaped[i] = tsvEscaper.Replace(s)
	}
	_, err := io.WriteString(w, strings.Join(escaped, "\t")+"\n")
	return err
}

// headerCell is a column name for the header line of CSV or TSV.
func headerCell(name string) string { return spreadsheetText(ui.SafeLine(name)) }

// rawCell is a value for CSV or TSV: as the server sent it, line breaks and
// all, with terminal controls escaped and formulas marked as text.
func rawCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return spreadsheetText(ui.SafeText(t))
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ui.SafeText(plainJSON(t))
	}
}

// spreadsheetText is s as a CSV or TSV value that a spreadsheet shows as
// the text it is. Excel, LibreOffice and Google Sheets run a value that
// starts with =, +, - or @ as a formula, and =HYPERLINK or a DDE call in a
// vendor's or an invoice's name, which other members of a workspace or
// imported data may have set, would run when the export is opened
// (CWE-1236). As OWASP advises against CSV injection, such a value, and one
// that starts with a tab or a line break or with the full-width forms of
// those four, gets a single quote in front. A number such as -1200.50, sent
// as text to keep its digits, stays as it is: a spreadsheet reads it as the
// number, not as a formula. The CSV writer quotes separators and quotes
// inside a value, so no value can start a cell of its own.
func spreadsheetText(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	if s == "" || !strings.ContainsRune("=+-@\t\r\n\uff1d\uff0b\uff0d\uff20", r) || plainNumber.MatchString(s) {
		return s
	}
	return "'" + s
}

// plainNumber is a number as a spreadsheet reads one, with its sign.
var plainNumber = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// plainJSON is v as one line of JSON, with "&" and "<" as they are:
// encoding/json escapes those for web pages by default.
func plainJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}
