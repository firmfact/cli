package cmd

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/mcp"
)

// The fuzz targets here cover the answers of workspace tools, which come
// from the service and so, for all the CLI knows, from a compromised or
// spoofed one. `go test` runs their seeds and every input in testdata/fuzz;
// CI fuzzes each for 30 seconds on a pull request and for longer every
// night (see .github/workflows/fuzz.yml). To fuzz one here:
//
//	go test -run '^$' -fuzz '^FuzzFindRows$' -fuzztime 1m ./cmd

// renderedAnswers are the answers in testdata/render, taken from a local
// server, on one line as the server sends them: seeds from the real thing.
func renderedAnswers(f *testing.F) []string {
	f.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "render", "*.json"))
	if err != nil || len(paths) == 0 {
		f.Fatalf("no answers in testdata/render: %v", err)
	}
	answers := make([]string, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			f.Fatalf("%s: %v", path, err)
		}
		answers = append(answers, buf.String())
	}
	return answers
}

// FuzzFindRows takes an answer of two text blocks, as a list comes (the
// rows, then the paging block), through everything that reads it: sorting
// the blocks into data, meta and notes, finding the rows, and printing the
// answer in every format. The rows found are the same each time, whatever
// order Go visits a map in; no format lets a terminal control through;
// and --json parses to exactly the answer.
func FuzzFindRows(f *testing.F) {
	paging := `{"meta":{"page":1,"total_pages":3,"total_count":42},"note":"Use page to retrieve other pages"}`
	for _, answer := range renderedAnswers(f) {
		f.Add(answer, paging)
	}
	f.Add("Demo workspace: this is sample data.", `{"workspace_data_source":"demo_sample_data","data":[{"name":"Acme","id":"v1","cost":"12.5"}]}`)
	f.Add(`[{"name":"Acme","tags":["a","b"]},{"name":"Globex","monthly_cost":1234.5}]`, `{"total_count":2,"truncated":false}`)
	f.Add(`{"response":"line one\nline two \u001b]52;c;cm0gLXJmIH4=\u0007","thread_id":7}`, "")
	f.Add(`{"data":{"data":[1,2]},"rows":[{"a":1}],"more":[{"b":2}]}`, `[1,"two",{"three":3}]`)
	f.Add("[]", `{"meta":{"page":"x\u009b2J","total_pages":2},"total_count":0}`)
	f.Fuzz(func(t *testing.T, first, second string) {
		out := parseToolResult([]mcp.Content{{Type: "text", Text: first}, {Type: "text", Text: second}})
		rows := findRows(out.Data)
		if again := findRows(out.Data); !reflect.DeepEqual(rows, again) {
			t.Fatalf("findRows gave %d rows, then %d", len(rows), len(again))
		}
		if list, ok := out.Data.([]any); ok && len(list) > 0 && allObjects(list) && len(rows) != len(list) {
			t.Errorf("a list of %d objects gave %d rows", len(list), len(rows))
		}
		for _, format := range []outputFormat{formatJSON, formatTable, formatCSV, formatTSV} {
			var stdout, stderr strings.Builder
			app := &App{Name: "firmfact", Out: &stdout, Err: &stderr, Format: format, JSONOutput: format == formatJSON}
			opts := renderOptions{
				format:       format,
				monthly:      true,
				nextPage:     func(page string) string { return "--page " + page },
				continueWith: func(id string) string { return "firmfact ask --thread " + id },
			}
			err := printToolOutput(app, mcp.Tool{Name: "list_things"}, out, opts)
			// Only CSV and TSV refuse an answer: one without rows.
			if err != nil && !(format.delimited() && len(rows) == 0) {
				t.Errorf("--format %s: %v", format, err)
			}
			assertNoTerminalControls(t, string(format)+" stdout", stdout.String())
			assertNoTerminalControls(t, string(format)+" stderr", stderr.String())
			if format == formatJSON {
				assertSameJSON(t, stdout.String(), out)
			}
		}
	})
}

