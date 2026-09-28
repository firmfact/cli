package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/mcp"
)

// listTestdata holds the answers of list tools and the golden files of how
// they print. Each answer is its text blocks in order, a string for a block
// of plain text and anything else for a block of JSON, as the service's MCP
// controller sends them (successful_tool_content) with the display block
// Mcp::ListDisplay puts in the paging block's meta.
var listTestdata = filepath.Join("testdata", "list")

// listAllocations takes paging and fields as the service's list tools do.
var listAllocations = mcp.Tool{
	Name:        "list_allocations",
	Title:       "List allocations",
	Annotations: readsOnly,
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"page":   map[string]any{"type": "integer", "minimum": 1},
		"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
		"fields": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}},
}

// listBlocks are the text blocks of the answer in testdata/list/<name>.json,
// JSON on one line as the service sends it.
func listBlocks(t testing.TB, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(listTestdata, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	blocks := make([]string, len(items))
	for i, item := range items {
		var text string
		if json.Unmarshal(item, &text) == nil {
			blocks[i] = text
			continue
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, item); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		blocks[i] = buf.String()
	}
	return blocks
}

// listOutput is what a list command printed, as its golden file holds it:
// stdout, then stderr under a line of its own.
func listOutput(stdout, stderr string) string {
	return stdout + "--- stderr ---\n" + stderr
}

// Each list prints as a person reads it: under a title that says whose
// data it is, with the service's labels in its headers, amounts grouped in
// the workspace's base currency, blanks where there is no value, the
// footnotes that say what the amounts are, and on stderr the count and a
// line for sample data. A service that sends no display block still gets
// headers in words and no ids that are UUIDs. CSV keeps every field under
// its own name, as scripts read it.
func TestListGolden(t *testing.T) {
	cases := []struct {
		name, fixture string
		args          []string
	}{
		{"vendors_demo", "vendors_demo", []string{"vendors", "list"}},
		{"vendors_wide", "vendors_demo", []string{"vendors", "list", "--wide"}},
		{"vendors_csv", "vendors_demo", []string{"vendors", "list", "--format", "csv"}},
		{"vendors_old_server", "vendors_old", []string{"vendors", "list"}},
		{"allocations", "allocations", []string{"allocations", "list"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			srv := (&mcpServer{result: toolResult(t, listBlocks(t, tc.fixture)...)}).start(t)
			signedInWithTools(t, srv.URL, listVendors, listAllocations)
			stdout, stderr, err := run("test", append([]string{"--host", srv.URL}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			assertNoTerminalControls(t, "stdout", stdout)
			assertNoTerminalControls(t, "stderr", stderr)
			assertGoldenFile(t, filepath.Join(listTestdata, tc.name+".golden"), listOutput(stdout, stderr))
		})
	}
}

// printListAt prints the answer in testdata/list as a list command does on
// a terminal of the given width: the notes, then the answer.
func printListAt(t *testing.T, fixture string, tool mcp.Tool, width int) (stdout, stderr string) {
	t.Helper()
	blocks := listBlocks(t, fixture)
	content := make([]mcp.Content, len(blocks))
	for i, b := range blocks {
		content[i] = mcp.Content{Type: "text", Text: b}
	}
	out := parseToolResult(content)
	var o, e strings.Builder
	app := &App{Name: "firmfact", Out: &o, Err: &e, Format: formatTable}
	for _, note := range out.notesToShow() {
		fmt.Fprintln(&e, note)
	}
	opts := renderOptions{format: formatTable, table: tableStyle{width: width}, nextPage: func(n string) string { return "--page " + n + " or --all" }}
	if err := printToolOutput(app, tool, out, opts); err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String()
}

// On a narrow terminal every line of a list fits: the title is cut before
// the currency beside it, names are cut before amounts, which never are,
// and the footnotes wrap between words.
func TestListOnANarrowTerminal(t *testing.T) {
	stdout, stderr := printListAt(t, "vendors_demo", listVendors, 52)
	assertGoldenFile(t, filepath.Join(listTestdata, "vendors_narrow.golden"), listOutput(stdout, stderr))

	for _, width := range []int{80, 52, 40, 30, 12} {
		stdout, _ := printListAt(t, "vendors_demo", listVendors, width)
		for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
			// A footnote's word longer than the line is the one exception.
			if n := utf8.RuneCountInString(line); n > width && strings.Contains(line, " ") {
				t.Errorf("width %d: a line of %d: %q", width, n, line)
			}
		}
		if !strings.HasPrefix(stdout, "Vendors") {
			t.Errorf("width %d: no title:\n%s", width, stdout)
		}
	}
}

// The title names the list and the workspace, and says when the data is
// sample data; the currency sits at the right edge of the table, and only
// when the table shows amounts in it.
func TestListTitleLine(t *testing.T) {
	d := &listDisplay{title: "Vendors", workspace: "Demo", demo: true, baseCurrency: "EUR"}
	money := tableField{key: "cost", header: "COST", money: true, unit: "base_currency"}
	name := tableField{key: "name", header: "NAME"}
	rows := []map[string]any{{"name": strings.Repeat("n", 40), "cost": 12.5}}
	cases := []struct {
		fields []tableField
		width  int
		want   string
	}{
		{[]tableField{name, money}, 0, "Vendors in Demo (sample data)" + strings.Repeat(" ", 6) + "costs in EUR"},
		{[]tableField{name}, 0, "Vendors in Demo (sample data)"},
		{[]tableField{name, money}, 44, "Vendors in Demo (sample data)" + strings.Repeat(" ", 3) + "costs in EUR"},
		{[]tableField{name, money}, 36, "Vendors in Demo (samp…  costs in EUR"},
		{[]tableField{name, money}, 20, "Vendors in Demo (sa…"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.width), func(t *testing.T) {
			got := titleLine(d, layTable(rows, c.fields, c.width), c.width)
			if got != c.want {
				t.Errorf("titleLine = %q, want %q", got, c.want)
			}
		})
	}
	short := layTable([]map[string]any{{"cost": 1.0}}, []tableField{money}, 0)
	if got := titleLine(&listDisplay{title: "Vendors", workspace: "Acme", baseCurrency: "USD"}, short, 0); got != "Vendors in Acme  costs in USD" {
		t.Errorf("a table narrower than its title: %q", got)
	}
}

// Without a display block, a list's headers are its field names in words,
// with the service's own labels for the fields it names, and ids that are
// UUIDs stay out of the table unless --wide asks for every field.
func TestListFieldsWithoutADisplayBlock(t *testing.T) {
	rows := []map[string]any{
		{"name": "Acme", "temporal_id": "118b1b32-b400-4491-ac53-d847ba35a1b9", "userdef_id": "ACME", "currency_userdef_id": "USD",
			"monthly_cost": 12.5, "cost_center_id": nil, "vendor_id": "V-1", "cost_center_userdef_id": "CC-1"},
		{"name": "Globex", "temporal_id": "bcce6995-21ff-45d8-a966-210edaf38612", "cost_center_id": "32462c67-ad03-4d62-8838-e27741874e61"},
	}
	headers := func(fields []tableField) string {
		var hs []string
		for _, f := range fields {
			hs = append(hs, f.header)
		}
		return strings.Join(hs, " | ")
	}
	if got, want := headers(listFields(rows, nil, tableStyle{})), "NAME | CODE | THIS MONTH | COST CENTRE | INVOICING CURRENCY | VENDOR ID"; got != want {
		t.Errorf("headers = %q, want %q", got, want)
	}
	if got, want := headers(listFields(rows, nil, tableStyle{wide: true})), "NAME | CODE | THIS MONTH | COST CENTRE ID | COST CENTRE | INVOICING CURRENCY | ID | VENDOR ID"; got != want {
		t.Errorf("--wide headers = %q, want %q", got, want)
	}
	if got, want := headers(listFields(rows, nil, tableStyle{columns: []string{"temporal_id", "name"}})), "ID | NAME"; got != want {
		t.Errorf("--columns headers = %q, want %q", got, want)
	}
}

// Without a display block, a table that shows costs says what they cover
// and that they are in the workspace base currency, not the invoicing
// currency when that is shown beside them; one that shows no costs, or
// had to leave them out to fit, says nothing of them.
func TestListFootnotesWithoutADisplayBlock(t *testing.T) {
	rows := []map[string]any{{"name": "Acme Market Data Services International", "cost": 1262160.0, "monthly_cost": 97089.23, "currency_userdef_id": "USD"}}
	notes := func(keys []string, width int) string {
		return strings.Join(fallbackFootnotes(layTable(rows, listFields(rows, nil, tableStyle{columns: keys}), width)), " | ")
	}
	for _, c := range []struct {
		keys  []string
		width int
		want  string
	}{
		{nil, 0, "13-month cost: cash basis, this month and six months either side. | This month: accrual basis. | " +
			"Amounts in the workspace base currency, not the invoicing currency."},
		{[]string{"name", "monthly_cost"}, 0, "This month: accrual basis. | Amounts in the workspace base currency."},
		{[]string{"name", "currency_userdef_id"}, 0, ""},
		{[]string{"name", "monthly_cost", "cost"}, 30, "This month: accrual basis. | Amounts in the workspace base currency."},
	} {
		if got := notes(c.keys, c.width); got != c.want {
			t.Errorf("%v at %d: %q, want %q", c.keys, c.width, got, c.want)
		}
	}
}

// A display block's columns come first, in its order and under its labels;
// --wide adds the fields it keeps back and any it does not name; a block
// whose columns no row has counts as none.
func TestListFieldsFromADisplayBlock(t *testing.T) {
	d := readListDisplay(map[string]any{"display": map[string]any{
		"schema": "list_display/1",
		"columns": []any{
			map[string]any{"key": "name", "label": "Naam"},
			map[string]any{"key": "missing", "label": "Nowhere"},
			map[string]any{"key": "cost", "label": "Kosten over 13 maanden", "kind": "money", "unit": "base_currency"},
			map[string]any{"key": ""},
			"not a column",
		},
		"hidden": []any{"temporal_id", 7, "name"},
		"labels": map[string]any{"temporal_id": "ID", "userdef_id": " ", "cost": 3},
	}})
	if d == nil {
		t.Fatal("no display block read")
	}
	rows := []map[string]any{{"name": "Acme", "cost": "12.5", "temporal_id": "t1", "userdef_id": "ACME", "tags": []any{"x"}}}
	describe := func(fields []tableField) string {
		var parts []string
		for _, f := range fields {
			parts = append(parts, fmt.Sprintf("%s=%s money:%v %s", f.key, f.header, f.money, f.unit))
		}
		return strings.Join(parts, "; ")
	}
	if got, want := describe(listFields(rows, d, tableStyle{})), "name=NAAM money:false ; cost=KOSTEN OVER 13 MAANDEN money:true base_currency"; got != want {
		t.Errorf("fields = %q, want %q", got, want)
	}
	if got, want := describe(listFields(rows, d, tableStyle{wide: true})), "name=NAAM money:false ; cost=KOSTEN OVER 13 MAANDEN money:true base_currency; temporal_id=ID money:false ; userdef_id=CODE money:false "; got != want {
		t.Errorf("--wide fields = %q, want %q", got, want)
	}
	if got, want := describe(listFields(rows, d, tableStyle{columns: []string{"temporal_id", "cost", "nope"}})), "temporal_id=ID money:false ; cost=KOSTEN OVER 13 MAANDEN money:true base_currency; nope=NOPE money:false "; got != want {
		t.Errorf("--columns fields = %q, want %q", got, want)
	}
	if got := describe(listFields([]map[string]any{{"other": 1.0}}, d, tableStyle{})); got != "other=OTHER money:false " {
		t.Errorf("columns no row has: %q", got)
	}
	for name, meta := range map[string]map[string]any{
		"no block":         {},
		"another schema":   {"display": map[string]any{"schema": "list_display/2"}},
		"not an object":    {"display": "list_display/1"},
		"schema not given": {"display": map[string]any{"title": "Vendors"}},
	} {
		if d := readListDisplay(meta); d != nil {
			t.Errorf("%s: read %+v", name, d)
		}
	}
}

// The count says what a list lists, in British English, and in the
// singular for one.
func TestListNoun(t *testing.T) {
	cases := []struct {
		tool string
		n    int
		want string
		ok   bool
	}{
		{"list_vendors", 37, "vendors", true},
		{"list_vendors", 1, "vendor", true},
		{"list_cost_centers", 2, "cost centres", true},
		{"list_cost_centers", 1, "cost centre", true},
		{"list_currencies", 1, "currency", true},
		{"list_contract_items", 1, "contract item", true},
		{"list_addresses", 1, "address", true},
		{"list_licenses", 1, "license", true},
		{"list_boxes", 1, "box", true},
		{"list_", 1, "", false},
		{"analyze_cost_trends", 3, "", false},
	}
	for _, c := range cases {
		got, ok := listNoun(c.tool, c.n)
		if got != c.want || ok != c.ok {
			t.Errorf("listNoun(%q, %d) = %q, %v; want %q, %v", c.tool, c.n, got, ok, c.want, c.ok)
		}
	}
	if got := noneFound("list_cost_centers"); got != "No cost centres found." {
		t.Errorf("noneFound = %q", got)
	}
}

// Sample data gets one quiet line in place of the notice the service
// writes for an assistant, which --json keeps; other notes print as they
// came, and real data gets no line at all.
func TestSampleDataNotice(t *testing.T) {
	cases := []struct {
		name  string
		out   toolOutput
		want  string
		shown []string
	}{
		{"display block", toolOutput{Meta: map[string]any{demoMarker: demoDataSource, "display": map[string]any{"schema": "list_display/1", "notice": "Voorbeeldgegevens."}}, shown: []string{"DEMO WORKSPACE: sample.", "Other."}},
			"Voorbeeldgegevens.", []string{"Other."}},
		{"stamp alone", toolOutput{Meta: map[string]any{demoMarker: demoDataSource}, shown: []string{"Demo workspace: sample."}},
			"Sample data in a Demo workspace, not your own spend.", nil},
		{"a demo block without a notice", toolOutput{Meta: map[string]any{"display": map[string]any{"schema": "list_display/1", "workspace": map[string]any{"demo": true}}}},
			"Sample data in a Demo workspace, not your own spend.", nil},
		{"real data", toolOutput{Meta: map[string]any{"display": map[string]any{"schema": "list_display/1", "workspace": map[string]any{"demo": false}}}, shown: []string{"DEMO WORKSPACE: kept, as nothing replaces it."}},
			"", []string{"DEMO WORKSPACE: kept, as nothing replaces it."}},
	}
	for _, c := range cases {
		if got := c.out.sampleDataNotice(); got != c.want {
			t.Errorf("%s: notice %q, want %q", c.name, got, c.want)
		}
		if got := c.out.notesToShow(); fmt.Sprint(got) != fmt.Sprint(c.shown) {
			t.Errorf("%s: notes %q, want %q", c.name, got, c.shown)
		}
	}
}

func TestWrapWords(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  []string
	}{
		{"13-month cost: cash basis.", 0, []string{"13-month cost: cash basis."}},
		{"13-month cost: cash basis.", 26, []string{"13-month cost: cash basis."}},
		{"13-month cost: cash basis, this month and six months either side.", 30, []string{"13-month cost: cash basis,", "this month and six months", "either side."}},
		{"an extraordinarily long word", 8, []string{"an", "extraordinarily", "long", "word"}},
	}
	for _, c := range cases {
		if got := wrapWords(c.s, c.width); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("wrapWords(%q, %d) = %q, want %q", c.s, c.width, got, c.want)
		}
	}
}

