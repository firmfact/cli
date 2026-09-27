package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/update"
)

// complete is what a shell gets for a Tab after args (the last one being
// the word under the cursor): the candidates, and cobra's directive.
func complete(t *testing.T, version string, args ...string) (candidates []string, directive string) {
	t.Helper()
	stdout, stderr, err := run(version, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatalf("__complete %q: %v\n%s", args, err, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	return lines[:len(lines)-1], lines[len(lines)-1]
}

// :4 is ShellCompDirectiveNoFileComp: nothing else, and no file names.
const noFiles = ":4"

// A workspace command's flag completes to the values its schema allows,
// as the server's tool list describes them; FIRMFACT_HOST picks the list
// as it does for the command itself.
func TestCompleteToolFlagValues(t *testing.T) {
	withServerTools(t)
	t.Setenv("FIRMFACT_HOST", fixtureHost)

	got, directive := complete(t, "test", "analyze", "cost-trends", "--entity-type", "")
	want := []string{"contract", "vendor", "party", "product", "cost_center"}
	if !slices.Equal(got, want) || directive != noFiles {
		t.Errorf("--entity-type: %q %s, want %q %s", got, directive, want, noFiles)
	}
	// A boolean takes a value only as --flag=value.
	if got, directive := complete(t, "test", "analyze", "cost-trends", "--include-insights="); !slices.Equal(got, []string{"true", "false"}) || directive != noFiles {
		t.Errorf("--include-insights=: %q %s", got, directive)
	}
	// Free text has nothing to offer, and no tool takes a file.
	if got, directive := complete(t, "test", "vendors", "list", "--query", ""); len(got) != 0 || directive != noFiles {
		t.Errorf("--query: %q %s", got, directive)
	}
}

// A list flag offers its items' values, without the ones earlier uses of
// the flag gave; a value the terminal would not show as it is, is not
// offered at all.
func TestCompleteListFlagValues(t *testing.T) {
	isolate(t)
	tool := mcp.Tool{Name: "list_contracts", Annotations: readsOnly, InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"statuses": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"active", "expired", "draft"}}},
			"level":    map[string]any{"type": "integer", "enum": []any{1.0, 2.0}},
			"sort":     map[string]any{"type": "string", "enum": []any{"name", "bad\x1b[2Jvalue", "\tcost"}},
		},
	}}
	signedInWithTools(t, fixtureHost, tool)
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"--statuses", ""}, []string{"active", "expired", "draft"}},
		{[]string{"--statuses", "expired", "--statuses", ""}, []string{"active", "draft"}},
		{[]string{"--statuses", "expired", "--statuses=draft", "--statuses", "a"}, []string{"active"}},
		{[]string{"--statuses", "expired,draft", "--statuses", ""}, []string{"active"}},
		{[]string{"--level", ""}, []string{"1", "2"}},
		{[]string{"--sort", ""}, []string{"name"}},
	}
	for _, c := range cases {
		args := append([]string{"--host", fixtureHost, "contracts", "list"}, c.args...)
		got, directive := complete(t, "test", args...)
		// The shell narrows the candidates down to what was typed.
		got = slices.DeleteFunc(got, func(s string) bool { return !strings.HasPrefix(s, c.args[len(c.args)-1]) })
		if !slices.Equal(got, c.want) || directive != noFiles {
			t.Errorf("%q: %q %s, want %q", c.args, got, directive, c.want)
		}
	}
}

func TestCompleteFormat(t *testing.T) {
	isolate(t)
	got, directive := complete(t, "test", "config", "show", "--format", "")
	var values []string
	for _, c := range got {
		values = append(values, strings.SplitN(c, "\t", 2)[0])
	}
	if !slices.Equal(values, []string{"table", "json", "csv", "tsv"}) || directive != noFiles {
		t.Errorf("--format: %q %s", got, directive)
	}
}

