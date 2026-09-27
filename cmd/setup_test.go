package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
)

// fastPolls makes the setup wait poll every millisecond (and retry within
// 15 ms) for the rest of the test.
func fastPolls(t *testing.T) {
	t.Helper()
	prev := setupPollInterval
	setupPollInterval = time.Millisecond
	t.Cleanup(func() { setupPollInterval = prev })
}

// The pauses between checks grow from the interval to 2.5 times it, and
// those before retrying a failed check double up to 15 times it: 2 s to 5 s
// and 2 s to 30 s at the default interval.
func TestSetupPollGaps(t *testing.T) {
	gap := setupPollInterval
	var gaps []time.Duration
	for range 8 {
		gaps = append(gaps, gap)
		gap = nextPollGap(gap)
	}
	if gaps[0] != 2*time.Second || gaps[1] != 2500*time.Millisecond || gaps[7] != 5*time.Second {
		t.Errorf("poll gaps = %v", gaps)
	}
	var retry time.Duration
	var retries []time.Duration
	for range 6 {
		retry = nextRetryGap(retry, 2*time.Second)
		retries = append(retries, retry)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if retries[i] != want[i] {
			t.Fatalf("retry gaps = %v, want %v", retries, want)
		}
	}
}

// A 503 in the middle of the wait (the progress endpoint's own answer when
// it cannot read the state) no longer ends a wait of minutes: the wait
// tries again and sees the setup through.
func TestSignupWaitSurvivesATransient503(t *testing.T) {
	isolate(t)
	fastPolls(t)
	f := &signupServer{
		progress: func(poll int) string {
			if poll < 4 {
				return `{"percentage":40,"current_activity":"Importing contracts","liveness":"running"}`
			}
			return `{"percentage":100,"liveness":"complete"}`
		},
		pollStatus: func(poll int) int {
			if poll == 2 || poll == 3 {
				return http.StatusServiceUnavailable
			}
			return 0
		},
	}
	srv := f.start(t)

	stdout, _, err := run("test", signupArgs(srv.URL)...)
	if err != nil {
		t.Fatalf("signup failed: %v", err)
	}
	if n := strings.Count(stdout, "40%  Could not check progress (Unavailable); trying again"); n != 1 {
		t.Errorf("retry line printed %d times in %q", n, stdout)
	}
	if !strings.Contains(stdout, "Your Demo workspace is ready.") {
		t.Errorf("stdout = %q", stdout)
	}
	if f.polls.Load() != 4 {
		t.Errorf("polls = %d, want 4", f.polls.Load())
	}
}

