package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
)

// Only plain names become commands, never one of the CLI's own, and when
// two tools want one command the first by name gets it, whatever order the
// server lists them in.
func TestPlanToolCommands(t *testing.T) {
	builtins := map[string]bool{"login": true, "config": true, "workspaces": true, "tools": true}
	names := []string{
		"list_vendors", "analyze_cost_trends", "chat_with_workspace",
		"help", "completion", "version", "login", "list_config", "list_workspaces", "analyze_help",
		"List_Vendors", "list-vendors", "vendors list", "list_vendors\x1b[2J", "_hidden", "9lives",
		"list__x", "analyze_", "list_", strings.Repeat("a", 65),
		"vendors", "analyze", "list_vendors",
	}
	tools := make([]mcp.Tool, len(names))
	for i, n := range names {
		tools[i] = mcp.Tool{Name: n}
	}
	want := []struct{ path, skipped, clash string }{
		{"vendors list", "", ""},
		{"analyze cost-trends", "", ""},
		{"chat-with-workspace", "", ""},
		{"help", skipReserved, "help"},
		{"completion", skipReserved, "completion"},
		{"version", skipReserved, "version"},
		{"login", skipReserved, "login"},
		{"config list", skipReserved, "config"},
		{"workspaces list", skipReserved, "workspaces"},
		{"analyze help", skipReserved, "analyze help"},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"", skipInvalid, ""},
		{"vendors", skipTaken, "vendors"},
		{"analyze", skipTaken, "analyze"},
		{"vendors list", skipTaken, "vendors list"},
	}
	plans := planToolCommands(tools, builtins)
	for i, p := range plans {
		if p.path() != want[i].path || p.skipped != want[i].skipped || p.clash != want[i].clash {
			t.Errorf("%q: %q skipped %q clash %q, want %q skipped %q clash %q",
				names[i], p.path(), p.skipped, p.clash, want[i].path, want[i].skipped, want[i].clash)
		}
	}
	if n := commandCount(plans); n != 3 {
		t.Errorf("commands = %d, want 3", n)
	}
	// 64 characters is the longest name that may be a command.
	if p := planToolCommands([]mcp.Tool{{Name: strings.Repeat("a", 64)}}, builtins); p[0].skipped != "" {
		t.Errorf("a 64-character name: skipped %q", p[0].skipped)
	}
}

// A cached tool cannot take the place of the CLI's own commands: help,
// completion, version and login stay the CLI's, and a tool is never called
// for them. `tools list` shows each tool without a command
// and why, in text and JSON.
func TestServerToolsCannotTakeBuiltInNames(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	var tools []mcp.Tool
	for _, n := range []string{"help", "completion", "version", "login", "list_config", "Bad-Name"} {
		tools = append(tools, mcp.Tool{Name: n, Title: "Takeover " + n, Description: "Server tool " + n, Annotations: readsOnly})
	}
	signedInWithTools(t, srv.URL, append(tools, listVendors)...)

	help, _, err := run("test", "--host", srv.URL, "help")
	if err != nil || !strings.Contains(help, "\nCommands:\n") || !strings.Contains(help, "vendors") || strings.Contains(help, "Takeover") {
		t.Errorf("help = %q, %v; want the CLI's own help", help, err)
	}
	if out, _, err := run("test", "--host", srv.URL, "completion", "bash"); err != nil || !strings.Contains(out, "bash completion") {
		t.Errorf("completion bash = %.80q, %v", out, err)
	}
	if out, _, err := run("test", "--host", srv.URL, "login", "--help"); err != nil || !strings.Contains(out, "--no-browser") {
		t.Errorf("login --help = %q, %v", out, err)
	}
	if out, _, err := run("test", "--host", srv.URL, "version"); err != nil || !strings.HasPrefix(out, "firmfact test\n") {
		t.Errorf("version = %q, %v; want the CLI's own version", out, err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("tools called: %+v", calls)
	}

	listed, _, err := run("test", "--host", srv.URL, "tools", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"firmfact vendors list  list_vendors  List vendors",
		"No command; run these with `firmfact call <tool>`:",
		"help         `firmfact help` is reserved for the CLI",
		"version      `firmfact version` is reserved for the CLI",
		"list_config  `firmfact config` is reserved for the CLI",
		"Bad-Name     its name cannot be a command name",
	} {
		if !strings.Contains(listed, want) {
			t.Errorf("tools list lacks %q:\n%s", want, listed)
		}
	}

	raw, _, err := run("test", "--host", srv.URL, "--json", "tools", "list")
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("tools list --json: %v\n%s", err, raw)
	}
	byName := map[string]map[string]any{}
	for _, tool := range got {
		byName[tool["name"].(string)] = tool
	}
	if tool := byName["list_vendors"]; tool["command"] != "vendors list" || tool["skipped"] != nil {
		t.Errorf("list_vendors = %v", tool)
	}
	if tool := byName["help"]; tool["command"] != nil || tool["skipped"] != "reserved" || tool["title"] != "Takeover help" {
		t.Errorf("help = %v", tool)
	}
	if tool := byName["Bad-Name"]; tool["skipped"] != "invalid_name" {
		t.Errorf("Bad-Name = %v", tool)
	}

	// `call` still reaches a tool that has no command.
	if _, _, err := run("test", "--host", srv.URL, "call", "help"); err != nil {
		t.Fatalf("call help: %v", err)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Name != "help" {
		t.Errorf("calls = %+v", calls)
	}
}

// A call the server answers with "no such tool", as a JSON-RPC error or as
// its tool error, fetches the tool list once and calls the tool once more.
func TestUnknownToolIsRefreshedAndCalledAgain(t *testing.T) {
	for name, f := range map[string]*mcpServer{
		"method not found": {unknown: 1},
		"tool error":       {unknownText: 1},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			f.result = vendorsResult
			f.tools = []mcp.Tool{listVendors, {Name: "list_contracts", Annotations: readsOnly}}
			srv := f.start(t)
			signedInWithTools(t, srv.URL, listVendors)

			stdout, _, err := run("test", "--host", srv.URL, "vendors", "list")
			if err != nil {
				t.Fatalf("vendors list: %v", err)
			}
			if !strings.Contains(stdout, "Acme") {
				t.Errorf("stdout = %q", stdout)
			}
			if calls, lists := len(f.calls()), f.listed(); calls != 2 || lists != 1 {
				t.Errorf("calls = %d, lists = %d; want 2 and 1", calls, lists)
			}
			// The fetched list is the cache now.
			if _, _, err := run("test", "--host", srv.URL, "contracts", "list"); err != nil {
				t.Errorf("contracts list after the refresh: %v", err)
			}
		})
	}
}

