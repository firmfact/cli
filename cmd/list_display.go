package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/ui"
)

// A list tool's answer says how to show its rows to a person in
// meta.display (schema list_display/1, Mcp::ListDisplay on the server): a
// title, the workspace and its base currency, the columns to show with
// their labels, the fields to keep for --wide, footnotes on what the
// amounts mean, and for a Demo workspace a notice written for people
// rather than for an assistant. The labels, title, footnotes and notice
// are in the language the user chose in firmfact.
//
// It changes only how a table looks: --json keeps the block in meta as
// sent, and CSV and TSV carry every field under its own name, as scripts
// expect. A service that sends no block gets a table made by the rules
// here: ids that are UUIDs left out, and headers in words.
//
// Every string in the block is the server's, so it goes through
// ui.SafeLine as it is read; the keys stay as sent, to find the fields
// they name, and go through ui.SafeLine wherever they are printed.

// listDisplaySchema is the only schema of the block this CLI reads. A
// block of another schema may mean something else by the same fields, so
// it counts as none.
const listDisplaySchema = "list_display/1"

// listDisplay is meta.display as a table uses it.
type listDisplay struct {
	title, workspace string
	demo             bool
	// baseCurrency is the workspace's base currency, such as EUR: what the
	// columns with unit base_currency are in.
	baseCurrency string
	columns      []displayColumn
	// hidden are the fields the rows carry that the table leaves out
	// unless asked (--wide, --columns).
	hidden []string
	// labels name every field the rows carry.
	labels    map[string]string
	footnotes []string
	notice    string
}

// displayColumn is one of the columns a list shows by default.
type displayColumn struct {
	key, label string
	// kind is "money" for an amount; unit is then what it is in.
	kind, unit string
}

// readListDisplay is meta's display block, or nil when it has none this
// CLI reads.
func readListDisplay(meta map[string]any) *listDisplay {
	raw, ok := meta["display"].(map[string]any)
	if !ok || raw["schema"] != listDisplaySchema {
		return nil
	}
	d := &listDisplay{
		title:        displayText(raw["title"]),
		baseCurrency: displayText(raw["base_currency"]),
		notice:       displayText(raw["notice"]),
		labels:       map[string]string{},
	}
	if ws, ok := raw["workspace"].(map[string]any); ok {
		d.workspace = displayText(ws["name"])
		d.demo = ws["demo"] == true
	}
	columns, _ := raw["columns"].([]any)
	for _, item := range columns {
		c, _ := item.(map[string]any)
		key, _ := c["key"].(string)
		if key == "" {
			continue
		}
		d.columns = append(d.columns, displayColumn{
			key: key, label: displayText(c["label"]), kind: displayText(c["kind"]), unit: displayText(c["unit"]),
		})
	}
	hidden, _ := raw["hidden"].([]any)
	for _, item := range hidden {
		if key, _ := item.(string); key != "" {
			d.hidden = append(d.hidden, key)
		}
	}
	labels, _ := raw["labels"].(map[string]any)
	for key, label := range labels {
		if text := displayText(label); text != "" {
			d.labels[key] = text
		}
	}
	footnotes, _ := raw["footnotes"].([]any)
	for _, item := range footnotes {
		if text := displayText(item); text != "" {
			d.footnotes = append(d.footnotes, text)
		}
	}
	return d
}

// displayText is a string of the block as it prints, or empty for anything
// else.
func displayText(v any) string {
	s, _ := v.(string)
	return ui.SafeLine(strings.TrimSpace(s))
}

// heading is the title line's text: "Vendors in Demo (sample data)";
// empty when the block has no title.
func (d *listDisplay) heading() string {
	if d.title == "" {
		return ""
	}
	text := d.title
	if d.workspace != "" {
		text += " in " + d.workspace
	}
	if d.demo {
		text += " (sample data)"
	}
	return text
}

// label is the header of a field, from the block, or made from the
// field's name when the block has none for it.
func (d *listDisplay) label(key string) string {
	if d != nil {
		if label := d.labels[key]; label != "" {
			return label
		}
		for _, c := range d.columns {
			if c.key == key && c.label != "" {
				return c.label
			}
		}
	}
	return fieldLabel(key)
}

// column is the block's default column for key, if it is one.
func (d *listDisplay) column(key string) (displayColumn, bool) {
	if d != nil {
		for _, c := range d.columns {
			if c.key == key {
				return c, true
			}
		}
	}
	return displayColumn{}, false
}

// fieldLabels are the labels the service gives the fields of list rows in
// English (config/locales/mcp_list_display.yml), for a service that sends
// no display block. userdef_id is the code a workspace gives a record;
// currency_userdef_id is the currency the record invoices in, never the
// currency of its costs, which are in the workspace base currency.
var fieldLabels = map[string]string{
	"id":                             "ID",
	"temporal_id":                    "ID",
	"userdef_id":                     "Code",
	"currency_userdef_id":            "Invoicing currency",
	"base_currency_userdef_id":       "Base currency",
	"monthly_cost":                   "This month",
	"party_name":                     "Organisational entity",
	"party_userdef_id":               "Organisational entity",
	"product_name":                   "Product",
	"db_contract_item_userdef_id":    "Linked source item",
	"master_contract_userdef_id":     "Master contract",
	"supersedes_contract_userdef_id": "Replaces contract",
}

