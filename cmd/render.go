package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/ui"
)

// A tool's answer as a person reads it; --json prints the answer whole (see
// toolOutput). The renderer is chosen by the shape of the answer rather than
// by the tool, so a new tool that answers in a known shape reads well
// without a CLI release:
//
//   - a chat answer (a string "response") is its text, as written;
//   - an analysis ("analysis_type") is its figures, its insights, then its
//     rows;
//   - anything else is its plain fields as "key: value" lines, then its rows.
//
// A shape with neither plain fields nor rows is printed as JSON, so nothing
// is lost.
//
// Every value is the server's, so it goes through ui.SafeText or
// ui.SafeLine (by way of cell) before it is printed.

type renderOptions struct {
	// monthly is --monthly: an analysis also prints its month-by-month
	// totals.
	monthly bool
	// continueWith is the command line that asks a follow-up in the chat
	// thread with the given id; nil when the command has no way to.
	continueWith func(threadID string) string
	// format is --format (or --json): how the answer prints.
	format outputFormat
	// table is how rows print as a table; its columns (--columns) are
	// those of CSV and TSV too.
	table tableStyle
	// all is --all: fetch every page of a list.
	all bool
	// nextPage is how the command asks for the given page, such as
	// "--page 2 or --all"; nil when it has no way to.
	nextPage func(page string) string
	// answered, when set, sees the answer before it prints: ask keeps the
	// thread it names.
	answered func(out toolOutput)
}

// printHuman prints v, a tool's answer, with the renderer its shape calls
// for.
func printHuman(app *App, v any, opts renderOptions) error {
	if answer, threadID, ok := chatResponse(v); ok {
		printAnswer(app, answer, threadID, opts)
		return nil
	}
	if obj, ok := v.(map[string]any); ok {
		if _, isAnalysis := obj["analysis_type"]; isAnalysis {
			return printAnalysis(app, obj, opts)
		}
	}
	return printGeneric(app, v, opts)
}

// chatResponse finds the text of a chat answer and the thread it belongs to:
// a string "response" on the answer itself or under its data.
func chatResponse(v any) (answer, threadID string, ok bool) {
	obj, isObject := v.(map[string]any)
	if !isObject {
		return "", "", false
	}
	if answer, ok := obj["response"].(string); ok {
		return answer, idText(obj["thread_id"]), true
	}
	if data, isObject := obj["data"].(map[string]any); isObject {
		if answer, ok := data["response"].(string); ok {
			threadID := idText(data["thread_id"])
			if threadID == "" {
				threadID = idText(obj["thread_id"])
			}
			return answer, threadID, true
		}
	}
	return "", "", false
}

