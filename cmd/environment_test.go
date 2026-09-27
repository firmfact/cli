package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
)

// FIRMFACT_HOST and FIRMFACT_PROFILE decide the host before the flags are
// parsed as well, where the flags still win.
func TestHostFromArgsHonoursTheEnvironment(t *testing.T) {
	isolate(t)
	app := &App{Config: &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
		"default": {Host: "https://default.example"},
		"other":   {Host: "https://other.example"},
	}}}
	cases := []struct {
		host, profile string
		args          []string
		want          string
	}{
		{"", "", nil, "https://default.example"},
		{"https://env.example", "", nil, "https://env.example"},
		{"localhost:5000", "", []string{"vendors", "list"}, "http://localhost:5000"},
		{"https://env.example", "", []string{"--host", "https://flag.example"}, "https://flag.example"},
		{"", "other", nil, "https://other.example"},
		{"", "other", []string{"--profile", "default"}, "https://default.example"},
		// The host's variable overrides the host of any profile, even one
		// the command line chose.
		{"https://env.example", "other", []string{"--profile=default"}, "https://env.example"},
		// Blank is unset.
		{"  ", " ", nil, "https://default.example"},
	}
	for _, c := range cases {
		t.Setenv("FIRMFACT_HOST", c.host)
		t.Setenv("FIRMFACT_PROFILE", c.profile)
		if got, err := hostFromArgs(app, c.args); err != nil || got != c.want {
			t.Errorf("FIRMFACT_HOST=%q FIRMFACT_PROFILE=%q %v: got %q, %v; want %q", c.host, c.profile, c.args, got, err, c.want)
		}
	}

	t.Setenv("FIRMFACT_PROFILE", "")
	for _, host := range []string{"http://firmfact.example", "http://firmfact.com@127.0.0.1:5000", "ftp://x"} {
		t.Setenv("FIRMFACT_HOST", host)
		_, err := hostFromArgs(app, nil)
		if err == nil || !strings.Contains(err.Error(), "FIRMFACT_HOST: ") {
			t.Errorf("FIRMFACT_HOST=%q: want an error naming the variable, got %v", host, err)
		}
		if code, _ := Classify(err); code != ExitUsage {
			t.Errorf("FIRMFACT_HOST=%q: exit %d, want %d", host, code, ExitUsage)
		}
	}
	t.Setenv("FIRMFACT_HOST", "http://firmfact.example")
	if got, err := hostFromArgs(app, []string{"--insecure-http"}); err != nil || got != "http://firmfact.example" {
		t.Errorf("--insecure-http with a plain http FIRMFACT_HOST: got %q, %v", got, err)
	}

	t.Setenv("FIRMFACT_HOST", "")
	t.Setenv("FIRMFACT_PROFILE", "othr")
	_, err := hostFromArgs(app, nil)
	if code, _ := Classify(err); code != ExitNotFound || err.Error() != `FIRMFACT_PROFILE: no profile "othr"; did you mean "other"?` {
		t.Errorf("FIRMFACT_PROFILE=othr: exit %d (%v)", code, err)
	}
}

// The workspace commands come from the tool cache of the host FIRMFACT_HOST
// names, and run there.
func TestFirmfactHostSelectsTheToolCache(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	if _, _, err := run("test", "vendors", "list"); err == nil || !strings.Contains(err.Error(), `unknown command "vendors"`) {
		t.Fatalf("without FIRMFACT_HOST the default host has no such command; got %v", err)
	}

	t.Setenv("FIRMFACT_HOST", srv.URL)
	if _, _, err := run("test", "vendors", "list", "--search", "acme"); err != nil {
		t.Fatalf("vendors list with FIRMFACT_HOST: %v", err)
	}
	if calls := f.calls(); len(calls) != 1 || calls[0].Arguments["search"] != "acme" {
		t.Errorf("calls = %+v", calls)
	}

	// --host still wins, and that host has no cached commands.
	if _, _, err := run("test", "--host", "http://127.0.0.1:1", "vendors", "list"); err == nil || !strings.Contains(err.Error(), `unknown command "vendors"`) {
		t.Errorf("--host over FIRMFACT_HOST: got %v", err)
	}
}

// singleWorkspaceVendors is list_vendors as the server describes it to a
// sign-in that reaches one workspace: without a workspace argument.
var singleWorkspaceVendors = mcp.Tool{
	Name:        "list_vendors",
	Title:       "List vendors",
	Annotations: readsOnly,
	InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"search": map[string]any{"type": "string"}},
	},
}