// britishWords are the words of the service's field and tool names that
// firmfact spells the British way when it shows them.
var britishWords = map[string]string{
	"center":        "centre",
	"centers":       "centres",
	"organization":  "organisation",
	"organizations": "organisations",
}

// fieldLabel is a header for a field made from its name: the service's
// label where fieldLabels has it, the record a code belongs to for another
// userdef_id (vendor_userdef_id is "Vendor"), and otherwise the name in
// words, "ID" as such: cost_center_id is "Cost centre ID".
func fieldLabel(key string) string {
	if label, ok := fieldLabels[key]; ok {
		return label
	}
	if record, ok := strings.CutSuffix(key, "_userdef_id"); ok && record != "" {
		key = record
	}
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' || unicode.IsSpace(r) })
	for i, w := range words {
		switch {
		case w == "id":
			words[i] = "ID"
		case britishWords[w] != "":
			words[i] = britishWords[w]
		}
	}
	return capitalise(ui.SafeLine(strings.Join(words, " ")))
}

// capitalise puts s's first letter in upper case.
func capitalise(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// listNoun is what a list tool lists, as a count says it: "vendors", "1
// vendor", "cost centres"; ok is false for a tool that lists nothing by its
// name.
func listNoun(tool string, n int) (noun string, ok bool) {
	what, isList := strings.CutPrefix(tool, "list_")
	if !isList || what == "" {
		return "", false
	}
	words := strings.Split(what, "_")
	for i, w := range words {
		if british := britishWords[w]; british != "" {
			words[i] = british
		}
	}
	if n == 1 {
		words[len(words)-1] = singular(words[len(words)-1])
	}
	return ui.SafeLine(strings.Join(words, " ")), true
}

// singular is the singular of an English plural, as the service names its
// lists: currencies, addresses, vendors.
func singular(word string) string {
	switch {
	case strings.HasSuffix(word, "ies") && len(word) > 3:
		return strings.TrimSuffix(word, "ies") + "y"
	case strings.HasSuffix(word, "sses"), strings.HasSuffix(word, "xes"), strings.HasSuffix(word, "ches"), strings.HasSuffix(word, "shes"):
		return strings.TrimSuffix(word, "es")
	case strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss"):
		return strings.TrimSuffix(word, "s")
	}
	return word
}

// uuidPattern is a UUID as the service writes its ids.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// holdsUUIDs reports whether key names an id (id, temporal_id, vendor_id)
// and every value the rows have for it is a UUID: the service's internal
// ids, which say nothing to a reader and take 36 columns from the names.
// --wide and --columns still show them, and --json and CSV keep them.
func holdsUUIDs(key string, rows []map[string]any) bool {
	if key != "id" && !strings.HasSuffix(key, "_id") {
		return false
	}
	seen := false
	for _, row := range rows {
		switch v := row[key].(type) {
		case nil:
		case string:
			if v == "" {
				continue
			}
			if !uuidPattern.MatchString(v) {
				return false
			}
			seen = true
		default:
			return false
		}
	}
	return seen
}

// demoDataSource is the value of demoMarker on a Demo workspace's data.
const demoDataSource = "demo_sample_data"

// sampleDataNotice is what tells a person that an answer is sample data:
// the display block's notice, or the CLI's own when the service sends
// none; empty for real data.
func (o toolOutput) sampleDataNotice() string {
	d := readListDisplay(o.Meta)
	if d != nil && d.notice != "" {
		return d.notice
	}
	if o.Meta[demoMarker] == demoDataSource || (d != nil && d.demo) {
		return "Sample data in a Demo workspace, not your own spend."
	}
	return ""
}

// assistantNoticePrefix starts the notice the service puts before a Demo
// workspace's data (DEMO_DATA_NOTICE in its MCP controller). It is written
// for an assistant, which must not take the figures for the user's own,
// and runs to two lines on a terminal; sampleDataNotice says the same to a
// person in one.
const assistantNoticePrefix = "demo workspace"

// notesToShow are the plain-text notes to print on stderr: all of them,
// but for the assistant's Demo notice on sample data, whose place
// sampleDataNotice takes. --json keeps every note.
func (o toolOutput) notesToShow() []string {
	if o.sampleDataNotice() == "" {
		return o.shown
	}
	var notes []string
	for _, note := range o.shown {
		if len(note) < len(assistantNoticePrefix) || !strings.EqualFold(note[:len(assistantNoticePrefix)], assistantNoticePrefix) {
			notes = append(notes, note)
		}
	}
	return notes
}

// printNotice prints the sample-data notice on stderr as one quiet line,
// faint where the terminal shows colour, and wrapped between words where
// it is narrower than the line.
func (a *App) printNotice(notice string) {
	width := 0
	if ui.IsTerminal(a.Err) {
		width = ui.Width(a.Err)
	}
	m := ui.Detect(a.Err)
	for _, line := range wrapWords(notice, width) {
		fmt.Fprintln(a.Err, m.Dim(line))
	}
}

// wrapWords breaks s into lines of at most width characters, between
// words; a word longer than width has a line of its own. A width of zero
// or less leaves s on one line.
func wrapWords(s string, width int) []string {
	if width <= 0 || utf8.RuneCountInString(s) <= width {
		return []string{s}
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
