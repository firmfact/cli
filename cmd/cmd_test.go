package cmd

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/update"
)

// isolate points config, cache and tokens at scratch space, so tests never
// see or touch the user's own sign-in.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// The CLI creates its config directory itself, as it would for a user,
	// so the directory's mode does not depend on the umask.
	t.Setenv("FIRMFACT_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("FIRMFACT_CACHE_DIR", t.TempDir())
	t.Setenv("FIRMFACT_TOKEN_STORE", "file")
	t.Setenv("FIRMFACT_TOKEN", "")
	t.Setenv("FIRMFACT_HOST", "")
	t.Setenv("FIRMFACT_PROFILE", "")
	t.Setenv("FIRMFACT_WORKSPACE", "")
	t.Setenv("FIRMFACT_SIGNUP_CODE", "")
	t.Setenv("FIRMFACT_DEBUG", "")
}

// run is one CLI invocation with its output captured.
func run(version string, args ...string) (stdout, stderr string, err error) {
	return runWithInput("", version, args...)
}

// runWithInput is run with input piped to standard input.
func runWithInput(input, version string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	streams := IOStreams{In: strings.NewReader(input), Out: &out, Err: &errOut}
	err = NewRootCommand(Build{Version: version}, args, streams).Execute()
	return out.String(), errOut.String(), err
}

func TestCommandPath(t *testing.T) {
	cases := map[string][2]string{
		"list_vendors":        {"vendors", "list"},
		"list_cost_centers":   {"cost-centers", "list"},
		"analyze_cost_trends": {"analyze", "cost-trends"},
		"chat_with_workspace": {"", "chat-with-workspace"},
	}
	for tool, want := range cases {
		group, verb := commandPath(tool)
		if group != want[0] || verb != want[1] {
			t.Errorf("commandPath(%q) = %q %q, want %q %q", tool, group, verb, want[0], want[1])
		}
	}
}

