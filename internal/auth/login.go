// Package auth runs `firmfact login`: OAuth authorization code + PKCE (S256)
// with an RFC 8252 loopback redirect, the same flow Claude Code uses against
// firmfact; the consent page in the browser picks the default workspace.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/firmfact/cli/internal/api"
)

// DefaultTimeout is how long Login waits for the browser when Options.Timeout
// is zero.
const DefaultTimeout = 5 * time.Minute

type Options struct {
	// OpenBrowser is false for --no-browser: the URL is printed only.
	OpenBrowser bool
	Out         io.Writer
	// Timeout bounds the wait for the browser's callback; zero means
	// DefaultTimeout.
	Timeout time.Duration
}

// LaunchBrowser opens u in the user's browser. It is a variable so tests can
// stand in for the browser and follow the redirect themselves.
var LaunchBrowser = openSystemBrowser

// Login waits for the browser until opts.Timeout, or until ctx is cancelled
// (Ctrl-C).
func Login(ctx context.Context, c *api.Client, opts Options) (*api.TokenResponse, error) {
	verifier := randomString(48)
	challenge := base64.RawURLEncoding.EncodeToString(sha256Sum(verifier))
	state := randomString(24)

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("could not open a local port for the login callback: %w", err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	authURL := c.Host + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {api.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"mcp"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	type result struct {
		code string
		err  error
	}
	// The first answer that belongs to this login ends the wait. Anything
	// else on the loopback port (another local process, a refreshed tab, a
	// second approval) is answered and ignored: it must neither end the
	// login nor block its handler, which would hold up the shutdown below.
	done := make(chan result, 1)
	var once sync.Once
	deliver := func(res result) (first bool) {
		once.Do(func() {
			done <- res
			first = true
		})
		return first
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// The page is the end of the sign-in: nothing may cache it, and no
		// link out of it may carry the code in a Referer.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Only GET is accepted here.", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if !sameState(q.Get("state"), state) {
			http.Error(w, "This does not belong to the sign-in in progress, so it was ignored. Use the link that `"+api.Name+" login` printed.", http.StatusBadRequest)
			return
		}
		var res result
		status, page := http.StatusOK, "Signed in to the firmfact CLI. You can close this tab."
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("login failed: %s", firstNonEmpty(q.Get("error_description"), q.Get("error")))
			page = "Login was not completed. You can close this tab."
		case q.Get("code") == "":
			// Exchanging nothing would only come back as a puzzling
			// invalid_grant.
			res.err = errors.New("login failed: the sign-in answer carried no authorisation code")
			status, page = http.StatusBadRequest, "The sign-in answer carried no authorisation code. Run `"+api.Name+" login` again."
		default:
			res.code = q.Get("code")
		}
		if !deliver(res) {
			status, page = http.StatusConflict, "This sign-in has already been handled. You can close this tab."
		}
		// http.Error writes a plain-text page with nosniff, which suits
		// every answer here, not only the failures.
		http.Error(w, page, status)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	// The link is printed either way, on its own line, so it can be copied
	// into a browser when opening one did not work. It has to be a browser
	// on this machine: the redirect comes back to 127.0.0.1.
	var launchErr error
	if opts.OpenBrowser {
		launchErr = LaunchBrowser(authURL)
	}
	switch {
	case !opts.OpenBrowser:
		fmt.Fprint(opts.Out, "Open this link in a browser on this machine to sign in:\n\n")
	case launchErr != nil:
		fmt.Fprintf(opts.Out, "Could not open your browser (%v).\nOpen this link in a browser on this machine to sign in:\n\n", launchErr)
	default:
		fmt.Fprint(opts.Out, "Opened your browser to sign in. If it did not open, use this link:\n\n")
	}
	fmt.Fprintf(opts.Out, "  %s\n\n", authURL)
	fmt.Fprintf(opts.Out, "Waiting up to %s for you to approve...\n", waitSpan(timeout))

	var res result
	select {
	case res = <-done:
	case <-time.After(timeout):
		return nil, errors.New("login timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, res.err
	}
	return c.ExchangeToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// sameState compares in constant time: a wrong state leaves the login
// waiting, so a local process can try as many as it likes and could
// otherwise time its guesses.
func sameState(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// waitSpan says how long login waits the way people say it: "5 minutes",
// not "5m0s".
func waitSpan(d time.Duration) string {
	unit := func(n time.Duration, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return fmt.Sprintf("%d %ss", n, name)
	}
	switch {
	case d >= time.Minute && d%time.Minute == 0:
		return unit(d/time.Minute, "minute")
	case d >= time.Second && d%time.Second == 0:
		return unit(d/time.Second, "second")
	}
	return d.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// openSystemBrowser hands u to the platform's opener as one argument; no
// shell ever parses it. It takes no context on purpose: the opener can be
// the browser itself, which must outlive the login.
//
//nolint:gosec,noctx // G204: fixed programs, one argument; noctx: see above
func openSystemBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}
