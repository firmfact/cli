package cmd

import (
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// How a document an upload reads back shows for a person: a block of what
// firmfact read from it, or a row of a batch's table. Everything in a
// document is the server's, so every string goes through ui.SafeLine or
// ui.SafeText before it is printed.

// maxReviewItems is how many of the things a document's review asks of a
// person are listed; the review page has the rest.
const maxReviewItems = 5

// nothingBooked closes the results of documents waiting on their review
// page, the link above it.
const nothingBooked = "Nothing is booked until someone publishes it there."

// wrapWidth is how wide an upload's paragraphs are: 72 columns, or less on
// a narrower terminal.
func wrapWidth(w io.Writer) int {
	if ui.IsTerminal(w) {
		return max(min(72, ui.Width(w)-1), 20)
	}
	return 72
}

// wrapText breaks s into lines of at most width columns, at spaces; a word
// longer than that has a line of its own.
func wrapText(s string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case ui.Columns(line)+1+ui.Columns(word) <= width:
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

// A document block: a header line with what firmfact made of the document
// and its state, then a line for each thing it read, as label and text.

// labelWidth is the width of a block's labels ("Order form").
const labelWidth = 10

type blockWriter struct{ w io.Writer }

// line writes text beside label, and the next lines of more below it.
func (b blockWriter) line(label, text string, more ...string) {
	fmt.Fprintf(b.w, "  %-*s  %s\n", labelWidth, label, text)
	for _, m := range more {
		fmt.Fprintf(b.w, "  %-*s  %s\n", labelWidth, "", m)
	}
}

// printDocument prints doc, a document in document_result/1, under name.
// duplicate marks a document the upload found already in the workspace.
func printDocument(w io.Writer, name string, doc *upload.Document, duplicate bool, width int) {
	header := name + ": " + describeDocument(doc)
	switch {
	case duplicate && !doc.Own:
		header = name + ": already in firmfact, uploaded by someone else; " + stateWords(doc)
	case duplicate:
		header += " (already in firmfact)"
	}
	fmt.Fprintln(w, header)
	if e := doc.Error; e != nil && (e.Message != "" || e.Code != "") {
		for _, line := range wrapText(ui.SafeLine(orDefault(e.Message, e.Code)), width-2) {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	b := blockWriter{w}
	if rd := doc.Read; rd != nil {
		if rd.Vendor != nil {
			b.line("Vendor", partyText(rd.Vendor))
		}
		if rd.LegalEntity != nil {
			b.line("Entity", partyText(rd.LegalEntity))
		}
		if label, text := readLine(doc.Type, rd); text != "" {
			b.line(label, text)
		}
		printRecords(w, rd.Records)
	}
	printContractMatch(b, doc)
	printVariance(b, w, doc.Variance)
	if rd := doc.Read; rd != nil {
		for _, c := range rd.Checks {
			if c.Status != "pass" && c.Message != "" {
				b.line("Check", ui.SafeLine(c.Message))
			}
		}
	}
	// What the review asks matters while the document waits for it; one
	// published or attached is past it.
	if doc.State == upload.StateReadyForReview {
		printReview(b, doc.Review)
	}
	for _, note := range serverNotes(doc) {
		b.line("Note", ui.SafeLine(note))
	}
	if doc.URL != "" {
		b.line("Review", ui.SafeLine(doc.URL))
	}
}

// describeDocument is what the header says of doc: its type and state, or
// the state alone when there is no type to name.
func describeDocument(doc *upload.Document) string {
	state := stateWords(doc)
	if doc.Type == "" || doc.State == upload.StateFailed || upload.InProgress(doc.State) {
		return state
	}
	return typeWords(doc.Type) + ", " + state
}

// stateWords is doc's state in words.
func stateWords(doc *upload.Document) string {
	switch doc.State {
	case upload.StateQueued:
		return "waiting to be read"
	case upload.StateReading:
		return "being read"
	case upload.StateMatching:
		return "being matched"
	case upload.StateRetrying:
		return "being read again"
	case upload.StateReadyForReview:
		return "ready for review"
	case upload.StatePublished:
		return "published"
	case upload.StateAttached:
		return "attached as a reference"
	case upload.StateFailed:
		return "could not be read"
	case upload.StateSkipped:
		if doc.Reason == "over_quota" {
			return "skipped, as this month's allowance of documents was used up"
		}
		return "skipped"
	case stateMissing:
		return "no longer in the workspace"
	}
	return words(doc.State)
}

// typeWords is a document type in words.
func typeWords(t string) string {
	switch t {
	case "bloomberg_netting_guidelines":
		return "Bloomberg netting guidelines"
	case "hr_roster":
		return "HR file"
	}
	return words(t)
}

// words is a server's identifier, such as order_form, as words.
func words(id string) string {
	return strings.ReplaceAll(ui.SafeLine(id), "_", " ")
}

// partyText is a vendor or legal entity as read, and how the review page
// links it.
func partyText(p *upload.Party) string {
	name := ui.SafeLine(orDefault(p.Name, "no name read"))
	switch p.Status {
	case "linked":
		if p.LinkedTo != "" && !equalFold(p.LinkedTo, p.Name) {
			return name + ", linked to " + ui.SafeLine(p.LinkedTo)
		}
		return name + ", linked"
	case "suggested":
		return name + ", a match is suggested"
	case "new":
		return name + ", new: publishing adds it"
	case "skipped":
		return name + ", skipped"
	case "unresolved", "":
		return name + ", not linked yet"
	}
	return name + ", " + words(p.Status)
}

// readLine is the line of a block that says what the document is: an
// invoice's number, dates and total; a contract's number, term, value and
// items.
func readLine(docType string, rd *upload.Read) (label, text string) {
	t := orDefault(docType, rd.Type)
	var dates upload.Dates
	if rd.Dates != nil {
		dates = *rd.Dates
	}
	total := ""
	if rd.Amounts != nil && rd.Amounts.Total != "" {
		total = moneyText(rd.Currency, rd.Amounts.Total)
	}
	number := ui.SafeLine(rd.Number)
	switch t {
	case "contract", "order_form":
		label = "Contract"
		if t == "order_form" {
			label = "Order form"
		}
		parts := []string{orDefault(number, ui.SafeLine(rd.Name))}
		switch {
		case dates.Start != "" && dates.End != "":
			parts = append(parts, dateWords(dates.Start)+" to "+dateWords(dates.End))
		case dates.Start != "":
			parts = append(parts, "from "+dateWords(dates.Start))
		}
		parts = append(parts, total)
		if rd.Items > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", rd.Items, plural(rd.Items, "item", "items")))
		}
		return label, joinSome(", ", parts...)
	case "invoice":
		label = "Invoice"
	default:
		label = "Document"
	}
	due := ""
	if dates.Due != "" {
		due = "due " + dateWords(dates.Due)
	}
	head := joinSome(", ", number, dateWords(dates.Document), due)
	return label, joinSome("; ", head, total)
}

// A spreadsheet's or an HR file's records take a line a type in its block,
// in the order the server sends them, saying what publishing would do with
// its rows:
//
//	People        250 read: 230 new, 15 with changes (Department, Cost centre), 5 unmatched
//
// A count that is zero is left out. The labels come from the server, in the
// member's language, so they are printed as they came, escaped.

// maxRecordLabel is the most columns a type's label takes; a longer one is
// cut, so that one label cannot push every line across the terminal. The
// longest firmfact sends, in any of its languages, is "Componentes de
// contrato".
const maxRecordLabel = 24

// maxChangedFields is how many changed fields a line names, as many as
// the server sends; should it send more, the line says how many more.
const maxChangedFields = 5

// printRecords writes a line for each type of record: its label, the
// rows of that type the file holds, and what publishing does with them.
// The labels line up with the rest of the block's while they fit its
// column, and the totals line up with each other.
func printRecords(w io.Writer, records []upload.RecordCounts) {
	labels := make([]string, len(records))
	totals := make([]string, len(records))
	labelCols, totalCols := labelWidth, 0
	for i := range records {
		labels[i] = cutCell(recordLabel(&records[i]), maxRecordLabel)
		totals[i] = countText(records[i].Total)
		labelCols = max(labelCols, ui.Columns(labels[i]))
		totalCols = max(totalCols, ui.Columns(totals[i]))
	}
	for i := range records {
		fmt.Fprintf(w, "  %-*s  %*s read%s\n", labelCols, labels[i], totalCols, totals[i], recordCounts(&records[i]))
	}
}

// recordLabel is the label of a type of record, or its key in words when
// the server sent none.
func recordLabel(r *upload.RecordCounts) string {
	if label := ui.SafeLine(strings.TrimSpace(r.Label)); label != "" {
		return label
	}
	name := words(strings.TrimSpace(r.Type))
	if name == "" {
		return "Records"
	}
	first, size := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(first)) + name[size:]
}

// recordCounts is what follows "read" on a type's line: its counts that
// are not zero, and the people the file no longer lists, who are not rows
// of it.
func recordCounts(r *upload.RecordCounts) string {
	var parts []string
	add := func(n int, text string) {
		if n > 0 {
			parts = append(parts, countText(n)+" "+text)
		}
	}
	add(r.New, "new")
	changed := "with changes"
	if fields := changedFields(r.ChangedFields); fields != "" {
		changed += " (" + fields + ")"
	}
	add(r.Changed, changed)
	add(r.Unchanged, "unchanged")
	add(r.Unmatched, "unmatched")
	add(r.Skipped, "skipped")
	add(r.Hidden, plural(r.Hidden, "matched to a record you cannot view", "matched to records you cannot view"))
	add(r.NotInReview, "not on the review page")
	text := ""
	if len(parts) > 0 {
		text = ": " + strings.Join(parts, ", ")
	}
	if r.Leavers > 0 {
		text += "; " + countText(r.Leavers) + " no longer in the file"
	}
	return text
}

// changedFields lists the labels of the fields that changed, escaped, the
// blank ones left out.
func changedFields(fields []string) string {
	var names []string
	for _, f := range fields {
		if name := ui.SafeLine(strings.TrimSpace(f)); name != "" {
			names = append(names, name)
		}
	}
	return someOf(names, maxChangedFields)
}

// countText is a count with its thousands grouped: 10,000.
func countText(n int) string { return groupThousands(strconv.Itoa(n)) }

// joinSome joins the parts that are not empty.
func joinSome(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// printContractMatch writes the contract an invoice is linked or
// suggested to, or the contract a contract document would update.
func printContractMatch(b blockWriter, doc *upload.Document) {
	m := doc.ContractMatch
	if m == nil {
		return
	}
	if m.Visible != nil && !*m.Visible {
		label := "Contract"
		if m.Status == "existing" {
			label = "Existing"
		}
		b.line(label, ui.SafeLine(orDefault(m.Message, "matched to a contract you cannot view")))
		return
	}
	switch m.Status {
	case "linked":
		text := contractLabel(m.Contract) + ", linked "
		if m.How == "automatic" {
			text += "automatically"
		} else {
			text += "on the review page"
		}
		if p := percentText(m.Score); p != "" {
			text += " (" + p + ")"
		}
		b.line("Contract", text)
	case "suggested":
		var names []string
		for i := range m.Suggestions {
			s := &m.Suggestions[i]
			name, number := ui.SafeLine(s.Name), ui.SafeLine(s.Number)
			if name == "" {
				name, number = orDefault(number, contractLabel(s)), ""
			}
			if detail := joinSome(", ", number, percentText(s.Score)); detail != "" {
				name += " (" + detail + ")"
			}
			names = append(names, name)
		}
		b.line("Contract", fmt.Sprintf("%d suggested: %s", len(names), someOf(names, 3)))
	case "new":
		b.line("Contract", ui.SafeLine(orDefault(m.Message, "a new contract, which publishing creates")))
	case "none":
		b.line("Contract", "none found")
	case "existing":
		b.line("Existing", ui.SafeLine(orDefault(m.Message, "same number as "+contractLabel(m.Contract))))
	}
}

// contractLabel names a contract: its name and number, or either.
func contractLabel(c *upload.Contract) string {
	if c == nil {
		return "a contract"
	}
	name, number := ui.SafeLine(c.Name), ui.SafeLine(c.Number)
	switch {
	case name != "" && number != "":
		return name + " (" + number + ")"
	case name != "":
		return name
	case number != "":
		return number
	}
	return orDefault(ui.SafeLine(c.ID), "a contract")
}

// printVariance writes how an invoice compares with its contract, and
// each line that does not match.
func printVariance(b blockWriter, w io.Writer, v *upload.Variance) {
	if v == nil {
		return
	}
	switch v.Status {
	case "variance", "none":
		text := ui.SafeLine(v.Summary)
		if v.Preview {
			text += " (preview)"
		}
		b.line("Variance", text)
		for _, l := range v.Lines {
			fmt.Fprintf(w, "  %4d  %s\n", l.Line, ui.SafeLine(orDefault(l.Message, l.Description)))
		}
	default:
		if text := orDefault(v.Message, v.Summary); text != "" {
			b.line("Variance", ui.SafeLine(text))
		}
	}
}

// printReview writes what the review page asks of a person.
func printReview(b blockWriter, rv *upload.Review) {
	if rv == nil {
		return
	}
	switch {
	case rv.AnalysisPending:
		b.line("To review", ui.SafeLine(orDefault(rv.Message, "the review page prepares it when it is opened")))
	case len(rv.Items) > 0:
		shown := rv.Items[:min(len(rv.Items), maxReviewItems)]
		var lines []string
		for _, item := range shown {
			lines = append(lines, ui.SafeLine(orDefault(item.Message, item.Name)))
		}
		if more := len(rv.Items) - len(shown) + rv.MoreItems; more > 0 {
			lines = append(lines, fmt.Sprintf("and %d more on the review page", more))
		}
		b.line("To review", lines[0], lines[1:]...)
	case rv.Summary != "":
		b.line("To review", ui.SafeLine(rv.Summary))
	}
}

// serverNotes are the notes of doc to show. The last note of a document
// waiting on its review page (other than netting guidelines, which apply
// when read) is the server's "Nothing is booked until someone publishes
// it", which nothingBooked says once for all of them.
func serverNotes(doc *upload.Document) []string {
	notes := doc.Notes
	if len(notes) > 0 && doc.State == upload.StateReadyForReview && !booked(doc) && doc.Type != "bloomberg_netting_guidelines" {
		notes = notes[:len(notes)-1]
	}
	return notes
}

func booked(doc *upload.Document) bool { return doc.Booked != nil && *doc.Booked }

// waitingForReview reports whether any of docs waits on its review page,
// with nothing booked yet.
func waitingForReview(docs []*upload.Document) bool {
	for _, d := range docs {
		if d.State == upload.StateReadyForReview && !booked(d) {
			return true
		}
	}
	return false
}

// printDocumentTable prints documents a row each: the amount, the
// contract, the variance and what the review asks.
func printDocumentTable(w io.Writer, files []*uploadFile) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, " #\tFILE\tAMOUNT\tCONTRACT\tVARIANCE\tREVIEW")
	for i, f := range files {
		fmt.Fprintf(tw, "%2d\t%s\t%s\t%s\t%s\t%s\n", i+1, f.label(), amountCell(f.doc), contractCell(f.doc), varianceCell(f.doc), reviewCell(f.doc))
	}
	_ = tw.Flush()
}

func amountCell(doc *upload.Document) string {
	if rd := doc.Read; rd != nil && rd.Amounts != nil && rd.Amounts.Total != "" {
		return moneyText(rd.Currency, rd.Amounts.Total)
	}
	return "-"
}

func contractCell(doc *upload.Document) string {
	m := doc.ContractMatch
	if m == nil {
		return "-"
	}
	if m.Visible != nil && !*m.Visible {
		return "hidden"
	}
	short := func(c *upload.Contract) string {
		if c == nil {
			return "linked"
		}
		return orDefault(ui.SafeLine(c.Number), ui.SafeLine(c.Name))
	}
	switch m.Status {
	case "linked":
		if m.How == "automatic" {
			return short(m.Contract) + ", auto"
		}
		return short(m.Contract) + ", chosen"
	case "suggested":
		return fmt.Sprintf("%d suggested", len(m.Suggestions))
	case "existing":
		return "updates " + short(m.Contract)
	}
	return words(m.Status)
}

func varianceCell(doc *upload.Document) string {
	v := doc.Variance
	switch {
	case v == nil:
		return "-"
	case v.Status == "variance" && v.Amount != "":
		return signedText(v.Amount)
	case v.Status == "none":
		return "none"
	}
	return "-"
}

func reviewCell(doc *upload.Document) string {
	switch {
	case upload.InProgress(doc.State):
		return "still being read"
	case doc.State == upload.StateSkipped && doc.Reason == "over_quota":
		return "over the allowance"
	case doc.State == upload.StateReadyForReview && booked(doc):
		// Netting guidelines, which apply when they are read.
		return "applied when read"
	case doc.State != upload.StateReadyForReview, doc.Review == nil && doc.ContractMatch == nil:
		return stateWords(doc)
	}
	if m := doc.ContractMatch; m != nil && m.Status == "suggested" {
		return "choose contract"
	}
	rv := doc.Review
	switch {
	case rv == nil:
		return "-"
	case rv.AnalysisPending:
		return "open to prepare"
	case rv.Counts != nil && rv.Counts.NeedsReview+rv.Counts.Suggested > 0:
		n := rv.Counts.NeedsReview + rv.Counts.Suggested
		return fmt.Sprintf("%d %s", n, plural(n, "item", "items"))
	case len(rv.Items) > 0:
		n := len(rv.Items) + rv.MoreItems
		return fmt.Sprintf("%d %s", n, plural(n, "item", "items"))
	}
	return "nothing flagged"
}

// stateCounts says how many of docs are in each state, in words.
func stateCounts(docs []*upload.Document) string {
	type bucket struct {
		words string
		n     int
	}
	order := []string{upload.StateReadyForReview, upload.StatePublished, upload.StateAttached, "reading", upload.StateFailed, upload.StateSkipped}
	buckets := map[string]*bucket{}
	var others []string
	for _, d := range docs {
		key := d.State
		if upload.InProgress(key) {
			key = "reading"
		}
		b, ok := buckets[key]
		if !ok {
			w := stateWords(&upload.Document{State: key})
			if key == "reading" {
				w = "still being read"
			}
			b = &bucket{words: w}
			buckets[key] = b
			if !contains(order, key) {
				others = append(others, key)
			}
		}
		b.n++
	}
	var parts []string
	for _, key := range append(order, others...) {
		if b, ok := buckets[key]; ok {
			parts = append(parts, fmt.Sprintf("%d %s", b.n, b.words))
		}
	}
	return strings.Join(parts, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// moneyText is an amount with its currency, to the cent, its thousands
// grouped: "EUR 12,450.00". The server sends amounts as decimal strings,
// which are rounded exactly, not through floating point.
func moneyText(currency string, amount upload.Decimal) string {
	text := decimalText(amount, false)
	if c := ui.SafeLine(strings.TrimSpace(currency)); c != "" {
		return c + " " + text
	}
	return text
}

// signedText is a difference to the cent, with its sign: +1,550.00.
func signedText(amount upload.Decimal) string { return decimalText(amount, true) }

func decimalText(amount upload.Decimal, signed bool) string {
	v, ok := amount.Rat()
	if !ok {
		return ui.SafeLine(string(amount))
	}
	text := groupThousands(v.FloatString(2))
	if signed && v.Sign() > 0 {
		text = "+" + text
	}
	return text
}

// percentText is a score between 0 and 1 as a whole percentage: 0.85 is
// 85%.
func percentText(score upload.Decimal) string {
	v, ok := score.Rat()
	if !ok {
		return ""
	}
	return v.Mul(v, big.NewRat(100, 1)).FloatString(0) + "%"
}

// dateWords is an ISO date as people write it: 2026-09-01 is 1 Sep 2026.
// Anything else is shown as it came.
func dateWords(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return ui.SafeLine(s)
	}
	return t.Format("2 Jan 2006")
}
