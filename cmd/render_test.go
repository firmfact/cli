package cmd

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/mcp"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata")

// The analysis tools with the arguments the tests pass; the server takes
// these and more (Chat::ToolSchema::AllocationTools).
var (
	analyzeCostTrends = mcp.Tool{Name: "analyze_cost_trends", Title: "Analyse spend and cost trends", Annotations: readsOnly,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"entity_type":      map[string]any{"type": "string", "enum": []any{"contract", "vendor", "party", "product", "cost_center"}},
			"entity_name":      map[string]any{"type": "string"},
			"calculation_type": map[string]any{"type": "string", "enum": []any{"accrual_based", "cash_based"}},
		}}}
	analyzeUtilization = mcp.Tool{Name: "analyze_utilization", Title: "Analyse utilisation", Annotations: readsOnly,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"vendor_id":          map[string]any{"type": "string"},
			"show_underutilized": map[string]any{"type": "boolean"},
		}}}
	analyzeAllocations = mcp.Tool{Name: "analyze_allocations", Title: "Analyse allocations", Annotations: readsOnly,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"analysis_type": map[string]any{"type": "string"},
			"party_name":    map[string]any{"type": "string"},
		}}}
)

// readFixture is an answer as the server's executors build it, from
// testdata/render. The fixtures were taken from a local server
// (analysis_executor.rb, and execute_chat in tool_registry.rb) and cut down
// to a few rows, with the totals made to match.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "render", name))
	if err != nil {
		t.Fatal(err)
	}
	// The server sends its JSON on one line.
	return compactJSON(t, string(raw))
}

// assertGolden compares got with testdata/render/<name>.golden. After a
// deliberate change, rewrite the files with
// `go test ./cmd -run TestRenderedAnswers -update` and read the diff.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	assertGoldenFile(t, filepath.Join("testdata", "render", name+".golden"), got)
}

// assertGoldenFile compares got with the golden file at path, or rewrites
// it with -update.
func assertGoldenFile(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (create it with -update)", err)
	}
	// A checkout on Windows may have given the file CRLF line endings.
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Errorf("output differs from %s (rewrite it with -update after a deliberate change)\n--- got:\n%s\n--- want:\n%s", path, got, want)
	}
}

// renderCase runs one workspace command against a server that answers with
// the fixture.
func renderCase(t *testing.T, fixture string, args ...string) (stdout, stderr string) {
	t.Helper()
	isolate(t)
	srv := (&mcpServer{result: toolResult(t, readFixture(t, fixture))}).start(t)
	signedInWithTools(t, srv.URL, chatWithWorkspace, analyzeCostTrends, analyzeUtilization, analyzeAllocations,
		mcp.Tool{Name: "analyze_tus", Annotations: readsOnly})
	stdout, stderr, err := run("test", append([]string{"--host", srv.URL}, args...)...)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	assertNoTerminalControls(t, "stdout", stdout)
	return stdout, stderr
}

// Each shape of answer the server gives reads as a person expects, and
// nothing the answer holds besides its rows (a total, its currency, the
// period, the insights) is left out.
func TestRenderedAnswers(t *testing.T) {
	cases := []struct {
		name, fixture string
		args          []string
	}{
		{"chat", "chat.json", []string{"chat-with-workspace", "--message", "Who is our largest vendor?"}},
		{"cost_trends", "cost_trends.json", []string{"analyze", "cost-trends", "--entity-type", "vendor"}},
		{"cost_trends_monthly", "cost_trends.json", []string{"analyze", "cost-trends", "--entity-type", "vendor", "--monthly"}},
		{"utilization", "utilization.json", []string{"analyze", "utilization"}},
		{"allocations_by_party", "allocations_by_party.json", []string{"analyze", "allocations", "--analysis-type", "by_party", "--party-name", "Clara"}},
		// No analysis_type: the generic rendering, fields above the rows.
		{"tus", "tus.json", []string{"call", "analyze_tus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := renderCase(t, tc.fixture, tc.args...)
			if stderr != "" {
				t.Errorf("stderr = %q", stderr)
			}
			assertGolden(t, tc.name, stdout)
		})
	}
}

// A chat answer is its markdown, as written, on stdout: not a JSON string
// with its newlines escaped, and "S&P" not turned into S\u0026P.
func TestChatAnswerIsItsText(t *testing.T) {
	stdout, _ := renderCase(t, "chat.json", "chat-with-workspace", "--message", "Who is our largest vendor?")
	var fixture struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(readFixture(t, "chat.json")), &fixture); err != nil {
		t.Fatal(err)
	}
	if stdout != fixture.Response+"\n" {
		t.Errorf("stdout = %q, want the response as text", stdout)
	}
	for _, want := range []string{"**S&P Global Inc.**", "| Vendor | Cost |\n|---|---|\n", "\n- LSEG stayed flat."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q: %q", want, stdout)
		}
	}
}

