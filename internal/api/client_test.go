package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
)

// useFileStore keeps tokens in a file in scratch space, never in the
// user's keyring. The CLI creates the directory, as it would for a user, so
// its mode does not depend on the umask.
func useFileStore(t *testing.T) {
	t.Helper()
	t.Setenv("FIRMFACT_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("FIRMFACT_TOKEN_STORE", "file")
	t.Setenv("FIRMFACT_TOKEN", "")
}

// A 401 triggers one refresh (refresh_token grant, client_id firmfact-cli),
// the request is retried with the new token, and the rotated pair is stored.
func TestRefreshesOnUnauthorized(t *testing.T) {
	useFileStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-rt" || r.Form.Get("client_id") != ClientID {
				t.Errorf("refresh form = %v", r.Form)
			}
			io.WriteString(w, `{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600,"token_type":"Bearer"}`)
		case "/api/v1/cli/me":
			if r.Header.Get("Authorization") != "Bearer new-at" {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"Token expired"}`)
				return
			}
			io.WriteString(w, `{"data":{"user":{"email":"user@yourfirm.example"}}}`)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	if err := c.SetToken(&config.Token{AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data struct {
			User struct {
				Email string `json:"email"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, &out, true); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if out.Data.User.Email != "user@yourfirm.example" {
		t.Errorf("got %+v", out)
	}
	stored, _ := config.LoadToken(c.Host)
	if stored.AccessToken != "new-at" || stored.RefreshToken != "new-rt" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestErrorCarriesServerCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"error":"That code is invalid or has expired.","code":"INVALID_CODE"}`)
	}))
	defer srv.Close()

	err := New(srv.URL).JSON(context.Background(), http.MethodPost, "/api/v1/signup/confirm", map[string]any{}, nil, false)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "INVALID_CODE" || apiErr.Status != 422 {
		t.Fatalf("got %#v", err)
	}
}

// page is what a fake host answers with.
type page struct{ contentType, body string }

// A 404 for one of the CLI's own endpoints without the API's error code is
// a host that lacks the endpoint: a firmfact older than the CLI, or not
// firmfact. The user hears that, not the host's page-not-found text. The
// API's own 404 (with its code) and a 404 elsewhere keep their message.
func TestMissingCLIEndpointIsUnsupported(t *testing.T) {
	useFileStore(t)
	pages := map[string]page{
		"html":         {"text/html", `<!DOCTYPE html><title>Not found</title><h1>The page you're looking for doesn't exist.</h1>`},
		"json":         {"application/json", `{"error":"The page you're looking for doesn't exist."}`},
		"rails":        {"application/json", `{"status":404,"error":"Not Found"}`},
		"with details": {"application/json", `{"error":"Not Found","details":{"path":"unknown"}}`},
		"empty":        {"text/plain", ``},
	}
	var answer atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := answer.Load().(page)
		w.Header().Set("Content-Type", p.contentType)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, p.body)
	}))
	defer srv.Close()
	c := New(srv.URL)
	if err := c.SetToken(&config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	want := srv.URL + " does not support this version of the CLI yet (or --host is wrong)"
	requests := []struct {
		method, path string
		auth         bool
	}{
		{http.MethodGet, "/api/v1/cli/me", true},
		{http.MethodGet, "/api/v1/cli/workspaces/Demo/setup_progress", true},
		{http.MethodGet, "/api/v1/signup/options?locale=en", false},
		{http.MethodPost, "/api/v1/signup", false},
		{http.MethodPost, "/api/v1/signup/confirm", false},
		{http.MethodPost, "/mcp", true},
	}
	for name, p := range pages {
		answer.Store(p)
		for _, r := range requests {
			err := c.JSON(context.Background(), r.method, r.path, map[string]any{}, nil, r.auth)
			var apiErr *Error
			if !errors.As(err, &apiErr) || !apiErr.Unsupported || apiErr.Status != http.StatusNotFound {
				t.Errorf("%s page, %s %s: got %#v, want an unsupported 404", name, r.method, r.path, err)
				continue
			}
			if err.Error() != want {
				t.Errorf("%s page, %s %s: message %q, want %q", name, r.method, r.path, err, want)
			}
		}
	}

	// The API's own "not found", and a 404 outside the CLI's endpoints.
	kept := []struct{ path, body, want string }{
		{"/api/v1/cli/workspaces/nope/setup_progress", `{"error":"Workspace not found","code":"NOT_FOUND"}`, "Workspace not found"},
		{"/oauth/revoke", `{"error":"Not Found"}`, "Not Found"},
		{"/mcpx", ``, "Not Found"},
		{"/api/v1/clients", ``, "Not Found"},
	}
	for _, k := range kept {
		answer.Store(page{"application/json", k.body})
		err := c.JSON(context.Background(), http.MethodGet, k.path, nil, nil, true)
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Unsupported || err.Error() != k.want {
			t.Errorf("%s: got %#v, want the message %q", k.path, err, k.want)
		}
	}
}

// The server's error text reaches the terminal, so its control characters are
// escaped: the message, and the field names and values of details.
func TestErrorEscapesTerminalControls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"error":"Invalid\u001b]52;c;cm0gLXJmIH4=\u0007","details":{"name\u009b2K":"is \u001b[31mtaken"}}`)
	}))
	defer srv.Close()

	err := New(srv.URL).JSON(context.Background(), http.MethodPost, "/api/v1/signup", map[string]any{}, nil, false)
	if err == nil {
		t.Fatal("want an error")
	}
	want := `Invalid\u001b]52;c;cm0gLXJmIH4=\u0007 (name\u009b2K is \u001b[31mtaken)`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// cancelAfter cancels a context after d and returns the time it did.
