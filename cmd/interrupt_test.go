package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
)

// runContext is run with a context the test cancels, as Ctrl-C would.
func runContext(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	err = NewRootCommand(Build{Version: "test"}, args, IOStreams{Out: &out, Err: &errOut}).ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// Ctrl-C during the Demo setup wait stops the wait, not the setup: the user
// is told they are signed in and how to check on it later.
func TestInterruptedSetupWaitSaysSetupContinues(t *testing.T) {
	isolate(t)
	prev := setupPollInterval
	setupPollInterval = time.Hour // only the interrupt can end the wait
	defer func() { setupPollInterval = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &signupServer{progress: func(int) string {
		time.AfterFunc(20*time.Millisecond, cancel)
		return `{"percentage":40,"current_activity":"Importing contracts","liveness":"running"}`
	}}
	srv := f.start(t)

	done := make(chan struct{})
	var stdout, stderr string
	var err error
	go func() {
		defer close(done)
		stdout, stderr, err = runContext(ctx, signupArgs(srv.URL)...)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("signup did not return after the interrupt")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "You are signed in; setup continues on the server. Check with `firmfact workspaces status`.") {
		t.Errorf("stdout = %q", stdout)
	}
	if tok, _ := config.LoadToken(srv.URL); tok == nil || tok.AccessToken != "at" {
		t.Errorf("the sign-in must be kept; stored token = %+v", tok)
	}
}

// A prompt waiting for an answer ends when the context is cancelled, even
// though the read of stdin itself cannot be interrupted.
func TestPromptEndsWhenCancelled(t *testing.T) {
	stdin, w := io.Pipe() // nothing is ever typed
	defer w.Close()
	app := &App{In: stdin, Out: io.Discard}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	errc := make(chan error, 1)
	go func() {
		_, err := app.Confirm(ctx, "Go ahead?")
		errc <- err
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt did not end on cancel")
	}
}

// Ctrl-C while logout revokes the session keeps the stored token: whether
// the session ended is unknown, and a second logout can still end it.
func TestInterruptedLogoutKeepsTheToken(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the user gives up while the server is slow to answer
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	storedToken(t, srv.URL, time.Hour)

	stdout, _, err := runContext(ctx, "--host", srv.URL, "logout")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if strings.Contains(stdout, "Signed out") {
		t.Errorf("stdout = %q", stdout)
	}
	if tok, _ := config.LoadToken(srv.URL); tok == nil {
		t.Error("the stored token must be kept")
	}
}
