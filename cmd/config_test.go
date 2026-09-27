package cmd

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
)

// writeConfigFile puts raw at the config file's path and returns the path.
func writeConfigFile(t *testing.T, raw string) string {
	t.Helper()
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// saveProfiles writes a config with the given profiles and current one.
func saveProfiles(t *testing.T, current string, profiles map[string]*config.Profile) {
	t.Helper()
	cfg := &config.Config{CurrentProfile: current, Profiles: profiles}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// check is the report's check of that name, or a zero one when it has none.
func (r doctorReport) check(name string) doctorCheck {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return doctorCheck{}
}

func assertFileHolds(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s changed:\n%s\nwant:\n%s", path, got, want)
	}
}

// brokenConfig has the trailing comma that once cost a user both profiles:
// the CLI carried on with the defaults, and `config use-profile ci` saved
// them over the file.
const brokenConfig = `{
  "current_profile": "default",
  "profiles": {
    "default": {"host": "https://firmfact.com"},
    "ci": {"host": "https://ci.firmfact.example"},
  }
}
`

// A config file that does not parse stops every command that uses a
// profile, with the file's path, where the problem is and the way out, and
// no command changes a byte of it. doctor reports it, and config reset
// moves it aside whole.
func TestBrokenConfigIsNeverSavedOver(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	srv := meServer(t)
	path := writeConfigFile(t, brokenConfig)

	for _, args := range [][]string{
		{"config", "use-profile", "ci"},
		{"config", "use-profile", "--create", "new"},
		{"config", "set-host", "localhost:5000"},
		{"config", "set", "host", "localhost:5000"},
		{"config", "set", "workspace", "ws-1"},
		{"config", "unset", "workspace"},
		{"config", "get", "host"},
		{"config", "show"},
		{"--json", "config", "show"},
		{"config", "profiles", "list"},
		{"config", "profiles", "rename", "ci", "ci2"},
		{"config", "profiles", "delete", "ci"},
		{"--host", srv.URL, "whoami"},
		{"--host", srv.URL, "workspaces", "use", "Bank BV"},
		{"--host", srv.URL, "login", "--no-browser"},
		{"logout"},
	} {
		_, _, err := run("test", args...)
		if err == nil {
			t.Fatalf("%v ran on the defaults", args)
		}
		msg := err.Error()
		if !strings.Contains(msg, path+": line 6, column 3: ") || !strings.Contains(msg, "`firmfact config reset`") {
			t.Errorf("%v: %v", args, err)
		}
		if code, _ := Classify(err); code != ExitFailed {
			t.Errorf("%v: exit %d, want %d", args, code, ExitFailed)
		}
		assertFileHolds(t, path, brokenConfig)
	}

	// Help needs no profile.
	for _, args := range [][]string{{"config"}, {"help", "config"}, {"config", "use-profile", "--help"}} {
		if _, _, err := run("test", args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}

	storedToken(t, srv.URL, time.Hour)
	stdout, _, err := run("test", "--host", srv.URL, "--json", "doctor")
	if err == nil {
		t.Fatal("doctor passed with a broken config file")
	}
	var report doctorReport
	decodeOnly(t, "doctor", stdout, &report)
	found := false
	for _, c := range report.Checks {
		if c.Name == "config" {
			found = true
			if c.OK || !strings.Contains(c.Detail, path+": line 6, column 3") || !strings.Contains(c.Detail, "config reset") {
				t.Errorf("config check = %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("no config check in %+v", report.Checks)
	}
	assertFileHolds(t, path, brokenConfig)

	stdout, _, err = run("test", "--json", "config", "reset")
	if err != nil {
		t.Fatal(err)
	}
	var reset resetResult
	decodeOnly(t, "config reset", stdout, &reset)
	if reset.Path != path || !strings.HasPrefix(reset.MovedTo, path+".") || !strings.HasSuffix(reset.MovedTo, ".bak") {
		t.Fatalf("reset = %+v", reset)
	}
	assertFileHolds(t, reset.MovedTo, brokenConfig)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config file still there: %v", err)
	}

	stdout, _, err = run("test", "config", "show")
	if err != nil || !strings.Contains(stdout, "host:      "+config.DefaultHost+"\n") {
		t.Errorf("config show after reset = %q, %v", stdout, err)
	}
	if stdout, _, err = run("test", "config", "reset"); err != nil || !strings.Contains(stdout, "There is no config file") {
		t.Errorf("a second reset = %q, %v", stdout, err)
	}
}

// A profile name that matches none fails, suggesting the one meant, and
// changes nothing; --create is how a new profile is made.
func TestUseProfileTypoFails(t *testing.T) {
	isolate(t)
	saveProfiles(t, "default", map[string]*config.Profile{
		"default": {Host: config.DefaultHost},
		"staging": {Host: "https://staging.firmfact.example", Workspace: "ws-s"},
	})
	path, _ := config.Path()
	before, _ := os.ReadFile(path)

	_, _, err := run("test", "config", "use-profile", "stagin")
	if code, _ := Classify(err); code != ExitNotFound {
		t.Fatalf("use-profile stagin: exit %d (%v), want %d", code, err, ExitNotFound)
	}
	if want := `no profile "stagin"; did you mean "staging"? Add --create to make a new profile`; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	_, _, err = run("test", "config", "use-profile", "prod")
	if err == nil || err.Error() != "no profile \"prod\"; add --create to make a new one, or see `firmfact config profiles list`" {
		t.Errorf("use-profile prod: %v", err)
	}
	assertFileHolds(t, path, string(before))

	stdout, _, err := run("test", "config", "use-profile", "staging")
	if err != nil || stdout != "Current profile is now staging.\n" {
		t.Fatalf("use-profile staging = %q, %v", stdout, err)
	}
	// The default profile is always there to go back to.
	if _, _, err := run("test", "config", "use-profile", "default"); err != nil {
		t.Fatalf("use-profile default: %v", err)
	}

	stdout, _, err = run("test", "config", "use-profile", "--create", "ci")
	if err != nil || !strings.Contains(stdout, "Created profile ci, on "+config.DefaultHost) {
		t.Fatalf("use-profile --create ci = %q, %v", stdout, err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentProfile != "ci" || cfg.Profiles["ci"] == nil || cfg.Profiles["staging"].Host != "https://staging.firmfact.example" {
		t.Errorf("config = %+v", cfg)
	}

	for _, name := range []string{"has space", ".dot", "", strings.Repeat("x", 65)} {
		_, _, err := run("test", "config", "use-profile", "--create", name)
		if code, _ := Classify(err); code != ExitUsage {
			t.Errorf("use-profile --create %q: exit %d (%v), want %d", name, code, err, ExitUsage)
		}
	}
}

// --profile and FIRMFACT_PROFILE must name a profile that exists. A typo
// once ran on the default host, https://firmfact.com, with the sign-in
// stored for it, and login and signup went on to make the profile. Now
// every command that uses the profile stops with the names meant, before
// anything is sent anywhere or saved, and so does a workspace command,
// which there are none of for a profile that does not exist.
func TestProfileTypoFails(t *testing.T) {
	isolate(t)
	// Should the check ever go, no command here may reach the default
	// host: those that would ask it something before failing go to srv.
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "1")
	useBrowser(t, func(u string) error {
		t.Errorf("a browser was opened at %s", u)
		return nil
	})
	prev := loginTimeout
	loginTimeout = 20 * time.Millisecond
	t.Cleanup(func() { loginTimeout = prev })
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	storedToken(t, srv.URL, time.Hour)
	if err := saveToolCache(srv.URL, []mcp.Tool{listVendors}); err != nil {
		t.Fatal(err)
	}
	saveProfiles(t, "default", map[string]*config.Profile{
		"default": {Host: srv.URL},
		"staging": {Host: srv.URL, Workspace: "ws-s"},
	})
	path, _ := config.Path()
	before, _ := os.ReadFile(path)

	commands := [][]string{
		{"whoami"},
		{"login"},
		{"login", "--no-browser"},
		{"signup", "--code", "123456"},
		{"logout"},
		{"workspaces", "use", "Demo"},
		{"config", "show"},
		{"config", "get", "host"},
		{"config", "unset", "workspace"},
		{"open"},
		{"call", "list_vendors"},
		{"vendors", "list"},
		// With a flag only the command it names would know.
		{"vendors", "list", "--limit", "2"},
	}
	for _, args := range commands {
		_, _, err := run("test", append([]string{"--profile", "stagin"}, args...)...)
		want := `--profile: no profile "stagin"; did you mean "staging"?`
		if code, _ := Classify(err); code != ExitNotFound || err.Error() != want {
			t.Errorf("--profile stagin %s: exit %d (%v), want %d: %s", strings.Join(args, " "), code, err, ExitNotFound, want)
		}
	}
	t.Setenv("FIRMFACT_PROFILE", "stagin")
	for _, args := range commands {
		_, _, err := run("test", args...)
		want := `FIRMFACT_PROFILE: no profile "stagin"; did you mean "staging"?`
		if code, _ := Classify(err); code != ExitNotFound || err.Error() != want {
			t.Errorf("FIRMFACT_PROFILE=stagin %s: exit %d (%v), want %d: %s", strings.Join(args, " "), code, err, ExitNotFound, want)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests reached a host", n)
	}
	assertFileHolds(t, path, string(before))

	// doctor reports the profile as a failed check, rather than stopping
	// where it cannot say what else is wrong.
	stdout, _, err := run("test", "--host", srv.URL, "--json", "doctor")
	if err == nil {
		t.Error("doctor passed with a profile that does not exist")
	}
	var report doctorReport
	decodeOnly(t, "doctor", stdout, &report)
	if c := report.check("config"); c.OK || c.Detail != `FIRMFACT_PROFILE: no profile "stagin"; did you mean "staging"?` {
		t.Errorf("doctor's config check = %+v", c)
	}

	// The commands that use no profile in use still run, among them the
	// ones to find and choose the right one, which say that the variable
	// names none.
	noted := "note: FIRMFACT_PROFILE names stagin, which is no profile, so commands will not run until it changes or is unset.\n"
	for _, args := range [][]string{
		{"version"},
		{"help", "whoami"},
		{"config", "profiles", "list"},
		{"config", "use-profile", "staging"},
	} {
		_, stderr, err := run("test", args...)
		if err != nil {
			t.Errorf("FIRMFACT_PROFILE=stagin %s: %v", strings.Join(args, " "), err)
		}
		if args[0] == "config" && !strings.HasSuffix(stderr, noted) {
			t.Errorf("FIRMFACT_PROFILE=stagin %s: stderr %q", strings.Join(args, " "), stderr)
		}
	}
	// A flag names the profile as the variable would, and wins over it.
	if _, _, err := run("test", "--profile", "staging", "config", "show"); err != nil {
		t.Errorf("--profile staging over FIRMFACT_PROFILE=stagin: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["stagin"]; ok || cfg.CurrentProfile != "staging" {
		t.Errorf("config = %+v", cfg)
	}

	// A variable that names a profile renamed since says so, and then stops
	// the commands that would use it.
	t.Setenv("FIRMFACT_PROFILE", "staging")
	_, stderr, err := run("test", "config", "profiles", "rename", "staging", "staging-eu")
	if err != nil || !strings.Contains(stderr, "note: FIRMFACT_PROFILE still names staging, which is now called staging-eu, so commands will not run until it changes.") {
		t.Errorf("rename = %v, stderr %q", err, stderr)
	}
	if _, _, err := run("test", "whoami"); err == nil || err.Error() != `FIRMFACT_PROFILE: no profile "staging"; did you mean "staging-eu"?` {
		t.Errorf("whoami after the rename: %v", err)
	}
}

// login, signup and workspaces use change the profile in use, the one
// --profile names, and leave the others as they were.
func TestSignInChangesTheProfileInUse(t *testing.T) {
	isolate(t)
	useBrowser(t, approvingBrowser)
	srv := meServer(t)
	saveProfiles(t, "default", map[string]*config.Profile{
		"default": {Host: config.DefaultHost, Workspace: "ws-default"},
		"staging": {Host: "https://staging.firmfact.example"},
	})
	if _, _, err := run("test", "--profile", "staging", "--host", srv.URL, "login"); err != nil {
		t.Fatalf("login: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["staging"]; p == nil || p.Host != srv.URL || p.Workspace != "demo-1" {
		t.Errorf("staging after login = %+v", p)
	}
	if p := cfg.Profiles["default"]; p.Host != config.DefaultHost || p.Workspace != "ws-default" {
		t.Errorf("default after login = %+v", p)
	}
	if _, _, err := run("test", "--profile", "staging", "workspaces", "use", "Bank BV"); err != nil {
		t.Fatalf("workspaces use: %v", err)
	}
	if cfg, err = config.Load(); err != nil || cfg.Profiles["staging"].Workspace != "prod-1" || cfg.Profiles["default"].Workspace != "ws-default" {
		t.Errorf("after workspaces use: %+v, %v", cfg.Profiles, err)
	}
}

// Renaming a profile keeps its host, the permission for plain http that
// came with it and its workspace; the current profile stays current.
func TestProfileRenameKeepsTheHost(t *testing.T) {
	isolate(t)
	saveProfiles(t, "lab", map[string]*config.Profile{
		"default": {Host: config.DefaultHost},
		"lab":     {Host: "http://lab.firmfact.example", InsecureHTTP: true, Workspace: "ws-lab"},
	})

	stdout, _, err := run("test", "--json", "config", "profiles", "rename", "lab", "lab2")
	if err != nil {
		t.Fatal(err)
	}
	var got profileJSON
	decodeOnly(t, "rename", stdout, &got)
	if got != (profileJSON{"lab2", "http://lab.firmfact.example", "ws-lab"}) {
		t.Errorf("rename = %+v", got)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["lab2"]; cfg.CurrentProfile != "lab2" || p == nil || !p.InsecureHTTP || cfg.Profiles["lab"] != nil {
		t.Errorf("config = %+v", cfg)
	}
	stdout, _, err = run("test", "config", "show")
	if err != nil || !strings.HasPrefix(stdout, "profile:   lab2\nhost:      http://lab.firmfact.example\n") {
		t.Errorf("config show = %q, %v", stdout, err)
	}

	cases := []struct {
		from, to string
		code     int
	}{
		{"lab", "lab3", ExitNotFound},
		{"lab2", "default", ExitFailed},
		{"lab2", "no good", ExitUsage},
	}
	for _, c := range cases {
		_, _, err := run("test", "config", "profiles", "rename", c.from, c.to)
		if code, _ := Classify(err); code != c.code {
			t.Errorf("rename %s %s: exit %d (%v), want %d", c.from, c.to, code, err, c.code)
		}
	}

	// The default profile can be renamed too, and is there again after.
	if _, _, err := run("test", "config", "profiles", "rename", "default", "prod"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = run("test", "config", "profiles", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  default  " + config.DefaultHost, "*  lab2     http://lab.firmfact.example", "  prod     " + config.DefaultHost} {
		if !strings.Contains(stdout, want) {
			t.Errorf("profiles list has no %q:\n%s", want, stdout)
		}
	}
}

// profiles list shows every profile, the default one included; delete
// refuses the current and the default profile, and a name that matches
// none.
func TestProfilesListAndDelete(t *testing.T) {
	isolate(t)
	saveProfiles(t, "staging", map[string]*config.Profile{
		"staging": {Host: "https://staging.firmfact.example", Workspace: "ws-s"},
		"ci":      {Host: "https://ci.firmfact.example"},
	})

	stdout, _, err := run("test", "--json", "config", "profiles", "list")
	if err != nil {
		t.Fatal(err)
	}
	var entries []profileEntry
	decodeOnly(t, "profiles list", stdout, &entries)
	want := []profileEntry{
		{"ci", "https://ci.firmfact.example", "", false},
		{"default", config.DefaultHost, "", false},
		{"staging", "https://staging.firmfact.example", "ws-s", true},
	}
	if len(entries) != len(want) {
		t.Fatalf("profiles = %+v", entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("profile %d = %+v, want %+v", i, entries[i], want[i])
		}
	}

	cases := []struct {
		name string
		code int
		msg  string
	}{
		{"staging", ExitFailed, "staging is the current profile"},
		{"default", ExitFailed, "the default profile is always there"},
		{"cj", ExitNotFound, `no profile "cj"; did you mean "ci"?`},
	}
	for _, c := range cases {
		_, _, err := run("test", "config", "profiles", "delete", c.name)
		if code, _ := Classify(err); code != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("delete %s: exit %d (%v), want %d and %q", c.name, code, err, c.code, c.msg)
		}
	}

	stdout, _, err = run("test", "config", "profiles", "delete", "ci")
	if err != nil || stdout != "Deleted profile ci, which used https://ci.firmfact.example.\n" {
		t.Fatalf("delete ci = %q, %v", stdout, err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["ci"]; ok || cfg.Profiles["staging"] == nil {
		t.Errorf("config = %+v", cfg.Profiles)
	}
	if _, _, err := run("test", "config", "profiles", "delete", "ci"); err == nil {
		t.Error("deleted ci twice")
	}
}

// get, set and unset read and change one setting of the profile in use.
func TestConfigGetSetUnset(t *testing.T) {
	isolate(t)
	get := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, _, err := run("test", append([]string{"config", "get"}, args...)...)
		if err != nil {
			t.Fatalf("get %v: %v", args, err)
		}
		return stdout
	}
	set := func(t *testing.T, args ...string) {
		t.Helper()
		if _, _, err := run("test", append([]string{"config", "set"}, args...)...); err != nil {
			t.Fatalf("set %v: %v", args, err)
		}
	}

	if got := get(t, "host"); got != config.DefaultHost+"\n" {
		t.Errorf("host = %q", got)
	}
	if got := get(t, "workspace"); got != "" {
		t.Errorf("workspace = %q", got)
	}
	set(t, "host", "Staging.Firmfact.Example")
	set(t, "workspace", "ws-1")
	if got := get(t, "host"); got != "https://staging.firmfact.example\n" {
		t.Errorf("host = %q", got)
	}
	// The same host keeps the workspace; another clears it, as it belongs
	// to the old host.
	set(t, "host", "https://staging.firmfact.example/")
	if got := get(t, "workspace"); got != "ws-1\n" {
		t.Errorf("workspace after the same host = %q", got)
	}
	set(t, "host", "other.firmfact.example")
	if got := get(t, "workspace"); got != "" {
		t.Errorf("workspace after another host = %q", got)
	}

	set(t, "workspace", "ws-2")
	stdout, _, err := run("test", "--json", "config", "get", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	var value configValueJSON
	decodeOnly(t, "get", stdout, &value)
	if value != (configValueJSON{"default", "workspace", "ws-2"}) {
		t.Errorf("get --json = %+v", value)
	}
	if _, _, err := run("test", "config", "unset", "workspace"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, "workspace"); got != "" {
		t.Errorf("workspace after unset = %q", got)
	}
	if _, _, err := run("test", "config", "unset", "host"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, "host"); got != config.DefaultHost+"\n" {
		t.Errorf("host after unset = %q", got)
	}

	// set makes a profile that --profile names, as set-host does, but
	// only under a name a new profile may have.
	set(t, "--profile", "ci", "host", "ci.firmfact.example")
	if got := get(t, "--profile", "ci", "host"); got != "https://ci.firmfact.example\n" {
		t.Errorf("ci host = %q", got)
	}

	cases := []struct {
		args []string
		code int
	}{
		{[]string{"config", "set", "host", "ftp://firmfact.example"}, ExitUsage},
		{[]string{"config", "set", "host", "http://firmfact.example"}, ExitUsage},
		{[]string{"config", "set", "colour", "red"}, ExitUsage},
		{[]string{"config", "get", "colour"}, ExitUsage},
		{[]string{"config", "set", "workspace", " "}, ExitUsage},
		{[]string{"--profile", "no good", "config", "set", "host", "x.example"}, ExitUsage},
		{[]string{"--profile", "nope", "config", "get", "host"}, ExitNotFound},
		{[]string{"--profile", "nope", "config", "unset", "host"}, ExitNotFound},
	}
	for _, c := range cases {
		_, _, err := run("test", c.args...)
		if code, _ := Classify(err); code != c.code {
			t.Errorf("%v: exit %d (%v), want %d", c.args, code, err, c.code)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["nope"]; ok {
		t.Error("a read of a profile that does not exist made it")
	}
}

// doctor checks every profile's host as a command would before sending a
// token there.
func TestDoctorChecksProfileHosts(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	srv := meServer(t)
	signedInWithTools(t, srv.URL, listVendors)
	saveProfiles(t, "default", map[string]*config.Profile{
		"default": {Host: srv.URL},
		"old":     {Host: "http://firmfact.example"},
	})

	stdout, _, err := run("test", "--json", "doctor")
	if err == nil {
		t.Fatal("doctor passed with a profile whose host is refused")
	}
	var report doctorReport
	decodeOnly(t, "doctor", stdout, &report)
	for _, c := range report.Checks {
		if c.Name == "config" {
			if c.OK || !strings.Contains(c.Detail, `profile "old": `) || !strings.Contains(c.Detail, "config set host") {
				t.Errorf("config check = %+v", c)
			}
			return
		}
	}
	t.Errorf("no config check in %+v", report.Checks)
}

func TestNearNames(t *testing.T) {
	names := []string{"ci", "default", "staging", "Production"}
	cases := map[string]string{
		"stagin":  `"staging"`,
		"STAGING": `"staging"`,
		"prod":    `"Production"`,
		"cj":      `"ci"`,
		"zzzzzz":  "",
	}
	for name, want := range cases {
		if got := strings.Join(nearNames(name, names), ","); got != want {
			t.Errorf("nearNames(%q) = %q, want %q", name, got, want)
		}
	}
	if d := editDistance("kitten", "sitting"); d != 3 {
		t.Errorf("editDistance = %d", d)
	}
}
