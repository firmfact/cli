package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/mcp"
)

// runAs is one invocation of the CLI run by name, as a user who claimed ff
// runs it, with its output captured.
func runAs(name string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	streams := IOStreams{In: strings.NewReader(""), Out: &out, Err: &errOut}
	err = newRootCommand(name, Build{Version: "test"}, args, streams, true).Execute()
	return out.String(), errOut.String(), err
}

// Help run as ff says ff throughout. The root lists the CLI's own commands
// apart from the workspace commands, and says where those come from when
// the host has none cached; a workspace command's flags show the schema's
// default and range, and its example carries the flags the tool requires.
// After a deliberate change, rewrite the files with
// `go test ./cmd -run TestHelpGolden -update` and read the diff.
func TestHelpGolden(t *testing.T) {
	withServerTools(t)
	cases := []struct {
		name string
		args []string
	}{
		{"root", []string{"--host", fixtureHost, "--help"}},
		{"root_without_tools", []string{"--host", "https://elsewhere.example", "--help"}},
		{"vendors_list", []string{"--host", fixtureHost, "vendors", "list", "--help"}},
		{"analyze_cost_trends", []string{"--host", fixtureHost, "analyze", "cost-trends", "--help"}},
		{"call", []string{"--host", fixtureHost, "call", "--help"}},
		{"update", []string{"update", "--help"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, err := runAs("ff", c.args...)
			if err != nil || stderr != "" {
				t.Fatalf("%v: %v, stderr %q", c.args, err, stderr)
			}
			// The product keeps its name; a command to type does not.
			if strings.Contains(stdout, "`firmfact") || strings.Contains(stdout, "  firmfact ") {
				t.Errorf("help run as ff names firmfact as a command:\n%s", stdout)
			}
			assertGoldenFile(t, filepath.Join("testdata", "help", c.name+".golden"), stdout)
		})
	}
}

// Hints and help texts name the command the way the user runs it: run as
// ff, the way in is `ff login`, not `firmfact login`.
func TestHintsUseTheInvokedName(t *testing.T) {
	isolate(t)
	_, _, err := runAs("ff", "--host", fixtureHost, "whoami")
	if err == nil || err.Error() != "not signed in: run `ff login` (or `ff signup`)" {
		t.Errorf("whoami: %v", err)
	}
	_, _, err = runAs("ff", "--host", fixtureHost, "tools", "list")
	if err == nil || !strings.Contains(err.Error(), "run `ff tools refresh`") {
		t.Errorf("tools list: %v", err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"help", "environment"}, "\n  ff tools refresh\n  ff vendors list --json"},
		{[]string{"help", "environment"}, "`ff config show` shows what is in use"},
		{[]string{"ask", "--help"}, "\n  ff ask --continue "},
		{[]string{"config", "show", "--help"}, "(see `ff help environment`)"},
		{[]string{"claim", "--help"}, `"ff claim <name> --undo"`},
		{[]string{"workspaces", "status", "--help"}, "\n  ff workspaces status --wait --json"},
	} {
		stdout, _, err := runAs("ff", c.args...)
		if err != nil || !strings.Contains(stdout, c.want) || strings.Contains(stdout, "firmfact "+c.args[len(c.args)-1]) {
			t.Errorf("%v: %v; want %q in\n%s", c.args, err, c.want, stdout)
		}
	}
}

// Every workspace command's help has one example, and it runs as shown:
// the flags its tool requires, with values the tool's schema takes.
func TestToolExamplesRun(t *testing.T) {
	tools := withServerTools(t)
	root := NewRootCommand(Build{Version: "test"}, []string{"--host", fixtureHost}, IOStreams{})
	checked := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		tool := c.Annotations[toolAnnotation]
		if tool == "" {
			return
		}
		line, ok := strings.CutPrefix(c.Example, "  firmfact ")
		if !ok || strings.Contains(line, "\n") {
			t.Errorf("%s: example %q, want one line that starts with the command", c.CommandPath(), c.Example)
			return
		}
		checked++
		assertRuns(t, tools, line, tool)
	}
	walk(root)
	if want := commandCount(planToolCommands(tools, commandNames(NewReferenceCommand("test")))); checked != want {
		t.Errorf("checked %d examples, want one for each of the %d workspace commands", checked, want)
	}
	for tool, want := range map[string]string{
		"analyze_cost_trends": "  firmfact analyze cost-trends --entity-type contract",
		"chat_with_workspace": `  firmfact chat-with-workspace --message "..."`,
		"list_vendors":        "  firmfact vendors list",
	} {
		for _, p := range planToolCommands(tools, nil) {
			if p.tool.Name == tool {
				if got := newToolCommand(&App{Name: "firmfact"}, p).Example; got != want {
					t.Errorf("%s: example %q, want %q", tool, got, want)
				}
			}
		}
	}
}

// listThings takes an argument of each type the schema has, for call.
var listThings = mcp.Tool{
	Name:        "list_things",
	Annotations: readsOnly,
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":  map[string]any{"type": "string"},
			"label":  map[string]any{"type": "string"},
			"limit":  map[string]any{"type": "integer"},
			"ratio":  map[string]any{"type": "number"},
			"active": map[string]any{"type": "boolean"},
			"fields": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			"kinds":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"a", "b", "c,d"}}},
			"filter": map[string]any{"type": "object"},
		},
	},
}

