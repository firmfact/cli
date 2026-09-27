package cmd

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/mcp"
)

// serverTools is testdata/tools.json: the tool list of a local firmfact
// server, as `firmfact tools list --json` prints it after `firmfact tools
// refresh`. Take it again when the server's tools change; the tests below
// then say whether the hints and the README still run.
func serverTools(t *testing.T) []mcp.Tool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("testdata/tools.json: %v", err)
	}
	return tools
}

const fixtureHost = "https://firmfact.example"

// withServerTools caches the fixture's tools for fixtureHost, so the
// commands built for it are the ones the server offers.
func withServerTools(t *testing.T) []mcp.Tool {
	t.Helper()
	isolate(t)
	tools := serverTools(t)
	if err := saveToolCache(fixtureHost, tools); err != nil {
		t.Fatal(err)
	}
	return tools
}

// commandWords splits a command line as a shell would for the quoting
// hints and the README use: words between blanks, double or single quotes
// around a word with blanks in it, and a word starting with # beginning a
// comment. A pipe or a redirection ends the command too: what follows is
// the shell's. So does a word that starts with a bracket, as the reason
// after a next step in the output the README shows: to a shell, it would
// be a syntax error.
func commandWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		case (r == '#' || r == '|' || r == '>' || r == '<' || r == '(') && !inWord:
			return words
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		t.Fatalf("unclosed quote in %q", line)
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// findCommand is the command line would run for fixtureHost, and the
// arguments left for it, found as Execute finds it.
func findCommand(t *testing.T, line string) (*cobra.Command, []string, error) {
	t.Helper()
	words := commandWords(t, line)
	root := NewRootCommand(Build{Version: "test"}, append([]string{"--host", fixtureHost}, words...), IOStreams{})
	cmd, rest, err := root.Find(words)
	// Find leaves a word a group does not know to the group's own check,
	// which Execute runs next.
	if err == nil && cmd.Annotations[groupAnnotation] != "" {
		if err = cmd.ParseFlags(rest); err == nil {
			err = cmd.ValidateArgs(cmd.Flags().Args())
		}
	}
	return cmd, rest, err
}

// runsTool is the server tool cmd calls: the one it was generated from, or
// the one a built-in command such as ask calls; empty for neither.
func runsTool(cmd *cobra.Command) string {
	if tool := cmd.Annotations[toolAnnotation]; tool != "" {
		return tool
	}
	return cmd.Annotations[callsAnnotation]
}

// assertRuns fails unless line would run tool (a built-in command that
// calls no tool when tool is empty) with every flag the tool requires, and
// with values its schema allows.
func assertRuns(t *testing.T, tools []mcp.Tool, line, tool string) {
	t.Helper()
	cmd, rest, err := findCommand(t, line)
	if err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	if got := runsTool(cmd); got != tool || !cmd.Runnable() {
		t.Fatalf("%q runs %q (runnable %v), want tool %q: no such command for the server's tools", line, cmd.CommandPath(), cmd.Runnable(), tool)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	if err := cmd.ValidateArgs(cmd.Flags().Args()); err != nil {
		t.Errorf("%q: %v", line, err)
	}
	if err := cmd.ValidateRequiredFlags(); err != nil {
		t.Errorf("%q: %v", line, err)
	}
	if tool == "" {
		return
	}
	i := slices.IndexFunc(tools, func(x mcp.Tool) bool { return x.Name == tool })
	if i < 0 {
		t.Fatalf("%q: the server has no tool %q", line, tool)
	}
	props, _ := tools[i].InputSchema["properties"].(map[string]any)
	for name, v := range props {
		f := cmd.Flags().Lookup(strings.ReplaceAll(name, "_", "-"))
		spec, _ := v.(map[string]any)
		enum, _ := spec["enum"].([]any)
		if f == nil || !f.Changed || len(enum) == 0 {
			continue
		}
		if !slices.Contains(enum, any(f.Value.String())) {
			t.Errorf("%q: --%s %s is not one of %v", line, f.Name, f.Value, enum)
		}
	}
}

// Every next step can be typed as shown: the tool exists and the hint
// carries the flags it requires. `analyze cost-trends` on its own failed
// on --entity-type, and signup ended on it.
func TestNextStepsRun(t *testing.T) {
	tools := withServerTools(t)
	for _, step := range allNextSteps {
		t.Run(step.command, func(t *testing.T) {
			assertRuns(t, tools, step.command, step.tool)
		})
	}
}

// allNextSteps must hold every hint, or TestNextStepsRun misses the one
// left out. Counting the literals in the source keeps the two together.
func TestAllNextStepsListed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literals := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok && len(lit.Elts) > 0 {
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "nextStep" {
					literals++
				}
			}
			return true
		})
	}
	if literals != len(allNextSteps) {
		t.Errorf("%d next steps are declared but allNextSteps lists %d; add the new one", literals, len(allNextSteps))
	}
}

