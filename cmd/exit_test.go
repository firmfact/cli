package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/update"
)

// exitStatusOf runs one invocation and returns the exit status main would
// give it, with the error's text.
func exitStatusOf(ctx context.Context, version string, args ...string) (int, string) {
	var out, errOut bytes.Buffer
	err := NewRootCommand(Build{Version: version}, args, IOStreams{Out: &out, Err: &errOut}).ExecuteContext(ctx)
	if err == nil {
		return 0, ""
	}
	code, _ := Classify(err)
	return code, err.Error()
}

// answering is a host that answers every request with status, the given
// content type and body, and a header line such as "Retry-After: 120".
func answering(t *testing.T, status int, contentType, body string, header ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range header {
			k, v, _ := strings.Cut(h, ": ")
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// needsEntityType is a tool with a required argument.
var needsEntityType = mcp.Tool{
	Name:        "analyze_cost_trends",
	Annotations: readsOnly,
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"entity_type": map[string]any{"type": "string"}},
		"required":   []any{"entity_type"},
	},
}

// Every class of failure has its own exit status, so a script or CI job
// can tell a typo from an expired sign-in from a server that is down. A
// typo in a group's subcommand (`vendors lst`, `config sho`) used to print
// the group's help and exit 0.
func TestExitStatuses(t *testing.T) {
	isolate(t)
	const html = "text/html"
	const jsonType = "application/json"

	withTools := answering(t, http.StatusOK, jsonType, `{}`)
	signedInWithTools(t, withTools, listVendors, needsEntityType)

	notSignedIn := answering(t, http.StatusOK, jsonType, `{}`)

	refused := answering(t, http.StatusUnauthorized, jsonType, `{"error":"Invalid token","code":"UNAUTHORIZED"}`)
	if err := config.SaveToken(refused, &config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ended := answering(t, http.StatusBadRequest, jsonType, `{"error":"invalid_grant"}`)
	storedToken(t, ended, -time.Minute)

	noWorkspaces := answering(t, http.StatusOK, jsonType, `{"data":{"user":{"email":"jan@yourfirm.example"},"workspaces":[]}}`)
	jsonNotFound := answering(t, http.StatusNotFound, jsonType, `{"error":"Workspace not found","code":"NOT_FOUND"}`)
	htmlNotFound := answering(t, http.StatusNotFound, html, `<!DOCTYPE html><title>Not found</title>`)
	rateLimited := answering(t, http.StatusTooManyRequests, jsonType, `{}`, "Retry-After: 120")
	down := answering(t, http.StatusServiceUnavailable, html, `<h1>Maintenance</h1>`)
	closed := closedHost(t)
	for _, host := range []string{noWorkspaces, jsonNotFound, htmlNotFound, rateLimited, down, closed} {
		storedToken(t, host, time.Hour)
	}

	// An MCP server without the tool asked for.
	unknownTool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", jsonType)
		switch req.Method {
		case "initialize":
			w.Header().Set("mcp-session-id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
			return
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, req.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Unknown tool: no_such_tool"}}`, req.ID)
	}))
	defer unknownTool.Close()
	storedToken(t, unknownTool.URL, time.Hour)

	signup := (&signupServer{}).start(t).URL

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		args []string
		want int
	}{
		{"version", nil, []string{"--version"}, 0},
		{"a group on its own shows its help", nil, []string{"config"}, 0},
		{"help", nil, []string{"help", "config"}, 0},

		{"unknown command", nil, []string{"no-such-command"}, ExitUsage},
		{"typo in config", nil, []string{"config", "sho"}, ExitUsage},
		{"typo in tools", nil, []string{"tools", "lst"}, ExitUsage},
		{"typo in workspaces", nil, []string{"workspaces", "lst"}, ExitUsage},
		{"typo in a workspace group", nil, []string{"--host", withTools, "vendors", "lst"}, ExitUsage},
		{"typo after help", nil, []string{"help", "config", "sho"}, ExitUsage},
		{"typo in completion", nil, []string{"completion", "bsh"}, ExitUsage},
		{"argument to a command that takes none", nil, []string{"whoami", "extra"}, ExitUsage},
		{"missing argument", nil, []string{"workspaces", "use"}, ExitUsage},
		{"unknown flag", nil, []string{"--no-such-flag", "whoami"}, ExitUsage},
		{"bad flag value", nil, []string{"--timeout", "0", "whoami"}, ExitUsage},
		{"bad --host", nil, []string{"--host", "ftp://firmfact.example", "whoami"}, ExitUsage},
		{"missing required flag", nil, []string{"--host", withTools, "analyze", "cost-trends"}, ExitUsage},
		{"exclusive flags", nil, []string{"--host", signup, "signup", "--code", "1", "--code-stdin"}, ExitUsage},
		{"argument not key=value", nil, []string{"--host", withTools, "call", "list_vendors", "--arg", "search"}, ExitUsage},
		{"signup missing details in a script", nil, []string{"--host", signup, "signup", "--email", "jan@yourfirm.example", "--no-password"}, ExitUsage},
		{"bad host for set-host", nil, []string{"config", "set-host", "ftp://firmfact.example"}, ExitUsage},

		{"not signed in", nil, []string{"--host", notSignedIn, "whoami"}, ExitSignedOut},
		{"token refused", nil, []string{"--host", refused, "whoami"}, ExitSignedOut},
		{"session ended", nil, []string{"--host", ended, "whoami"}, ExitSignedOut},

		{"no such workspace", nil, []string{"--host", noWorkspaces, "workspaces", "use", "nope"}, ExitNotFound},
		{"the API found nothing", nil, []string{"--host", jsonNotFound, "whoami"}, ExitNotFound},
		{"no such tool", nil, []string{"--host", unknownTool.URL, "call", "no_such_tool"}, ExitNotFound},

		{"rate-limited", nil, []string{"--host", rateLimited, "whoami"}, ExitUnavailable},
		{"server down", nil, []string{"--host", down, "whoami"}, ExitUnavailable},
		{"no answer", nil, []string{"--host", closed, "whoami"}, ExitUnavailable},

		{"endpoint missing", nil, []string{"--host", htmlNotFound, "whoami"}, ExitUnsupported},

		{"interrupted", cancelled, []string{"--host", noWorkspaces, "whoami"}, ExitInterrupted},

		{"anything else", nil, []string{"--host", notSignedIn, "tools", "list"}, ExitFailed},
	}
	for _, c := range cases {
		ctx := c.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if got, msg := exitStatusOf(ctx, "test", c.args...); got != c.want {
			t.Errorf("%s: %v exits %d (%s), want %d", c.name, c.args, got, msg, c.want)
		}
	}
}