func cancelAfter(d time.Duration) (context.Context, <-chan time.Time) {
	ctx, cancel := context.WithCancel(context.Background())
	at := make(chan time.Time, 1)
	time.AfterFunc(d, func() {
		at <- time.Now()
		cancel()
	})
	return ctx, at
}

// Ctrl-C cancels the command's context, and a request to a server that never
// answers must give up at once rather than at the 60 s client timeout.
func TestCancelAbortsAHangingRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancelled := cancelAfter(50 * time.Millisecond)
	err := New(srv.URL).JSON(ctx, http.MethodGet, "/api/v1/signup/options", nil, nil, false)
	returned := time.Now()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if err.Error() != context.Canceled.Error() {
		t.Errorf("an interrupted request is not a connection problem; got %q", err)
	}
	if lag := returned.Sub(<-cancelled); lag > 100*time.Millisecond {
		t.Errorf("request returned %s after the cancel, want under 100ms", lag)
	}
}

// The wait for a 429's Retry-After (up to 30 s) ends when the context is
// cancelled, and no retry is sent.
func TestCancelEndsTheRateLimitWait(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancelled := cancelAfter(50 * time.Millisecond)
	err := New(srv.URL).JSON(ctx, http.MethodGet, "/api/v1/signup/options", nil, nil, false)
	returned := time.Now()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if lag := returned.Sub(<-cancelled); lag > 100*time.Millisecond {
		t.Errorf("the retry wait ended %s after the cancel, want under 100ms", lag)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1 (no retry after the cancel)", n)
	}
}

// A redirect is not followed, so the bearer token never reaches the address
// it points to: Go keeps the Authorization header on a redirect to the same
// host name, even from https to plain http. The error says where it
// pointed.
func TestRedirectIsNotFollowed(t *testing.T) {
	useFileStore(t)

	var hits atomic.Int32
	var gotAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		io.WriteString(w, `{"data":{}}`)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/me":
			http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
		default:
			http.Redirect(w, r, "/users/sign_in", http.StatusFound)
		}
	}))
	defer origin.Close()

	c := New(origin.URL)
	if err := c.SetToken(&config.Token{AccessToken: "secret", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, nil, true)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTemporaryRedirect {
		t.Fatalf("want the 307 as an *Error, got %#v", err)
	}
	if want := origin.URL + " redirects to " + target.URL + ", which the CLI does not follow; check the host"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d request(s) (Authorization %q), want none", n, gotAuth.Load())
	}

	// A redirect within the host names the path.
	err = c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/workspaces", nil, nil, true)
	if want := origin.URL + " redirects to /users/sign_in, which the CLI does not follow; check the host"; err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

