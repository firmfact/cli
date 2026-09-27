package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
)

// visit is one request the stand-in browser makes to the loopback
// redirect. The state is taken from the sign-in link unless the query sets
// one; an empty state in the query leaves it out.
type visit struct {
	method string // GET when empty
	query  url.Values
}

// answer is what the loopback server said to one visit.
type answer struct {
	status int
	header http.Header
	body   string
	err    error
}

// browser stands in for the user's browser: once the sign-in link is
// opened it makes its visits in order, as a user approving (or anything
// else on this machine) would.
type browser struct {
	visits []visit

	link      atomic.Value // the query of the last link opened
	challenge atomic.Value // its code_challenge
	mu        sync.Mutex
	answers   []answer
	finished  chan struct{}
}

func newBrowser(visits ...visit) *browser {
	return &browser{visits: visits, finished: make(chan struct{})}
}

func (b *browser) open(authURL string) error {
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	q := u.Query()
	b.link.Store(q)
	b.challenge.Store(q.Get("code_challenge"))
	go func() {
		defer close(b.finished)
		for _, v := range b.visits {
			a := call(v.method, q.Get("redirect_uri"), q.Get("state"), v.query)
			b.mu.Lock()
			b.answers = append(b.answers, a)
			b.mu.Unlock()
		}
	}()
	return nil
}

// wait returns the answers once every visit has had one.
func (b *browser) wait(t *testing.T) []answer {
	t.Helper()
	select {
	case <-b.finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the browser's visits did not finish")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.answers
}

// call makes one request to the loopback redirect. It gives up after a few
// seconds, so a handler that blocks shows up as an error, not a hung test.
func call(method, redirectURI, state string, query url.Values) answer {
	q := url.Values{"state": {state}}
	for k, v := range query {
		q[k] = v
	}
	if q.Get("state") == "" {
		q.Del("state")
	}
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, redirectURI+"?"+q.Encode(), nil)
	if err != nil {
		return answer{err: err}
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, header: resp.Header, body: string(body), err: err}
}

func useBrowser(t *testing.T, open func(string) error) {
	prev := LaunchBrowser
	LaunchBrowser = open
	t.Cleanup(func() { LaunchBrowser = prev })
}