// An analysis says its total in its currency, over which period and how
// costs were counted, above its insights and rows; the monthly totals come
// with --monthly. --json keeps "&" as it is.
func TestCostTrendsKeepTheirTotals(t *testing.T) {
	stdout, _ := renderCase(t, "cost_trends.json", "analyze", "cost-trends", "--entity-type", "vendor")
	lines := strings.Split(stdout, "\n")
	fields := func(i int) string { return strings.Join(strings.Fields(lines[i]), " ") }
	for i, want := range []string{
		"Total cost: EUR 787,000.00",
		"Period: 2026-07-01 to 2026-10-31 (4 months)",
		"Calculation: Cost spread evenly over service period (budget planning)",
	} {
		if fields(i) != want {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	for _, want := range []string{"Average cost per entity: EUR 393,500.00", "Total entities: 2", "- 1 vendors show high growth (>10% monthly average)"} {
		if !strings.Contains(strings.Join(strings.Fields(stdout), " "), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "S&P Global Inc.") || strings.Contains(stdout, "2026-09  EUR") {
		t.Errorf("want the entities table and no monthly totals without --monthly:\n%s", stdout)
	}

	monthly, _ := renderCase(t, "cost_trends.json", "analyze", "cost-trends", "--entity-type", "vendor", "--monthly")
	for _, want := range []string{"2026-09 EUR 205,000.00 current month", "2026-10 EUR 212,000.00 forecast", "2026-07 EUR 180,000.00"} {
		if !strings.Contains(strings.Join(strings.Fields(monthly), " "), want) {
			t.Errorf("--monthly lacks %q:\n%s", want, monthly)
		}
	}
	_, stderr := renderCase(t, "utilization.json", "analyze", "utilization", "--monthly")
	if stderr != "This analysis has no monthly totals.\n" {
		t.Errorf("--monthly without monthly totals: stderr = %q", stderr)
	}

	jsonOut, _ := renderCase(t, "cost_trends.json", "--json", "analyze", "cost-trends", "--entity-type", "vendor")
	if !strings.Contains(jsonOut, `"entity_name": "S&P Global Inc."`) || strings.Contains(jsonOut, `\u0026`) {
		t.Errorf("--json must keep & as it is:\n%s", jsonOut)
	}
	var got struct {
		Data struct {
			Summary map[string]any `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil || got.Data.Summary["total_cost"] != "787000.004321" {
		t.Errorf("--json total_cost = %v (%v), want the server's exact figure", got.Data.Summary["total_cost"], err)
	}
}

func TestMoneyFormat(t *testing.T) {
	cases := []struct {
		m    money
		v    any
		want string
	}{
		{money{code: "EUR"}, "1257170.693865", "EUR 1,257,170.69"},
		{money{code: "USD"}, 23312.92, "USD 23,312.92"},
		{money{symbol: "€"}, 3439.84, "€3,439.84"},
		{money{}, "999.995", "1,000.00"},
		{money{code: "EUR"}, "-1234", "EUR -1,234.00"},
		{money{code: "EUR"}, 0.0, "EUR 0.00"},
		{money{code: "EUR"}, "n/a", "n/a"},
	}
	for _, tc := range cases {
		if got := tc.m.format(tc.v); got != tc.want {
			t.Errorf("%+v.format(%v) = %q, want %q", tc.m, tc.v, got, tc.want)
		}
	}
}

// A shape the CLI knows nothing about keeps its plain fields, including
// those under data, and is JSON when printing fields would leave something
// out.
func TestGenericAnswers(t *testing.T) {
	cases := map[string]struct {
		answer string
		want   string
	}{
		"fields and rows":      {`{"total":2,"currency":"EUR","data":[{"name":"A","id":"x1"}]}`, "currency: EUR\ntotal: 2\n\nNAME  ID\nA     x1\n"},
		"fields under data":    {`{"status":"done","data":{"status":"queued","count":3,"empty":[]}}`, "status: done\ncount: 3\ndata.status: queued\n"},
		"a nested object":      {`{"status":"done","detail":{"why":"x"}}`, "{\n  \"detail\": {\n    \"why\": \"x\"\n  },\n  \"status\": \"done\"\n}\n"},
		"a list of values":     {`[1,"S&P"]`, "[\n  1,\n  \"S&P\"\n]\n"},
		"a chat under data":    {`{"data":{"response":"Hi & bye","thread_id":"t"},"x":1}`, "Hi & bye\n"},
		"an analysis, no rows": {`{"analysis_type":"by_cost","currency_symbol":"€","total_monthly_cost":12.5,"filters":{}}`, "Total monthly cost:  €12.50\nAnalysis type:       by_cost\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(tc.answer), &v); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			app := &App{Out: &out, Err: &out}
			if err := printHuman(app, v, renderOptions{}); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", out.String(), tc.want)
			}
		})
	}
}

// A chat answer and an analysis are server text like any other: terminal
// controls in them are escaped, and a chat answer keeps its newlines.
func TestRenderedAnswersCannotDriveTheTerminal(t *testing.T) {
	answers := map[string]any{
		"chat": map[string]any{"response": "line one\nline two " + osc52, "thread_id": "t\x1b[2J"},
		"analysis": map[string]any{
			"analysis_type": "cost_trends\x1b[2K",
			"currency":      "EUR\u009b31m",
			"summary":       map[string]any{"total_cost": "12.5"},
			"period":        map[string]any{"start_date": "soon" + osc52},
			"insights":      []any{"grew\nby " + osc52},
			"entities":      []any{map[string]any{"entity_name": "Acme\x1b]8;;https://evil.example\x1b\\"}},
		},
	}
	for name, answer := range answers {
		var out strings.Builder
		app := &App{Out: &out, Err: &out}
		if err := printHuman(app, answer, renderOptions{monthly: true}); err != nil {
			t.Fatal(err)
		}
		assertNoTerminalControls(t, name, out.String())
		if name == "chat" && !strings.HasPrefix(out.String(), "line one\nline two \\u001b]52;") {
			t.Errorf("chat: %q", out.String())
		}
		if name == "analysis" && !strings.Contains(out.String(), `EUR\u009b31m 12.50`) {
			t.Errorf("analysis: %q", out.String())
		}
	}
}