// The README's examples of workspace commands run too, and of built-in
// commands that call a tool, such as ask. Other built-in commands are left
// to their own tests: the fixture does not describe them.
func TestReadmeCommandsRun(t *testing.T) {
	tools := withServerTools(t)
	f, err := os.Open(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	checked := 0
	inCode := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if !inCode || strings.HasSuffix(line, `\`) {
			continue
		}
		var rest string
		if r, ok := strings.CutPrefix(line, "firmfact "); ok {
			rest = r
		} else if r, ok := strings.CutPrefix(line, "ff "); ok {
			rest = r
		} else {
			continue
		}
		cmd, _, err := findCommand(t, rest)
		switch {
		case err != nil:
			t.Errorf("README: %q: %v", line, err)
		case runsTool(cmd) != "":
			checked++
			assertRuns(t, tools, rest, runsTool(cmd))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Error("found no workspace commands in the README's examples; is the parsing still right?")
	}
}

// --help says which flags a tool requires and which values it takes, and
// the defaults and ranges of its schema, before a run fails on them.
func TestToolFlagHelp(t *testing.T) {
	withServerTools(t)
	stdout, _, err := run("test", "--host", fixtureHost, "analyze", "cost-trends", "--help")
	if err != nil {
		t.Fatal(err)
	}
	var entityType, entityName string
	for _, line := range strings.Split(stdout, "\n") {
		switch {
		case strings.Contains(line, "--entity-type "):
			entityType = line
		case strings.Contains(line, "--entity-name "):
			entityName = line
		}
	}
	if !strings.HasSuffix(entityType, "(one of: contract, vendor, party, product, cost_center) (required)") {
		t.Errorf("--entity-type help = %q", entityType)
	}
	if entityName == "" || strings.Contains(entityName, "(required)") {
		t.Errorf("--entity-name help = %q", entityName)
	}

	cases := []struct {
		spec     map[string]any
		required bool
		want     string
	}{
		{map[string]any{"type": "integer", "description": "Months back", "enum": []any{float64(3), float64(6)}}, false, "Months back (one of: 3, 6)"},
		{map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"name", "id"}}}, true, "(any of: name, id) (repeatable, or comma-separated) (required)"},
		// Text items with no fixed values take commas as well (commaList).
		{map[string]any{"type": "array", "description": "Fields to return.", "items": map[string]any{"type": "string"}}, false, "Fields to return. (repeatable, or comma-separated)"},
		{map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, false, "(repeatable)"},
		{map[string]any{"type": "boolean", "description": "Include insights"}, false, "Include insights"},
		// The schema's default and range, unless the description names the
		// default already; false is what leaving out a boolean means.
		{map[string]any{"type": "integer", "description": "Page size.", "default": float64(100), "minimum": float64(1), "maximum": float64(200)}, false, "Page size. (default: 100) (from 1 to 200)"},
		{map[string]any{"type": "integer", "description": "Months back (default: 6)", "default": float64(6)}, false, "Months back (default: 6)"},
		{map[string]any{"type": "integer", "enum": []any{"default", "custom"}, "default": "custom"}, false, "(one of: default, custom) (default: custom)"},
		{map[string]any{"type": "number", "minimum": 0.5}, true, "(at least 0.5) (required)"},
		{map[string]any{"type": "number", "maximum": float64(1000000)}, false, "(at most 1000000)"},
		{map[string]any{"type": "boolean", "default": false}, false, ""},
		{map[string]any{"type": "boolean", "default": true}, false, "(default: true)"},
		{map[string]any{"type": "array", "default": []any{"name", "id"}}, false, "(repeatable) (default: name, id)"},
	}
	for _, c := range cases {
		if got := flagUsage(c.spec, c.required); got != c.want {
			t.Errorf("flagUsage(%v, %v) = %q, want %q", c.spec, c.required, got, c.want)
		}
	}
}
