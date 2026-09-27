package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
)

// decodeOnly decodes stdout into v and fails unless it holds that one
// JSON value and nothing else, which is what a script piping it needs.
func decodeOnly(t *testing.T, what, stdout string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: stdout is not JSON: %v\n%s", what, err, stdout)
	}
	if dec.More() {
		t.Fatalf("%s: more than one JSON value on stdout:\n%s", what, stdout)
	}
}

// meServer answers /api/v1/cli/me with a user and two workspaces.
func meServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			io.WriteString(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},
				"workspaces":[{"id":"prod-1","name":"Bank BV"},{"id":"demo-1","name":"Demo","default":true,"demo":true}]}}`)
		case "/api/v1/cli/version":
			io.WriteString(w, `{"minimum_version":"0.0.1"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConfigCommandsJSON(t *testing.T) {
	isolate(t)
	stdout, stderr, err := run("test", "--json", "config", "set-host", "localhost:5001")
	if err != nil {
		t.Fatal(err)
	}
	var got profileJSON
	decodeOnly(t, "set-host", stdout, &got)
	if got != (profileJSON{"default", "http://localhost:5001", ""}) {
		t.Errorf("set-host = %+v", got)
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}

	stdout, _, err = run("test", "--json", "config", "use-profile", "--create", "other")
	if err != nil {
		t.Fatal(err)
	}
	decodeOnly(t, "use-profile", stdout, &got)
	if got != (profileJSON{"other", config.DefaultHost, ""}) {
		t.Errorf("use-profile = %+v", got)
	}

	stdout, _, err = run("test", "--json", "--profile", "default", "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	decodeOnly(t, "show", stdout, &got)
	if got != (profileJSON{"default", "http://localhost:5001", ""}) {
		t.Errorf("show = %+v", got)
	}
}

func TestWorkspacesUseJSON(t *testing.T) {
	isolate(t)
	srv := meServer(t)
	storedToken(t, srv.URL, time.Hour)

	stdout, _, err := run("test", "--host", srv.URL, "--json", "workspaces", "use", "demo")
	if err != nil {
		t.Fatal(err)
	}
	var got Workspace
	decodeOnly(t, "workspaces use", stdout, &got)
	if got.ID != "demo-1" || !got.Demo {
		t.Errorf("workspace = %+v", got)
	}
}

// login --json prints the result alone on stdout; the link and the wait,
// which the person signing in needs, go to stderr.
func TestLoginJSON(t *testing.T) {
	isolate(t)
	useBrowser(t, approvingBrowser)
	srv := meServer(t) // no MCP: the commands cannot be loaded

	stdout, stderr, err := run("test", "--host", srv.URL, "--json", "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var got loginResult
	decodeOnly(t, "login", stdout, &got)
	if got.Host != srv.URL || got.User.Email != "jan@yourfirm.example" || got.Workspace != "demo-1" || len(got.Workspaces) != 2 || got.Commands != nil {
		t.Errorf("login = %+v", got)
	}
	if !strings.Contains(stderr, "Opened your browser to sign in") || !strings.Contains(stderr, "could not load the workspace commands yet") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestLogoutJSON(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)
	storedToken(t, srv.URL, time.Hour)

	var got logoutResult
	for _, want := range []logoutResult{{srv.URL, true}, {srv.URL, false}} {
		stdout, _, err := run("test", "--host", srv.URL, "--json", "logout")
		if err != nil {
			t.Fatal(err)
		}
		decodeOnly(t, "logout", stdout, &got)
		if got != want {
			t.Errorf("logout = %+v, want %+v", got, want)
		}
	}
	assertTokenGone(t, srv.URL)
}

// tools refresh --json prints what tools list --json does, fresh.
func TestToolsRefreshJSON(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("mcp-session-id", "sess-1")
		result := `{}`
		if req.Method == "tools/list" {
			result = `{"tools":[{"name":"list_vendors","title":"List vendors","description":"Vendors","inputSchema":{"type":"object"}}]}`
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()
	storedToken(t, srv.URL, time.Hour)

	refreshed, _, err := run("test", "--host", srv.URL, "--json", "tools", "refresh")
	if err != nil {
		t.Fatal(err)
	}
	var tools []map[string]any
	decodeOnly(t, "tools refresh", refreshed, &tools)
	if len(tools) != 1 || tools[0]["name"] != "list_vendors" {
		t.Errorf("tools = %v", tools)
	}
	listed, _, err := run("test", "--host", srv.URL, "--json", "tools", "list")
	if err != nil || listed != refreshed {
		t.Errorf("tools list --json = %q, %v; want what refresh printed", listed, err)
	}
}

// doctor --json lists every check, and fails the way doctor fails when
// one does, with the report still on stdout.
func TestDoctorJSON(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	srv := meServer(t)
	storedToken(t, srv.URL, time.Hour)

	stdout, _, err := run("test", "--host", srv.URL, "--json", "doctor")
	if err == nil || err.Error() != "1 check(s) failed" {
		t.Fatalf("want the missing commands to fail, got %v", err)
	}
	var got doctorReport
	decodeOnly(t, "doctor", stdout, &got)
	if got.Host != srv.URL || got.Version != "test" || got.OK {
		t.Errorf("report = %+v", got)
	}
	names := map[string]bool{}
	for _, c := range got.Checks {
		names[c.Name] = true
		if c.OK == (c.Name == "commands") {
			t.Errorf("check %+v", c)
		}
	}
	for _, want := range []string{"version", "proxy", "connection", "clock", "token store", "sign-in", "config", "commands"} {
		if !names[want] {
			t.Errorf("no %q check in %+v", want, got.Checks)
		}
	}

	signedInWithTools(t, srv.URL, listVendors)
	stdout, _, err = run("test", "--host", srv.URL, "--json", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	decodeOnly(t, "doctor", stdout, &got)
	if !got.OK {
		t.Errorf("report = %+v", got)
	}
}

// doctor against a host without the CLI's version endpoint says the
// server predates the CLI, rather than only that it answered 404.
func TestDoctorOnAServerThatPredatesTheCLI(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	host := answering(t, http.StatusNotFound, "application/json", `{"error":"The page you're looking for doesn't exist."}`)

	stdout, _, err := run("test", "--host", host, "--json", "doctor")
	if err == nil {
		t.Fatal("doctor passed against a host without the CLI's endpoints")
	}
	var got doctorReport
	decodeOnly(t, "doctor", stdout, &got)
	for _, c := range got.Checks {
		if c.Name != "connection" {
			continue
		}
		if c.OK || !strings.HasSuffix(c.Detail, "; the server predates the CLI (or the host is wrong)") || !strings.HasPrefix(c.Detail, host+" answered 404 in ") {
			t.Errorf("connection check = %+v", c)
		}
		return
	}
	t.Errorf("no connection check in %+v", got.Checks)
}

func TestUpdateJSON(t *testing.T) {
	isolate(t)
	fakeGitHub(t) // the latest is v0.1.0
	srv := meServer(t)
	prev := api.Version
	defer func() { api.Version = prev }()

	stdout, _, err := run("0.1.0", "--host", srv.URL, "--json", "update")
	if err != nil {
		t.Fatal(err)
	}
	var got updateResult
	decodeOnly(t, "update", stdout, &got)
	if got != (updateResult{Version: "0.1.0", Latest: "0.1.0"}) {
		t.Errorf("update = %+v", got)
	}
}