// A tool the fresh list no longer has is not called again: the command
// says the server has no such tool, and it is gone from the commands.
func TestToolGoneFromTheServer(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, unknown: 1, tools: []mcp.Tool{{Name: "list_contracts"}}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	_, _, err := run("test", "--host", srv.URL, "vendors", "list")
	want := "the server has no tool \"list_vendors\"; `firmfact tools list` shows the ones it has"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if code, _ := Classify(err); code != ExitNotFound {
		t.Errorf("exit %d, want %d", code, ExitNotFound)
	}
	if calls, lists := len(f.calls()), f.listed(); calls != 1 || lists != 1 {
		t.Errorf("calls = %d, lists = %d; want 1 and 1", calls, lists)
	}
	if _, _, err := run("test", "--host", srv.URL, "vendors", "list"); err == nil || !strings.Contains(err.Error(), `unknown command "vendors"`) {
		t.Errorf("vendors list after the refresh: %v, want no such command", err)
	}
}

// The refresh and the second call happen once: a server that keeps saying
// "no such tool" gets its answer back.
func TestUnknownToolIsCalledAgainOnce(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, unknown: 3, tools: []mcp.Tool{listVendors}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	_, _, err := run("test", "--host", srv.URL, "call", "list_vendors")
	if err == nil || err.Error() != "Unknown tool: list_vendors" {
		t.Fatalf("err = %v, want the server's answer", err)
	}
	if calls, lists := len(f.calls()), f.listed(); calls != 2 || lists != 1 {
		t.Errorf("calls = %d, lists = %d; want 2 and 1", calls, lists)
	}
}

func TestUnknownTool(t *testing.T) {
	text := func(s string) *mcp.CallResult {
		return &mcp.CallResult{IsError: true, Content: []mcp.Content{{Type: "text", Text: s}}}
	}
	cases := []struct {
		res  *mcp.CallResult
		err  error
		want bool
	}{
		{nil, &mcp.RPCError{Method: "tools/call", Code: mcp.ErrMethodNotFound}, true},
		{nil, &mcp.RPCError{Method: "tools/call", Code: mcp.ErrInvalidParams}, true},
		{nil, &mcp.RPCError{Method: "initialize", Code: mcp.ErrMethodNotFound}, false},
		{nil, &mcp.RPCError{Method: "tools/call", Code: -32603}, false},
		{text("Tool execution failed: Unknown tool: list_vendors"), nil, true},
		{text("Unknown tool: list_vendors"), nil, true},
		{text("Unknown tool: list_vendors_v2"), nil, false},
		{text("Chat failed: Unknown tool: list_contracts"), nil, false},
		{&mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: "Unknown tool: list_vendors"}}}, nil, false},
	}
	for i, c := range cases {
		if got := unknownTool("list_vendors", c.res, c.err); got != c.want {
			t.Errorf("case %d: unknownTool = %v, want %v", i, got, c.want)
		}
	}
}