func TestOrgFromEmailMatchesServerRule(t *testing.T) {
	if got := orgFromEmail("jan@acme-bank.example"); got != "Acme Bank" {
		t.Errorf("got %q", got)
	}
	if got := orgFromEmail("no-at-sign"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestFindRowsPrefersData(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"meta":{"page":1},"data":[{"name":"A","id":"1","cost":2,"nested":{"x":1}}]}`), &v)
	rows := findRows(v)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	cols := columnsFor(rows)
	if strings.Join(cols, ",") != "name,id,cost" {
		t.Errorf("columns = %v", cols)
	}
}

func TestHostFromArgs(t *testing.T) {
	app := &App{Config: &config.Config{CurrentProfile: "default", Profiles: map[string]*config.Profile{
		"other":   {Host: "https://firmfact.example"},
		"plain":   {Host: "http://firmfact.example", InsecureHTTP: true},
		"broken":  {Host: "http://firmfact.example"},
		"no-host": {Host: ""},
	}}}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"vendors", "list", "--host", "localhost:5000"}, "http://localhost:5000"},
		{[]string{"--host=https://Firmfact.Example/"}, "https://firmfact.example"},
		{[]string{"--profile", "other", "whoami"}, "https://firmfact.example"},
		{[]string{"--profile=plain", "whoami"}, "http://firmfact.example"},
		{[]string{"--insecure-http", "--host", "http://firmfact.example"}, "http://firmfact.example"},
		{[]string{"--host", "https://a.example", "--host", "https://b.example"}, "https://b.example"},
		{nil, config.DefaultHost},
	}
	for _, c := range cases {
		if got, err := hostFromArgs(app, c.args); err != nil || got != c.want {
			t.Errorf("%v: got %q, %v; want %q", c.args, got, err, c.want)
		}
	}
	for _, args := range [][]string{
		{"--host", "http://firmfact.com@127.0.0.1:5000"},
		{"--host", "http://firmfact.example"},
		{"--insecure-http=false", "--host", "http://firmfact.example"},
		{"--profile", "broken"},
		{"--profile", "no-host"},
		{"--profile", "nope"},
	} {
		if got, err := hostFromArgs(app, args); err == nil {
			t.Errorf("%v: got %q, want an error", args, got)
		}
	}
	// A profile that does not exist has no host, not the default one.
	_, err := hostFromArgs(app, []string{"--profile=othr", "whoami"})
	if code, _ := Classify(err); code != ExitNotFound || err.Error() != `--profile: no profile "othr"; did you mean "other"?` {
		t.Errorf("--profile othr: exit %d (%v)", code, err)
	}
}

// signupServer fakes the signup endpoints; progress answers the Demo setup
// polls (nil means the test never waits for setup).
type signupServer struct {
	signupBody, confirmBody map[string]map[string]any
	progress                func(poll int) string
	// pollStatus, when set, is the HTTP status of each poll; 0 is 200.
	pollStatus func(poll int) int
	polls      atomic.Int32
	// requests counts the requests to each signup endpoint.
	options, signups, confirms atomic.Int32
	// refusal, when set, is the 422 answer to the signup request, and
	// confirmRefusal the one to the code.
	refusal, confirmRefusal string
	// created and confirmed, when set, are the answers to the signup
	// request and to the code in place of the usual ones: the code sent,
	// and a sign-in for the Demo workspace.
	created, confirmed string
	// wrongCodes is how many codes are turned away as invalid before one
	// is taken.
	wrongCodes int32
}

func (f *signupServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/signup/options":
			f.options.Add(1)
			io.WriteString(w, `{"data":{"organization_types":[{"value":"financial_services","label":"Financial services"}],"default_currency":"EUR","terms":[]}}`)
		case "/api/v1/signup":
			f.signups.Add(1)
			_ = json.NewDecoder(r.Body).Decode(&f.signupBody)
			if f.refusal != "" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				io.WriteString(w, f.refusal)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, cmp.Or(f.created, `{"success":true,"status":"code_sent"}`))
		case "/api/v1/signup/confirm":
			n := f.confirms.Add(1)
			_ = json.NewDecoder(r.Body).Decode(&f.confirmBody)
			if n <= f.wrongCodes {
				w.WriteHeader(http.StatusUnprocessableEntity)
				io.WriteString(w, `{"error":"That code is invalid or has expired. Run signup again to get a new one.","code":"INVALID_CODE"}`)
				return
			}
			if f.confirmRefusal != "" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				io.WriteString(w, f.confirmRefusal)
				return
			}
			io.WriteString(w, cmp.Or(f.confirmed, `{"status":"confirmed","access_token":"at","refresh_token":"rt","expires_in":3600,"default_account":{"id":"demo-1","name":"Demo"}}`))
		case "/api/v1/cli/workspaces/demo-1/setup_progress":
			if f.progress == nil || r.Header.Get("Authorization") != "Bearer at" {
				t.Errorf("unexpected setup poll (auth %q)", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			poll := int(f.polls.Add(1))
			if f.pollStatus != nil {
				if status := f.pollStatus(poll); status != 0 && status != http.StatusOK {
					// The server's own rescue when it cannot read the state.
					w.WriteHeader(status)
					io.WriteString(w, `{"error":"Unavailable"}`)
					return
				}
			}
			io.WriteString(w, f.progress(poll))
		default:
			// tools refresh after signup: fail quietly, it is optional.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func signupArgs(host string, extra ...string) []string {
	return append([]string{"--host", host, "signup",
		"--email", "jan@yourfirm.example", "--name", "Jan", "--org", "Bank BV", "--type", "financial_services",
		"--currency", "usd", "--language", "en", "--no-password", "--accept-terms", "--code", "123456"}, extra...)
}

// A scripted signup takes two runs against a fake server. The first sends
// the details, has the code emailed and succeeds with the next step; the
// second confirms the code without sending anything again, and stores the
// token.
func TestScriptedSignup(t *testing.T) {
	isolate(t)
	f := &signupServer{}
	srv := f.start(t)

	stdout, _, err := run("test", scriptedSignupArgs(srv.URL, "--currency", "usd", "--no-password")...)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	user := f.signupBody["user"]
	if user["email"] != "jan@yourfirm.example" || user["base_currency"] != "USD" || user["accept_terms"] != true || user["password"] != "" {
		t.Errorf("signup body = %v", user)
	}
	if f.confirms.Load() != 0 {
		t.Error("the first run must not confirm anything")
	}
	if !strings.Contains(stdout, "To finish, run `firmfact signup --email jan@yourfirm.example` with the code in FIRMFACT_SIGNUP_CODE, or piped to --code-stdin.") {
		t.Errorf("first run stdout = %q", stdout)
	}

	stdout, stderr, err := run("test", "--host", srv.URL, "signup", "--email", "jan@yourfirm.example", "--code", "123456", "--no-wait")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(stdout, `Welcome to firmfact. You are signed in, with "Demo" as your default workspace.`) {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "could not load the workspace commands yet") {
		t.Errorf("stderr = %q", stderr)
	}
	if strings.Contains(stdout, "Preparing your Demo workspace") {
		t.Error("--no-wait must not wait for the Demo workspace")
	}
	if n := f.signups.Load(); n != 1 {
		t.Errorf("signup requests = %d, want 1: the second run must not send the details again", n)
	}
	if got := f.confirmBody["user"]; got["email"] != "jan@yourfirm.example" || got["confirmation_code"] != "123456" {
		t.Errorf("confirm body = %v", got)
	}
	tok, err := config.LoadToken(srv.URL)
	if err != nil || tok == nil || tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Fatalf("stored token = %+v, %v", tok, err)
	}
	cfg, _ := config.Load()
	if ws := cfg.Profile("").Workspace; ws != "demo-1" {
		t.Errorf("default workspace = %q", ws)
	}
}

// With a code, signup goes straight to confirming it: no options, no
// second signup request (which would email the code again and count
// against the per-address limit), whatever other details are passed.
func TestSignupWithCodeOnlyConfirms(t *testing.T) {
	isolate(t)
	f := &signupServer{}
	srv := f.start(t)

	if _, _, err := run("test", signupArgs(srv.URL, "--no-wait")...); err != nil {
		t.Fatalf("signup failed: %v", err)
	}
	if f.options.Load() != 0 || f.signups.Load() != 0 || f.confirms.Load() != 1 {
		t.Errorf("requests: %d options, %d signup, %d confirm; want only the confirm", f.options.Load(), f.signups.Load(), f.confirms.Load())
	}

	// The code is confirmed for an address, so a script must name it.
	_, _, err := run("test", "--host", srv.URL, "signup", "--code", "123456")
	if code, _ := Classify(err); code != ExitUsage || !strings.Contains(err.Error(), "missing --email") {
		t.Errorf("without --email: exit %d, %v", code, err)
	}
	if f.confirms.Load() != 1 {
		t.Error("a code without an address must not be sent")
	}
}

// A given code the server refuses points at the run that gets a new one,
// which is not a run with the same code again.
func TestSignupWithAWrongCodeSaysHowToGetANewOne(t *testing.T) {
	isolate(t)
	f := &signupServer{confirmRefusal: `{"error":"That code is invalid or has expired. Run signup again to get a new one.","code":"INVALID_CODE"}`}
	srv := f.start(t)
	t.Setenv("FIRMFACT_SIGNUP_CODE", "999999")

	_, _, err := run("test", "--host", srv.URL, "signup", "--email", "jan@yourfirm.example")
	if err == nil || !strings.Contains(err.Error(), "run `firmfact signup` with your details again, without the code (--code, --code-stdin or FIRMFACT_SIGNUP_CODE)") {
		t.Fatalf("want a how-to-get-a-new-code error, got %v", err)
	}
	if f.signups.Load() != 0 {
		t.Error("a run with a code must not send the details")
	}
}

// scriptedSignupArgs are signupArgs without the password and code flags,
// for the tests that pass those another way.
func scriptedSignupArgs(host string, extra ...string) []string {
	return append([]string{"--host", host, "signup",
		"--email", "jan@yourfirm.example", "--name", "Jan", "--org", "Bank BV", "--type", "financial_services",
		"--currency", "EUR", "--language", "en", "--accept-terms", "--no-wait"}, extra...)
}

// The password piped to --password-stdin reaches the server exactly as
// given, bar the line ending, and the code comes from FIRMFACT_SIGNUP_CODE;
// neither is in the arguments, and the password is in no output.
func TestScriptedSignupWithPasswordStdin(t *testing.T) {
	isolate(t)
	f := &signupServer{}
	srv := f.start(t)

	const password = " Correct-Horse-9 battery "
	stdout, stderr, err := runWithInput(password+"\r\nthe rest is not read\n", "test", scriptedSignupArgs(srv.URL, "--password-stdin")...)
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}
	if got := f.signupBody["user"]["password"]; got != password {
		t.Errorf("password sent = %q, want %q", got, password)
	}
	if strings.Contains(stdout, "Correct-Horse") || strings.Contains(stderr, "Correct-Horse") {
		t.Errorf("the password is in the output: stdout %q, stderr %q", stdout, stderr)
	}

	t.Setenv("FIRMFACT_SIGNUP_CODE", "654321")
	stdout, _, err = run("test", scriptedSignupArgs(srv.URL)...)
	if err != nil {
		t.Fatalf("confirming: %v", err)
	}
	if got := f.confirmBody["user"]["confirmation_code"]; got != "654321" {
		t.Errorf("code sent = %q", got)
	}
	if !strings.Contains(stdout, "You are signed in") {
		t.Errorf("stdout = %q", stdout)
	}
}

// A signup the server refuses over the password says why, without the
// password.
func TestSignupRefusalLeavesThePasswordOut(t *testing.T) {
	isolate(t)
	f := &signupServer{refusal: `{"error":"Some signup details are invalid.","code":"INVALID_USER","details":{"password":["is too common"]}}`}
	srv := f.start(t)

	const password = "Password1234"
	stdout, stderr, err := runWithInput(password+"\n", "test", scriptedSignupArgs(srv.URL, "--password-stdin")...)
	if err == nil || !strings.Contains(err.Error(), "is too common") {
		t.Fatalf("want the server's refusal, got %v", err)
	}
	for name, text := range map[string]string{"stdout": stdout, "stderr": stderr, "error": err.Error()} {
		if strings.Contains(text, password) {
			t.Errorf("the password is in %s: %q", name, text)
		}
	}
	if f.confirmBody != nil {
		t.Error("a refused signup must not be confirmed")
	}
}

func TestSignupCodeStdin(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_SIGNUP_CODE", "111111") // the flag wins
	f := &signupServer{}
	srv := f.start(t)

	if _, _, err := runWithInput(" 222222 \n", "test", scriptedSignupArgs(srv.URL, "--no-password", "--code-stdin")...); err != nil {
		t.Fatalf("signup failed: %v", err)
	}
	if got := f.confirmBody["user"]["confirmation_code"]; got != "222222" {
		t.Errorf("code sent = %q", got)
	}
}

// Wrong combinations and empty input fail before anything is sent, and
// --password, which put the password in the arguments, is refused without
// repeating it.
func TestSignupSecretFlagsRefused(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	cases := []struct {
		name, input string
		args        []string
		want        string
	}{
		{"password flag", "", []string{"--password", "Hunter2-Hunter2"}, "--password is no longer accepted"},
		{"password flag with =", "", []string{"--password=Hunter2-Hunter2"}, "--password-stdin"},
		{"both from stdin", "pw\n123456\n", []string{"--password-stdin", "--code-stdin"}, "FIRMFACT_SIGNUP_CODE"},
		{"password stdin and no password", "pw\n", []string{"--password-stdin", "--no-password"}, "[no-password password-stdin]"},
		{"code and code stdin", "123456\n", []string{"--no-password", "--code", "1", "--code-stdin"}, "[code code-stdin]"},
		{"nothing piped", "", []string{"--password-stdin"}, "--password-stdin found no password on standard input"},
		{"empty line", "\n", []string{"--password-stdin"}, "--password-stdin read an empty password"},
		{"empty code", "\r\n", []string{"--no-password", "--code-stdin"}, "--code-stdin read an empty code"},
	}
	for _, c := range cases {
		stdout, stderr, err := runWithInput(c.input, "test", scriptedSignupArgs(srv.URL, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error with %q, got %v", c.name, c.want, err)
			continue
		}
		if strings.Contains(stdout+stderr+err.Error(), "Hunter2") {
			t.Errorf("%s: the password is repeated: stdout %q, stderr %q, error %q", c.name, stdout, stderr, err)
		}
	}
}

// A script that has not got the code yet has done its part: it exits 0
// and is told how to finish once the email arrives. Waiting for the code
// is no failure, and used to exit 1.
func TestSignupWithoutCodeInScripts(t *testing.T) {
	isolate(t)
	f := &signupServer{}
	srv := f.start(t)

	stdout, _, err := run("test", scriptedSignupArgs(srv.URL, "--no-password")...)
	if code, _ := Classify(err); code != 0 {
		t.Fatalf("exit %d (%v), want 0", code, err)
	}
	if !strings.Contains(stdout, "To finish, run `firmfact signup --email jan@yourfirm.example` with the code in FIRMFACT_SIGNUP_CODE, or piped to --code-stdin.") {
		t.Errorf("stdout = %q", stdout)
	}
	if f.signupBody == nil || f.confirmBody != nil {
		t.Errorf("signup body %v, confirm body %v: want the signup sent and nothing confirmed", f.signupBody, f.confirmBody)
	}
}

// Without --no-wait, signup polls the Demo setup until it is complete,
// printing each new progress line once.
func TestSignupWaitsForDemoSetup(t *testing.T) {
	isolate(t)
	prev := setupPollInterval
	setupPollInterval = time.Millisecond
	defer func() { setupPollInterval = prev }()

	f := &signupServer{progress: func(poll int) string {
		if poll < 3 {
			return `{"percentage":40,"current_activity":"Importing contracts","liveness":"running"}`
		}
		return `{"percentage":100,"liveness":"complete"}`
	}}
	srv := f.start(t)

	stdout, _, err := run("test", signupArgs(srv.URL)...)
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}
	if n := strings.Count(stdout, "40%  Importing contracts"); n != 1 {
		t.Errorf("progress line printed %d times in %q", n, stdout)
	}
	if !strings.Contains(stdout, "100%  Ready.") || !strings.Contains(stdout, "Your Demo workspace is ready.") {
		t.Errorf("stdout = %q", stdout)
	}
	if f.polls.Load() != 3 {
		t.Errorf("polls = %d, want 3", f.polls.Load())
	}
}

func TestSignupGivesUpWaitingAfterTheTimeout(t *testing.T) {
	isolate(t)
	f := &signupServer{progress: func(int) string {
		return `{"percentage":10,"message":"Creating account Demo 2/2","liveness":"running"}`
	}}
	srv := f.start(t)

	_, _, err := run("test", signupArgs(srv.URL, "--wait-timeout", "1ns")...)
	if err == nil || !strings.Contains(err.Error(), "your Demo workspace is still being set up after 1ns; setup continues on the server. Check with `firmfact workspaces status --wait`") {
		t.Fatalf("want a still-running error, got %v", err)
	}
	// Worth another try later, unlike a setup that failed.
	if code, _ := Classify(err); code != ExitUnavailable {
		t.Errorf("exit %d, want %d", code, ExitUnavailable)
	}
}

func TestSignupWithoutTermsRefusesInScripts(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/signup/options" {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		io.WriteString(w, `{"data":{"organization_types":[{"value":"law_firm","label":"Law firm"}],"default_currency":"EUR","terms":[{"title":"Terms","url":"https://x/terms"}]}}`)
	}))
	defer srv.Close()

	stdout, _, err := run("test", "--host", srv.URL, "signup", "--email", "a@lawfirm.example", "--name", "A", "--org", "Firm",
		"--type", "law_firm", "--currency", "EUR", "--language", "en", "--no-password")
	if err == nil || !strings.Contains(err.Error(), "--accept-terms") {
		t.Fatalf("want an --accept-terms error, got %v", err)
	}
	// The documents are listed before the refusal, so a script's log shows
	// what --accept-terms accepts.
	if !strings.Contains(stdout, "Terms: https://x/terms") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestStepAfterTool(t *testing.T) {
	cases := map[string]string{
		"list_vendors":        "analyze cost-trends",
		"list_allocations":    "analyze allocations",
		"analyze_cost_trends": "ask ",
	}
	for tool, want := range cases {
		step, ok := stepAfterTool(tool)
		if !ok || !strings.HasPrefix(step.command, want) {
			t.Errorf("stepAfterTool(%q) = %q, want %q", tool, step.command, want)
		}
	}
	if _, ok := stepAfterTool("list_workspaces"); ok {
		t.Error("list_workspaces needs no next step")
	}
}

// Below the server's minimum version every command is refused with the
// upgrade command; update and doctor stay usable. The check runs beside a
// command, so the minimum it learns applies from the next run.
func TestMinimumVersionGate(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/version":
			io.WriteString(w, `{"minimum_version":"0.2.0"}`)
		case "/api/v1/cli/me":
			untilChecked(cacheDir, r, func(s update.Status) bool { return s.Minimum == "0.2.0" })
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[]}}`)
		case "/latest":
			redirectToTag(w, r, "v0.3.0")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	prevURL, prevVersion := update.LatestReleaseURL, api.Version
	update.LatestReleaseURL = srv.URL + "/latest"
	defer func() { update.LatestReleaseURL, api.Version = prevURL, prevVersion }()
	storedToken(t, srv.URL, time.Hour)

	stdout, _, err := run("0.1.0", "--host", srv.URL, "whoami")
	if err != nil || !strings.Contains(stdout, "jan@yourfirm.example") {
		t.Fatalf("the run that learns the minimum still runs: %q, %v", stdout, err)
	}
	_, _, err = run("0.1.0", "--host", srv.URL, "whoami")
	if err == nil || !strings.Contains(err.Error(), "needs 0.2.0 or newer") {
		t.Fatalf("want a minimum-version refusal, got %v", err)
	}

	stdout, _, err = run("0.2.5", "--host", srv.URL, "config", "show")
	if err != nil {
		t.Fatalf("a supported version must run: %v", err)
	}
	if !strings.Contains(stdout, "host:      "+srv.URL) {
		t.Errorf("stdout = %q", stdout)
	}
}