// statusServer answers /api/v1/cli/me with a production and a Demo
// workspace (the default), and each one's setup progress with setup.
func statusServer(t *testing.T, setup func(id string, poll int) (status int, body string)) (host string, polls *atomic.Int32) {
	t.Helper()
	polls = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/cli/me" {
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},
				"workspaces":[{"id":"prod-1","name":"Bank BV","mcp_available":true},{"id":"demo-1","name":"Demo","default":true,"demo":true,"mcp_available":true}]}}`)
			return
		}
		id, prefixed := strings.CutPrefix(r.URL.Path, "/api/v1/cli/workspaces/")
		id, suffixed := strings.CutSuffix(id, "/setup_progress")
		if !prefixed || !suffixed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, body := setup(id, int(polls.Add(1)))
		if status == -1 { // no answer at all
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	storedToken(t, srv.URL, time.Hour)
	return srv.URL, polls
}

const (
	setupRunningBody = `{"percentage":40,"current_activity":"Importing contracts","liveness":"running"}`
	// A finished setup can carry the figures cached before it finished.
	setupCompleteBody = `{"percentage":99,"current_activity":"Processing demo documents","message":"Setup completed successfully!","liveness":"complete"}`
	setupFailedBody   = `{"percentage":0,"message":"Setup failed","liveness":"failed"}`
)

// workspaces status says where a setup stands, for the workspace named or
// the one commands use, and --json prints the workspace with its setup.
func TestWorkspacesStatus(t *testing.T) {
	isolate(t)
	host, _ := statusServer(t, func(id string, _ int) (int, string) {
		if id == "demo-1" {
			return http.StatusOK, setupRunningBody
		}
		return http.StatusOK, setupCompleteBody
	})

	stdout, _, err := run("test", "--host", host, "workspaces", "status")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing chosen, so the token's default; the hint needs no id.
	want := "Demo is being set up (40%): Importing contracts\nFollow it with `firmfact workspaces status --wait`.\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	stdout, _, err = run("test", "--host", host, "workspaces", "status", "bank bv")
	if err != nil || stdout != "Bank BV is ready.\n" {
		t.Errorf("status bank bv = %q, %v", stdout, err)
	}

	stdout, _, err = run("test", "--host", host, "--json", "workspaces", "status", "prod-1")
	if err != nil {
		t.Fatal(err)
	}
	var got Workspace
	decodeOnly(t, "status --json", stdout, &got)
	if got.ID != "prod-1" || got.Setup == nil || got.Setup.State != "complete" || got.Setup.Percentage != 100 || got.Setup.Activity != "" {
		t.Errorf("status --json = %+v (setup %+v)", got, got.Setup)
	}

	if code, msg := exitStatusOf(t.Context(), "test", "--host", host, "workspaces", "status", "nope"); code != ExitNotFound {
		t.Errorf("unknown workspace: exit %d (%s), want %d", code, msg, ExitNotFound)
	}
}

// --wait follows the setup, through a dropped connection, to the end; with
// --json the progress goes to stderr and stdout holds the result alone.
func TestWorkspacesStatusWait(t *testing.T) {
	isolate(t)
	fastPolls(t)
	host, polls := statusServer(t, func(_ string, poll int) (int, string) {
		switch poll {
		case 1:
			return http.StatusOK, setupRunningBody
		case 2:
			return -1, ""
		case 3:
			return http.StatusBadGateway, `<html>Bad gateway</html>`
		}
		return http.StatusOK, setupCompleteBody
	})

	stdout, stderr, err := run("test", "--host", host, "--json", "workspaces", "status", "--wait")
	if err != nil {
		t.Fatalf("status --wait: %v (stderr %q)", err, stderr)
	}
	var got Workspace
	decodeOnly(t, "status --wait --json", stdout, &got)
	if got.ID != "demo-1" || got.Setup == nil || got.Setup.State != "complete" {
		t.Errorf("result = %+v (setup %+v)", got, got.Setup)
	}
	for _, want := range []string{"Waiting for Demo to be ready", "40%  Importing contracts", "40%  Could not check progress", "Demo is ready."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if polls.Load() != 4 {
		t.Errorf("polls = %d, want 4", polls.Load())
	}
}

// A setup that failed ends the wait with an error, and --json still says
// where it ended; a refusal that will not pass (here a 404) ends it at
// once instead of being retried.
func TestWorkspacesStatusWaitEnds(t *testing.T) {
	isolate(t)
	fastPolls(t)
	host, _ := statusServer(t, func(id string, _ int) (int, string) {
		if id == "prod-1" {
			return http.StatusOK, setupFailedBody
		}
		return http.StatusNotFound, `{"error":"Workspace not found","code":"NOT_FOUND"}`
	})

	stdout, _, err := run("test", "--host", host, "--json", "workspaces", "status", "Bank BV", "--wait")
	if err == nil || !strings.Contains(err.Error(), "the setup of Bank BV did not finish (Setup failed): open "+host+" to retry it") {
		t.Fatalf("want a failed setup, got %v", err)
	}
	if code, _ := Classify(err); code != ExitFailed {
		t.Errorf("exit %d, want %d", code, ExitFailed)
	}
	var got Workspace
	decodeOnly(t, "failed --json", stdout, &got)
	if got.Setup == nil || got.Setup.State != "failed" {
		t.Errorf("result = %+v", got)
	}

	start := time.Now()
	_, _, err = run("test", "--host", host, "workspaces", "status", "demo-1", "--wait")
	if code, _ := Classify(err); code != ExitNotFound {
		t.Errorf("404: exit %d (%v), want %d", code, err, ExitNotFound)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a 404 must end the wait at once")
	}
}

// A server that keeps failing is retried until the wait runs out, and the
// error then says what the last check met.
func TestSetupWaitGivesUpOnAServerThatStaysDown(t *testing.T) {
	isolate(t)
	fastPolls(t)
	host, polls := statusServer(t, func(string, int) (int, string) {
		return http.StatusServiceUnavailable, `{"error":"Unavailable"}`
	})

	_, _, err := run("test", "--host", host, "workspaces", "status", "prod-1", "--wait", "--wait-timeout", "50ms")
	if err == nil || !strings.Contains(err.Error(), "gave up waiting for Bank BV after 50ms; the last check failed: Unavailable. Setup continues on the server; check with `firmfact workspaces status prod-1 --wait`") {
		t.Fatalf("want a gave-up error, got %v", err)
	}
	if code, _ := Classify(err); code != ExitUnavailable {
		t.Errorf("exit %d, want %d", code, ExitUnavailable)
	}
	if polls.Load() < 2 {
		t.Errorf("polls = %d: the failures must be retried", polls.Load())
	}
}

// workspaces list shows each workspace's setup, and --json carries it.
func TestWorkspacesListSetupColumn(t *testing.T) {
	isolate(t)
	host, _ := statusServer(t, func(id string, _ int) (int, string) {
		if id == "demo-1" {
			return http.StatusOK, setupRunningBody
		}
		return http.StatusOK, setupCompleteBody
	})

	stdout, _, err := run("test", "--host", host, "workspaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "KIND        SETUP           DATA ACCESS") ||
		!strings.Contains(lines[1], "production  ready           yes") || !strings.Contains(lines[2], "demo        preparing, 40%  yes") {
		t.Errorf("table:\n%s", stdout)
	}

	stdout, _, err = run("test", "--host", host, "--json", "workspaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	var rows []Workspace
	decodeOnly(t, "list --json", stdout, &rows)
	if len(rows) != 2 || rows[0].Setup == nil || rows[0].Setup.State != "complete" || rows[1].Setup == nil || rows[1].Setup.Percentage != 40 {
		t.Errorf("rows = %+v", rows)
	}
}

// manyWorkspacesServer answers /api/v1/cli/me with n workspaces, ws-0 to
// ws-<n-1>, and each one's setup progress with setup, which gets the
// workspace's number.
func manyWorkspacesServer(t *testing.T, n int, setup func(i int) (status int, body string)) (host string, polls *atomic.Int32) {
	t.Helper()
	polls = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/cli/me" {
			rows := make([]string, n)
			for i := range rows {
				rows[i] = fmt.Sprintf(`{"id":"ws-%d","name":"W%d","mcp_available":true}`, i, i)
			}
			fmt.Fprintf(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[%s]}}`, strings.Join(rows, ","))
			return
		}
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/api/v1/cli/workspaces/ws-%d/setup_progress", &i); err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		polls.Add(1)
		status, body := setup(i)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	storedToken(t, srv.URL, time.Hour)
	return srv.URL, polls
}

