package cmd

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/update"
)

// silentHost takes connections (the kernel does, without Accept) and never
// answers them.
func silentHost(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return "http://" + l.Addr().String()
}

// closedHost is an address nothing listens on.
func closedHost(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

// fakeGitHub answers the latest-release question at once, so a test waits
// only for the host it is about.
func fakeGitHub(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectToTag(w, r, "v0.1.0")
	}))
	t.Cleanup(srv.Close)
	prev := update.LatestReleaseURL
	update.LatestReleaseURL = srv.URL
	t.Cleanup(func() { update.LatestReleaseURL = prev })
}

// redirectToTag answers as GitHub's latest-release page does: with a
// redirect to the release's tag.
func redirectToTag(w http.ResponseWriter, r *http.Request, tag string) {
	http.Redirect(w, r, "/firmfact/cli/releases/tag/"+tag, http.StatusFound)
}

// untilChecked waits, for up to ten seconds, until the version check has
// recorded for the host r was sent to what done looks for. A fake server's
// handler calls it to answer only once the check beside the command has,
// as a slower server would, so a test need not race the two.
func untilChecked(cacheDir string, r *http.Request, done func(update.Status) bool) {
	host := "http://" + r.Host
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if s, _ := update.Cached(host, cacheDir, false); done(s) {
			return
		}
	}
}