func allObjects(list []any) bool {
	for _, item := range list {
		if _, ok := item.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// assertSameJSON checks that printed, what --json wrote, is v as JSON:
// escaping the characters a terminal would act on changes no value.
func assertSameJSON(t *testing.T, printed string, v any) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal([]byte(printed), &got); err != nil {
		t.Fatalf("--json printed no JSON (%v): %q", err, printed)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--json printed %s, not %s", printed, raw)
	}
}

// FuzzColumnsFor takes the rows of an answer to a table, CSV and TSV. The
// columns are every plain field of the rows, once, the leading ones first;
// a table fitted to a terminal fits it; and CSV reads back as exactly the
// values written, one record per row.
func FuzzColumnsFor(f *testing.F) {
	for _, answer := range renderedAnswers(f) {
		f.Add(answer, 80)
	}
	f.Add(`[{"name":"Acme","id":"v1","cost":"1234.5","remark":"two\nlines\tand a tab"},{"name":"Globex","tags":["x"],"zeta":true}]`, 20)
	f.Add(`{"data":[{"entity_name":"Clara","monthly_cost":3439.84,"currency":"EUR"}]}`, 9)
	f.Add(`[{"":"","a":{"b":1}},{"a":2,"b":null}]`, 1)
	f.Add(`[{"name":"only"},{"name":""},{"name":null}]`, 0)
	f.Fuzz(func(t *testing.T, answer string, width int) {
		var v any
		if json.Unmarshal([]byte(answer), &v) != nil {
			return
		}
		rows := findRows(v)
		cols := columnsFor(rows)
		if again := columnsFor(rows); !reflect.DeepEqual(cols, again) {
			t.Fatalf("columnsFor gave %q, then %q", cols, again)
		}
		checkColumns(t, rows, cols)

		// 0 prints the table whole, as for a pipe.
		w := int(uint(width) % 200)
		var table, notes strings.Builder
		if err := printTable(&table, &notes, rows, tableStyle{width: w}); err != nil {
			t.Fatal(err)
		}
		assertNoTerminalControls(t, "table", table.String())
		assertNoTerminalControls(t, "notes", notes.String())
		lines := printedLines(table.String())
		if want := lineCount(rows, cols); len(lines) != want {
			t.Errorf("a table of %d rows has %d lines:\n%s", len(rows), len(lines), table.String())
		}
		for _, line := range lines {
			if w > 0 && utf8.RuneCountInString(line) > w {
				t.Errorf("a line of %d characters in a table fitted to %d: %q", utf8.RuneCountInString(line), w, line)
			}
		}

		var csvOut strings.Builder
		if err := writeDelimited(&csvOut, rows, nil, false); err != nil {
			t.Fatal(err)
		}
		assertNoTerminalControls(t, "csv", csvOut.String())
		records, err := csv.NewReader(strings.NewReader(csvOut.String())).ReadAll()
		if err != nil {
			t.Fatalf("the CSV does not read back (%v):\n%s", err, csvOut.String())
		}
		if len(records) != lineCount(rows, cols) {
			t.Fatalf("CSV of %d rows reads back as %d records:\n%s", len(rows), len(records), csvOut.String())
		}
		for i, rec := range records {
			for j, col := range cols {
				want := headerCell(col)
				if i > 0 {
					want = rawCell(rows[i-1][col])
				}
				if rec[j] != want {
					t.Errorf("CSV record %d, column %q reads back as %q, not %q", i, col, rec[j], want)
				}
			}
		}

		var tsvOut strings.Builder
		if err := writeDelimited(&tsvOut, rows, nil, true); err != nil {
			t.Fatal(err)
		}
		assertNoTerminalControls(t, "tsv", tsvOut.String())
		tsvLines := printedLines(tsvOut.String())
		if len(tsvLines) != lineCount(rows, cols) {
			t.Fatalf("TSV of %d rows has %d lines:\n%s", len(rows), len(tsvLines), tsvOut.String())
		}
		for _, line := range tsvLines {
			if n := strings.Count(line, "\t") + 1; n != len(cols) {
				t.Errorf("a TSV line of %d fields, not %d: %q", n, len(cols), line)
			}
		}
	})
}

// lineCount is how many lines rows print as, a header included: none when
// there is no column to print.
func lineCount(rows []map[string]any, cols []string) int {
	if len(cols) == 0 {
		return 0
	}
	return len(rows) + 1
}

// printedLines splits output into its lines; nothing printed is no lines.
func printedLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// checkColumns holds cols to what columnsFor promises: every field that is
// a plain value in each row that has it, once; the leading columns first,
// in their order; then the others by name.
func checkColumns(t *testing.T, rows []map[string]any, cols []string) {
	t.Helper()
	present, nested := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		for k, v := range row {
			present[k] = true
			switch v.(type) {
			case map[string]any, []any:
				nested[k] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, col := range cols {
		if seen[col] {
			t.Errorf("column %q twice in %q", col, cols)
		}
		seen[col] = true
		if !present[col] || nested[col] {
			t.Errorf("column %q is no plain field of the rows", col)
		}
	}
	for k := range present {
		if !nested[k] && !seen[k] {
			t.Errorf("field %q has no column in %q", k, cols)
		}
	}
	rank := map[string]int{}
	for i, k := range leadingColumns {
		rank[k] = i + 1
	}
	lead := 0
	for lead < len(cols) && rank[cols[lead]] > 0 {
		if lead > 0 && rank[cols[lead]] < rank[cols[lead-1]] {
			t.Errorf("leading columns out of order in %q", cols)
		}
		lead++
	}
	rest := cols[lead:]
	for _, col := range rest {
		if rank[col] > 0 {
			t.Errorf("leading column %q after the others in %q", col, cols)
		}
	}
	if !sort.StringsAreSorted(rest) {
		t.Errorf("columns after the leading ones are not by name: %q", rest)
	}
}