// workspaces list reads the setup states a few at a time rather than one
// after another: the first alone, then setupReads at once, never more, and
// each state lands on its own workspace's row.
func TestWorkspacesListReadsSetupStatesAtOnce(t *testing.T) {
	isolate(t)
	const n = 1 + 2*setupReads
	var arrived, inFlight, most atomic.Int32
	host, _ := manyWorkspacesServer(t, n, func(i int) (int, string) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			if m := most.Load(); now <= m || most.CompareAndSwap(m, now) {
				break
			}
		}
		// After the first, each request waits for the rest of its batch
		// of setupReads, so they are all in flight together; one read at
		// a time would wait out the limit here.
		if i > 0 {
			batch := (arrived.Add(1)-1)/setupReads + 1
			for deadline := time.Now().Add(3 * time.Second); arrived.Load() < batch*setupReads && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		}
		return http.StatusOK, fmt.Sprintf(`{"percentage":%d,"liveness":"running"}`, i*10)
	})

	stdout, stderr, err := run("test", "--host", host, "--json", "workspaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	if most.Load() != setupReads {
		t.Errorf("at most %d setup reads at once, want %d", most.Load(), setupReads)
	}
	var rows []Workspace
	decodeOnly(t, "list --json", stdout, &rows)
	if len(rows) != n {
		t.Fatalf("%d rows, want %d", len(rows), n)
	}
	for i, row := range rows {
		if row.Setup == nil || row.Setup.Percentage != float64(i*10) {
			t.Errorf("row %d (%s): setup %+v", i, row.ID, row.Setup)
		}
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
}

