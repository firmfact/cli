package httpx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// target counts the requests that reach it and remembers any Authorization
// header among them.
type target struct {
	hits atomic.Int32
	auth atomic.Value
}

func (tg *target) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tg.hits.Add(1)
		if a := r.Header.Get("Authorization"); a != "" {
			tg.auth.Store(a)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func redirectTo(to string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, http.StatusTemporaryRedirect)
	})
}

// The API client returns a redirect as the answer: Go would carry the
// bearer token along to the same host name even over plain http.
func TestAPIClientDoesNotFollowRedirects(t *testing.T) {
	var tg target
	to := tg.start(t)
	from := httptest.NewServer(redirectTo(to.URL + "/api/v1/cli/me"))
	defer from.Close()

	req, _ := http.NewRequest(http.MethodGet, from.URL+"/api/v1/cli/me", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := New(Options{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the 307 itself", resp.StatusCode)
	}
	if n := tg.hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d request(s), want none", n)
	}
}

// The updater follows GitHub's redirects, but every hop must be https.
func TestFollowHTTPSRefusesPlainHTTP(t *testing.T) {
	var tg target
	plain := tg.start(t)
	mux := http.NewServeMux()
	mux.Handle("/to-plain", redirectTo(plain.URL+"/asset"))
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("asset")) })
	secure := httptest.NewTLSServer(mux)
	defer secure.Close()
	mux.Handle("/to-https", redirectTo(secure.URL+"/asset"))

	// A transport that trusts the test server's certificate.
	c := newClient(Options{Timeout: 5 * time.Second, FollowHTTPS: true}, secure.Client().Transport)

	req, _ := http.NewRequest(http.MethodGet, secure.URL+"/to-https", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("an https redirect must be followed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, secure.URL+"/to-plain", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if resp, err = c.Do(req); err == nil {
		resp.Body.Close()
	}
	var insecure *InsecureRedirectError
	if !errors.As(err, &insecure) || !strings.HasPrefix(insecure.To, plain.URL) {
		t.Fatalf("want an *InsecureRedirectError naming %s, got %v", plain.URL, err)
	}
	if n := tg.hits.Load(); n != 0 {
		t.Errorf("the http target got %d request(s), want none (Authorization %v)", n, tg.auth.Load())
	}
}

// silentListener accepts connections (the kernel does, without Accept) and
// never answers them.
func silentListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return "http://" + l.Addr().String()
}

func get(t *testing.T, c *Client, ctx context.Context, rawURL string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// A host that takes the connection and never answers fails at the limit,
// which the message names: the one for the whole request, or the wait for
// the answer when that is shorter.
func TestSilentServerTimesOut(t *testing.T) {
	host := silentListener(t)
	cases := []struct {
		opts Options
		want string
	}{
		{Options{Timeout: 300 * time.Millisecond, HeaderTimeout: 300 * time.Millisecond}, "did not answer within 300ms"},
		{Options{Timeout: 5 * time.Second, HeaderTimeout: 200 * time.Millisecond}, "did not answer within 200ms"},
	}
	for _, tc := range cases {
		start := time.Now()
		err := get(t, New(tc.opts), context.Background(), host+"/api/v1/cli/version")
		took := time.Since(start)
		var e *Error
		if !errors.As(err, &e) || e.Kind != AnswerTimeout {
			t.Fatalf("%+v: want an AnswerTimeout *Error, got %#v", tc.opts, err)
		}
		if want := strings.TrimPrefix(host, "http://") + " " + tc.want; e.Error() != want {
			t.Errorf("message = %q, want %q", e.Error(), want)
		}
		if took > 2*time.Second {
			t.Errorf("%+v: gave up after %s", tc.opts, took)
		}
	}
}

// A caller that sets a deadline of its own, such as the MCP client for a
// chat, gets that instead of Timeout: an answer that streams in for longer
// than Timeout is read to the end. Without a deadline, Timeout ends it.
func TestContextDeadlineReplacesTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i := 0; i < 4; i++ {
			time.Sleep(100 * time.Millisecond)
			io.WriteString(w, "ping\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "done")
	}))
	defer srv.Close()
	c := New(Options{Timeout: 150 * time.Millisecond})

	read := func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}
	if _, err := read(context.Background()); err == nil {
		t.Error("without a deadline of its own, the request should stop at Timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if body, err := read(ctx); err != nil || !strings.HasSuffix(body, "done") {
		t.Errorf("with a deadline of its own: %q, %v", body, err)
	}
}

