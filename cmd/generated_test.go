package cmd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/firmfact/cli/internal/mcp"
)

// The workspace commands made from testdata/tools.json, the tool list of a
// local firmfact server: each command, its tool and whether that writes,
// and each flag with its type, the required ones marked; then the tools
// that get no command, and why. A change to how a schema becomes flags
// shows up here as a diff. After a deliberate change, or a new tools.json,
// rewrite the file with `go test ./cmd -run TestToolCommandTreeGolden
// -update` and read the diff.
func TestToolCommandTreeGolden(t *testing.T) {
	tools := withServerTools(t)
	root := NewRootCommand(Build{Version: "test"}, []string{"--host", fixtureHost}, IOStreams{})

	var b strings.Builder
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if tool := c.Annotations[toolAnnotation]; tool != "" {
			effect := "reads"
			if t, _ := findTool(tools, tool); !t.ReadOnly() {
				effect = "writes"
			}
			fmt.Fprintf(&b, "%s  (%s, %s)\n", strings.TrimPrefix(c.CommandPath(), "firmfact "), tool, effect)
			var flags []string
			c.LocalFlags().VisitAll(func(f *pflag.Flag) {
				line := "  --" + f.Name + " " + f.Value.Type()
				if req := f.Annotations[cobra.BashCompOneRequiredFlag]; len(req) == 1 && req[0] == "true" {
					line += "  required"
				}
				if f.Hidden {
					line += "  hidden"
				}
				flags = append(flags, line)
			})
			sort.Strings(flags)
			for _, f := range flags {
				b.WriteString(f + "\n")
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	b.WriteString("\nNo command:\n")
	for _, p := range planToolCommands(tools, commandNames(NewReferenceCommand("test"))) {
		if p.skipped != "" {
			fmt.Fprintf(&b, "  %s: %s\n", p.tool.Name, skipReason(p, "firmfact"))
		}
	}
	assertGoldenFile(t, filepath.Join("testdata", "tool_commands.golden"), b.String())
}

// allTypes takes an argument of every JSON type a schema can give.
var allTypes = mcp.Tool{
	Name:        "list_things",
	Annotations: readsOnly,
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":  map[string]any{"type": "string"},
			"limit":  map[string]any{"type": "integer"},
			"ratio":  map[string]any{"type": "number"},
			"active": map[string]any{"type": "boolean"},
			"fields": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			"tags":   map[string]any{"type": "array"},
			"filter": map[string]any{"type": "object"},
			"anyhow": map[string]any{},
		},
	},
}

// Each flag's value is sent as the JSON type its schema gives, as `call`
// sends it: a number as a number, a list item by the items' type (text
// items separated by commas too, see commaList), an object as an object;
// text stays text even when it looks like a number.
// Only the flags given are sent, a zero or false among them, and a value
// the type cannot take is refused before anything is sent.
func TestGeneratedCommandSendsSchemaTypes(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, allTypes)

	_, _, err := run("test", "--host", srv.URL, "--json", "things", "list",
		"--query", "123", "--limit", "0", "--ratio", "0.25", "--active=false",
		"--fields", "name, legal", "--fields", "id", "--ids", "7", "--ids", "8",
		"--tags", "a,b", "--filter", `{"a":1}`, "--anyhow", "true")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"active":false,"anyhow":"true","fields":["name","legal","id"],"filter":{"a":1},"ids":[7,8],"limit":0,"query":"123","ratio":0.25,"tags":["a,b"]}`
	calls := f.calls()
	if len(calls) != 1 || mustJSON(t, calls[0].Arguments) != want {
		t.Fatalf("calls = %+v, want arguments %s", calls, want)
	}

	if _, _, err := run("test", "--host", srv.URL, "--json", "things", "list", "--ratio", "1"); err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, f.calls()[1].Arguments); got != `{"ratio":1}` {
		t.Errorf("with one flag given, sent %s", got)
	}

	for _, arg := range []string{
		"--limit=ten", "--ratio=a lot", "--active=maybe",
		"--ids=x", "--filter=[1]", "--filter=a=1",
	} {
		flag, value, _ := strings.Cut(arg, "=")
		_, _, err := run("test", "--host", srv.URL, "things", "list", arg)
		want := fmt.Sprintf("invalid argument %q for %q flag: ", value, flag)
		if code, _ := Classify(err); code != ExitUsage || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v (exit %d), want a usage error starting %q", arg, err, code, want)
		}
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("%d calls, want no more after the refusals", n)
	}

	// Help says what an object flag takes, and an example fills one in
	// with a value that is sent.
	help, _, err := run("test", "--host", srv.URL, "things", "list", "--help")
	if err != nil || !strings.Contains(help, `--filter string        (a JSON object, such as '{"key":"value"}')`) {
		t.Errorf("help = %q, %v", help, err)
	}
	if value, _ := exampleValue("object", nil); value != "'{}'" {
		t.Errorf("an object flag's example value = %s", value)
	}
}

// A tool's error is the command's: no output, and an exit status that is
// not 0, so a script sees that the call failed. With --json the error is
// the JSON envelope on stderr, and stdout stays empty.
func TestGeneratedCommandToolErrorExitStatus(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: `{"isError":true,"content":[{"type":"text","text":"Workspace not found"}]}`}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	for _, args := range [][]string{{"vendors", "list"}, {"--json", "vendors", "list"}, {"call", "list_vendors"}} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", srv.URL}, args...)...)
		if code != ExitFailed || msg != "Workspace not found" {
			t.Errorf("%v: exit %d (%s), want %d", args, code, msg, ExitFailed)
		}
		stdout, _, _ := run("test", append([]string{"--host", srv.URL}, args...)...)
		if stdout != "" {
			t.Errorf("%v: stdout = %q", args, stdout)
		}
	}
}