// tokenServer is the authorization server's token endpoint: it hands out
// tokens for "the-code" exchanged with the verifier matching the challenge
// in b's link, from the redirect the link named (RFC 6749 4.1.3). It
// counts the exchanges, and runs during(), if set, while the login is
// still waiting on the first.
func tokenServer(t *testing.T, b *browser, during func()) (*httptest.Server, *atomic.Int32) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		exchanges.Add(1)
		if during != nil {
			during()
		}
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		challenge, _ := b.challenge.Load().(string)
		link, _ := b.link.Load().(url.Values)
		if r.PostForm.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge ||
			r.PostForm.Get("client_id") != api.ClientID || r.PostForm.Get("grant_type") != "authorization_code" ||
			r.PostForm.Get("redirect_uri") == "" || r.PostForm.Get("redirect_uri") != link.Get("redirect_uri") {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		io.WriteString(w, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &exchanges
}

var approve = visit{query: url.Values{"code": {"the-code"}}}

// The code is exchanged with the verifier that matches the link's S256
// challenge, and the page that says so is neither cached nor referred on.
func TestLoginExchangesTheCodeWithPKCE(t *testing.T) {
	b := newBrowser(approve)
	useBrowser(t, b.open)
	srv, _ := tokenServer(t, b, nil)

	var out strings.Builder
	tr, err := Login(context.Background(), api.New(srv.URL), Options{OpenBrowser: true, Out: &out, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tr.AccessToken != "at" || tr.RefreshToken != "rt" {
		t.Errorf("token = %+v", tr)
	}
	for _, want := range []string{"Opened your browser to sign in.", "\n\n  " + srv.URL + "/oauth/authorize?", "Waiting up to 10 seconds for you to approve..."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	link, _ := b.link.Load().(url.Values)
	if link.Get("scope") != "mcp" || link.Get("code_challenge_method") != "S256" || link.Get("response_type") != "code" {
		t.Errorf("sign-in link = %v", link)
	}

	answers := b.wait(t)
	if len(answers) != 1 || answers[0].status != http.StatusOK {
		t.Fatalf("answers = %+v", answers)
	}
	h := answers[0].header
	if h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers of the result page = %v", h)
	}
}

// Anything that is not this login's answer, over GET, is turned away and
// the login goes on waiting for the real one.
func TestLoginIgnoresStrayCallbacks(t *testing.T) {
	b := newBrowser(
		visit{query: url.Values{"code": {"the-code"}, "state": {"someone-else"}}},
		visit{query: url.Values{"error": {"access_denied"}, "state": {"someone-else"}}},
		visit{query: url.Values{"code": {"the-code"}, "state": {""}}},
		visit{method: http.MethodPost, query: url.Values{"code": {"the-code"}}},
		visit{method: http.MethodHead, query: url.Values{"code": {"the-code"}}},
		approve,
	)
	useBrowser(t, b.open)
	srv, exchanges := tokenServer(t, b, nil)

	tr, err := Login(context.Background(), api.New(srv.URL), Options{OpenBrowser: true, Out: io.Discard, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tr.AccessToken != "at" || exchanges.Load() != 1 {
		t.Errorf("token = %+v after %d exchanges", tr, exchanges.Load())
	}

	answers := b.wait(t)
	want := []int{http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusMethodNotAllowed, http.StatusOK}
	if len(answers) != len(want) {
		t.Fatalf("answers = %+v", answers)
	}
	for i, a := range answers {
		if a.err != nil || a.status != want[i] {
			t.Errorf("visit %d: status %d, %v; want %d", i, a.status, a.err, want[i])
			continue
		}
		if a.header.Get("Cache-Control") != "no-store" || a.header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("visit %d: headers = %v", i, a.header)
		}
		if a.status == http.StatusMethodNotAllowed && a.header.Get("Allow") != http.MethodGet {
			t.Errorf("visit %d: Allow = %q", i, a.header.Get("Allow"))
		}
	}
	if !strings.Contains(answers[0].body, "ignored") {
		t.Errorf("a foreign state's page = %q", answers[0].body)
	}
}

// A refreshed tab or a second approval, arriving while the code is being
// exchanged, gets an answer at once instead of holding up the shutdown.
func TestLoginAnswersLaterCallbacksWithoutBlocking(t *testing.T) {
	b := newBrowser(approve)
	useBrowser(t, b.open)
	var (
		mu    sync.Mutex
		later []answer
	)
	srv, _ := tokenServer(t, b, func() {
		link, _ := b.link.Load().(url.Values)
		for range 3 {
			a := call("", link.Get("redirect_uri"), link.Get("state"), approve.query)
			mu.Lock()
			later = append(later, a)
			mu.Unlock()
		}
	})

	start := time.Now()
	if _, err := Login(context.Background(), api.New(srv.URL), Options{OpenBrowser: true, Out: io.Discard, Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("login: %v", err)
	}
	// The shutdown gives a stuck handler 2 seconds before it gives up.
	if took := time.Since(start); took >= 2*time.Second {
		t.Errorf("login took %s", took)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(later) != 3 {
		t.Fatalf("later callbacks = %+v", later)
	}
	for i, a := range later {
		if a.err != nil || a.status != http.StatusConflict || !strings.Contains(a.body, "already been handled") {
			t.Errorf("later callback %d: status %d, %q, %v", i, a.status, a.body, a.err)
		}
	}
	if answers := b.wait(t); len(answers) != 1 || answers[0].status != http.StatusOK {
		t.Errorf("first answer = %+v", answers)
	}
}

func TestLoginReportsADeniedConsent(t *testing.T) {
	b := newBrowser(visit{query: url.Values{"error": {"access_denied"}, "error_description": {"The user said no"}}})
	useBrowser(t, b.open)
	_, err := Login(context.Background(), api.New("http://127.0.0.1:1"), Options{OpenBrowser: true, Out: io.Discard, Timeout: 10 * time.Second})
	if err == nil || err.Error() != "login failed: The user said no" {
		t.Fatalf("want the server's reason, got %v", err)
	}
}

// An answer without a code ends the login there; exchanging nothing would
// only come back as invalid_grant.
func TestLoginRejectsAMissingCode(t *testing.T) {
	b := newBrowser(visit{query: url.Values{"code": {""}}})
	useBrowser(t, b.open)
	srv, exchanges := tokenServer(t, b, nil)
	_, err := Login(context.Background(), api.New(srv.URL), Options{OpenBrowser: true, Out: io.Discard, Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "no authorisation code") {
		t.Fatalf("want a missing code, got %v", err)
	}
	if n := exchanges.Load(); n != 0 {
		t.Errorf("%d token exchanges", n)
	}
	if answers := b.wait(t); len(answers) != 1 || answers[0].status != http.StatusBadRequest {
		t.Errorf("answers = %+v", answers)
	}
}

// A code the token endpoint refuses (used already, or expired while the
// browser waited) ends the login with the server's answer, exchanged once.
func TestLoginReportsARefusedExchange(t *testing.T) {
	b := newBrowser(visit{query: url.Values{"code": {"a-used-code"}}})
	useBrowser(t, b.open)
	srv, exchanges := tokenServer(t, b, nil)
	tr, err := Login(context.Background(), api.New(srv.URL), Options{OpenBrowser: true, Out: io.Discard, Timeout: 10 * time.Second})
	if err == nil || tr != nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("got %+v, %v; want the refusal", tr, err)
	}
	if n := exchanges.Load(); n != 1 {
		t.Errorf("%d token exchanges, want 1", n)
	}
	// The browser was told the sign-in went through: its part did.
	if answers := b.wait(t); len(answers) != 1 || answers[0].status != http.StatusOK {
		t.Errorf("answers = %+v", answers)
	}
}

// The redirect goes to 127.0.0.1, and nothing answers on that port at any
// other address of this machine.
func TestLoginListensOnLoopbackOnly(t *testing.T) {
	useBrowser(t, func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		redirect, err := url.Parse(u.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		if redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" {
			t.Errorf("redirect_uri = %s", redirect)
		}
		for _, ip := range otherAddresses(t) {
			addr := net.JoinHostPort(ip.String(), redirect.Port())
			if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
				conn.Close()
				t.Errorf("the login callback answers on %s", addr)
			}
		}
		return nil
	})
	_, err := Login(context.Background(), api.New("http://127.0.0.1:1"), Options{OpenBrowser: true, Out: io.Discard, Timeout: 20 * time.Millisecond})
	if err == nil || err.Error() != "login timed out" {
		t.Fatalf("want a timeout, got %v", err)
	}
}

// otherAddresses lists this machine's addresses other than 127.0.0.1: the
// IPv6 loopback and every interface address a dial without a zone can
// reach.
func otherAddresses(t *testing.T) []net.IP {
	ips := []net.IP{net.IPv6loopback}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Logf("no interface addresses: %v", err)
		return ips
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, n.IP)
	}
	return ips
}

// With --no-browser nothing is opened, and nobody approving means a timeout.
func TestLoginTimesOut(t *testing.T) {
	useBrowser(t, func(string) error {
		t.Error("--no-browser must not open a browser")
		return nil
	})
	var out strings.Builder
	_, err := Login(context.Background(), api.New("http://127.0.0.1:1"), Options{OpenBrowser: false, Out: &out, Timeout: 20 * time.Millisecond})
	if err == nil || err.Error() != "login timed out" {
		t.Fatalf("want a timeout, got %v", err)
	}
	for _, want := range []string{"Open this link in a browser on this machine to sign in:\n\n  http://127.0.0.1:1/oauth/authorize?", "Waiting up to 20ms"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// When the browser cannot be opened the user is told why and gets the link.
func TestLoginPrintsTheLinkWhenNoBrowserOpens(t *testing.T) {
	useBrowser(t, func(string) error { return errors.New(`exec: "xdg-open": executable file not found in $PATH`) })
	var out strings.Builder
	_, err := Login(context.Background(), api.New("http://127.0.0.1:1"), Options{OpenBrowser: true, Out: &out, Timeout: 20 * time.Millisecond})
	if err == nil || err.Error() != "login timed out" {
		t.Fatalf("want a timeout, got %v", err)
	}
	want := "Could not open your browser (exec: \"xdg-open\": executable file not found in $PATH).\n" +
		"Open this link in a browser on this machine to sign in:\n\n  http://127.0.0.1:1/oauth/authorize?"
	if !strings.HasPrefix(out.String(), want) || strings.Contains(out.String(), "Opened your browser") {
		t.Errorf("output = %q", out.String())
	}
}

// Ctrl-C ends the wait for the browser at once, well before the timeout.
func TestLoginStopsWaitingWhenCancelled(t *testing.T) {
	useBrowser(t, func(string) error { return nil }) // opened, never approved
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	_, err := Login(ctx, api.New("http://127.0.0.1:1"), Options{OpenBrowser: true, Out: io.Discard, Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s after the cancel", waited)
	}
}

func TestWaitSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		DefaultTimeout:          "5 minutes",
		time.Minute:             "1 minute",
		90 * time.Second:        "90 seconds",
		time.Second:             "1 second",
		20 * time.Millisecond:   "20ms",
		1500 * time.Millisecond: "1.5s",
	} {
		if got := waitSpan(d); got != want {
			t.Errorf("waitSpan(%s) = %q, want %q", d, got, want)
		}
	}
}