// A host without the CLI's endpoints (a firmfact older than the CLI, or a
// --host that is not firmfact) answers them with its page-not-found page,
// in HTML or JSON. Every command that meets one says the host does not
// support the CLI, and exits as unsupported.
func TestMissingEndpointExitStatus(t *testing.T) {
	isolate(t)
	pages := map[string][2]string{
		"html": {"text/html", `<!DOCTYPE html><title>Not found</title><h1>The page you're looking for doesn't exist.</h1>`},
		"json": {"application/json", `{"error":"The page you're looking for doesn't exist."}`},
	}
	for name, p := range pages {
		host := answering(t, http.StatusNotFound, p[0], p[1])
		storedToken(t, host, time.Hour)
		want := host + " does not support this version of the CLI yet (or --host is wrong)"
		for _, args := range [][]string{
			{"whoami"},           // /api/v1/cli/me
			{"tools", "refresh"}, // /mcp
			{"signup", "--email", "jan@yourfirm.example", "--no-password"},    // /api/v1/signup/options
			{"signup", "--email", "jan@yourfirm.example", "--code", "123456"}, // /api/v1/signup/confirm
		} {
			args = append([]string{"--host", host}, args...)
			if got, msg := exitStatusOf(context.Background(), "test", args...); got != ExitUnsupported || msg != want {
				t.Errorf("%s page, %v: exit %d (%s), want %d (%s)", name, args[2:], got, msg, ExitUnsupported, want)
			}
		}

		// --json names the status too.
		_, _, err := run("test", "--host", host, "--json", "whoami")
		var errOut bytes.Buffer
		ReportError(&errOut, err, true)
		var got struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if jerr := json.Unmarshal(errOut.Bytes(), &got); jerr != nil || got.Error.Message != want || got.Error.Code != ExitUnsupported || got.Error.Status != "unsupported" {
			t.Errorf("%s page, --json: %q, want status unsupported", name, errOut.String())
		}
	}
}