// idText is an id the server sent as a string or as a number.
func idText(v any) string {
	switch t := v.(type) {
	case string:
		return ui.SafeLine(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// printAnswer prints a chat answer as the text it is, markdown and all:
// JSON is no way to read a paragraph. On a terminal, stderr then shows how
// to ask a follow-up in the same thread; a script that wants the thread has
// it in --json.
func printAnswer(app *App, answer, threadID string, opts renderOptions) {
	fmt.Fprintln(app.Out, ui.SafeText(strings.TrimRight(answer, "\n")))
	if threadID == "" || opts.continueWith == nil || !ui.IsTerminal(app.Err) {
		return
	}
	m := ui.Detect(app.Err)
	fmt.Fprintf(app.Err, "\n%s %s\n", m.Dim("Continue:"), m.Orange(opts.continueWith(threadID)))
}

// fact is one line of an answer's header.
type fact struct{ label, value string }

// printAnalysis prints the figures of an analysis (its totals, period and
// how costs were counted, then every other plain field of it and of its
// summary), its insights as bullets, and its rows. With --monthly it adds
// the month-by-month totals.
func printAnalysis(app *App, obj map[string]any, opts renderOptions) error {
	s := &sections{w: app.Out}
	if facts := analysisFacts(obj); len(facts) > 0 {
		tw := tabwriter.NewWriter(s.next(), 0, 2, 2, ' ', 0)
		for _, f := range facts {
			fmt.Fprintf(tw, "%s:\t%s\n", f.label, f.value)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if insights, _ := obj["insights"].([]any); len(insights) > 0 {
		w := s.next()
		for _, insight := range insights {
			fmt.Fprintf(w, "- %s\n", cell(insight))
		}
	}
	if rows := findRows(obj); len(rows) > 0 {
		if err := printTable(s.next(), app.Err, rows, opts.table); err != nil {
			return err
		}
	}
	if !opts.monthly {
		return nil
	}
	months := monthlyTotals(obj)
	if len(months) == 0 {
		fmt.Fprintln(app.Err, "This analysis has no monthly totals.")
		return nil
	}
	return printMonthly(s.next(), months, currencyOf(obj))
}

// hiddenFacts are analysis fields the header shows in another form, or that
// are written for a language model rather than a reader. --json keeps them.
var hiddenFacts = map[string]bool{
	"currency":                     true, // on every amount
	"currency_symbol":              true, // on every amount
	"calculation_type":             true, // the Calculation line
	"calculation_type_description": true, // the Calculation line
	"amounts_note":                 true, // the amounts carry their currency
}

// analysisFacts are the header lines of an analysis, in the order a reader
// wants them: the totals with their currency, the period and how costs were
// counted, then the other plain fields of the summary and of the analysis.
func analysisFacts(obj map[string]any) []fact {
	summary, _ := obj["summary"].(map[string]any)
	cur := currencyOf(obj)
	var totals, others []fact
	amounts := false
	add := func(key string, v any) {
		if v == nil || hiddenFacts[key] || !isScalar(v) {
			return
		}
		f := fact{label: factLabel(key), value: cell(v)}
		if !isCostKey(key) {
			others = append(others, f)
			return
		}
		f.value, amounts = cur.format(v), true
		if strings.HasPrefix(key, "total_") {
			totals = append(totals, f)
		} else {
			others = append(others, f)
		}
	}
	for _, k := range sortedKeys(summary) {
		add(k, summary[k])
	}
	for _, k := range sortedKeys(obj) {
		if _, inSummary := summary[k]; !inSummary {
			add(k, obj[k])
		}
	}

	facts := totals
	if name := cur.name(); !amounts && name != "" {
		facts = append(facts, fact{"Currency", name})
	}
	if period := periodText(obj["period"]); period != "" {
		facts = append(facts, fact{"Period", period})
	}
	if calc := calculationText(obj); calc != "" {
		facts = append(facts, fact{"Calculation", calc})
	}
	return append(facts, others...)
}

// isCostKey tells an amount of money by its name: total_cost,
// total_monthly_cost, average_cost_per_entity, but not cost_center_id.
func isCostKey(key string) bool {
	return strings.HasSuffix(key, "cost") || strings.Contains(key, "cost_per_")
}

// factLabel turns a field name into a label: total_monthly_cost becomes
// "Total monthly cost".
func factLabel(key string) string {
	label := strings.ReplaceAll(ui.SafeLine(key), "_", " ")
	if label == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(label)
	return string(unicode.ToUpper(r)) + label[size:]
}

// periodText is an analysis period as dates: "2026-07-01 to 2026-10-31
// (4 months)". The server sends some bounds as timestamps; the time of day
// says nothing about a period counted in months or days.
func periodText(v any) string {
	period, ok := v.(map[string]any)
	if !ok {
		if s, isString := v.(string); isString {
			return ui.SafeLine(s)
		}
		return ""
	}
	start, end := dateText(period["start_date"]), dateText(period["end_date"])
	var text string
	switch {
	case start != "" && end != "":
		text = start + " to " + end
	case start != "":
		text = "from " + start
	case end != "":
		text = "until " + end
	default:
		return ""
	}
	if months, ok := period["months_analyzed"].(float64); ok {
		unit := "months"
		if months == 1 {
			unit = "month"
		}
		text += fmt.Sprintf(" (%s %s)", cell(months), unit)
	}
	return text
}

func dateText(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if len(s) >= 10 {
		if _, err := time.Parse(time.DateOnly, s[:10]); err == nil {
			return s[:10]
		}
	}
	return ui.SafeLine(s)
}

// calculationText says how costs were counted, in the server's words when
// it gives them ("Cost spread evenly over service period (budget
// planning)"), else by the calculation type.
func calculationText(obj map[string]any) string {
	if description, _ := obj["calculation_type_description"].(string); strings.TrimSpace(description) != "" {
		return ui.SafeLine(description)
	}
	if calc, _ := obj["calculation_type"].(string); calc != "" {
		return ui.SafeLine(calc)
	}
	return ""
}

// monthlyTotals are the month-by-month totals of an analysis (cost trends
// has them in its summary).
func monthlyTotals(obj map[string]any) []map[string]any {
	months, ok := obj["monthly_totals"]
	if summary, isObject := obj["summary"].(map[string]any); isObject {
		if inSummary, found := summary["monthly_totals"]; found {
			months, ok = inSummary, true
		}
	}
	if !ok {
		return nil
	}
	return findRows(months)
}

// printMonthly prints the month-by-month totals, marking the current month
// and the months still to come, which are a forecast.
func printMonthly(w io.Writer, months []map[string]any, cur money) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MONTH\tCOST")
	for _, month := range months {
		cost := "no data" // the server's nil: no entity has data that month
		if month["cost"] != nil {
			c := cur
			if code, _ := month["currency"].(string); strings.TrimSpace(code) != "" {
				c = money{code: ui.SafeLine(strings.TrimSpace(code))}
			}
			cost = c.format(month["cost"])
		}
		line := cell(month["month"]) + "\t" + cost
		// Only a line with a note ends its cost in a tab, so no line
		// carries trailing spaces.
		switch {
		case month["is_current_month"] == true:
			line += "\tcurrent month"
		case month["is_historical"] == false:
			line += "\tforecast"
		}
		fmt.Fprintln(tw, line)
	}
	return tw.Flush()
}

// money is how an analysis states its currency: an ISO code ("EUR") goes
// before an amount with a space, as "EUR 1,600.00"; a symbol ("€") goes
// against it, as "€1,600.00".
type money struct{ code, symbol string }

func currencyOf(obj map[string]any) money {
	summary, _ := obj["summary"].(map[string]any)
	pick := func(key string) string {
		for _, m := range []map[string]any{summary, obj} {
			if s, _ := m[key].(string); strings.TrimSpace(s) != "" {
				return ui.SafeLine(strings.TrimSpace(s))
			}
		}
		return ""
	}
	return money{code: pick("currency"), symbol: pick("currency_symbol")}
}

func (m money) name() string {
	if m.code != "" {
		return m.code
	}
	return m.symbol
}

// format writes an amount to the cent with thousands grouped. The server
// sends exact decimals as strings ("1257170.693865") and rounded figures as
// numbers; both are amounts. Anything else is shown as it came.
func (m money) format(v any) string {
	n, ok := amount(v)
	if !ok {
		return cell(v)
	}
	text := groupThousands(strconv.FormatFloat(n, 'f', 2, 64))
	switch {
	case m.code != "":
		return m.code + " " + text
	case m.symbol != "":
		return m.symbol + text
	}
	return text
}

func amount(v any) (float64, bool) {
	var n float64
	switch t := v.(type) {
	case float64:
		n = t
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

// groupThousands puts commas between the thousands of a formatted number:
// "-1257170.69" becomes "-1,257,170.69".
func groupThousands(s string) string {
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, d := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if hasFrac {
		b.WriteString("." + frac)
	}
	return sign + b.String()
}

// printGeneric prints the plain fields of an answer as "key: value" lines,
// then its rows as a table: a total or a currency beside the rows is part
// of the answer. Without rows, only an answer of nothing but plain fields
// is printed that way; one with more in it is printed as JSON, so nothing
// is left out.
func printGeneric(app *App, v any, opts renderOptions) error {
	fields := plainFields(v)
	rows := findRows(v)
	if len(rows) == 0 && (len(fields) == 0 || !onlyPlainFields(v)) {
		return app.PrintJSON(v)
	}
	s := &sections{w: app.Out}
	if len(fields) > 0 {
		w := s.next()
		for _, f := range fields {
			fmt.Fprintf(w, "%s: %s\n", f.label, f.value)
		}
	}
	if len(rows) > 0 {
		return printTable(s.next(), app.Err, rows, opts.table)
	}
	return nil
}

// plainFields are the fields of an object, and of the object under its
// data key, that hold one value each, sorted by name. A field under data
// that the object has too is labelled data.<name>.
func plainFields(v any) []fact {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var fields []fact
	add := func(m map[string]any, label func(string) string) {
		for _, k := range sortedKeys(m) {
			if value := m[k]; value != nil && isScalar(value) {
				fields = append(fields, fact{label: label(k), value: cell(value)})
			}
		}
	}
	add(obj, ui.SafeLine)
	if data, ok := obj["data"].(map[string]any); ok {
		add(data, func(k string) string {
			if _, clash := obj[k]; clash {
				return "data." + ui.SafeLine(k)
			}
			return ui.SafeLine(k)
		})
	}
	return fields
}

// onlyPlainFields reports whether printing an object's plain fields shows
// all of it: nothing in it, or under its data, is a non-empty list or
// object.
func onlyPlainFields(v any) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, value := range obj {
		if data, isObject := value.(map[string]any); isObject && k == "data" {
			for _, inner := range data {
				if !plainOrEmpty(inner) {
					return false
				}
			}
		} else if !plainOrEmpty(value) {
			return false
		}
	}
	return true
}

// plainOrEmpty reports whether v is one value, nothing, or an empty list or
// object: a value plainFields shows, or nothing worth showing.
func plainOrEmpty(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return true
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, float64, bool:
		return true
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sections separates the blocks of an answer with a blank line.
type sections struct {
	w       io.Writer
	started bool
}

func (s *sections) next() io.Writer {
	if s.started {
		fmt.Fprintln(s.w)
	}
	s.started = true
	return s.w
}

// findRows returns the first array of objects in v, searching data first.
func findRows(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		var rows []map[string]any
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return nil
			}
			rows = append(rows, m)
		}
		return rows
	case map[string]any:
		if data, ok := t["data"]; ok {
			if rows := findRows(data); len(rows) > 0 {
				return rows
			}
		}
		for _, k := range sortedKeys(t) {
			if arr, ok := t[k].([]any); ok {
				if rows := findRows(arr); len(rows) > 0 {
					return rows
				}
			}
		}
	}
	return nil
}

// cell renders one value on one line, with terminal controls escaped (see
// ui.SafeLine): a tab or newline from the server would otherwise break the
// columns, and an escape sequence would reach the terminal.
func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return ui.SafeLine(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		raw, _ := json.Marshal(t)
		return ui.SafeLine(string(raw))
	}
}
