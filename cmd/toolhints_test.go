package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/mcp"
)

// deleteVendor is a tool the server says may delete or overwrite data.
var deleteVendor = mcp.Tool{
	Name:  "delete_vendor",
	Title: "Delete a vendor",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]any{"type": "string"},
	}},
	Annotations: &mcp.ToolAnnotations{ReadOnlyHint: hint(false), DestructiveHint: hint(true)},
}

// Off a terminal a destructive tool does not run without --yes, from its
// command or from `call`: the command fails as a usage error before any
// call and says how to go ahead. A tool the server gives no hints counts as
// destructive, as MCP's defaults have it.
func TestDestructiveToolNeedsYesOffATerminal(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: `{"isError":false,"content":[{"type":"text","text":"{\"deleted\":true}"}]}`}
	srv := f.start(t)
	unhinted := mcp.Tool{Name: "purge_vendors", InputSchema: map[string]any{"type": "object"}}
	signedInWithTools(t, srv.URL, deleteVendor, unhinted, listVendors)

	for _, args := range [][]string{
		{"delete-vendor", "--id", "v1"},
		{"--workspace", "Acme Bank", "delete-vendor", "--id", "v1"},
		{"call", "delete_vendor", "--arg", "id=v1"},
		{"purge-vendors"},
		{"call", "purge_vendors"},
	} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", srv.URL}, args...)...)
		if code != ExitUsage || !strings.Contains(msg, "may delete or overwrite data") || !strings.Contains(msg, "use --yes to go ahead") {
			t.Errorf("%v: exit %d (%s), want %d and how to go ahead", args, code, msg, ExitUsage)
		}
	}
	_, _, err := run("test", "--host", srv.URL, "--workspace", "Acme Bank", "delete-vendor", "--id", "v1")
	if want := "`firmfact delete-vendor` may delete or overwrite data in workspace \"Acme Bank\", and there is no terminal to ask on; use --yes to go ahead"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none without --yes", calls)
	}

	for _, args := range [][]string{
		{"delete-vendor", "--id", "v1", "--yes"},
		{"delete-vendor", "--id", "v1", "-y"},
		{"call", "delete_vendor", "--arg", "id=v1", "--yes"},
		{"purge-vendors", "--yes"},
	} {
		if _, _, err := run("test", append([]string{"--host", srv.URL}, args...)...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	calls := f.calls()
	if len(calls) != 4 || calls[0].Name != "delete_vendor" || calls[0].Arguments["id"] != "v1" || calls[3].Name != "purge_vendors" {
		t.Errorf("calls = %+v", calls)
	}
	for _, c := range calls {
		if _, sent := c.Arguments["yes"]; sent {
			t.Errorf("--yes reached the server: %+v", c)
		}
	}

	// A tool that reads, or only adds, runs without asking; --yes is
	// accepted there too, so a script keeps working when the hints change.
	for _, args := range [][]string{{"vendors", "list"}, {"vendors", "list", "--yes"}, {"call", "list_vendors"}} {
		if _, _, err := run("test", append([]string{"--host", srv.URL}, args...)...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// Help marks a command whose tool writes, in the list of commands and in
// its own help, and one that may delete or overwrite data also says that
// it asks and shows --yes. A tool that only reads has neither.
func TestHelpMarksToolsThatWrite(t *testing.T) {
	isolate(t)
	host := "https://firmfact.example"
	signedInWithTools(t, host, listVendors, chatWithWorkspace, deleteVendor)

	root, _, err := run("test", "--host", host, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Chat with workspace (writes to your workspace)",
		"Delete a vendor (writes to your workspace, may delete or overwrite data)",
	} {
		if !strings.Contains(root, want) {
			t.Errorf("--help lacks %q:\n%s", want, root)
		}
	}
	group, _, _ := run("test", "--host", host, "vendors", "--help")
	if strings.Contains(group, "writes to your workspace") {
		t.Errorf("vendors --help marks a tool that only reads:\n%s", group)
	}

	chat, _, _ := run("test", "--host", host, "chat-with-workspace", "--help")
	if !strings.Contains(chat, "This command writes to your workspace.") || strings.Contains(chat, "--yes") {
		t.Errorf("chat-with-workspace --help:\n%s", chat)
	}
	del, _, _ := run("test", "--host", host, "delete-vendor", "--help")
	for _, want := range []string{"so it asks before it runs. --yes goes ahead without asking", "-y, --yes", yesFlagUsage} {
		if !strings.Contains(del, want) {
			t.Errorf("delete-vendor --help lacks %q:\n%s", want, del)
		}
	}
	list, _, _ := run("test", "--host", host, "vendors", "list", "--help")
	if strings.Contains(list, "writes to your workspace") || strings.Contains(list, "--yes") {
		t.Errorf("vendors list --help:\n%s", list)
	}
}

// `tools list` marks the tools that write after their titles, and --json
// says what the CLI makes of each tool's hints.
func TestToolsListMarksToolsThatWrite(t *testing.T) {
	isolate(t)
	host := "https://firmfact.example"
	signedInWithTools(t, host, listVendors, chatWithWorkspace, deleteVendor, mcp.Tool{Name: "help"})

	out, _, err := run("test", "--host", host, "tools", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			lines[fields[len(fields)-1]] = line
			if len(fields) > 1 {
				lines[fields[0]+" "+fields[1]] = line
			}
		}
	}
	if !strings.HasSuffix(lines["firmfact chat-with-workspace"], "Chat with workspace (writes to your workspace)") ||
		!strings.HasSuffix(lines["firmfact delete-vendor"], "Delete a vendor (writes to your workspace, may delete or overwrite data)") ||
		!strings.HasSuffix(lines["firmfact vendors"], "List vendors") {
		t.Errorf("tools list:\n%s", out)
	}
	if !strings.Contains(out, "is reserved for the CLI (writes to your workspace, may delete or overwrite data)") {
		t.Errorf("tools list does not mark a tool without a command:\n%s", out)
	}

	raw, _, err := run("test", "--host", host, "--json", "tools", "list")
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Name        string `json:"name"`
		Writes      *bool  `json:"writes"`
		Destructive *bool  `json:"destructive"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]bool{
		"list_vendors":        {false, false},
		"chat_with_workspace": {true, false},
		"delete_vendor":       {true, true},
		"help":                {true, true},
	}
	for _, tool := range got {
		w := want[tool.Name]
		if tool.Writes == nil || tool.Destructive == nil || *tool.Writes != w[0] || *tool.Destructive != w[1] {
			t.Errorf("%s: writes %v, destructive %v; want %v", tool.Name, tool.Writes, tool.Destructive, w)
		}
	}
}

// `call` of a tool the cache does not have fetches the list first, so it
// knows the tool's hints: a script with no list cached reads without --yes,
// and the list is cached for the next command.
func TestCallFetchesTheHintsOfAnUncachedTool(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, tools: []mcp.Tool{listVendors, deleteVendor}}
	srv := f.start(t)
	storedToken(t, srv.URL, time.Hour)

	if _, _, err := run("test", "--host", srv.URL, "call", "list_vendors"); err != nil {
		t.Fatal(err)
	}
	if calls, lists := len(f.calls()), f.listed(); calls != 1 || lists != 1 {
		t.Errorf("calls = %d, lists = %d; want 1 and 1", calls, lists)
	}
	if tc := loadToolCache(srv.URL); tc == nil || len(tc.Tools) != 2 {
		t.Errorf("cache = %+v, want the fetched list", tc)
	}
	// Cached now, and destructive: no list is fetched, and --yes is needed.
	if code, _ := exitStatusOf(t.Context(), "test", "--host", srv.URL, "call", "delete_vendor"); code != ExitUsage {
		t.Errorf("call delete_vendor: exit %d, want %d", code, ExitUsage)
	}
	if calls, lists := len(f.calls()), f.listed(); calls != 1 || lists != 1 {
		t.Errorf("calls = %d, lists = %d; want 1 and 1", calls, lists)
	}
}

// A tool the server now describes as destructive, where the cached list
// did not, is not called again after the server's "no such tool": the
// command did not ask first.
func TestRefreshedToolThatTurnedDestructiveIsNotCalledAgain(t *testing.T) {
	isolate(t)
	turned := listVendors
	turned.Annotations = deleteVendor.Annotations
	f := &mcpServer{result: vendorsResult, unknown: 1, tools: []mcp.Tool{turned}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	_, _, err := run("test", "--host", srv.URL, "vendors", "list")
	if err == nil || !strings.Contains(err.Error(), "list_vendors did not run: the server now says it may delete or overwrite data") {
		t.Fatalf("err = %v", err)
	}
	if calls, lists := len(f.calls()), f.listed(); calls != 1 || lists != 1 {
		t.Errorf("calls = %d, lists = %d; want 1 and 1", calls, lists)
	}
}

// A destructive tool with an argument of its own named yes keeps --yes for
// it, and its command says to go ahead through `call` instead.
func TestToolArgumentNamedYes(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	tool := deleteVendor
	tool.InputSchema = map[string]any{"type": "object", "properties": map[string]any{"yes": map[string]any{"type": "boolean"}}}
	signedInWithTools(t, srv.URL, tool)

	_, _, err := run("test", "--host", srv.URL, "delete-vendor", "--yes")
	if err == nil || !strings.Contains(err.Error(), "use `firmfact call delete_vendor --yes` to go ahead") {
		t.Errorf("err = %v", err)
	}
	help, _, _ := run("test", "--host", srv.URL, "delete-vendor", "--help")
	if !strings.Contains(help, "`firmfact call delete_vendor --yes` goes ahead without asking") {
		t.Errorf("help:\n%s", help)
	}
	if _, _, err := run("test", "--host", srv.URL, "call", "delete_vendor", "--arg", "yes=true", "--yes"); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Arguments["yes"] != true {
		t.Errorf("calls = %+v", calls)
	}
}
