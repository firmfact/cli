package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/auth"
	"github.com/firmfact/cli/internal/config"
)

// approvingBrowser follows the sign-in link's redirect at once, as a user
// clicking Approve would.
func approvingBrowser(authURL string) error {
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	q := u.Query()
	cb := url.Values{"code": {"the-code"}, "state": {q.Get("state")}}
	go func() {
		if resp, err := http.Get(q.Get("redirect_uri") + "?" + cb.Encode()); err == nil {
			resp.Body.Close()
		}
	}()
	return nil
}

func useBrowser(t *testing.T, open func(string) error) {
	prev := auth.LaunchBrowser
	auth.LaunchBrowser = open
	t.Cleanup(func() { auth.LaunchBrowser = prev })
}

// login stores the token and the host, adopts the token's default workspace
// and greets the user.
func TestLogin(t *testing.T) {
	isolate(t)
	useBrowser(t, approvingBrowser)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			io.WriteString(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},
				"workspaces":[{"id":"prod-1","name":"Bank BV"},{"id":"demo-1","name":"Demo","default":true,"demo":true}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound) // no MCP: the tool refresh is optional
		}
	}))
	defer srv.Close()

	stdout, _, err := run("test", "--host", srv.URL, "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(stdout, "You're in: jan@yourfirm.example on "+srv.URL+".") {
		t.Errorf("stdout = %q", stdout)
	}
	if tok, err := config.LoadToken(srv.URL); err != nil || tok == nil || tok.AccessToken != "at" {
		t.Fatalf("stored token = %+v, %v", tok, err)
	}
	cfg, _ := config.Load()
	if p := cfg.Profile(""); p.Host != srv.URL || p.Workspace != "demo-1" {
		t.Errorf("profile = %+v", p)
	}
}

// A host from FIRMFACT_HOST overrides the profile only while it is set: a
// login there keeps the sign-in, by host, but leaves the profile's host
// and workspace as they were, and says so. A host named with --host is
// kept, as TestLogin shows.
func TestLoginOnFirmfactHostLeavesTheProfile(t *testing.T) {
	isolate(t)
	useBrowser(t, approvingBrowser)
	srv := meServer(t)
	cfg := &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
		"default": {Host: config.DefaultHost, Workspace: "ws-production"},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRMFACT_HOST", srv.URL)

	stdout, stderr, err := run("test", "--json", "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	var result loginResult
	decodeOnly(t, "login", stdout, &result)
	if result.Host != srv.URL || result.Workspace != "demo-1" {
		t.Errorf("result = %+v; want the sign-in's own default workspace", result)
	}
	if want := "note: FIRMFACT_HOST is set, so profile default still points at " + config.DefaultHost + "; commands use " + srv.URL + " until it is unset."; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q", stderr)
	}
	if tok, err := config.LoadToken(srv.URL); err != nil || tok == nil || tok.AccessToken != "at" {
		t.Fatalf("stored token = %+v, %v", tok, err)
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := saved.Profile("default"); p.Host != config.DefaultHost || p.Workspace != "ws-production" || p.InsecureHTTP {
		t.Errorf("profile = %+v; want it as it was", p)
	}
}

// workspaces use follows the rule of a sign-in. On the host FIRMFACT_HOST
// names, which the profile does not keep, it refuses before it asks the
// server, and the profile is left as it was: saved there, the workspace
// would be ignored while the variable is set and sent to the wrong host
// once it is not. With --host it points the profile at that host, as
// `login --host` does, and says so.
func TestWorkspacesUseOnAnotherHost(t *testing.T) {
	isolate(t)
	srv := meServer(t)
	storedToken(t, srv.URL, time.Hour)
	saveProfiles(t, "default", map[string]*config.Profile{
		"default": {Host: config.DefaultHost, Workspace: "ws-production"},
	})
	path, _ := config.Path()
	before, _ := os.ReadFile(path)

	t.Setenv("FIRMFACT_HOST", srv.URL)
	stdout, _, err := run("test", "workspaces", "use", "Demo")
	want := "FIRMFACT_HOST is set, so this runs on " + srv.URL + ", while profile default stays on " + config.DefaultHost +
		" and its default workspace belongs there; name the workspace with --workspace or FIRMFACT_WORKSPACE instead, " +
		"or point the profile at " + srv.URL + " too with `firmfact --host " + srv.URL + " workspaces use Demo`"
	if code, _ := Classify(err); code != ExitUsage || err.Error() != want {
		t.Errorf("under FIRMFACT_HOST: exit %d, %v\nwant %s", code, err, want)
	}
	if stdout != "" {
		t.Errorf("stdout = %q", stdout)
	}
	assertFileHolds(t, path, string(before))
	// A name the hint cannot print as it is gives way to a placeholder.
	if _, _, err := run("test", "workspaces", "use", "--", "--host=https://evil.example"); err == nil ||
		!strings.HasSuffix(err.Error(), "workspaces use <workspace>`") {
		t.Errorf("hint for a name that reads as a flag: %v", err)
	}

	// On the profile's own host, named by the variable, nothing stands in
	// the way.
	saveProfiles(t, "default", map[string]*config.Profile{"default": {Host: srv.URL}})
	if _, _, err := run("test", "workspaces", "use", "Bank BV"); err != nil {
		t.Fatalf("on the profile's own host: %v", err)
	}
	if cfg, _ := config.Load(); cfg.Profile("default").Workspace != "prod-1" {
		t.Errorf("profile = %+v", cfg.Profile("default"))
	}

	// --host moves the profile to its host, workspace and all.
	t.Setenv("FIRMFACT_HOST", "")
	saveProfiles(t, "default", map[string]*config.Profile{
		"default": {Host: config.DefaultHost, Workspace: "ws-production"},
	})
	stdout, _, err = run("test", "--host", srv.URL, "workspaces", "use", "Demo")
	if err != nil {
		t.Fatalf("--host: %v", err)
	}
	if want := "Profile default now uses " + srv.URL + ".\nDefault workspace is now Demo.\n"; !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout = %q, want it to start %q", stdout, want)
	}
	if cfg, _ := config.Load(); cfg.Profile("default").Host != srv.URL || cfg.Profile("default").Workspace != "demo-1" {
		t.Errorf("profile after --host = %+v", cfg.Profile("default"))
	}
	stdout, _, err = run("test", "config", "show")
	if err != nil || !strings.Contains(stdout, "workspace: demo-1\n") {
		t.Errorf("config show = %q, %v", stdout, err)
	}
}

