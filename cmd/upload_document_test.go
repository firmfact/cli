package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// documentShape is a document of a fixture such as
// testdata/upload/shapes.json, and the name it is shown under.
type documentShape struct {
	Name      string          `json:"name"`
	Duplicate bool            `json:"duplicate"`
	Document  upload.Document `json:"document"`
}

// shapesIn reads the documents of the fixture file in testdata/upload.
func shapesIn(t *testing.T, file string) []documentShape {
	t.Helper()
	var out []documentShape
	if err := json.Unmarshal([]byte(readFixtureFile(t, filepath.Join(uploadTestdata, file))), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// shapes are the documents in testdata/upload/shapes.json: one of each
// shape document_result/1 can take that the upload tests do not reach.
func shapes(t *testing.T) []documentShape { return shapesIn(t, "shapes.json") }

// Someone else's document is its state and link; one still being read, or
// skipped for the allowance, says so; netting guidelines were booked when
// read, and keep their notes; an order form keeps its note but not the
// server's own "nothing is booked"; contracts are suggested, chosen,
// hidden, new or not found; a type or state this CLI does not know is
// shown as it is.
func TestDocumentBlocks(t *testing.T) {
	var buf bytes.Buffer
	for i, s := range shapes(t) {
		if i > 0 {
			buf.WriteString("\n")
		}
		printDocument(&buf, s.Name, &s.Document, s.Duplicate, 72)
	}
	assertGoldenFile(t, filepath.Join(uploadTestdata, "shapes.golden"), buf.String())
}

// A spreadsheet's records take a line a type, from the shapes in
// testdata/upload/records_shapes.json: an older server's spreadsheet has
// none, as before; a count that is zero or less is left out, and so are
// changed fields that are blank; a count this CLI does not know is in the
// total alone; people the file no longer lists come after the rows; labels
// in other languages line up by their characters, not their bytes; a type
// without a label is named by its key; and a label that tries to drive the
// terminal, or to start a line of its own, is escaped, and cut when it
// would push the lines across it.
func TestRecordLines(t *testing.T) {
	var buf bytes.Buffer
	for i, s := range shapesIn(t, "records_shapes.json") {
		if i > 0 {
			buf.WriteString("\n")
		}
		printDocument(&buf, s.Name, &s.Document, s.Duplicate, 72)
	}
	assertGoldenFile(t, filepath.Join(uploadTestdata, "records_shapes.golden"), buf.String())
	assertNoTerminalControls(t, "records", buf.String())
	for _, line := range strings.Split(buf.String(), "\n") {
		if safe := ui.SafeLine(line); safe != line {
			t.Errorf("a line with characters a terminal acts on: %q (escaped %q)", line, safe)
		}
	}
}

// The table says the same of each shape in a few words.
func TestDocumentTable(t *testing.T) {
	var files []*uploadFile
	for _, s := range shapes(t) {
		files = append(files, &uploadFile{name: s.Name, doc: &s.Document})
	}
	var buf bytes.Buffer
	printDocumentTable(&buf, files)
	assertGoldenFile(t, filepath.Join(uploadTestdata, "shapes_table.golden"), buf.String())
	if page := documentsPage(files); page != docsHost+"/accounts/"+acmeID+"/documents" {
		t.Errorf("documents page = %q", page)
	}
	if page := documentsPage([]*uploadFile{{doc: &upload.Document{ID: "x", URL: "https://elsewhere.example/d/x"}}}); page != "" {
		t.Errorf("a link of another shape made a documents page: %q", page)
	}
	docs := make([]*upload.Document, len(files))
	for i, f := range files {
		docs[i] = f.doc
	}
	if got := stateCounts(docs); got != "5 ready for review, 1 published, 1 attached as a reference, 1 still being read, 1 skipped, 1 some new state" {
		t.Errorf("state counts = %q", got)
	}
}

// Amounts are exact to the cent, grouped, and signed where they are a
// difference; scores are whole percentages; dates are written out. What
// cannot be read as one is shown as it came, escaped.
func TestUploadFormats(t *testing.T) {
	cases := map[string]string{
		moneyText("EUR", "12450"):         "EUR 12,450.00",
		moneyText("", "0.005"):            "0.01",
		moneyText(" usd\x1b[31m ", "1.5"): `usd\u001b[31m 1.50`,
		signedText("1550.00"):             "+1,550.00",
		signedText("-1550.004"):           "-1,550.00",
		signedText("0"):                   "0.00",
		signedText("n/a\x1b"):             `n/a\u001b`,
		signedText("1e999999999"):         "1e999999999",
		moneyText("EUR", "12,450.00"):     "EUR 12,450.00",
		percentText("0.845"):              "85%",
		percentText("1"):                  "100%",
		percentText("high"):               "",
		dateWords("2026-09-01"):           "1 Sep 2026",
		dateWords("01/09/2026\x07"):       `01/09/2026\u0007`,
		elapsed(45e9):                     "45s",
		elapsed(185e9):                    "3m05s",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	lines := wrapText("We could not extract data from this document. A-very-long-word-that-does-not-fit-at-all here", 20)
	if strings.Join(lines, "|") != "We could not extract|data from this|document.|A-very-long-word-that-does-not-fit-at-all|here" {
		t.Errorf("wrapped = %q", lines)
	}
}

// A file refused on its way says why, in the server's words, or by its
// code when there are none.
func TestProblemBlocks(t *testing.T) {
	var buf bytes.Buffer
	for _, f := range []*uploadFile{
		{name: "a.docx", outcome: outcomeRefused, message: "a.docx: the contents do not match the .docx extension, so it was not stored."},
		{name: "b.pdf", outcome: outcomeBusy, code: "scanner_busy"},
		{name: "c.pdf", outcome: outcomeInProgress, message: "c.pdf is being uploaded to this workspace right now."},
		{name: "d.pdf", outcome: outcomeFailed, message: "d.pdf changed while it was being uploaded, so nothing was stored; upload it again once it is complete"},
	} {
		printProblem(&buf, f, 72)
	}
	want := `a.docx: not stored
  a.docx: the contents do not match the .docx extension, so it was not
  stored.
b.pdf: not stored, as firmfact was busy
  refused (scanner_busy)
c.pdf: being uploaded by someone else
  c.pdf is being uploaded to this workspace right now.
d.pdf: not sent
  d.pdf changed while it was being uploaded, so nothing was stored;
  upload it again once it is complete
`
	if buf.String() != want {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), want)
	}
}

// FuzzDocumentBlock takes an entry of document_result/1, which the upload
// endpoints send and so, for all the CLI knows, a compromised or spoofed
// server, through everything that shows one to a person: its block, as an
// upload of the caller's and as someone else's, its row of a batch's
// table and the states a batch sums up. No terminal control and no
// character that turns text around gets through, and no entry makes the
// CLI fail. `go test` runs the seeds, the fixtures in testdata/upload and
// every input in testdata/fuzz; to fuzz it here:
//
//	go test -run '^$' -fuzz '^FuzzDocumentBlock$' -fuzztime 1m ./cmd
func FuzzDocumentBlock(f *testing.F) {
	paths, err := filepath.Glob(filepath.Join(uploadTestdata, "documents", "*.json"))
	if err != nil || len(paths) == 0 {
		f.Fatalf("no documents in testdata/upload/documents: %v", err)
	}
	for _, path := range paths {
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(readFixtureFile(f, path))); err != nil {
			f.Fatalf("%s: %v", path, err)
		}
		f.Add(buf.String())
	}
	for _, file := range []string{"shapes.json", "records_shapes.json"} {
		var shapes []struct {
			Document json.RawMessage `json:"document"`
		}
		if err := json.Unmarshal([]byte(readFixtureFile(f, filepath.Join(uploadTestdata, file))), &shapes); err != nil {
			f.Fatal(err)
		}
		for _, s := range shapes {
			f.Add(string(s.Document))
		}
	}
	f.Add(hostileEntry)
	f.Add(`{"state":"ready_for_review","read":{"amounts":{"total":1e400},"lines":[{"quantity":"x"}]},"variance":{"status":"variance","amount":"-0","lines":[{"line":-5}]}}`)
	// An exponent that big.Rat would spend minutes and gigabytes on.
	f.Add(`{"read":{"currency":"EUR","amounts":{"total":"1e999999999"}},"contract_match":{"status":"linked","score":1e999999999}}`)
	f.Fuzz(func(t *testing.T, entry string) {
		var doc upload.Document
		if json.Unmarshal([]byte(entry), &doc) != nil {
			return
		}
		var out strings.Builder
		printDocument(&out, "file.pdf", &doc, false, 72)
		printDocument(&out, "file.pdf", &doc, true, 20)
		printDocumentTable(&out, []*uploadFile{{name: "file.pdf", doc: &doc}})
		out.WriteString(stateCounts([]*upload.Document{&doc}) + "\n")
		assertNoTerminalControls(t, "document", out.String())
		for _, line := range strings.Split(out.String(), "\n") {
			if safe := ui.SafeLine(line); safe != line {
				t.Errorf("a line with characters a terminal acts on: %q (escaped %q)", line, safe)
			}
		}
	})
}
