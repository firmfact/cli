package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/firmfact/cli/cmd"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// childArgsEnv, when set, makes the test binary run the program with these
// arguments (a JSON array) instead of the tests, so a test can run the real
// thing in a child process and signal it.
const childArgsEnv = "FIRMFACT_TEST_MAIN_ARGS"

func TestMain(m *testing.M) {
	if raw := os.Getenv(childArgsEnv); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			panic(err)
		}
		os.Exit(run(args, cmd.OSStreams()))
	}
	os.Exit(m.Run())
}

// isolate points config, cache and tokens at scratch space; a child process
// inherits it.
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
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "1")
}

// A binary that go install built from a release reports that release,
// since no build stamped a version into it; a stamped version stands, and
// a build without a module version stays "dev".
func TestBuildVersion(t *testing.T) {
	built := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "github.com/firmfact/cli", Version: v}}, true
		}
	}
	cases := []struct {
		stamped string
		info    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"dev", built("v0.3.0"), "0.3.0"},
		{"dev", built("v0.3.1-0.20260927101010-b005a6f00000+dirty"), "0.3.1-0.20260927101010-b005a6f00000+dirty"},
		{"dev", built("(devel)"), "dev"},
		{"dev", built(""), "dev"},
		{"dev", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
		{"0.2.0", built("v0.3.0"), "0.2.0"},
	}
	for _, c := range cases {
		if got := buildVersion(c.stamped, c.info); got != c.want {
			t.Errorf("buildVersion(%q) = %q, want %q", c.stamped, got, c.want)
		}
	}
	if update.IsRelease(buildVersion("dev", built("v0.3.1-0.20260927101010-b005a6f00000"))) {
		t.Error("a build of an untagged commit counts as a release")
	}
}

// The build stamped into the binary stands; what Go recorded fills in
// what was not stamped, but only when it is about the same commit.
func TestBuildInfo(t *testing.T) {
	const (
		full = "b005a6f00000c0ffee0123456789abcdef012345"
		when = "2026-09-27T10:10:10Z"
	)
	built := func(v string, settings ...string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/firmfact/cli", Version: v}}
			for i := 0; i < len(settings); i += 2 {
				bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
			}
			return bi, true
		}
	}
	checkout := built("v0.3.1-0.20260927101010-b005a6f00000+dirty", "vcs.revision", full, "vcs.time", when, "vcs.modified", "true")
	cases := []struct {
		name    string
		stamped cmd.Build
		info    func() (*debug.BuildInfo, bool)
		want    cmd.Build
	}{
		{"a release build", cmd.Build{Version: "0.4.0", Commit: full, Date: when}, built("(devel)"),
			cmd.Build{Version: "0.4.0", Commit: full, Date: when}},
		{"a build from a checkout", cmd.Build{Version: "dev"}, checkout,
			cmd.Build{Version: "0.3.1-0.20260927101010-b005a6f00000+dirty", Commit: full, Date: when, Modified: true}},
		{"a stamped build of a changed checkout", cmd.Build{Version: "0.4.0", Commit: full}, checkout,
			cmd.Build{Version: "0.4.0", Commit: full, Date: when, Modified: true}},
		{"a stamp of another commit", cmd.Build{Version: "0.4.0", Commit: "0123456789ab"}, checkout,
			cmd.Build{Version: "0.4.0", Commit: "0123456789ab"}},
		{"go install ...@<commit>", cmd.Build{Version: "dev"}, built("v0.3.1-0.20260927101010-b005a6f00000"),
			cmd.Build{Version: "0.3.1-0.20260927101010-b005a6f00000", Commit: "b005a6f00000", Date: when}},
		{"go install ...@v0.3.0", cmd.Build{Version: "dev"}, built("v0.3.0"),
			cmd.Build{Version: "0.3.0"}},
		{"no build information", cmd.Build{Version: "dev"}, func() (*debug.BuildInfo, bool) { return nil, false },
			cmd.Build{Version: "dev"}},
	}
	for _, c := range cases {
		if got := buildInfo(c.stamped, c.info); got != c.want {
			t.Errorf("%s: buildInfo = %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// .goreleaser.yaml stamps exactly the variables main reads, and version
// --json shows what they were stamped with.
func TestReleaseBuildStamp(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var stamps []string
	for _, m := range regexp.MustCompile(`-X main\.(\w+)=`).FindAllStringSubmatch(string(raw), -1) {
		stamps = append(stamps, m[1])
	}
	if strings.Join(stamps, " ") != "version commit date" {
		t.Errorf(".goreleaser.yaml stamps %v; want version, commit and date", stamps)
	}

	isolate(t)
	prev := [3]string{version, commit, date}
	version, commit, date = "0.4.0", "b005a6f00000c0ffee0123456789abcdef012345", "2026-09-27T10:10:10Z"
	defer func() { version, commit, date = prev[0], prev[1], prev[2] }()
	var out, errOut bytes.Buffer
	if code := run([]string{"version", "--json"}, cmd.IOStreams{Out: &out, Err: &errOut}); code != 0 {
		t.Fatalf("version --json: exit %d, %s", code, errOut.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("version --json is not JSON: %v\n%s", err, out.String())
	}
	for key, want := range map[string]string{"version": version, "commit": commit, "date": date, "go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH} {
		if got[key] != want {
			t.Errorf("%s = %v, want %s", key, got[key], want)
		}
	}
}

// run returns the exit status of the error's class and reports it on
// stderr; TestExitStatuses in package cmd holds the full table.
func TestRunExitStatus(t *testing.T) {
	isolate(t)
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"--version"}, 0},
		{[]string{"config"}, 0},
		{[]string{"no-such-command"}, cmd.ExitUsage},
		{[]string{"config", "sho"}, cmd.ExitUsage},
		{[]string{"--host", "localhost:1", "whoami"}, cmd.ExitSignedOut},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		code := run(c.args, cmd.IOStreams{Out: &out, Err: &errOut})
		if code != c.want {
			t.Errorf("%v: exit %d, want %d (stderr %q)", c.args, code, c.want, errOut.String())
		}
		if c.want != 0 && !strings.HasPrefix(errOut.String(), "error: ") {
			t.Errorf("%v: stderr = %q", c.args, errOut.String())
		}
	}
}

// jsonError is the error envelope --json writes on stderr.
type jsonError struct {
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
		Status  string `json:"status"`
	} `json:"error"`
}

