package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/mcp"
)

// --jq filters what --json prints: a string as its text, anything else as
// one line of JSON, a value per line. The notes still go to stderr, as
// with --json.
func TestJQFiltersAToolResult(t *testing.T) {
	isolate(t)
	srv := (&mcpServer{result: vendorsResult}).start(t)
	signedInWithTools(t, srv.URL, listVendors)

	cases := []struct {
		expr, want string
	}{
		{".data[].name", "Acme\nGlobex\n"},
		{".data[0]", `{"annual_cost":1200.5,"id":"v1","name":"Acme","tags":["x"]}` + "\n"},
		{".meta.total_count", "3\n"},
		{"[.data[].annual_cost] | add", "1280.5\n"},
		{".data[] | select(.annual_cost > 100) | .id", "v1\n"},
		{".data[] | [.name, .annual_cost, .tags[0]] | @tsv", "Acme\t1200.5\tx\nGlobex\t80\t\n"},
		{".nothing", "null\n"},
		{"empty", ""},
		// halt ends the output without an error.
		{".data[0].name, halt, 1", "Acme\n"},
	}
	for _, c := range cases {
		stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list", "--jq", c.expr)
		if err != nil || stdout != c.want {
			t.Errorf("--jq %q = %q (%v), want %q", c.expr, stdout, err, c.want)
		}
		if !strings.Contains(stderr, "Demo workspace: this is sample data.") {
			t.Errorf("--jq %q: stderr = %q", c.expr, stderr)
		}
	}

	// The expression sees exactly what --json prints.
	asJSON, _, err := run("test", "--host", srv.URL, "vendors", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, err := run("test", "--host", srv.URL, "vendors", "list", "--jq", ".")
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if json.Unmarshal([]byte(asJSON), &want) != nil || json.Unmarshal([]byte(stdout), &got) != nil || mustJSON(t, got) != mustJSON(t, want) {
		t.Errorf("--jq . = %s, want what --json prints: %s", stdout, asJSON)
	}
}

// With --all the expression runs on each row as it arrives, as it would
// on the NDJSON piped to jq.
func TestJQRunsOnEachRowOfAll(t *testing.T) {
	isolate(t)
	srv := (&mcpServer{respond: pagedVendors(t)}).start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, _, err := run("test", "--host", srv.URL, "vendors", "list", "--all", "--jq", ".name")
	if want := "Vendor 1\nVendor 2\nVendor 3\nVendor 4\nVendor 5\n"; err != nil || stdout != want {
		t.Errorf("--all --jq .name = %q (%v), want %q", stdout, err, want)
	}
}

// A mistake in the expression, or --jq beside another format, stops the
// command with exit status 2 before anything is sent; an expression that
// fails on the answer is a failure (1), after what it printed so far.
func TestJQErrors(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--jq", ".data[", "vendors", "list"}, `invalid argument ".data[" for "--jq" flag: not a jq expression: unexpected EOF`},
		{[]string{"--jq", "  ", "vendors", "list"}, `invalid argument "  " for "--jq" flag: give a jq expression, such as .data[].name`},
		{[]string{"--jq", "nosuchfunction", "vendors", "list"}, `invalid argument "nosuchfunction" for "--jq" flag: not a jq expression: function not defined: nosuchfunction/0`},
		{[]string{"--jq", ".", "--format", "csv", "vendors", "list"}, "--jq filters JSON, so it cannot go with --format csv"},
		{[]string{"--jq", ".", "--format", "table", "vendors", "list"}, "--jq filters JSON, so it cannot go with --format table"},
	} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", srv.URL}, c.args...)...)
		if code != ExitUsage || msg != c.msg {
			t.Errorf("%v: exit %d (%s), want %d (%s)", c.args, code, msg, ExitUsage, c.msg)
		}
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("calls = %+v, want none", calls)
	}

	// --json and --format json go with it.
	for _, extra := range [][]string{{"--json"}, {"--format", "json"}} {
		stdout, _, err := run("test", append([]string{"--host", srv.URL, "vendors", "list", "--jq", ".data[0].id"}, extra...)...)
		if err != nil || stdout != "v1\n" {
			t.Errorf("%v: %q (%v)", extra, stdout, err)
		}
	}

	stdout, _, err := run("test", "--host", srv.URL, "vendors", "list", "--jq", ".data[0].name, .data[].nope[]")
	if code, _ := Classify(err); code != ExitFailed || err.Error() != "--jq: cannot iterate over: null" {
		t.Errorf("err = %v (exit %d)", err, code)
	}
	if stdout != "Acme\n" {
		t.Errorf("stdout = %q, want what came before the failure", stdout)
	}
}

// Every command that prints JSON takes --jq, the CLI's own too.
func TestJQOnABuiltInCommand(t *testing.T) {
	isolate(t)
	stdout, _, err := run("test", "--jq", ".host", "config", "show")
	if err != nil || stdout != "https://firmfact.com\n" {
		t.Errorf("config show --jq .host = %q (%v)", stdout, err)
	}
	stdout, _, err = run("test", "version", "--jq", "{version}")
	if err != nil || stdout != `{"version":"test"}`+"\n" {
		t.Errorf("version --jq {version} = %q (%v)", stdout, err)
	}
}

// What the expression prints is escaped like any server text; numbers
// come out as the JSON wrote them, however many digits; and the process's
// environment is not the expression's to read.
func TestJQOutputIsSafe(t *testing.T) {
	t.Setenv("FIRMFACT_TOKEN", "secret-token")
	filter := func(expr string, v any) string {
		t.Helper()
		var out bytes.Buffer
		app := &App{Out: &out}
		if err := (jqFlag{&app.jq}).Set(expr); err != nil {
			t.Fatal(err)
		}
		if err := app.PrintJSON(v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return out.String()
	}
	v := map[string]any{"name": "Evil" + osc52, "note": "C1\u009b", "id": json.RawMessage(`12345678901234567891`)}
	for _, expr := range []string{".name", ".", "[.note]"} {
		assertNoTerminalControls(t, "--jq "+expr, filter(expr, v))
	}
	if got := filter(".name", v); got != `Evil\u001b]52;c;cm0gLXJmIH4=\u0007`+"\n" {
		t.Errorf(".name = %q", got)
	}
	if got := filter(".id", v); got != "12345678901234567891\n" {
		t.Errorf(".id = %q", got)
	}
	for _, expr := range []string{"$ENV.FIRMFACT_TOKEN", "env.FIRMFACT_TOKEN", "[$ENV[]]"} {
		if got := filter(expr, v); strings.Contains(got, "secret-token") {
			t.Errorf("%s = %q", expr, got)
		}
	}
}

// The flag is a global one, like --json, and a tool's own argument of the
// same name keeps its flag.
func TestJQFlagDefersToAToolArgument(t *testing.T) {
	isolate(t)
	tool := mcp.Tool{Name: "list_vendors", Annotations: readsOnly, InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"jq": map[string]any{"type": "string"}},
	}}
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, tool)

	stdout, _, err := run("test", "--host", srv.URL, "vendors", "list", "--jq", ".x")
	if err != nil || !strings.HasPrefix(stdout, "NAME") {
		t.Errorf("stdout = %q (%v), want the table: --jq is the tool's", stdout, err)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Arguments["jq"] != ".x" {
		t.Errorf("calls = %+v", calls)
	}
}