// doctor once hung on a stalled socket. Against a host that never answers
// it now gives up at --timeout, asks that host only once (the sign-in check
// would only wait again), and says what happened without pointing at
// itself.
func TestDoctorGivesUpOnASilentHost(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	host := silentHost(t)
	storedToken(t, host, time.Hour)

	start := time.Now()
	stdout, _, err := run("dev", "--host", host, "--timeout", "300ms", "doctor")
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "check(s) failed") {
		t.Fatalf("want failed checks, got %v", err)
	}
	if took > 2*time.Second {
		t.Errorf("doctor took %s with --timeout 300ms", took)
	}
	addr := strings.TrimPrefix(host, "http://")
	for _, want := range []string{
		"connection     " + addr + " did not answer within 300ms",
		"token store    " + filepath.Join(os.Getenv("FIRMFACT_CONFIG_DIR"), "credentials.json") + " (FIRMFACT_TOKEN_STORE=file)",
		"sign-in        not checked, as the connection failed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "doctor`") {
		t.Errorf("doctor must not tell the user to run doctor:\n%s", stdout)
	}
}

// Any other command ends a failure to reach the host with a pointer to
// doctor, in one line without Go's own wording.
func TestNetworkErrorsPointAtDoctor(t *testing.T) {
	isolate(t)
	closed := closedHost(t)
	silent := silentHost(t)
	storedToken(t, closed, time.Hour)
	storedToken(t, silent, time.Hour)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--host", closed, "whoami"},
			strings.TrimPrefix(closed, "http://") + " refused the connection; run `firmfact doctor`"},
		{[]string{"--host", silent, "--timeout", "200ms", "whoami"},
			strings.TrimPrefix(silent, "http://") + " did not answer within 200ms; allow longer with --timeout, or run `firmfact doctor`"},
	}
	for _, c := range cases {
		if _, _, err := run("dev", c.args...); err == nil || err.Error() != c.want {
			t.Errorf("%v: got %v, want %q", c.args, err, c.want)
		}
	}

	// A name that does not resolve (.invalid never does, RFC 6761), with
	// a token from the environment so the request is made.
	t.Setenv("FIRMFACT_TOKEN", "at")
	t.Setenv("FIRMFACT_HOST", "https://firmfact.invalid")
	_, _, err := run("dev", "--host", "https://firmfact.invalid", "whoami")
	if err == nil || !strings.HasPrefix(err.Error(), "cannot find firmfact.invalid: ") || !strings.HasSuffix(err.Error(), "; run `firmfact doctor`") {
		t.Errorf("got %v, want a DNS failure pointing at doctor", err)
	}
	if err != nil && (strings.Contains(err.Error(), "dial tcp") || strings.Contains(err.Error(), "Get \"")) {
		t.Errorf("Go's own wording leaked: %q", err)
	}
}

func TestParseTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"90s": 90 * time.Second, "2m": 2 * time.Minute, "90": 90 * time.Second, "1.5": 1500 * time.Millisecond, " 500ms ": 500 * time.Millisecond,
	} {
		if got, err := parseTimeout(in); err != nil || got != want {
			t.Errorf("parseTimeout(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0s", "-5s", "soon", "10 s"} {
		if got, err := parseTimeout(in); err == nil {
			t.Errorf("parseTimeout(%q) = %s, want an error", in, got)
		}
	}
}

// --timeout beats FIRMFACT_TIMEOUT, which beats the defaults; a bad
// FIRMFACT_TIMEOUT stops the commands that make requests, and says which
// setting is wrong.
func TestTimeoutSources(t *testing.T) {
	isolate(t)
	app := &App{}
	t.Setenv("FIRMFACT_TIMEOUT", "")
	if d, err := app.RequestTimeout(); d != 0 || err != nil {
		t.Errorf("default: %s, %v", d, err)
	}
	t.Setenv("FIRMFACT_TIMEOUT", "45")
	if d, err := app.RequestTimeout(); d != 45*time.Second || err != nil {
		t.Errorf("environment: %s, %v", d, err)
	}
	app.Timeout = 5 * time.Second
	if d, err := app.RequestTimeout(); d != 5*time.Second || err != nil {
		t.Errorf("flag over environment: %s, %v", d, err)
	}

	t.Setenv("FIRMFACT_TIMEOUT", "soon")
	_, _, err := run("dev", "--host", closedHost(t), "whoami")
	if err == nil || !strings.HasPrefix(err.Error(), "FIRMFACT_TIMEOUT: ") {
		t.Errorf("got %v, want the setting named", err)
	}
	if _, _, err := run("dev", "--timeout", "0", "whoami"); err == nil || !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("--timeout 0: got %v", err)
	}
}

// useProxy stands in for the proxy the environment names: net/http reads
// the variables once per process, so setting them in a test would change
// nothing, or every test after it.
func useProxy(t *testing.T, proxy func(string) (*url.URL, error)) {
	t.Helper()
	prev := proxyFor
	proxyFor = proxy
	t.Cleanup(func() { proxyFor = prev })
}

// doctor's proxy line names the proxy in use, with its password masked,
// and the variable it came from; or says why one that is set is left out,
// as net/http leaves out without a word an address it cannot read.
func TestProxyCheck(t *testing.T) {
	cases := []struct {
		host  string
		env   map[string]string
		proxy string // what net/http chose, "" for none
		err   error
		ok    bool
		want  string
	}{
		{"https://firmfact.com", nil, "", nil, true, "none; connecting directly"},
		{"https://firmfact.com", map[string]string{"HTTPS_PROXY": "http://jan:secret@proxy.example:3128"}, "http://jan:secret@proxy.example:3128", nil, true, "http://jan:xxxxx@proxy.example:3128 (from HTTPS_PROXY)"},
		{"https://firmfact.com", map[string]string{"https_proxy": "proxy.example:3128"}, "http://proxy.example:3128", nil, true, "http://proxy.example:3128 (from https_proxy)"},
		{"http://firmfact.example", map[string]string{"HTTP_PROXY": "http://proxy.example:3128"}, "http://proxy.example:3128", nil, true, "http://proxy.example:3128 (from HTTP_PROXY)"},
		// HTTP_PROXY is for plain http hosts only.
		{"https://firmfact.com", map[string]string{"HTTP_PROXY": "http://proxy.example:3128"}, "", nil, true, "none; connecting directly"},
		{"https://firmfact.com", map[string]string{"HTTPS_PROXY": "http://proxy.example:3128", "NO_PROXY": "firmfact.com"}, "", nil, true, "none; NO_PROXY leaves out firmfact.com"},
		{"http://localhost:5000", map[string]string{"HTTP_PROXY": "http://proxy.example:3128"}, "", nil, true, "none; HTTP_PROXY is not used for this machine"},
		{"http://[::1]:5000", map[string]string{"http_proxy": "http://proxy.example:3128"}, "", nil, true, "none; http_proxy is not used for this machine"},
		{"https://firmfact.com", map[string]string{"HTTPS_PROXY": "::bad"}, "", nil, false, "HTTPS_PROXY is set but is not a proxy address, so the CLI connects directly"},
		{"http://firmfact.example", map[string]string{"HTTP_PROXY": "http://proxy.example:3128"}, "", errors.New("refusing to use HTTP_PROXY with a CGI request"), false, "HTTP_PROXY is not usable: refusing to use HTTP_PROXY with a CGI request"},
	}
	for _, c := range cases {
		useProxy(t, func(target string) (*url.URL, error) {
			if target != c.host {
				t.Errorf("asked about %q, want %q", target, c.host)
			}
			if c.proxy == "" {
				return nil, c.err
			}
			return url.Parse(c.proxy)
		})
		ok, detail := proxyCheck(c.host, func(name string) string { return c.env[name] })
		if ok != c.ok || detail != c.want {
			t.Errorf("%s with %v: %v %q, want %v %q", c.host, c.env, ok, detail, c.ok, c.want)
		}
	}
}

// readsAsProxy takes what net/http takes: a URL with a scheme it knows, or
// one that parses with http:// in front.
func TestReadsAsProxy(t *testing.T) {
	for value, want := range map[string]bool{
		"http://proxy.example:3128":   true,
		"https://proxy.example":       true,
		"socks5://proxy.example:1080": true,
		"proxy.example:3128":          true,
		"10.0.0.1:8080":               true,
		"::bad":                       false,
		"proxy.example:port":          false,
	} {
		if got := readsAsProxy(value); got != want {
			t.Errorf("readsAsProxy(%q) = %v, want %v", value, got, want)
		}
	}
}

// doctor shows the proxy before the connection it carries.
func TestDoctorShowsTheProxy(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	srv := meServer(t)
	useProxy(t, func(string) (*url.URL, error) { return url.Parse("http://jan:secret@proxy.example:3128") })

	stdout, _, _ := run("test", "--host", srv.URL, "--json", "doctor")
	var report doctorReport
	decodeOnly(t, "doctor", stdout, &report)
	var names []string
	for _, c := range report.Checks {
		names = append(names, c.Name)
		if c.Name == "proxy" && (!c.OK || c.Detail != "http://jan:xxxxx@proxy.example:3128 (from HTTP_PROXY)") {
			t.Errorf("proxy check = %+v", c)
		}
	}
	if got := strings.Join(names, ","); !strings.HasPrefix(got, "version,proxy,connection,") {
		t.Errorf("checks = %s, want the proxy before the connection", got)
	}

	stdout, _, _ = run("test", "--host", srv.URL, "doctor")
	if !strings.Contains(stdout, "  proxy          http://jan:xxxxx@proxy.example:3128 (from HTTP_PROXY)\n") || strings.Contains(stdout, "secret") {
		t.Errorf("doctor:\n%s", stdout)
	}
}

// FIRMFACT_NO_UPDATE_CHECK keeps doctor off GitHub too, so the variable
// keeps the CLI to the firmfact host, as the README promises.
func TestDoctorLeavesGitHubOutWhenTheCheckIsOff(t *testing.T) {
	isolate(t)
	var asked atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		redirectToTag(w, r, "v0.1.0")
	}))
	t.Cleanup(gh.Close)
	prev := update.LatestReleaseURL
	update.LatestReleaseURL = gh.URL
	t.Cleanup(func() { update.LatestReleaseURL = prev })
	prevVersion := api.Version
	t.Cleanup(func() { api.Version = prevVersion })
	srv := meServer(t)

	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "1")
	stdout, _, _ := run("0.0.9", "--host", srv.URL, "--json", "doctor")
	if c := versionCheck(t, stdout); !c.OK || c.Detail != "0.0.9 (FIRMFACT_NO_UPDATE_CHECK is set, so GitHub was not asked for a newer release)" {
		t.Errorf("version check = %+v", c)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("GitHub was asked %d time(s) with the check off", n)
	}

	// Blank, or 0 or false as for FIRMFACT_DEBUG, leaves it on.
	for i, value := range []string{"", "0", "false"} {
		t.Setenv("FIRMFACT_NO_UPDATE_CHECK", value)
		stdout, _, _ = run("0.0.9", "--host", srv.URL, "--json", "doctor")
		if c := versionCheck(t, stdout); !strings.Contains(c.Detail, "0.1.0 is available") || asked.Load() != int32(i+1) {
			t.Errorf("FIRMFACT_NO_UPDATE_CHECK=%q: version check = %+v, GitHub asked %d time(s)", value, c, asked.Load())
		}
	}
}