// A workspace the command names is sent even when the schema has no
// workspace argument, so the server can refuse one the sign-in does not
// reach instead of the command quietly running in another. The profile's
// default only goes where the schema takes it.
func TestNamedWorkspaceIsAlwaysSent(t *testing.T) {
	cases := []struct {
		name      string
		tool      mcp.Tool
		env       string
		args      []string
		workspace any // nil: not sent
	}{
		{"profile default, single workspace", singleWorkspaceVendors, "", nil, nil},
		{"--workspace, single workspace", singleWorkspaceVendors, "", []string{"--workspace", "ws-2"}, "ws-2"},
		{"FIRMFACT_WORKSPACE, single workspace", singleWorkspaceVendors, "ws-env", nil, "ws-env"},
		{"--workspace over FIRMFACT_WORKSPACE", singleWorkspaceVendors, "ws-env", []string{"--workspace", "ws-2"}, "ws-2"},
		{"profile default, several workspaces", listVendors, "", nil, "ws-profile"},
		{"FIRMFACT_WORKSPACE over the profile", listVendors, "ws-env", nil, "ws-env"},
		{"--workspace over both", listVendors, "ws-env", []string{"--workspace=ws-2"}, "ws-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			f := &mcpServer{result: vendorsResult}
			srv := f.start(t)
			signedInWithTools(t, srv.URL, c.tool)
			cfg := &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
				"default": {Host: srv.URL, Workspace: "ws-profile"},
			}}
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FIRMFACT_WORKSPACE", c.env)

			if _, _, err := run("test", append(c.args, "vendors", "list")...); err != nil {
				t.Fatalf("vendors list: %v", err)
			}
			calls := f.calls()
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			if got, sent := calls[0].Arguments["workspace"]; got != c.workspace || sent != (c.workspace != nil) {
				t.Errorf("workspace = %v (sent: %v), want %v", got, sent, c.workspace)
			}
		})
	}
}

// On a host other than the profile's, named with --host or FIRMFACT_HOST,
// a workspace command does not send the profile's default workspace: it
// belongs to the profile's host.
func TestProfileWorkspaceStaysOnItsHost(t *testing.T) {
	isolate(t)
	f := &mcpServer{result: vendorsResult}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)
	cfg := &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
		"default": {Host: config.DefaultHost, Workspace: "ws-production"},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("test", "--host", srv.URL, "vendors", "list"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRMFACT_HOST", srv.URL)
	if _, _, err := run("test", "vendors", "list"); err != nil {
		t.Fatal(err)
	}
	for i, call := range f.calls() {
		if ws, sent := call.Arguments["workspace"]; sent {
			t.Errorf("call %d sent workspace %v", i, ws)
		}
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("%d calls, want 2", n)
	}
}