// A renewal that never reached the server says nothing about the session:
// the user is told about the connection, not asked to sign in again.
func TestRefreshWithoutAnAnswerIsAConnectionError(t *testing.T) {
	useFileStore(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	host := srv.URL
	srv.Close() // nothing listens there any more

	c := New(host)
	if err := c.SetToken(&config.Token{AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, nil, true)
	var netErr *httpx.Error
	if !errors.As(err, &netErr) || netErr.Kind != httpx.Refused {
		t.Fatalf("want a refused connection, got %v", err)
	}
	if strings.Contains(err.Error(), "login") {
		t.Errorf("a connection problem must not send the user to sign in again: %q", err)
	}
}

// rotatingServer is an OAuth server that rotates refresh tokens, as the
// firmfact server does: each one renews once, and a second use is refused
// with invalid_grant.
type rotatingServer struct {
	mu        sync.Mutex
	access    string
	refresh   string
	renewals  atomic.Int32
	slow      time.Duration // how long a renewal takes
	tokenCode int           // when set, /oauth/token answers only this status
}

func (s *rotatingServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			n := s.renewals.Add(1)
			_ = r.ParseForm()
			time.Sleep(s.slow)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.tokenCode != 0 {
				w.WriteHeader(s.tokenCode)
				io.WriteString(w, `{"error":"server_error"}`)
				return
			}
			if r.Form.Get("refresh_token") != s.refresh {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant","error_description":"The provided authorization grant is invalid, expired or revoked."}`)
				return
			}
			s.access, s.refresh = fmt.Sprintf("at-%d", n), fmt.Sprintf("rt-%d", n)
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600,"token_type":"Bearer"}`, s.access, s.refresh)
		case "/api/v1/cli/me":
			s.mu.Lock()
			ok := r.Header.Get("Authorization") == "Bearer "+s.access
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// expiredClient is a client for host whose stored token has expired.
func expiredClient(t *testing.T, host, access, refresh string) *Client {
	t.Helper()
	if err := config.SaveToken(host, &config.Token{AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return New(host)
}

// Two commands whose token expired renew it once between them, and both
// go on with the new one. Without the lock the slower would send the
// refresh token the faster had just used up, and be refused.
func TestConcurrentRenewalsRotateOnce(t *testing.T) {
	useFileStore(t)
	s := &rotatingServer{access: "at-0", refresh: "rt-0", slow: 100 * time.Millisecond}
	srv := s.start(t)
	clients := []*Client{expiredClient(t, srv.URL, "at-0", "rt-0"), New(srv.URL)}

	var wg sync.WaitGroup
	for _, c := range clients {
		if _, err := c.Token(); err != nil { // both hold the expired token
			t.Fatal(err)
		}
		wg.Go(func() {
			if err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, nil, true); err != nil {
				t.Errorf("request failed: %v", err)
			}
		})
	}
	wg.Wait()
	if n := s.renewals.Load(); n != 1 {
		t.Errorf("%d renewals, want 1", n)
	}
}

// A renewal the server refuses ends the session, in a line that names the
// way back in with the name the CLI was run by.
func TestRefusedRenewalEndsTheSession(t *testing.T) {
	useFileStore(t)
	s := &rotatingServer{access: "at-0", refresh: "rt-current"}
	srv := s.start(t)
	prev := Name
	Name = "fct"
	t.Cleanup(func() { Name = prev })

	c := expiredClient(t, srv.URL, "at-old", "rt-used-up")
	err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, nil, true)
	var ended *SessionEndedError
	if !errors.As(err, &ended) || err.Error() != "Your session has ended. Run fct login." {
		t.Fatalf("got %v", err)
	}
}

// Without a sign-in, a request that needs one names the way in with the
// name the CLI was run by, and is still ErrNotSignedIn to errors.Is.
func TestNotSignedInNamesTheCommand(t *testing.T) {
	useFileStore(t)
	prev := Name
	Name = "fct"
	t.Cleanup(func() { Name = prev })

	err := New("https://firmfact.example").JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, nil, true)
	if !errors.Is(err, ErrNotSignedIn) || err.Error() != "not signed in: run `fct login` (or `fct signup`)" {
		t.Fatalf("got %v", err)
	}
}

// A server that fails while renewing has not ended the session: the user
// is told to try again, not to sign in.
func TestServerErrorDuringRenewalIsNotTheEnd(t *testing.T) {
	useFileStore(t)
	s := &rotatingServer{access: "at-0", refresh: "rt-0", tokenCode: http.StatusBadGateway}
	srv := s.start(t)

	c := expiredClient(t, srv.URL, "at-0", "rt-0")
	err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, nil, true)
	if err == nil || !strings.Contains(err.Error(), "could not renew your session") || strings.Contains(err.Error(), "login") {
		t.Fatalf("got %v", err)
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		t.Errorf("the renewal's answer must not pass for the request's: %#v", apiErr)
	}
	if tok, _ := config.LoadToken(srv.URL); tok == nil || tok.RefreshToken != "rt-0" {
		t.Errorf("the stored sign-in must be kept: %+v", tok)
	}
}

// busyServer answers the first `busy` requests with status and header,
// then with an empty JSON object, and counts the requests.
type busyServer struct {
	status   int
	header   func(h http.Header)
	busy     int32
	requests atomic.Int32
}

func (s *busyServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requests.Add(1) <= s.busy {
			if s.header != nil {
				s.header(w.Header())
			}
			w.WriteHeader(s.status)
			io.WriteString(w, `{"error":"Unable to acquire capacity, please retry"}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func retryIn(v string) func(http.Header) {
	return func(h http.Header) { h.Set("Retry-After", v) }
}

// A 503 or a 429 with a short Retry-After is waited out and sent again:
// the server turned the request away without running it. With Busy set,
// the client says so before each wait; without, it waits in silence.
func TestBusyServerIsWaitedOut(t *testing.T) {
	cases := map[string]struct {
		status int
		say    bool
	}{
		"503 said":   {http.StatusServiceUnavailable, true},
		"429 silent": {http.StatusTooManyRequests, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &busyServer{status: tc.status, header: retryIn("1"), busy: 1}
			srv := s.start(t)
			c := New(srv.URL)
			var said bytes.Buffer
			if tc.say {
				c.Busy = &said
			}
			start := time.Now()
			if err := c.JSON(context.Background(), http.MethodPost, "/mcp", map[string]any{}, nil, false); err != nil {
				t.Fatalf("want the retry to succeed, got %v", err)
			}
			if n := s.requests.Load(); n != 2 {
				t.Errorf("requests = %d, want 2", n)
			}
			if waited := time.Since(start); waited < time.Second {
				t.Errorf("retried after %s, want the 1s the server asked for", waited)
			}
			want := ""
			if tc.say {
				want = "\rServer busy; retrying in 1s...\n"
			}
			if said.String() != want {
				t.Errorf("said %q, want %q", said.String(), want)
			}
		})
	}
}

// A Retry-After given as an HTTP-date is waited out too, measured from the
// answer's own Date.
func TestRetryAfterDateIsWaitedOut(t *testing.T) {
	s := &busyServer{status: http.StatusServiceUnavailable, busy: 1, header: func(h http.Header) {
		now := time.Now().UTC()
		h.Set("Date", now.Format(http.TimeFormat))
		h.Set("Retry-After", now.Add(time.Second).Format(http.TimeFormat))
	}}
	srv := s.start(t)
	c := New(srv.URL)
	var said bytes.Buffer
	c.Busy = &said
	if err := c.JSON(context.Background(), http.MethodGet, "/api/v1/signup/options", nil, nil, false); err != nil {
		t.Fatalf("want the retry to succeed, got %v", err)
	}
	if n := s.requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
	if want := "\rServer busy; retrying in 1s...\n"; said.String() != want {
		t.Errorf("said %q, want %q", said.String(), want)
	}
}

// Retry-After is a number of seconds or an HTTP-date; a date is measured
// from the answer's Date header when there is one, else from the clock.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	date := func(d time.Duration) string { return now.Add(d).Format(http.TimeFormat) }
	cases := []struct {
		retryAfter, date string
		want             time.Duration
		ok               bool
	}{
		{"5", "", 5 * time.Second, true},
		{" 0 ", "", 0, true},
		{date(7 * time.Second), "", 7 * time.Second, true},
		// The answer's Date wins over a clock an hour off.
		{date(time.Hour + 3*time.Second), date(time.Hour), 3 * time.Second, true},
		{"Sunday, 27-Sep-26 10:00:04 GMT", "", 4 * time.Second, true}, // RFC 850
		{"Sun Sep 27 10:00:02 2026", "", 2 * time.Second, true},       // asctime
		{date(-time.Minute), "", 0, true},
		{"-1", "", 0, false},
		{"soon", "", 0, false},
		{"", "", 0, false},
		{"99999999999999999999", "", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		h.Set("Retry-After", tc.retryAfter)
		if tc.date != "" {
			h.Set("Date", tc.date)
		}
		got, ok := retryAfter(h, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Retry-After %q, Date %q: got %s, %v; want %s, %v", tc.retryAfter, tc.date, got, ok, tc.want, tc.ok)
		}
	}
}

// A 503 without Retry-After is a server that is down, not busy, and one
// that asks for more than 30 s is not waited for: either fails at once
// with the server's own words, or the CLI's for a rate limit.
func TestLongOrNoRetryAfterIsNotWaitedFor(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header func(http.Header)
		want   string
	}{
		{"503 without Retry-After", http.StatusServiceUnavailable, nil, "Unable to acquire capacity, please retry"},
		{"503 in a minute", http.StatusServiceUnavailable, retryIn("60"), "Unable to acquire capacity, please retry"},
		{"429 at a date", http.StatusTooManyRequests, func(h http.Header) {
			now := time.Now().UTC()
			h.Set("Date", now.Format(http.TimeFormat))
			h.Set("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat))
		}, "too many requests; wait a while and try again (retry after 90s)"},
	}
	for _, tc := range cases {
		s := &busyServer{status: tc.status, header: tc.header, busy: 3}
		srv := s.start(t)
		c := New(srv.URL)
		var said bytes.Buffer
		c.Busy = &said
		err := c.JSON(context.Background(), http.MethodPost, "/mcp", map[string]any{}, nil, false)
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status || err.Error() != tc.want {
			t.Errorf("%s: got %v, want a %d saying %q", tc.name, err, tc.status, tc.want)
		}
		if n := s.requests.Load(); n != 1 {
			t.Errorf("%s: requests = %d, want 1", tc.name, n)
		}
		if said.Len() != 0 {
			t.Errorf("%s: said %q about a wait that did not happen", tc.name, said.String())
		}
	}
}

// Every request names the CLI's version in its User-Agent, and the commit
// it was built from when the build recorded one.
func TestUserAgentNamesTheBuild(t *testing.T) {
	prevVersion, prevCommit := Version, Commit
	defer func() { Version, Commit = prevVersion, prevCommit }()
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.UserAgent())
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	cases := []struct {
		version, commit, want string
	}{
		{"0.4.0", "0123456789abcdef0123456789abcdef01234567", "firmfact-cli/0.4.0 (commit 0123456789ab)"},
		{"0.3.1-0.20260927101010-b005a6f00000", "b005a6f00000", "firmfact-cli/0.3.1-0.20260927101010-b005a6f00000 (commit b005a6f00000)"},
		{"0.3.0", "", "firmfact-cli/0.3.0"},
	}
	for _, c := range cases {
		Version, Commit = c.version, c.commit
		if err := New(srv.URL).JSON(context.Background(), http.MethodGet, "/api/v1/cli/version", nil, nil, false); err != nil {
			t.Fatal(err)
		}
		if got.Load() != c.want {
			t.Errorf("User-Agent = %q, want %q", got.Load(), c.want)
		}
	}
}