// ageToolCache makes host's cached tool list look fetched age ago.
func ageToolCache(t *testing.T, host string, age time.Duration) {
	t.Helper()
	tc := loadToolCache(host)
	if tc == nil {
		t.Fatal("no tool cache")
	}
	tc.FetchedAt = time.Now().Add(-age)
	path, err := toolCachePath(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteJSON(path, tc, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestToolCacheStale(t *testing.T) {
	isolate(t)
	host := "https://firmfact.example"
	now := time.Now()
	if !toolCacheStale(host, now) {
		t.Error("no cache should be stale")
	}
	if err := saveToolCache(host, []mcp.Tool{listVendors}); err != nil {
		t.Fatal(err)
	}
	if toolCacheStale(host, now.Add(time.Minute)) {
		t.Error("a fresh cache should not be stale")
	}
	if !toolCacheStale(host, now.Add(25*time.Hour)) {
		t.Error("a day-old cache should be stale")
	}
	if !toolCacheStale(host, now.Add(-time.Hour)) {
		t.Error("a cache from the future should be stale")
	}
}

// A script does not refresh a stale list behind its back: off a terminal a
// workspace command makes its own call and nothing else.
func TestScriptsLeaveAStaleToolListAlone(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, tools: []mcp.Tool{listVendors}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)
	ageToolCache(t, srv.URL, 48*time.Hour)

	if _, _, err := run("test", "--host", srv.URL, "vendors", "list"); err != nil {
		t.Fatal(err)
	}
	if n := f.listed(); n != 0 {
		t.Errorf("tools/list requests = %d, want none", n)
	}
	if !toolCacheStale(srv.URL, time.Now()) {
		t.Error("the cache was refreshed")
	}
}

// The background refresh fetches and caches the list beside the command,
// and settle waits for it.
func TestToolRefreshInTheBackground(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, tools: []mcp.Tool{listVendors, {Name: "list_contracts"}}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)
	ageToolCache(t, srv.URL, 48*time.Hour)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Name: "firmfact", Config: cfg, HostFlag: srv.URL, Out: io.Discard, Err: io.Discard}
	r := startToolRefresh(context.Background(), app)
	r.settle(context.Background())
	tools, err := r.result(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("refresh = %v, %v", tools, err)
	}
	if tc := loadToolCache(srv.URL); tc == nil || len(tc.Tools) != 2 || toolCacheStale(srv.URL, time.Now()) {
		t.Errorf("cache after the refresh = %+v", tc)
	}
	// No refresh is no wait.
	var none *toolRefresh
	none.settle(context.Background())
}

// A tool list cached by the CLI before it kept the tools' annotations, as
// that CLI wrote it: no format, and tools without hints. Read as it is,
// every tool would count as one that may delete data, and a script would
// need --yes to list vendors. It counts as no cache instead: the commands
// that fetch a missing list fetch it again, and `vendors list` says how to
// get the commands back rather than asking for --yes. A cache from a newer
// CLI, of a format this one does not know, counts as none too.
func TestToolCacheOfAnotherFormatCountsAsNone(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, tools: []mcp.Tool{listVendors}}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)
	path, err := toolCachePath(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for name, format := range map[string]string{"no format": "", "format 1": `"format":1,`, "format 3": `"format":3,`} {
		old := `{` + format + `"host":` + mustJSON(t, srv.URL) + `,"fetched_at":` + mustJSON(t, time.Now()) +
			`,"tools":[{"name":"list_vendors","title":"List vendors","description":"","inputSchema":` + mustJSON(t, listVendors.InputSchema) + `}]}`
		if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		if tc := loadToolCache(srv.URL); tc != nil {
			t.Errorf("%s: loaded %+v", name, tc)
		}
		if !toolCacheStale(srv.URL, time.Now()) {
			t.Errorf("%s: not stale", name)
		}
		_, _, err := run("test", "--host", srv.URL, "vendors", "list")
		if code, _ := Classify(err); code != ExitUsage || !strings.Contains(fmt.Sprint(err), "tools refresh") || strings.Contains(fmt.Sprint(err), "--yes") {
			t.Errorf("%s: vendors list = %v (exit %d), want the unknown command and how to fetch it", name, err, code)
		}
	}

	// `call` fetches a list it has none of, and the tool, which only reads,
	// runs without --yes. The fresh cache is this CLI's own.
	if _, stderr, err := run("test", "--host", srv.URL, "call", "list_vendors"); err != nil {
		t.Fatalf("call list_vendors: %v\n%s", err, stderr)
	}
	if n := f.listed(); n != 1 {
		t.Errorf("tools/list requests = %d, want 1", n)
	}
	tc := loadToolCache(srv.URL)
	if tc == nil || tc.Format != toolCacheFormat || len(tc.Tools) != 1 || !tc.Tools[0].ReadOnly() {
		t.Fatalf("cache after call = %+v", tc)
	}
	if _, stderr, err := run("test", "--host", srv.URL, "vendors", "list"); err != nil {
		t.Errorf("vendors list after the fetch: %v\n%s", err, stderr)
	}
}