// The deadline replaces the limit for the whole request only: the wait for
// the server to start answering still applies, and its message names it.
func TestContextDeadlineKeepsTheHeaderWait(t *testing.T) {
	host := silentListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := get(t, New(Options{HeaderTimeout: 200 * time.Millisecond}), ctx, host+"/mcp")
	var e *Error
	if !errors.As(err, &e) || e.Kind != AnswerTimeout || !strings.HasSuffix(e.Error(), "did not answer within 200ms") {
		t.Fatalf("want the 200ms wait for an answer, got %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("gave up after %s", took)
	}
}

// TooSlow reads like the other timeouts and names the caller's limit the
// way --timeout takes it.
func TestTooSlow(t *testing.T) {
	cause := context.DeadlineExceeded
	e := TooSlow("firmfact.com", 5*time.Minute, cause)
	if e.Kind != AnswerTimeout || e.Error() != "firmfact.com did not finish answering within 5m" || !errors.Is(e, cause) {
		t.Errorf("TooSlow = %#v (%q)", e, e.Error())
	}
	for d, want := range map[time.Duration]string{
		30 * time.Second:        "30s",
		90 * time.Second:        "90s",
		60 * time.Second:        "60s",
		2 * time.Minute:         "2m",
		150 * time.Second:       "150s",
		300 * time.Millisecond:  "300ms",
		1500 * time.Millisecond: "1.5s",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%s) = %q, want %q", d, got, want)
		}
	}
}

// Ctrl-C is not a connection problem, and must not read like one.
func TestCancelIsNotExplained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := get(t, New(Options{}), ctx, silentListener(t)+"/")
	if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
		t.Fatalf("want context.Canceled as it is, got %v", err)
	}
}

func TestRefusedConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	err = get(t, New(Options{}), context.Background(), "http://"+addr+"/")
	var e *Error
	if !errors.As(err, &e) || e.Kind != Refused || e.Error() != addr+" refused the connection" {
		t.Fatalf("got %#v", err)
	}
}

// The test server's certificate is signed by no authority the system
// knows. The reason comes from the platform's verifier, so only its prefix
// is fixed.
func TestUntrustedCertificate(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake
	srv.StartTLS()
	defer srv.Close()

	err := get(t, New(Options{}), context.Background(), srv.URL+"/")
	var e *Error
	if !errors.As(err, &e) || e.Kind != Untrusted {
		t.Fatalf("got %#v", err)
	}
	prefix := "cannot verify the certificate of " + strings.TrimPrefix(srv.URL, "https://") + ": "
	if !strings.HasPrefix(e.Error(), prefix) || strings.Contains(e.Error(), "x509:") {
		t.Errorf("message = %q, want it to start %q and to leave out Go's prefixes", e.Error(), prefix)
	}
}

// A name that does not resolve, looked up for real. The .invalid top-level
// domain never resolves (RFC 6761); without a working resolver the lookup
// still fails as DNS.
func TestUnknownHostName(t *testing.T) {
	err := get(t, New(Options{}), context.Background(), "https://firmfact.invalid/")
	var e *Error
	if !errors.As(err, &e) || e.Kind != NotFound || !strings.HasPrefix(e.Error(), "cannot find firmfact.invalid: ") {
		t.Fatalf("got %#v", err)
	}
}