// --all prints every page as one list, under the first page's title and
// footnotes, with the Demo line once.
func TestAllKeepsTheListDisplay(t *testing.T) {
	isolate(t)
	blocks := listBlocks(t, "vendors_demo")
	var paging map[string]any
	if err := json.Unmarshal([]byte(blocks[2]), &paging); err != nil {
		t.Fatal(err)
	}
	display := paging["meta"].(map[string]any)["display"]
	f := &mcpServer{respond: func(call toolCall) string {
		page, _ := call.Arguments["page"].(float64)
		rows := []any{map[string]any{"name": fmt.Sprintf("Vendor %v", page), "userdef_id": fmt.Sprintf("V%v", page), "cost": 1000 * page, "monthly_cost": nil}}
		return toolResult(t, blocks[0],
			mustJSON(t, map[string]any{"workspace_data_source": "demo_sample_data", "data": rows}),
			mustJSON(t, map[string]any{"meta": map[string]any{"total_count": 2, "page": page, "per_page": 1, "total_pages": 2, "display": display}}))
	}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "Vendors in Demo (sample data)") || !strings.Contains(stdout, "Vendor 1") || !strings.Contains(stdout, "Vendor 2") ||
		!strings.HasSuffix(stdout, "Amounts in EUR, the workspace base currency.\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if stderr != "2 vendors.\nSample data in the Demo workspace, not your own spend.\n" {
		t.Errorf("stderr = %q", stderr)
	}

	_, stderr, err = run("test", "--host", srv.URL, "--json", "vendors", "list", "--all")
	if err != nil || stderr != "Sample data in the Demo workspace, not your own spend.\n" {
		t.Errorf("--json --all: stderr = %q (%v)", stderr, err)
	}
}