// A failure among the reads going at once stops the rest, with one note,
// and the list still prints.
func TestWorkspacesListStopsReadingAtAFailure(t *testing.T) {
	isolate(t)
	const n = 1 + 2*setupReads
	host, polls := manyWorkspacesServer(t, n, func(i int) (int, string) {
		switch {
		case i == 2:
			return http.StatusInternalServerError, `{"error":"Internal error"}`
		case i > 0:
			time.Sleep(100 * time.Millisecond)
		}
		return http.StatusOK, setupCompleteBody
	})

	stdout, stderr, err := run("test", "--host", host, "workspaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stderr, "note: could not read the setup states") != 1 {
		t.Errorf("stderr = %q", stderr)
	}
	if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != n+1 || !strings.Contains(lines[1], "ready") {
		t.Errorf("table:\n%s", stdout)
	}
	if got := polls.Load(); got > 1+setupReads {
		t.Errorf("%d setup reads, want no more after the batch that failed", got)
	}
}

// A server without the progress endpoint still lists the workspaces: the
// setup is unknown, the reason is noted once, and no more is asked.
func TestWorkspacesListWithoutSetupEndpoint(t *testing.T) {
	isolate(t)
	host, polls := statusServer(t, func(string, int) (int, string) {
		return http.StatusNotFound, `<!DOCTYPE html><title>Not found</title>`
	})

	stdout, stderr, err := run("test", "--host", host, "workspaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stdout, "unknown") != 2 || strings.Count(stderr, "note: could not read the setup states") != 1 {
		t.Errorf("stdout %q, stderr %q", stdout, stderr)
	}
	if polls.Load() != 1 {
		t.Errorf("setup requests = %d, want 1", polls.Load())
	}

	stdout, _, err = run("test", "--host", host, "--json", "workspaces", "list")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	decodeOnly(t, "list --json", stdout, &rows)
	if _, ok := rows[0]["setup"]; ok || len(rows) != 2 {
		t.Errorf("rows = %v, want no setup", rows)
	}
}

// transientError tells what a long wait should retry from what it should
// not: what may pass by itself, against a refusal or a certificate that
// will not change.
func TestTransientError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&httpx.Error{Kind: httpx.Refused}, true},
		{&httpx.Error{Kind: httpx.AnswerTimeout}, true},
		{&httpx.Error{Kind: httpx.Untrusted}, false},
		{&api.Error{Status: http.StatusServiceUnavailable}, true},
		{&api.Error{Status: http.StatusBadGateway}, true},
		{&api.Error{Status: http.StatusTooManyRequests}, true},
		{&api.Error{Status: http.StatusNotImplemented}, false},
		{&api.Error{Status: http.StatusNotFound}, false},
		{&api.Error{Status: http.StatusUnauthorized}, false},
		{&api.SessionEndedError{Name: "firmfact"}, false},
		{api.ErrNotSignedIn, false},
		{context.Canceled, false},
		{fmt.Errorf("reading: %w", &api.Error{Status: http.StatusGatewayTimeout}), true},
	}
	for _, c := range cases {
		if got := transientError(c.err); got != c.want {
			t.Errorf("transientError(%T %v) = %v, want %v", c.err, c.err, got, c.want)
		}
	}
}

// The live setup line fits one row of the terminal, less its last column:
// the bar gives up room first, then the activity, which is cut with an
// ellipsis.
func TestProgressLineFitsTheTerminal(t *testing.T) {
	const activity = "Importing contracts, vendors and the allocations between them"
	for _, mode := range []ui.ColorMode{ui.NoColor, ui.TrueColor} {
		for _, width := range []int{120, 80, 50, 30, 24} {
			line := progressLine(mode, width, 40, activity)
			if n := ui.Columns(line); n > width-1 {
				t.Errorf("mode %d, width %d: %d columns in %q", mode, width, n, line)
			}
			if !strings.Contains(line, " 40%") {
				t.Errorf("mode %d, width %d: no percentage in %q", mode, width, line)
			}
		}
		if line := progressLine(mode, 120, 40, activity); !strings.HasSuffix(line, "  "+activity) {
			t.Errorf("mode %d: a wide terminal cut the activity: %q", mode, line)
		}
		if line := progressLine(mode, 80, 40, activity); !strings.HasSuffix(line, "…") || ui.Columns(line) < 75 {
			t.Errorf("mode %d: at 80 columns %q, want the activity cut to fit", mode, line)
		}
	}
	if got, want := progressLine(ui.NoColor, 80, 100, "Ready"), "  ["+strings.Repeat("#", 30)+"] 100%  Ready"; got != want {
		t.Errorf("done = %q, want %q", got, want)
	}
	if got, want := progressLine(ui.NoColor, 30, 5, ""), "  [#---------]   5%"; got != want {
		t.Errorf("no activity at 30 columns = %q, want %q", got, want)
	}
}
