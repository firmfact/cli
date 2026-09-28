package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// The CLI's colours are the terminal's own green and red for results that
// went well or badly, and brand orange for what to type next; a rainbow
// only ever sweeps across the logo. These tests paint in the basic colour
// mode, which green and red take in every mode.

const (
	green = "\x1b[32m"
	red   = "\x1b[31m"
	reset = "\x1b[0m"
)

// painted is s in the colour code starts.
func painted(code, s string) string { return code + s + reset }

// colourCodes matches the colour codes (SGR sequences) in output.
var colourCodes = regexp.MustCompile("\x1b\\[[0-9;]*m")

// withoutColour is s with its colour codes taken out: what a terminal
// shows of it, colour aside.
func withoutColour(s string) string { return colourCodes.ReplaceAllString(s, "") }

// inColour is the style of an upload's results on a colour terminal.
var inColour = resultStyle{m: ui.Basic}

// fixtureDocument is a document of testdata/upload/documents.
func fixtureDocument(t *testing.T, file string) *upload.Document {
	t.Helper()
	var doc upload.Document
	if err := json.Unmarshal([]byte(readFixtureFile(t, filepath.Join(uploadTestdata, "documents", file))), &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

// A document's block paints a variance above the contract red, none green,
// a review with nothing flagged green and a document that could not be
// read red; the rest of it, and every character of it, stays as it was.
func TestDocumentBlocksInColour(t *testing.T) {
	cases := []struct {
		file string
		want []string
		// plain lines must stay unpainted.
		plain []string
	}{
		{"lseg_invoice.json",
			[]string{"  Variance    " + painted(red, "EUR 1,550.00 (14.2%) above the contract") + " (preview)\n"},
			[]string{"  To review   Line 3 (Exchange fees)", "LSEG-2026-09.pdf: invoice, ready for review\n"}},
		{"bbg_invoice.json",
			[]string{
				"  Variance    " + painted(green, "None: every line matches BBG-77812") + " (preview)\n",
				"  To review   " + painted(green, "Nothing flagged; ready to publish.") + "\n",
			}, nil},
		{"scan_failed.json", []string{"scan-0034.pdf: " + painted(red, "could not be read") + "\n"}, nil},
	}
	for _, c := range cases {
		doc := fixtureDocument(t, c.file)
		var coloured, plain bytes.Buffer
		inColour.printDocument(&coloured, doc.Filename, doc, false, 72)
		resultStyle{}.printDocument(&plain, doc.Filename, doc, false, 72)
		got := coloured.String()
		for _, want := range append(c.want, c.plain...) {
			if !strings.Contains(got, want) {
				t.Errorf("%s: no %q in\n%q", c.file, want, got)
			}
		}
		if withoutColour(got) != plain.String() {
			t.Errorf("%s: colour changed the text:\n%s\nwant\n%s", c.file, withoutColour(got), plain.String())
		}
		if strings.Contains(plain.String(), "\x1b") {
			t.Errorf("%s: colour in the plain block: %q", c.file, plain.String())
		}
	}

	// A duplicate someone else uploaded says its state in the header, red
	// as ever for one that could not be read.
	theirs := &upload.Document{State: upload.StateFailed}
	var block bytes.Buffer
	inColour.printDocument(&block, "x.pdf", theirs, true, 72)
	if want := "x.pdf: already in firmfact, uploaded by someone else; " + painted(red, "could not be read") + "\n"; !strings.HasPrefix(block.String(), want) {
		t.Errorf("someone else's duplicate: %q, want it to start %q", block.String(), want)
	}
}

// A variance is red above the contract, and with --fail-on-variance over
// the threshold on either side; below the contract and within the
// threshold it stays plain, as do lines that differ but add up to the
// contract, unless the threshold counts them. None is green.
func TestVarianceColours(t *testing.T) {
	threshold := func(s string) *upload.Threshold {
		t.Helper()
		th, err := upload.ParseThreshold(s)
		if err != nil {
			t.Fatal(err)
		}
		return &th
	}
	variance := func(amount string) *upload.Variance {
		return &upload.Variance{Status: "variance", Currency: "EUR", Amount: upload.Decimal(amount), Contract: "1000.00", Summary: "a summary"}
	}
	cases := []struct {
		name      string
		v         *upload.Variance
		threshold *upload.Threshold
		want      string
	}{
		{"above", variance("1550.00"), nil, painted(red, "+1,550.00")},
		{"above, within the threshold", variance("10.00"), threshold("5%"), painted(red, "+10.00")},
		{"below", variance("-10.00"), nil, "-10.00"},
		{"below, within the threshold", variance("-10.00"), threshold("50"), "-10.00"},
		{"below, over the threshold", variance("-10.00"), threshold("5"), painted(red, "-10.00")},
		{"offsetting lines", variance("0.00"), nil, "0.00"},
		{"offsetting lines, a residue below a cent", variance("0.004"), nil, "+0.00"},
		{"offsetting lines, any variance", variance("0.00"), threshold(upload.AnyVariance), painted(red, "0.00")},
		{"none", &upload.Variance{Status: "none", Summary: "None"}, nil, painted(green, "none")},
		{"not available", &upload.Variance{Status: "not_available", Reason: "no_match"}, nil, "-"},
		{"no preview", nil, nil, "-"},
	}
	for _, c := range cases {
		s := resultStyle{m: ui.Basic, threshold: c.threshold}
		doc := &upload.Document{State: upload.StateReadyForReview, Variance: c.v}
		if got := s.varianceCell(doc); got != c.want {
			t.Errorf("%s: cell %q, want %q", c.name, got, c.want)
		}
		if got := (resultStyle{threshold: c.threshold}).varianceCell(doc); strings.Contains(got, "\x1b") {
			t.Errorf("%s: a plain cell with colour: %q", c.name, got)
		}
		// The block paints the summary as the table paints the amount.
		var block bytes.Buffer
		s.printVariance(blockWriter{&block}, &block, c.v)
		paintedRed, paintedGreen := strings.HasPrefix(c.want, red), strings.HasPrefix(c.want, green)
		if got := block.String(); strings.Contains(got, red) != paintedRed || strings.Contains(got, green) != paintedGreen {
			t.Errorf("%s: block %q, where the cell is %q", c.name, got, c.want)
		}
	}
}

// The review column paints nothing flagged green, and a document the
// upload failed on red; a document still to be dealt with stays plain.
func TestReviewCellColours(t *testing.T) {
	for _, s := range shapes(t) {
		doc := &s.Document
		got, plain := inColour.reviewCell(doc), (resultStyle{}).reviewCell(doc)
		want := plain
		switch plain {
		case "nothing flagged":
			want = painted(green, plain)
		case "over the allowance", "could not be read", "no longer in the workspace":
			want = painted(red, plain)
		}
		if got != want {
			t.Errorf("%s: review cell %q, want %q", s.Name, got, want)
		}
	}
	for _, c := range []struct {
		doc  *upload.Document
		want string
	}{
		{fixtureDocument(t, "scan_failed.json"), painted(red, "could not be read")},
		{&upload.Document{State: stateMissing}, painted(red, "no longer in the workspace")},
		{&upload.Document{State: upload.StateSkipped, Reason: "over_quota"}, painted(red, "over the allowance")},
		{&upload.Document{State: upload.StateSkipped}, "skipped"},
	} {
		if got := inColour.reviewCell(c.doc); got != c.want {
			t.Errorf("%s: review cell %q, want %q", stateWords(c.doc), got, c.want)
		}
	}
}

// The block's review line is green for exactly the documents the table
// says "nothing flagged" of: no branch of reviewCell ahead of flagsNothing
// says it of another document. Every fixture document is checked.
func TestFlagsNothingAgreesWithTheTable(t *testing.T) {
	var docs []*upload.Document
	for _, file := range []string{"shapes.json", "records_shapes.json"} {
		for _, s := range shapesIn(t, file) {
			docs = append(docs, &s.Document)
		}
	}
	files, err := filepath.Glob(filepath.Join(uploadTestdata, "documents", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixture documents: %v", err)
	}
	for _, file := range files {
		docs = append(docs, fixtureDocument(t, filepath.Base(file)))
	}
	flagged := 0
	for _, d := range docs {
		table := (resultStyle{}).reviewCell(d) == "nothing flagged"
		if flagsNothing(d) != table {
			t.Errorf("%s: flagsNothing %v, but the table says %q", d.Filename, flagsNothing(d), (resultStyle{}).reviewCell(d))
		}
		if table {
			flagged++
		}
	}
	if flagged == 0 {
		t.Error("no fixture has nothing flagged")
	}
}

// A batch's table in colour lines up as the plain one does: the colour
// codes take no columns. The plain table is the golden one.
func TestDocumentTableInColour(t *testing.T) {
	var files []*uploadFile
	for _, s := range shapes(t) {
		files = append(files, &uploadFile{name: s.Name, doc: &s.Document})
	}
	for _, file := range []string{"lseg_invoice.json", "bbg_invoice.json", "scan_failed.json"} {
		doc := fixtureDocument(t, file)
		files = append(files, &uploadFile{name: doc.Filename, doc: doc})
	}
	var coloured, plain bytes.Buffer
	inColour.printDocumentTable(&coloured, files)
	resultStyle{}.printDocumentTable(&plain, files)
	got := coloured.String()
	if withoutColour(got) != plain.String() {
		t.Errorf("the coloured table does not line up:\n%s\nwant\n%s", withoutColour(got), plain.String())
	}
	for _, want := range []string{
		painted(green, "none") + "       " + painted(green, "nothing flagged") + "\n",
		painted(red, "+1,550.00") + "  2 items\n",
		painted(red, "could not be read") + "\n",
		painted(red, "over the allowance") + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
}

// writeColumns lays out plain cells as text/tabwriter did, wide characters
// and empty cells included, and coloured ones as if they were plain.
func TestWriteColumnsLaysOutAsTabwriter(t *testing.T) {
	rows := [][]string{
		{"UPLOADED", "FILE", "STATE", "ID"},
		{"27 Sep 2026 18:59", "Zürich-Q3.pdf", "ready for review", "a1"},
		{"-", "", "could not be read", "b2"},
		{"1 Oct 2026 09:00", "x.pdf", "published", ""},
	}
	var want bytes.Buffer
	tw := tabwriter.NewWriter(&want, 0, 2, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := writeColumns(&got, rows); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Errorf("got\n%q\nwant\n%q", got.String(), want.String())
	}

	rows[2][2] = ui.Basic.Red(rows[2][2])
	rows[1][1] = ui.Basic.Green(rows[1][1])
	got.Reset()
	if err := writeColumns(&got, rows); err != nil {
		t.Fatal(err)
	}
	if withoutColour(got.String()) != want.String() {
		t.Errorf("coloured cells moved the columns:\n%s\nwant\n%s", withoutColour(got.String()), want.String())
	}
}

// What failed in a batch is red in its summary: a file that was not
// stored, and the documents that could not be read, are gone or were
// skipped over the allowance. Other skips stay plain, counted apart from
// those over the allowance.
func TestProblemsAndCountsInColour(t *testing.T) {
	ready, failed, skipped := &upload.Document{State: upload.StateReadyForReview}, &upload.Document{State: upload.StateFailed}, &upload.Document{State: upload.StateSkipped}
	overQuota, missing := &upload.Document{State: upload.StateSkipped, Reason: "over_quota"}, &upload.Document{State: stateMissing}
	for _, c := range []struct {
		docs []*upload.Document
		want string
	}{
		{[]*upload.Document{ready, failed, skipped}, "1 ready for review, " + painted(red, "1 could not be read") + ", 1 skipped"},
		{[]*upload.Document{ready, overQuota}, "1 ready for review, " + painted(red, "1 over the allowance")},
		{[]*upload.Document{skipped, overQuota}, "1 skipped, " + painted(red, "1 over the allowance")},
		{[]*upload.Document{overQuota, skipped, overQuota}, "1 skipped, " + painted(red, "2 over the allowance")},
		{[]*upload.Document{missing, ready}, "1 ready for review, " + painted(red, "1 no longer in the workspace")},
	} {
		if got := inColour.stateCounts(c.docs); got != c.want {
			t.Errorf("state counts %q, want %q", got, c.want)
		}
		if got := (resultStyle{}).stateCounts(c.docs); got != withoutColour(c.want) {
			t.Errorf("plain state counts %q, want %q", got, withoutColour(c.want))
		}
	}

	busy := &uploadFile{name: "b.pdf", outcome: outcomeBusy, code: "scanner_busy"}
	var block bytes.Buffer
	inColour.printProblem(&block, busy, 72)
	if want := "b.pdf: " + painted(red, "not stored, as firmfact was busy") + "\n  refused (scanner_busy)\n"; block.String() != want {
		t.Errorf("problem block %q, want %q", block.String(), want)
	}
	for _, c := range []struct {
		f    *uploadFile
		want string
	}{
		{&uploadFile{name: "a.docx", outcome: outcomeRefused, message: "a.docx: the contents do not match."}, "a.docx: " + painted(red, "the contents do not match.")},
		{&uploadFile{name: "c.pdf", outcome: outcomeInProgress, message: "c.pdf is being uploaded right now."}, painted(red, "c.pdf is being uploaded right now.")},
		{busy, "b.pdf: " + painted(red, "refused (scanner_busy)")},
	} {
		if got := inColour.problemLine(c.f); got != c.want {
			t.Errorf("problem line %q, want %q", got, c.want)
		}
		if got := (resultStyle{}).problemLine(c.f); got != withoutColour(c.want) {
			t.Errorf("plain problem line %q, want %q", got, withoutColour(c.want))
		}
	}

	run := &uploadRun{files: []*uploadFile{
		{name: "a.pdf", outcome: outcomeCreated, doc: &upload.Document{ID: "1", State: upload.StateReadyForReview}},
		{name: "b.pdf", outcome: outcomeCreated, doc: &upload.Document{ID: "2", State: upload.StateSkipped, Reason: "over_quota"}},
		{name: "c.docx", outcome: outcomeRefused},
		busy,
	}}
	want := "2 uploaded, " + painted(red, "1 refused") + ", " + painted(red, "1 not sent") + ": 1 ready for review, " + painted(red, "1 over the allowance") + "."
	if got := run.summaryLine(inColour); got != want {
		t.Errorf("summary line %q, want %q", got, want)
	}
	if got := run.summaryLine(resultStyle{}); got != withoutColour(want) {
		t.Errorf("plain summary line %q, want %q", got, withoutColour(want))
	}
}

// update's last line is plain off a terminal (TestUpdatedLineOnATerminal
// has it in green). A test cannot reach it through update itself, which
// would replace the test binary.
func TestUpdatedLineOffATerminal(t *testing.T) {
	if got, want := updatedLine(&App{Out: &bytes.Buffer{}}, "/usr/local/bin/firmfact"), "Done. Updated /usr/local/bin/firmfact."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Off a terminal, the error line has no colour.
func TestErrorPrefixIsPlainOffATerminal(t *testing.T) {
	var buf bytes.Buffer
	ReportError(&buf, errors.New("boom"), false)
	if got := buf.String(); got != "error: boom\n" {
		t.Errorf("got %q", got)
	}
}