// decodeError parses stderr as exactly one line holding the envelope.
func decodeError(t *testing.T, stderr string) jsonError {
	t.Helper()
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("want one line on stderr, got %q", stderr)
	}
	var e jsonError
	dec := json.NewDecoder(strings.NewReader(stderr))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("stderr is not the error envelope: %v\n%s", err, stderr)
	}
	return e
}

// With --json a failure is one line of JSON on stderr, with the exit
// status as its code, and nothing on stdout; also when the command line
// failed to parse before reaching --json.
func TestJSONErrorEnvelope(t *testing.T) {
	isolate(t)
	cases := []struct {
		args    []string
		code    int
		status  string
		message string
	}{
		{[]string{"--json", "config", "sho"}, cmd.ExitUsage, "usage", "unknown command \"sho\" for \"firmfact config\"; did you mean `firmfact config set` or `firmfact config show`?"},
		{[]string{"--no-such-flag", "--json", "whoami"}, cmd.ExitUsage, "usage", "unknown flag: --no-such-flag"},
		{[]string{"--host", "localhost:1", "whoami", "--json"}, cmd.ExitSignedOut, "not_signed_in", "not signed in: run `firmfact login` (or `firmfact signup`)"},
		// --format json is --json, errors included.
		{[]string{"--format", "json", "config", "sho"}, cmd.ExitUsage, "usage", "unknown command \"sho\" for \"firmfact config\"; did you mean `firmfact config set` or `firmfact config show`?"},
		{[]string{"--no-such-flag", "--format=JSON", "whoami"}, cmd.ExitUsage, "usage", "unknown flag: --no-such-flag"},
		{[]string{"--host", "localhost:1", "whoami", "--format", "json"}, cmd.ExitSignedOut, "not_signed_in", "not signed in: run `firmfact login` (or `firmfact signup`)"},
		{[]string{"--json", "--format", "csv", "config", "show"}, cmd.ExitUsage, "usage", "--json is --format json; give one or the other"},
		// So is --jq, which filters the JSON.
		{[]string{"--jq", ".host", "config", "sho"}, cmd.ExitUsage, "usage", "unknown command \"sho\" for \"firmfact config\"; did you mean `firmfact config set` or `firmfact config show`?"},
		{[]string{"--no-such-flag", "--jq", ".x", "whoami"}, cmd.ExitUsage, "usage", "unknown flag: --no-such-flag"},
		{[]string{"--no-such-flag", "--jq=.x", "whoami"}, cmd.ExitUsage, "usage", "unknown flag: --no-such-flag"},
		{[]string{"--jq", ".[", "config", "show"}, cmd.ExitUsage, "usage", "invalid argument \".[\" for \"--jq\" flag: not a jq expression: unexpected EOF"},
		{[]string{"--host", "localhost:1", "whoami", "--jq", ".user.email"}, cmd.ExitSignedOut, "not_signed_in", "not signed in: run `firmfact login` (or `firmfact signup`)"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		code := run(c.args, cmd.IOStreams{Out: &out, Err: &errOut})
		e := decodeError(t, errOut.String())
		if code != c.code || e.Error.Code != c.code || e.Error.Status != c.status || e.Error.Message != c.message {
			t.Errorf("%v: exit %d, envelope %+v; want %d %q %q", c.args, code, e.Error, c.code, c.status, c.message)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout = %q", c.args, out.String())
		}
	}

	// --json given as a flag's value is not --json.
	var out, errOut bytes.Buffer
	run([]string{"--profile", "--json", "--no-such-flag"}, cmd.IOStreams{Out: &out, Err: &errOut})
	if !strings.HasPrefix(errOut.String(), "error: ") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// A --json command stopped by SIGTERM reports that like any failure,
// with status 130.
func TestInterruptedJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM to send")
	}
	isolate(t)
	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	if err := config.SaveToken(srv.URL, &config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal([]string{"--json", "--host", srv.URL, "whoami"})
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), childArgsEnv+"="+string(raw))
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		_ = child.Process.Kill()
		t.Fatalf("no request arrived; stderr %q", stderr.String())
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != cmd.ExitInterrupted {
		t.Fatalf("exit %v, want status %d; stderr %q", err, cmd.ExitInterrupted, stderr.String())
	}
	if e := decodeError(t, stderr.String()); e.Error.Code != cmd.ExitInterrupted || e.Error.Status != "interrupted" {
		t.Errorf("envelope = %+v", e.Error)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// Two commands started together with an expired token renew it once
// between them, and both succeed. The server rotates refresh tokens, so a
// second renewal with the same one is refused (invalid_grant); before the
// lock, one of two such commands ended the session.
func TestTwoProcessesRenewOnce(t *testing.T) {
	isolate(t)
	var (
		mu       sync.Mutex
		access   = "at-0"
		refresh  = "rt-0"
		renewals atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			n := renewals.Add(1)
			_ = r.ParseForm()
			time.Sleep(200 * time.Millisecond) // both commands are running by now
			mu.Lock()
			defer mu.Unlock()
			if r.Form.Get("refresh_token") != refresh {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			access, refresh = fmt.Sprintf("at-%d", n), fmt.Sprintf("rt-%d", n)
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600,"token_type":"Bearer"}`, access, refresh)
		case "/api/v1/cli/me":
			mu.Lock()
			ok := r.Header.Get("Authorization") == "Bearer "+access
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"data":{"user":{"name":"User","email":"user@yourfirm.example"},"workspaces":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	expired := &config.Token{AccessToken: "at-0", RefreshToken: "rt-0", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := config.SaveToken(srv.URL, expired); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal([]string{"--host", srv.URL, "whoami"})
	children := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for i := range children {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), childArgsEnv+"="+string(raw))
		child.Stdout, child.Stderr = &outputs[i], &outputs[i]
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		children[i] = child
	}
	for i, child := range children {
		if err := child.Wait(); err != nil {
			t.Errorf("command %d: %v\n%s", i, err, outputs[i].String())
		} else if !strings.Contains(outputs[i].String(), "user@yourfirm.example") {
			t.Errorf("command %d printed %q", i, outputs[i].String())
		}
	}
	if n := renewals.Load(); n != 1 {
		t.Errorf("%d renewals, want 1", n)
	}
}