// call reads each --arg as the type the tool's schema gives it: query=123
// is sent as the text "123", as `things list --query 123` would send it,
// not the number JSON makes of it. An array takes its items as its flag
// does, commas and all (see TestListFlagItems), and an item that holds a
// comma in double quotes or a JSON array; an argument the schema does not
// describe is still read as JSON when it can be.
func TestCallReadsArgumentsByTheSchema(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listThings)

	_, _, err := run("test", "--host", srv.URL, "call", "list_things",
		"--arg", "query=123", "--arg", `label="quoted, once"`, "--arg", "limit=10", "--arg", "ratio=0.5",
		"--arg", "active=true", "--arg", "fields=name, legal", "--arg", "fields=id",
		"--arg", `fields="Acme, Inc."`, "--arg", `fields=["a, b"]`,
		"--arg", "ids=[1,2]", "--arg", "ids=3", "--arg", "kinds=a,b", "--arg", "kinds=c,d",
		"--arg", `filter={"a":1}`, "--arg", "extra=42", "--arg", "note=hello")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"query": "123", "label": "quoted, once", "limit": 10, "ratio": 0.5, "active": true,
		"fields": []any{"name", "legal", "id", "Acme, Inc.", "a, b"}, "ids": []any{1, 2, 3},
		"kinds": []any{"a", "b", "c,d"}, "filter": map[string]any{"a": 1},
		"extra": 42, "note": "hello",
	}
	calls := f.calls()
	if len(calls) != 1 || mustJSON(t, calls[0].Arguments) != mustJSON(t, want) {
		t.Fatalf("calls = %+v, want arguments %s", calls, mustJSON(t, want))
	}

	// A value the schema's type cannot take is refused before anything is
	// sent, as a flag of that type refuses it.
	for _, arg := range []string{"limit=ten", "limit=1.5", "ratio=NaN", "active=maybe", "ids=x", "fields= , ", "filter=[1]"} {
		_, _, err := run("test", "--host", srv.URL, "call", "list_things", "--arg", arg)
		if code, _ := Classify(err); code != ExitUsage || !strings.Contains(err.Error(), "--arg "+arg+": ") {
			t.Errorf("--arg %s: %v (exit %d)", arg, err, code)
		}
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("calls = %d, want the first alone", n)
	}
}

// A list flag takes an item each time it is given. Text items with no
// fixed values, such as fields, may also be separated by commas, as
// --columns takes them: `--fields name,id` asks for two fields, not one
// called "name,id", which the server would quietly match with none. Items
// with fixed values are separated too when each part is one of them; an
// item that is one of them keeps its commas, and anything else goes as
// typed, for the server to judge.
func TestListFlagItems(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	kinds := mcp.Tool{Name: "list_kinds", Annotations: readsOnly, InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kinds": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"a,b", "c", "d"}}},
			"sizes": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "enum": []any{1.0, 2.0}}},
		},
	}}
	signedInWithTools(t, srv.URL, listVendors, kinds)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"vendors", "list", "--fields", "name,id"}, `{"fields":["name","id"]}`},
		{[]string{"vendors", "list", "--fields", " name, temporal_id ,", "--fields", "id"}, `{"fields":["name","temporal_id","id"]}`},
		{[]string{"vendors", "list", "--fields", "name", "--fields", "id"}, `{"fields":["name","id"]}`},
		{[]string{"kinds", "list", "--kinds", "a,b", "--kinds", "c"}, `{"kinds":["a,b","c"]}`},
		{[]string{"kinds", "list", "--kinds", "c, d", "--kinds", "a,b"}, `{"kinds":["c","d","a,b"]}`},
		{[]string{"kinds", "list", "--kinds", "c,e"}, `{"kinds":["c,e"]}`},
		{[]string{"kinds", "list", "--sizes", "1,2", "--sizes", "1"}, `{"sizes":[1,2,1]}`},
	}
	for i, c := range cases {
		if _, _, err := run("test", append([]string{"--host", srv.URL}, c.args...)...); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := mustJSON(t, f.calls()[i].Arguments); got != c.want {
			t.Errorf("%v: sent %s, want %s", c.args, got, c.want)
		}
	}

	// A value that names no item at all is refused, not sent as none.
	_, _, err := run("test", "--host", srv.URL, "vendors", "list", "--fields", " , ")
	if code, _ := Classify(err); code != ExitUsage || err.Error() != `invalid argument " , " for "--fields" flag: it names no item` {
		t.Errorf("--fields \" , \": exit %d (%v)", code, err)
	}
	if n := len(f.calls()); n != len(cases) {
		t.Errorf("calls = %d, want %d", n, len(cases))
	}

	stdout, _, err := run("test", "--host", srv.URL, "vendors", "list", "--help")
	if err != nil || !strings.Contains(stdout, "(repeatable, or comma-separated)") {
		t.Errorf("vendors list --help = %q, %v", stdout, err)
	}
}

// Two properties that make the same flag, such as a_b and a-b, leave it to
// the first by name. pflag panics on the second, and as the workspace
// commands are made on every run, no command for the host could run.
func TestPropertiesThatShareAFlag(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, mcp.Tool{Name: "list_things", Annotations: readsOnly, InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a_b": map[string]any{"type": "string"},
			"a-b": map[string]any{"type": "integer"},
		},
	}})

	if _, _, err := run("test", "--host", srv.URL, "things", "list", "--a-b", "5"); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); len(calls) != 1 || mustJSON(t, calls[0].Arguments) != `{"a-b":5}` {
		t.Errorf("calls = %+v", calls)
	}
}