func TestLoginTimesOut(t *testing.T) {
	isolate(t)
	useBrowser(t, func(string) error { return nil }) // opened, never approved
	prev := loginTimeout
	loginTimeout = 20 * time.Millisecond
	defer func() { loginTimeout = prev }()

	_, _, err := run("test", "--host", "http://127.0.0.1:1", "login")
	if err == nil || err.Error() != "login timed out" {
		t.Fatalf("want a timeout, got %v", err)
	}
}

// whoami, workspaces list and workspaces use print names from the server;
// escape sequences in them are shown, not obeyed.
func TestSessionCommandsEscapeServerNames(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan\u001b]0;pwned\u0007","email":"jan@yourfirm.example\u009b2K"},
			"workspaces":[{"id":"prod-1","name":"Bank\nBV\u001b]52;c;cm0gLXJmIH4=\u0007","mcp_available":true}]}}`)
	}))
	defer srv.Close()
	if err := config.SaveToken(srv.URL, &config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"whoami"}, {"workspaces", "list"}, {"workspaces", "use", "prod-1"}} {
		stdout, stderr, err := run("test", append([]string{"--host", srv.URL}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		assertNoTerminalControls(t, strings.Join(args, " ")+" stdout", stdout)
		assertNoTerminalControls(t, strings.Join(args, " ")+" stderr", stderr)
		switch args[0] {
		case "whoami":
			if !strings.HasPrefix(stdout, `Jan\u001b]0;pwned\u0007 <jan@yourfirm.example\u009b2K>`) {
				t.Errorf("whoami = %q", stdout)
			}
		default:
			if !strings.Contains(stdout, `Bank BV\u001b]52;`) {
				t.Errorf("%v = %q", args, stdout)
			}
		}
	}
}

// recordingServer answers /api/v1/cli/me and records every Authorization
// header it is sent.
func recordingServer(t *testing.T) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// FIRMFACT_TOKEN is for firmfact.com (or FIRMFACT_HOST): --host pointing
// elsewhere gets a clear refusal and not a single request.
func TestEnvTokenIsNotSentToAnotherHost(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_TOKEN", "env-secret")
	srv, seen := recordingServer(t)

	_, _, err := run("test", "--host", srv.URL, "whoami")
	if err == nil || !strings.Contains(err.Error(), "FIRMFACT_TOKEN is only sent to the default host https://firmfact.com") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("the server was sent %q", *seen)
	}

	// Naming the host in FIRMFACT_HOST is how a script uses the token there.
	t.Setenv("FIRMFACT_HOST", srv.URL)
	if _, _, err := run("test", "--host", srv.URL, "whoami"); err != nil {
		t.Fatalf("with FIRMFACT_HOST: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != "Bearer env-secret" {
		t.Errorf("Authorization = %q", *seen)
	}
}

// The reproduction behind strict hosts: a user name in front of the real
// host once sent the token in cleartext to 127.0.0.1.
func TestHostWithUserNameIsRefused(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_TOKEN", "env-secret")
	srv, seen := recordingServer(t)
	host := "http://firmfact.com@" + strings.TrimPrefix(srv.URL, "http://")
	if err := config.SaveToken(srv.URL, &config.Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"--host", host, "whoami"}, {"--insecure-http", "--host", host, "whoami"}} {
		_, _, err := run("test", args...)
		if err == nil || !strings.Contains(err.Error(), "user name or password") {
			t.Errorf("%v: want a refusal, got %v", args, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("the server was sent %q", *seen)
	}
}

// Plain http to another machine needs --insecure-http; set-host keeps that
// permission in the profile, so later commands need not repeat it.
func TestInsecureHTTPIsExplicit(t *testing.T) {
	isolate(t)
	_, _, err := run("test", "--host", "http://firmfact.example", "config", "show")
	if err == nil || !strings.Contains(err.Error(), "--insecure-http") {
		t.Fatalf("want a plain-http refusal, got %v", err)
	}
	if _, _, err := run("test", "config", "set-host", "http://firmfact.example"); err == nil {
		t.Fatal("set-host must refuse plain http without --insecure-http")
	}

	_, stderr, err := run("test", "--insecure-http", "config", "set-host", "http://firmfact.example")
	if err != nil {
		t.Fatalf("set-host --insecure-http: %v", err)
	}
	if !strings.Contains(stderr, "unencrypted") {
		t.Errorf("stderr = %q", stderr)
	}
	stdout, _, err := run("test", "config", "show")
	if err != nil || !strings.Contains(stdout, "host:      http://firmfact.example\n") {
		t.Fatalf("config show = %q, %v", stdout, err)
	}

	// Back to https drops the permission.
	if _, _, err := run("test", "config", "set-host", "staging.firmfact.example"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if p := cfg.Profile(""); p.Host != "https://staging.firmfact.example" || p.InsecureHTTP {
		t.Errorf("profile = %+v", p)
	}
}

// An invalid stored host stops the commands that would use it, with the
// fix; it is never replaced by the default host. set-host still runs.
func TestInvalidStoredHostFailsClearly(t *testing.T) {
	isolate(t)
	cfg := &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
		"default": {Host: "https://firmfact.com@evil.example", Workspace: "ws-1"},
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"whoami"}, {"config", "show"}, {"tools", "list"}} {
		_, _, err := run("test", args...)
		if err == nil || !strings.Contains(err.Error(), `the host stored in profile "default" is not usable`) ||
			!strings.Contains(err.Error(), "config set-host") {
			t.Errorf("%v: got %v", args, err)
		}
	}

	stdout, _, err := run("test", "config", "set-host", "localhost:5000")
	if err != nil {
		t.Fatalf("set-host must repair a bad host: %v", err)
	}
	if !strings.Contains(stdout, "Profile now uses http://localhost:5000.") {
		t.Errorf("stdout = %q", stdout)
	}
}

// revokeServer fakes /oauth/revoke, answering with status and recording
// each request's form and Authorization header.
type revokeServer struct {
	mu       sync.Mutex
	status   int
	requests []url.Values
	auth     []string
}

func (f *revokeServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/revoke" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		f.requests = append(f.requests, r.PostForm)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *revokeServer) sent() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.requests...)
}

// storedToken is what login leaves behind: a live access token and the
// refresh token that renews it.
func storedToken(t *testing.T, host string, expiresIn time.Duration) {
	t.Helper()
	tok := &config.Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(expiresIn), ClientID: "firmfact-cli"}
	if err := config.SaveToken(host, tok); err != nil {
		t.Fatal(err)
	}
}

func assertTokenGone(t *testing.T, host string) {
	t.Helper()
	if tok, err := config.LoadToken(host); err != nil || tok != nil {
		t.Errorf("stored token = %+v, %v; want it removed", tok, err)
	}
}

// logout revokes the refresh token first, since that is what keeps the
// session alive, then the access token while it is still live; the stored
// copy goes.
func TestLogoutRevokesTheRefreshTokenFirst(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)
	storedToken(t, srv.URL, time.Hour)

	stdout, stderr, err := run("test", "--host", srv.URL, "logout")
	if err != nil {
		t.Fatalf("logout: %v (stderr %q)", err, stderr)
	}
	if stdout != "Signed out of "+srv.URL+".\n" {
		t.Errorf("stdout = %q", stdout)
	}
	sent := f.sent()
	if len(sent) != 2 {
		t.Fatalf("revoke requests = %v, want two", sent)
	}
	want := []url.Values{
		{"token": {"rt"}, "token_type_hint": {"refresh_token"}, "client_id": {"firmfact-cli"}},
		{"token": {"at"}, "token_type_hint": {"access_token"}, "client_id": {"firmfact-cli"}},
	}
	for i := range want {
		if sent[i].Encode() != want[i].Encode() {
			t.Errorf("request %d = %v, want %v", i, sent[i], want[i])
		}
	}
	for _, a := range f.auth {
		if a != "" {
			t.Errorf("revoke sent Authorization %q; a public client names itself with client_id", a)
		}
	}
	assertTokenGone(t, srv.URL)
}

// An expired access token is not revoked (the server would not act on it
// anyway); the refresh token still is.
func TestLogoutWithAnExpiredAccessToken(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)
	storedToken(t, srv.URL, -time.Minute)

	if _, _, err := run("test", "--host", srv.URL, "logout"); err != nil {
		t.Fatalf("logout: %v", err)
	}
	sent := f.sent()
	if len(sent) != 1 || sent[0].Get("token") != "rt" || sent[0].Get("token_type_hint") != "refresh_token" {
		t.Fatalf("revoke requests = %v, want only the refresh token", sent)
	}
	assertTokenGone(t, srv.URL)
}

// When the server refuses or cannot be reached, the local copy still goes,
// but logout says the session lives on and exits non-zero.
func TestLogoutReportsAFailedRevoke(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusInternalServerError}
	failing := f.start(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	for name, host := range map[string]string{"500": failing.URL, "unreachable": down.URL} {
		storedToken(t, host, time.Hour)
		stdout, stderr, err := run("test", "--host", host, "logout")
		if err == nil {
			t.Fatalf("%s: logout succeeded; want an error", name)
		}
		if !strings.Contains(stderr, "removed from this machine; the session is still valid on the server") {
			t.Errorf("%s: stderr = %q", name, stderr)
		}
		if !strings.Contains(err.Error(), "could not revoke the session on "+host) {
			t.Errorf("%s: error = %v", name, err)
		}
		// The exit status is the cause's: both are worth a retry.
		if code, _ := Classify(err); code != ExitUnavailable {
			t.Errorf("%s: exit %d, want %d", name, code, ExitUnavailable)
		}
		if strings.Contains(stdout, "Signed out") {
			t.Errorf("%s: stdout = %q", name, stdout)
		}
		assertTokenGone(t, host)
	}
	if sent := f.sent(); len(sent) != 1 || sent[0].Get("token_type_hint") != "refresh_token" {
		t.Errorf("revoke requests = %v; a failed refresh-token revoke should stop there", sent)
	}
}

// Nothing stored: nothing to revoke, and no error.
func TestLogoutWhenNotSignedIn(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)

	stdout, _, err := run("test", "--host", srv.URL, "logout")
	if err != nil || stdout != "Not signed in to "+srv.URL+".\n" {
		t.Fatalf("logout = %q, %v", stdout, err)
	}
	if sent := f.sent(); len(sent) != 0 {
		t.Errorf("revoke requests = %v", sent)
	}
}

// FIRMFACT_TOKEN was never stored, so logout refuses rather than revoking
// it or quietly signing out of the stored session instead.
func TestLogoutRefusesAnEnvironmentToken(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)
	storedToken(t, srv.URL, time.Hour)
	t.Setenv("FIRMFACT_TOKEN", "env-secret")
	t.Setenv("FIRMFACT_HOST", srv.URL)

	_, _, err := run("test", "--host", srv.URL, "logout")
	if err == nil || !strings.Contains(err.Error(), "a token from the environment is not stored") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if sent := f.sent(); len(sent) != 0 {
		t.Errorf("revoke requests = %v", sent)
	}
	t.Setenv("FIRMFACT_TOKEN", "")
	if tok, err := config.LoadToken(srv.URL); err != nil || tok == nil || tok.RefreshToken != "rt" {
		t.Errorf("the stored sign-in must be left alone: %+v, %v", tok, err)
	}
}

// workspaces use exists to change the config, so a config it cannot save
// fails the command, rather than a warning under "Default workspace is
// now ..." and exit status 0.
func TestWorkspacesUseFailsWhenTheConfigCannotBeSaved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory does not stop writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	isolate(t)
	srv := meServer(t)
	storedToken(t, srv.URL, time.Hour)
	if _, _, err := run("test", "--host", srv.URL, "whoami"); err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("FIRMFACT_CONFIG_DIR")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	stdout, _, err := run("test", "--host", srv.URL, "workspaces", "use", "Bank BV")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("got %v, want the failed save", err)
	}
	if strings.Contains(stdout, "Default workspace is now") {
		t.Errorf("stdout = %q", stdout)
	}
}