// Each setting comes from its flag, else its variable, else the profile;
// config show says which variables supplied what it shows.
func TestFlagBeatsEnvironmentBeatsProfile(t *testing.T) {
	isolate(t)
	cfg := &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
		"default": {Host: "https://default.example", Workspace: "ws-default"},
		"other":   {Host: "https://other.example", Workspace: "ws-other"},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	show := func(t *testing.T, args ...string) profileJSON {
		t.Helper()
		stdout, _, err := run("test", append(args, "--json", "config", "show")...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var got profileJSON
		decodeOnly(t, "config show", stdout, &got)
		return got
	}

	if got := show(t); got != (profileJSON{"default", "https://default.example", "ws-default"}) {
		t.Errorf("profile only: %+v", got)
	}

	t.Setenv("FIRMFACT_PROFILE", "other")
	if got := show(t); got != (profileJSON{"other", "https://other.example", "ws-other"}) {
		t.Errorf("FIRMFACT_PROFILE: %+v", got)
	}
	if got := show(t, "--profile", "default"); got != (profileJSON{"default", "https://default.example", "ws-default"}) {
		t.Errorf("--profile over FIRMFACT_PROFILE: %+v", got)
	}

	// The profile's workspace belongs to its host, so on another it is not
	// used: the sign-in's own default applies there.
	t.Setenv("FIRMFACT_HOST", "env.example")
	if got := show(t); got != (profileJSON{"other", "https://env.example", ""}) {
		t.Errorf("FIRMFACT_HOST: %+v", got)
	}
	if got := show(t, "--host", "other.example"); got != (profileJSON{"other", "https://other.example", "ws-other"}) {
		t.Errorf("--host naming the profile's own host: %+v", got)
	}

	t.Setenv("FIRMFACT_WORKSPACE", "ws-env")
	if got := show(t); got != (profileJSON{"other", "https://env.example", "ws-env"}) {
		t.Errorf("all from the environment: %+v", got)
	}
	if got := show(t, "--host", "flag.example", "--workspace", "ws-flag"); got != (profileJSON{"other", "https://flag.example", "ws-flag"}) {
		t.Errorf("flags over the environment: %+v", got)
	}

	stdout, _, err := run("test", "--workspace", "ws-flag", "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	want := "profile:   other (from FIRMFACT_PROFILE)\n" +
		"host:      https://env.example (from FIRMFACT_HOST)\n" +
		"workspace: ws-flag\n"
	if stdout != want {
		t.Errorf("config show =\n%s\nwant\n%s", stdout, want)
	}

	// Nothing the environment supplied was written to the config.
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.CurrentProfile != "default" || saved.Profile("other").Host != "https://other.example" || saved.Profile("other").Workspace != "ws-other" {
		t.Errorf("config changed: %+v %+v", saved, saved.Profiles["other"])
	}
}

// A command that changes a setting the environment overrides says that the
// variable still decides what the next commands use.
func TestChangingASettingTheEnvironmentOverrides(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_HOST", "env.example")
	_, stderr, err := run("test", "config", "set-host", "profile.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "note: FIRMFACT_HOST is set, so commands use host env.example until it is unset.") {
		t.Errorf("set-host stderr = %q", stderr)
	}
	t.Setenv("FIRMFACT_HOST", "")

	if _, _, err := run("test", "--profile", "ci", "config", "set-host", "ci.example"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRMFACT_PROFILE", "ci")
	_, stderr, err = run("test", "config", "use-profile", "--create", "other")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "note: FIRMFACT_PROFILE is set, so commands use profile ci until it is unset.") {
		t.Errorf("use-profile stderr = %q", stderr)
	}
	t.Setenv("FIRMFACT_PROFILE", "")

	srv := meServer(t)
	storedToken(t, srv.URL, time.Hour)
	t.Setenv("FIRMFACT_WORKSPACE", "demo-1")
	stdout, stderr, err := run("test", "--host", srv.URL, "--json", "workspaces", "use", "Bank BV")
	if err != nil {
		t.Fatal(err)
	}
	var w Workspace
	decodeOnly(t, "workspaces use", stdout, &w)
	if w.ID != "prod-1" || !strings.Contains(stderr, "note: FIRMFACT_WORKSPACE is set, so commands use workspace demo-1 until it is unset.") {
		t.Errorf("workspaces use = %+v, stderr %q", w, stderr)
	}

	// Without the variables, no note.
	t.Setenv("FIRMFACT_WORKSPACE", "")
	if _, stderr, _ := run("test", "--host", srv.URL, "workspaces", "use", "Bank BV"); strings.Contains(stderr, "note:") {
		t.Errorf("stderr without the variable = %q", stderr)
	}
}

// The variables that turn something on or off all read alike, as
// FIRMFACT_DEBUG always has: blank, 0, false, no and off, in any case, are
// off. So FIRMFACT_NO_UPDATE_CHECK=0 leaves the daily check on, and
// CI=false is no CI job.
func TestOnOffVariables(t *testing.T) {
	for value, on := range map[string]bool{
		"": false, " ": false, "0": false, "false": false, "FALSE": false, "no": false, "Off": false,
		"1": true, "true": true, "yes": true, " on ": true,
	} {
		t.Setenv("FIRMFACT_NO_UPDATE_CHECK", value)
		t.Setenv("FIRMFACT_DEBUG", value)
		t.Setenv("CI", value)
		if updateCheckOff() != on || debugFromEnv() != on || envOn(envCI) != on {
			t.Errorf("%q: check off %v, debug %v, CI %v; want all %v", value, updateCheckOff(), debugFromEnv(), envOn(envCI), on)
		}
	}
}

// `help environment` names every variable the CLI reads, and the root help
// lists the topic.
func TestEnvironmentHelpTopic(t *testing.T) {
	isolate(t)
	stdout, _, err := run("test", "help", "environment")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"FIRMFACT_HOST", "FIRMFACT_PROFILE", "FIRMFACT_WORKSPACE", "FIRMFACT_TOKEN ", "FIRMFACT_TOKEN_STORE",
		"FIRMFACT_SIGNUP_CODE", "FIRMFACT_TIMEOUT", "FIRMFACT_NO_UPDATE_CHECK", "FIRMFACT_CONFIG_DIR",
		"FIRMFACT_CACHE_DIR", "HTTPS_PROXY", "SSL_CERT_FILE", "NO_COLOR", "CI ",
	} {
		if !strings.Contains(stdout, name) {
			t.Errorf("help environment does not mention %s", name)
		}
	}
	// Each list of variables is a paragraph of its own, which no line of
	// prose splits: a note between two variables reads as part of the list.
	for _, para := range strings.Split(stdout, "\n\n") {
		lines := strings.Split(strings.Trim(para, "\n"), "\n")
		indented := 0
		for _, line := range lines {
			if strings.HasPrefix(line, "  ") {
				indented++
			}
		}
		if indented > 0 && indented < len(lines) {
			t.Errorf("a list of variables mixed with prose:\n%s", para)
		}
	}
	// The same text without `help`, as for any help topic.
	if again, _, err := run("test", "environment"); err != nil || again != stdout {
		t.Errorf("environment = %q, %v", again, err)
	}

	stdout, _, err = run("test", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Additional help topics:") || !strings.Contains(stdout, "environment") {
		t.Errorf("--help does not list the topic:\n%s", stdout)
	}
}