// workspacesServer answers /api/v1/cli/me with these workspaces, and a
// revoke with 200, for logout.
func workspacesServer(t *testing.T, workspaces string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"name":"Jan","email":"jan@yourfirm.example"},"workspaces":`+workspaces+`}}`)
		case "/oauth/revoke":
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The workspaces a command last listed complete --workspace, the argument
// of `workspaces use` and `workspaces status`, and `config set workspace`,
// by name with the id beside it. Names two workspaces share are offered by
// id. Logging out forgets them.
func TestCompleteWorkspaceNames(t *testing.T) {
	isolate(t)
	srv := workspacesServer(t, `[{"id":"ws-1","name":"Acme Bank","default":true},{"id":"ws-2","name":"Demo"},{"id":"ws-3","name":"demo"}]`)
	storedToken(t, srv.URL, time.Hour)

	// Nothing listed yet, nothing to offer.
	if got, directive := complete(t, "test", "--host", srv.URL, "workspaces", "use", ""); len(got) != 0 || directive != noFiles {
		t.Errorf("before any listing: %q %s", got, directive)
	}
	if _, stderr, err := run("test", "--host", srv.URL, "whoami"); err != nil {
		t.Fatalf("whoami: %v\n%s", err, stderr)
	}
	want := []string{"Acme Bank\tws-1", "ws-2\tDemo", "ws-3\tdemo"}
	for _, args := range [][]string{
		{"workspaces", "use", ""},
		{"workspaces", "status", ""},
		{"whoami", "--workspace", ""},
		{"config", "set", "workspace", ""},
	} {
		got, directive := complete(t, "test", append([]string{"--host", srv.URL}, args...)...)
		if !slices.Equal(got, want) || directive != noFiles {
			t.Errorf("%q: %q %s, want %q", args, got, directive, want)
		}
	}
	// One workspace per command line; and another host has its own list.
	if got, _ := complete(t, "test", "--host", srv.URL, "workspaces", "use", "Demo", ""); len(got) != 0 {
		t.Errorf("second argument: %q", got)
	}
	if got, _ := complete(t, "test", "--host", "localhost:1", "workspaces", "use", ""); len(got) != 0 {
		t.Errorf("another host: %q", got)
	}

	// The copy keeps what completion needs and nothing about the user.
	files, _ := filepath.Glob(filepath.Join(os.Getenv("FIRMFACT_CACHE_DIR"), "workspaces-*.json"))
	if len(files) != 1 {
		t.Fatalf("workspace lists = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || strings.Contains(string(raw), "jan@yourfirm.example") || !strings.Contains(string(raw), "Acme Bank") {
		t.Errorf("cached list = %s, %v", raw, err)
	}

	if _, stderr, err := run("test", "--host", srv.URL, "logout"); err != nil {
		t.Fatalf("logout: %v\n%s", err, stderr)
	}
	if got, _ := complete(t, "test", "--host", srv.URL, "workspaces", "use", ""); len(got) != 0 {
		t.Errorf("after logout: %q", got)
	}
}

// A Tab never waits for, or is refused by, the daily version check: its
// answer must come at once, and a refusal would leave the shell with none.
func TestCompletionSkipsTheVersionCheck(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	prevVersion := api.Version
	t.Cleanup(func() { api.Version = prevVersion })
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(update.Status{CheckedAt: time.Now(), Host: config.DefaultHost, Minimum: "9.9.9"})
	if err := os.WriteFile(filepath.Join(cacheDir, "version-check.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("0.1.0", "whoami"); err == nil || !strings.Contains(err.Error(), "needs 9.9.9 or newer") {
		t.Fatalf("whoami: want a minimum-version refusal, got %v", err)
	}
	for _, request := range []string{"__complete", "__completeNoDesc"} {
		stdout, _, err := run("0.1.0", request, "--format", "")
		if err != nil || !strings.HasPrefix(stdout, "table") {
			t.Errorf("%s: %q, %v", request, stdout, err)
		}
	}
}

func TestWorkspaceCompletions(t *testing.T) {
	got := workspaceCompletions([]listedWorkspace{
		{ID: "ws-1", Name: "Acme Bank"},
		{ID: "ws-2", Name: " "},               // no name to offer
		{ID: "ws-3", Name: "Bad\x1b]0;x\x07"}, // not typed as shown
		{ID: "ws\n4", Name: "Also\nbad"},      // neither is
		{ID: "ws-5", Name: "Treasury"},
		{ID: "ws-6", Name: "treasury "},
	})
	want := []string{"Acme Bank\tws-1", "ws-2", `ws-3` + "\t" + `Bad\u001b]0;x\u0007`, "ws-5\tTreasury", "ws-6\ttreasury "}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