// A refusal on a minimum that is due for another check waits for that
// check first, so a server that has lowered its minimum since lets the
// command run at once rather than a day later.
func TestAStaleRefusalIsCheckedAgain(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	var minimum atomic.Value
	minimum.Store("0.1.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/cli/version":
			fmt.Fprintf(w, `{"minimum_version":%q}`, minimum.Load())
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	prevVersion := api.Version
	defer func() { api.Version = prevVersion }()
	storedToken(t, srv.URL, time.Hour)
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	stale := func(minimum string) {
		raw, _ := json.Marshal(update.Status{CheckedAt: time.Now().Add(-48 * time.Hour), Host: srv.URL, Minimum: minimum})
		if err := os.WriteFile(filepath.Join(cacheDir, "version-check.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stale("9.9.9")
	stdout, _, err := run("0.2.0", "--host", srv.URL, "whoami")
	if err != nil || !strings.Contains(stdout, "jan@yourfirm.example") {
		t.Errorf("the lowered minimum must let the command run: %q, %v", stdout, err)
	}
	if s, due := update.Cached(srv.URL, cacheDir, false); due || s.Minimum != "0.1.0" {
		t.Errorf("cached %+v, due %v", s, due)
	}

	stale("0.1.0")
	minimum.Store("9.9.9")
	if _, _, err := run("0.2.0", "--host", srv.URL, "whoami"); err != nil {
		t.Errorf("a stale minimum the version meets is not checked first: %v", err)
	}
	stale("9.9.9")
	if _, _, err := run("0.2.0", "--host", srv.URL, "whoami"); err == nil || !strings.Contains(err.Error(), "needs 9.9.9 or newer") {
		t.Errorf("want a minimum-version refusal, got %v", err)
	}
}

// A snapshot is not held to the server's minimum version: numbered from
// 0.0.0, or after the last release with a dev pre-release, it would be
// refused by the very server it was built to test. A release candidate is
// held to it, since it comes before the release it names, and update
// offers it that release.
func TestSnapshotsAreNotGatedButReleaseCandidatesAre(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/cli/version":
			io.WriteString(w, `{"minimum_version":"0.3.0"}`)
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[]}}`)
		case "/latest":
			redirectToTag(w, r, "v0.3.0")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	prevURL, prevBase, prevVersion := update.LatestReleaseURL, update.DownloadBase, api.Version
	update.LatestReleaseURL = srv.URL + "/latest"
	// Nothing is downloaded below; should update ever try, it finds
	// nothing here rather than the real releases.
	update.DownloadBase = srv.URL + "/download"
	defer func() { update.LatestReleaseURL, update.DownloadBase, api.Version = prevURL, prevBase, prevVersion }()
	storedToken(t, srv.URL, time.Hour)

	for _, v := range []string{"0.0.0-SNAPSHOT-b005a6f", "0.2.1-dev+b005a6f"} {
		stdout, _, err := run(v, "--host", srv.URL, "whoami")
		if err != nil || !strings.Contains(stdout, "jan@yourfirm.example") {
			t.Errorf("%s: whoami = %q, %v; a snapshot must not be gated", v, stdout, err)
		}
		stdout, _, _ = run(v, "--host", srv.URL, "--json", "doctor")
		if c := versionCheck(t, stdout); !c.OK || !strings.Contains(c.Detail, "development build") {
			t.Errorf("%s: doctor version check = %+v", v, c)
		}
	}

	_, _, err := run("0.3.0-rc1", "--host", srv.URL, "whoami")
	if err == nil || !strings.Contains(err.Error(), "needs 0.3.0 or newer") {
		t.Errorf("0.3.0-rc1: want a minimum-version refusal, got %v", err)
	}
	stdout, _, _ := run("0.3.0-rc1", "--host", srv.URL, "--json", "doctor")
	if c := versionCheck(t, stdout); c.OK || !strings.Contains(c.Detail, "below the minimum 0.3.0") {
		t.Errorf("0.3.0-rc1: doctor version check = %+v", c)
	}

	// A copy that Homebrew owns is told how to upgrade rather than
	// replaced, which shows the offer without a download.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	t.Setenv("HOMEBREW_PREFIX", filepath.Dir(filepath.Dir(exe)))
	if update.InstallMethod() != update.Homebrew {
		t.Skipf("the test binary at %s does not pass for a Homebrew install", exe)
	}
	stdout, _, err = run("0.3.0-rc1", "--host", srv.URL, "--json", "update")
	if err != nil {
		t.Fatal(err)
	}
	var got updateResult
	decodeOnly(t, "update", stdout, &got)
	if got != (updateResult{Version: "0.3.0-rc1", Latest: "0.3.0", Target: "0.3.0", Upgrade: "brew upgrade firmfact"}) {
		t.Errorf("update = %+v; want 0.3.0 offered to 0.3.0-rc1", got)
	}
}

// versionCheck is the version check in doctor's --json report.
func versionCheck(t *testing.T, stdout string) doctorCheck {
	t.Helper()
	var report doctorReport
	decodeOnly(t, "doctor", stdout, &report)
	for _, c := range report.Checks {
		if c.Name == "version" {
			return c
		}
	}
	t.Fatalf("no version check in %+v", report.Checks)
	return doctorCheck{}
}

// A cached minimum above the running version must not trap the user: the
// commands that only undo or repair things on this machine still run
// (logout, claim --undo, config, tools), and so does version. Workspace commands stay refused,
// and a tool whose name would put it under the config group gets no
// command there, so it cannot run as one of the exempt config commands.
func TestOutdatedVersionCanStillSignOutAndUndo(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	t.Setenv("LOCALAPPDATA", t.TempDir())
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)
	prevURL, prevVersion := update.LatestReleaseURL, api.Version
	update.LatestReleaseURL = srv.URL + "/latest"
	defer func() { update.LatestReleaseURL, api.Version = prevURL, prevVersion }()

	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(update.Status{CheckedAt: time.Now(), Host: srv.URL, Minimum: "9.9.9"})
	if err := os.WriteFile(filepath.Join(cacheDir, "version-check.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	signedInWithTools(t, srv.URL, mcp.Tool{Name: "list_config", Title: "List configuration"}, listVendors)
	storedToken(t, srv.URL, time.Hour)

	for _, args := range [][]string{{"whoami"}, {"vendors", "list"}} {
		_, _, err := run("0.1.0", append([]string{"--host", srv.URL}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "needs 9.9.9 or newer") {
			t.Errorf("%v: want a minimum-version refusal, got %v", args, err)
		}
	}
	if _, _, err := run("0.1.0", "--host", srv.URL, "config", "list"); err == nil || !strings.Contains(err.Error(), `unknown command "list" for "firmfact config"`) {
		t.Errorf("config list: want no such command, got %v", err)
	}
	for _, args := range [][]string{{"config", "show"}, {"tools", "list"}, {"version"}} {
		if _, _, err := run("0.1.0", append([]string{"--host", srv.URL}, args...)...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}

	stdout, _, err := run("0.1.0", "--host", srv.URL, "logout")
	if err != nil || !strings.Contains(stdout, "Signed out of "+srv.URL) {
		t.Fatalf("logout = %q, %v", stdout, err)
	}
	if sent := f.sent(); len(sent) == 0 || sent[0].Get("token") != "rt" {
		t.Errorf("revoke requests = %v", sent)
	}

	home := os.Getenv("HOME")
	rc := filepath.Join(home, ".bashrc")
	block := "export KEEP=1\n\n# >>> firmfact claim ff >>>\nalias ff=firmfact\n# <<< firmfact claim ff <<<\n"
	if err := os.WriteFile(rc, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run("0.1.0", "--host", srv.URL, "claim", "ff", "--undo", "--shell", "bash"); err != nil {
		t.Fatalf("claim --undo: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got, _ := os.ReadFile(rc); string(got) != "export KEEP=1\n" {
			t.Errorf(".bashrc after claim --undo = %q", got)
		}
	}
}

// When claim may not change the rc file, here a link to a file that does
// not exist, it still makes the command link and shows the block for the
// user to add by hand, without a dated copy that was never made.
func TestClaimShowsTheBlockForAnRCItLeavesAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows gets a .cmd shim and no rc block")
	}
	// A PATH without the user's own directories, where no other ff hides
	// the one claim makes.
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash in /usr/bin or /bin")
	}
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	home := os.Getenv("HOME")
	rc := filepath.Join(home, ".bashrc")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), rc); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := run("0.1.0", "claim", "ff", "--yes", "--shell", "bash")
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, stdout)
	}
	for _, want := range []string{"to the end of " + rc + " yourself", "does not exist", "export PATH=", "now links to firmfact"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "dated copy") || strings.Contains(stdout, "is yours") {
		t.Errorf("output claims an rc change that did not happen:\n%s", stdout)
	}
	if _, err := os.Readlink(filepath.Join(home, ".local", "bin", "ff")); err != nil {
		t.Errorf("no command link: %v", err)
	}
}

// The Demo setup line is redrawn in place on a terminal, so the server's
// activity text must stay one line without escape sequences of its own.
func TestSignupProgressEscapesServerText(t *testing.T) {
	isolate(t)
	prev := setupPollInterval
	setupPollInterval = time.Millisecond
	defer func() { setupPollInterval = prev }()

	f := &signupServer{progress: func(poll int) string {
		if poll < 2 {
			return `{"percentage":40,"current_activity":"Importing\n\u001b]52;c;cm0gLXJmIH4=\u0007","liveness":"running"}`
		}
		return `{"percentage":40,"message":"Stuck\u009b2K","liveness":"failed"}`
	}}
	srv := f.start(t)

	stdout, stderr, err := run("test", signupArgs(srv.URL)...)
	if err == nil || !strings.Contains(err.Error(), `the setup of your Demo workspace did not finish (Stuck\u009b2K)`) {
		t.Fatalf("want the failure with its escape shown, got %v", err)
	}
	assertNoTerminalControls(t, "stdout", stdout)
	assertNoTerminalControls(t, "stderr", stderr)
	assertNoTerminalControls(t, "error", err.Error())
	if !strings.Contains(stdout, `40%  Importing \u001b]52;c;cm0gLXJmIH4=\u0007`+"\n") {
		t.Errorf("stdout = %q", stdout)
	}
}