// Go's errors as the transport reports them, and the lines they become.
func TestExplainMessages(t *testing.T) {
	c := New(Options{})
	req, _ := http.NewRequest(http.MethodGet, "https://firmfact.example/api/v1/cli/me", nil)
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: req.URL.String(), Err: err} }
	dial := func(err error) error { return wrap(&net.OpError{Op: "dial", Net: "tcp", Err: err}) }
	cases := []struct {
		err  error
		kind Kind
		want string
	}{
		{dial(&net.DNSError{Err: "no such host", Name: "firmfact.example", IsNotFound: true}), NotFound,
			"cannot find firmfact.example: no such host name"},
		{dial(&net.DNSError{Err: "i/o timeout", Name: "firmfact.example", IsTimeout: true}), NotFound,
			"cannot find firmfact.example: the DNS lookup timed out"},
		{dial(&net.DNSError{Err: "server misbehaving", Name: "firmfact.example"}), NotFound,
			"cannot find firmfact.example: the DNS lookup failed (server misbehaving)"},
		{wrap(&net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("dial tcp 10.0.0.1:3128: connect: no route to host")}), Unreachable,
			"could not reach the proxy for firmfact.example: dial tcp 10.0.0.1:3128: connect: no route to host"},
		{wrap(io.EOF), Unreachable,
			"could not reach firmfact.example: the connection closed before an answer came"},
		{wrap(&net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: errors.New("connection reset by peer")}}), Unreachable,
			"could not reach firmfact.example: connection reset by peer"},
	}
	for _, tc := range cases {
		err := c.explain(req, tc.err, &progress{start: time.Now()})
		var e *Error
		if !errors.As(err, &e) || e.Kind != tc.kind || e.Error() != tc.want {
			t.Errorf("explain(%v) = %#v, want kind %d %q", tc.err, err, tc.kind, tc.want)
		}
	}
}

// The step a request was on when it ran out of time decides the message.
func TestTimedOutNamesTheStep(t *testing.T) {
	c := New(Options{Timeout: time.Hour})
	var dialing, handshaking, waiting progress
	for _, p := range []*progress{&dialing, &handshaking, &waiting} {
		p.start = time.Now()
	}
	handshaking.handshake.Store(true)
	waiting.handshake.Store(true)
	waiting.connected.Store(true)
	for p, want := range map[*progress]string{
		&dialing:     "h did not accept a connection within 10s",
		&handshaking: "h did not complete the TLS handshake within 10s",
		&waiting:     "h did not answer within 30s",
	} {
		if _, got := c.timedOut(p, "h"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}

	// The limit on the whole request came first.
	short := New(Options{Timeout: time.Millisecond})
	p := &progress{start: time.Now().Add(-time.Second)}
	if kind, got := short.timedOut(p, "h"); kind != ConnectTimeout || got != "h did not accept a connection within 1ms" {
		t.Errorf("got %d %q", kind, got)
	}
}

// hangUp is a server that reads each request and hangs up without an
// answer: with a reset (a close with linger 0 sends RST) or an ordinary
// close.
func hangUp(t *testing.T, reset bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = http.ReadRequest(bufio.NewReader(conn))
			if reset {
				_ = conn.(*net.TCPConn).SetLinger(0)
			}
			conn.Close()
		}
	}()
	return "http://" + l.Addr().String()
}

// A connection the server resets, or closes without an answer, is broken;
// one it refuses, or a request that was stopped, is not.
func TestBroken(t *testing.T) {
	for name, reset := range map[string]bool{"reset": true, "closed": false} {
		err := get(t, New(Options{}), context.Background(), hangUp(t, reset)+"/")
		if err == nil || !Broken(err) {
			t.Errorf("%s: Broken(%v) = false, want true", name, err)
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if err := get(t, New(Options{}), context.Background(), "http://"+addr+"/"); err == nil || Broken(err) {
		t.Errorf("a refused connection: Broken(%v) = true, want false", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := get(t, New(Options{}), ctx, silentListener(t)+"/"); Broken(err) {
		t.Errorf("a cancelled request: Broken(%v) = true, want false", err)
	}
}

// ProxyFor never names a proxy for this machine, as net/http sends no
// request for it through one, and refuses what is not a URL.
func TestProxyForThisMachine(t *testing.T) {
	for _, target := range []string{"http://localhost:5000", "http://127.0.0.1:5000", "http://[::1]:5000"} {
		if p, err := ProxyFor(target); p != nil || err != nil {
			t.Errorf("ProxyFor(%q) = %v, %v; want no proxy", target, p, err)
		}
	}
	if _, err := ProxyFor("http://%zz"); err == nil {
		t.Error("ProxyFor took a target that is not a URL")
	}
}