// A CLI below the server's minimum version is refused as unsupported.
func TestTooOldExitStatus(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	host := answering(t, http.StatusOK, "application/json", `{"minimum_version":"9.9.9"}`)
	prevURL, prevVersion := update.LatestReleaseURL, api.Version
	update.LatestReleaseURL = host
	defer func() { update.LatestReleaseURL, api.Version = prevURL, prevVersion }()
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(update.Status{CheckedAt: time.Now(), Host: host, Minimum: "9.9.9"})
	if err := os.WriteFile(filepath.Join(cacheDir, "version-check.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if got, msg := exitStatusOf(context.Background(), "0.1.0", "--host", host, "whoami"); got != ExitUnsupported {
		t.Errorf("exit %d (%s), want %d", got, msg, ExitUnsupported)
	}
	// A group on its own only shows its help, so it needs no check.
	if got, msg := exitStatusOf(context.Background(), "0.1.0", "--host", host, "workspaces"); got != 0 {
		t.Errorf("workspaces: exit %d (%s), want 0", got, msg)
	}
}

// A typo says what was meant, on one line. A command the tool list may not
// hold yet points at `tools refresh`, and on a host with no tool list at
// all, at how the workspace commands get there.
func TestUnknownCommandSuggestions(t *testing.T) {
	isolate(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"config", "sho"}, "unknown command \"sho\" for \"firmfact config\"; did you mean `firmfact config set` or `firmfact config show`?"},
		{[]string{"sigup"}, "unknown command \"sigup\" for \"firmfact\"; did you mean `firmfact signup`?"},
		{[]string{"vendors", "list"}, "unknown command \"vendors\" for \"firmfact\"; workspace commands appear after `firmfact login`; in scripts run `firmfact tools refresh` first or use `firmfact call <tool>`"},
		{[]string{"help", "vendors"}, "unknown command \"vendors\" for \"firmfact\"; workspace commands appear after `firmfact login`; in scripts run `firmfact tools refresh` first or use `firmfact call <tool>`"},
		{[]string{"config", "zzz"}, "unknown command \"zzz\" for \"firmfact config\"; see `firmfact config --help`"},
		// A flag of the command that is not there does not hide why it is
		// not there; a flag before any command still gets its own error.
		{[]string{"vendors", "list", "--limit", "2"}, "unknown command \"vendors\" for \"firmfact\"; workspace commands appear after `firmfact login`; in scripts run `firmfact tools refresh` first or use `firmfact call <tool>`"},
		{[]string{"--json", "vendors", "list", "--fields=name,id"}, "unknown command \"vendors\" for \"firmfact\"; workspace commands appear after `firmfact login`; in scripts run `firmfact tools refresh` first or use `firmfact call <tool>`"},
		{[]string{"config", "sho", "--all"}, "unknown command \"sho\" for \"firmfact config\"; did you mean `firmfact config set` or `firmfact config show`?"},
		{[]string{"--no-such-flag", "vendors", "list"}, "unknown flag: --no-such-flag"},
		{[]string{"config", "--no-such-flag"}, "unknown flag: --no-such-flag"},
	}
	for _, c := range cases {
		_, _, err := run("test", c.args...)
		if code, _ := Classify(err); err == nil || err.Error() != c.want || code != ExitUsage {
			t.Errorf("%v: got %v (exit %d), want %q (exit %d)", c.args, err, code, c.want, ExitUsage)
		}
	}

	// With a tool list cached, one without vendors, the list may be old;
	// so too when the command comes with flags of its own.
	if err := saveToolCache(config.DefaultHost, []mcp.Tool{{Name: "list_contracts"}}); err != nil {
		t.Fatal(err)
	}
	want := "unknown command \"vendors\" for \"firmfact\"; see `firmfact --help`, or run `firmfact tools refresh` if it is a workspace command"
	for _, args := range [][]string{{"vendors", "list"}, {"vendors", "list", "--limit", "2"}} {
		if _, _, err := run("test", args...); err == nil || err.Error() != want {
			t.Errorf("with a tool list, %v: got %v, want %q", args, err, want)
		}
	}
	// A typo in a group's subcommand is one too, flags or not.
	want = "unknown command \"lst\" for \"firmfact contracts\"; did you mean `firmfact contracts list`?"
	if _, _, err := run("test", "contracts", "lst", "--limit", "2"); err == nil || err.Error() != want {
		t.Errorf("contracts lst --limit 2: got %v, want %q", err, want)
	}

	// A group's help does not suggest it runs on its own.
	stdout, _, err := run("test", "config", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "firmfact config [flags]") || !strings.Contains(stdout, "firmfact config [command]") {
		t.Errorf("config --help:\n%s", stdout)
	}
}

// A system keyring that did not answer is worth another try once it is
// unlocked: exit status 5, like a server that did not answer, and never 3,
// which would say the sign-in is gone.
func TestKeyringTimeoutExitStatus(t *testing.T) {
	err := fmt.Errorf("could not read your sign-in: %w", config.ErrKeyringTimeout)
	if code, status := Classify(err); code != ExitUnavailable || status != "unavailable" {
		t.Errorf("Classify = %d %q, want %d unavailable", code, status, ExitUnavailable)
	}
}

// ReportError writes one "error:" line, or with --json one line of JSON
// whose code is the exit status; an interrupt gets no line in text.
func TestReportError(t *testing.T) {
	var buf bytes.Buffer
	notFound := withExit(ExitNotFound, errors.New("no workspace \"x\"\u001b]0;"))
	if code := ReportError(&buf, notFound, false); code != ExitNotFound || buf.String() != "error: no workspace \"x\"\\u001b]0;\n" {
		t.Errorf("text: %d %q", code, buf.String())
	}

	buf.Reset()
	if code := ReportError(&buf, notFound, true); code != ExitNotFound {
		t.Errorf("json: exit %d", code)
	}
	if strings.Count(buf.String(), "\n") != 1 || !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("want one line, got %q", buf.String())
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	dec := json.NewDecoder(&buf)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("not the envelope: %v", err)
	}
	if got.Error.Message != `no workspace "x"\u001b]0;` || got.Error.Code != ExitNotFound || got.Error.Status != "not_found" {
		t.Errorf("envelope = %+v", got)
	}

	buf.Reset()
	if code := ReportError(&buf, ErrInterrupted, false); code != ExitInterrupted || buf.Len() != 0 {
		t.Errorf("interrupt: %d %q", code, buf.String())
	}
}
