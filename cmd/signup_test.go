package cmd

import (
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/config"
)

// A signup from a domain the server does not know is held for a person to
// review. The run ends there, successfully, with no code to wait for and
// nothing confirmed or stored.
func TestSignupHeldForReview(t *testing.T) {
	isolate(t)
	f := &signupServer{created: `{"success":true,"status":"held_for_review"}`}
	srv := f.start(t)

	stdout, _, err := run("test", scriptedSignupArgs(srv.URL, "--no-password")...)
	if code, _ := Classify(err); code != 0 {
		t.Fatalf("exit %d (%v), want 0", code, err)
	}
	if !strings.Contains(stdout, "yourfirm.example is not on our list of known work domains yet, so a person will\nreview your signup.") {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.Contains(stdout, "6-digit code") || strings.Contains(stdout, "To finish, run") {
		t.Errorf("a held signup is told to wait for a code: %q", stdout)
	}
	if f.confirms.Load() != 0 {
		t.Errorf("%d confirmations sent", f.confirms.Load())
	}
	assertTokenGone(t, srv.URL)
}

// A confirmed email that the server will not sign in from the CLI (SSO or
// two-factor authentication, or a workspace still to be set up on the web)
// ends the run with what to do next, and without a sign-in or a wait.
func TestSignupConfirmedWithoutASignIn(t *testing.T) {
	cases := map[string]struct {
		answer string
		want   string
	}{
		"browser login required": {
			`{"status":"confirmed","reason":"browser_login_required"}`,
			"Your email is confirmed. Your organisation signs in with SSO or two-factor\nauthentication, so finish with `firmfact login` in a browser.\n",
		},
		"workspace set up on the web": {
			`{"status":"confirmed"}`,
			"Your email is confirmed. Finish setting up your workspace on the web, then run `firmfact login`.\n",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			f := &signupServer{confirmed: c.answer}
			srv := f.start(t)

			stdout, _, err := run("test", signupArgs(srv.URL)...)
			if err != nil {
				t.Fatalf("signup: %v", err)
			}
			if !strings.HasSuffix(stdout, c.want) {
				t.Errorf("stdout = %q, want it to end %q", stdout, c.want)
			}
			if f.polls.Load() != 0 {
				t.Errorf("%d setup polls", f.polls.Load())
			}
			assertTokenGone(t, srv.URL)
			if cfg, err := config.Load(); err != nil || cfg.Profile("").Workspace != "" {
				t.Errorf("default workspace saved: %+v, %v", cfg, err)
			}
		})
	}
}

// A setup the server reports stalled ends the wait like one that failed:
// with the command failed, pointing at the web to retry, and the user
// still signed in.
func TestSignupStopsAtAStalledSetup(t *testing.T) {
	isolate(t)
	fastPolls(t)
	f := &signupServer{progress: func(poll int) string {
		if poll < 2 {
			return `{"percentage":60,"current_activity":"Processing demo documents","liveness":"running"}`
		}
		return `{"percentage":60,"message":"Setup has not moved for 15 minutes","liveness":"stalled"}`
	}}
	srv := f.start(t)

	stdout, _, err := run("test", signupArgs(srv.URL)...)
	want := "the setup of your Demo workspace did not finish (Setup has not moved for 15 minutes): open " + srv.URL + " to retry it"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if code, _ := Classify(err); code != ExitFailed {
		t.Errorf("exit %d, want %d", code, ExitFailed)
	}
	if !strings.Contains(stdout, "60%  Processing demo documents\n") || strings.Contains(stdout, "Ready") {
		t.Errorf("stdout = %q", stdout)
	}
	if tok, err := config.LoadToken(srv.URL); err != nil || tok == nil || tok.AccessToken != "at" {
		t.Errorf("stored %+v, %v; want the sign-in kept", tok, err)
	}
}
