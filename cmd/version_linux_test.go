package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// runOnTerminal is one invocation of the given version with stdout on a
// terminal, where the daily check may ask GitHub. It returns how long the
// command took.
func runOnTerminal(t *testing.T, version string, args ...string) (time.Duration, error) {
	t.Helper()
	master, tty := openPTY(t)
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, master)
		close(drained)
	}()
	start := time.Now()
	err := NewRootCommand(Build{Version: version}, args, IOStreams{Out: tty, Err: io.Discard}).Execute()
	took := time.Since(start)
	tty.Close()
	<-drained
	return took, err
}

// versionServer is a host that supports 0.1.0 and up, whose whoami
// answers once ready returns.
func versionServer(t *testing.T, ready func(r *http.Request)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/cli/version":
			io.WriteString(w, `{"minimum_version":"0.1.0"}`)
		case "/api/v1/cli/me":
			ready(r)
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	storedToken(t, srv.URL, time.Hour)
	return srv.URL
}

// The daily check runs beside the command. A GitHub that never answers
// adds nothing to the command's time; the server's minimum, which did come
// in, is kept, and the check stays due. In CI, or with stdout not on a
// terminal, GitHub is not asked at all, and the server's check alone
// completes the day's check for such runs; a run on a terminal still asks
// GitHub that day.
func TestVersionCheckRunsBesideTheCommand(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	arrived := make(chan struct{}, 1)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer github.Close()
	prevURL, prevVersion := update.LatestReleaseURL, api.Version
	update.LatestReleaseURL = github.URL
	defer func() { update.LatestReleaseURL, api.Version = prevURL, prevVersion }()

	// whoami answers once GitHub has been asked and the server's answer
	// recorded, so the check is well under way as the command ends.
	host := versionServer(t, func(r *http.Request) {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
		}
		untilChecked(cacheDir, r, func(s update.Status) bool { return s.Minimum == "0.1.0" })
	})
	took, err := runOnTerminal(t, "0.1.0", "--host", host, "whoami")
	if err != nil {
		t.Fatal(err)
	}
	// Waiting for GitHub would take the check's limit of 3 s.
	if took > 2*time.Second {
		t.Errorf("whoami took %s beside a GitHub that never answers", took)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("GitHub was asked %d times, want once", n)
	}
	if s, due := update.Cached(host, cacheDir, true); !due || s.Minimum != "0.1.0" {
		t.Errorf("cached %+v, due %v; want the minimum kept and the check due", s, due)
	}

	checked := func(r *http.Request) {
		untilChecked(cacheDir, r, func(s update.Status) bool { return !s.CheckedAt.IsZero() })
	}
	for _, c := range []struct {
		name     string
		ci       string
		terminal bool
	}{
		{"CI=1", "1", true},
		{"stdout to a pipe", "", false},
	} {
		t.Setenv("CI", c.ci)
		host := versionServer(t, checked)
		if c.terminal {
			_, err = runOnTerminal(t, "0.1.0", "--host", host, "whoami")
		} else {
			_, _, err = run("0.1.0", "--host", host, "whoami")
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n := asked.Load(); n != 1 {
			t.Errorf("%s: GitHub was asked again", c.name)
		}
		if s, due := update.Cached(host, cacheDir, false); due || s.Minimum != "0.1.0" {
			t.Errorf("%s: cached %+v, due %v; want the server's check to complete the day's", c.name, s, due)
		}
		if _, due := update.Cached(host, cacheDir, true); !due {
			t.Errorf("%s: a check that left GitHub out stops a terminal asking GitHub today", c.name)
		}
	}
}

// CI=false or CI=0 is no CI job: a person on a terminal still hears of a
// new release.
func TestCIFalseIsNotCI(t *testing.T) {
	_, tty := openPTY(t)
	t.Cleanup(func() { tty.Close() })
	app := &App{Out: tty}
	for value, ask := range map[string]bool{"": true, "false": true, "0": true, "1": false, "true": false} {
		t.Setenv("CI", value)
		if got := askGitHub(app); got != ask {
			t.Errorf("CI=%q: askGitHub = %v, want %v", value, got, ask)
		}
	}
}

// A command with a person at the terminal waits longer for a locked
// keyring's unlock prompt; a script, without one on stdin or stderr, does
// not.
func TestKeyringPatienceNeedsAPerson(t *testing.T) {
	_, tty := openPTY(t)
	t.Cleanup(func() { tty.Close() })
	for _, c := range []struct {
		name string
		app  *App
		want time.Duration
	}{
		{"terminal", &App{In: tty, Err: tty}, keyringUnlockWait},
		{"stdin from a pipe", &App{In: strings.NewReader(""), Err: tty}, 0},
		{"stderr to a file", &App{In: tty, Err: io.Discard}, 0},
	} {
		if got := c.app.keyringPatience(); got != c.want {
			t.Errorf("%s: patience %s, want %s", c.name, got, c.want)
		}
	}
}
