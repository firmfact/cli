// Package httpx builds the CLI's HTTP clients. They share one kind of
// transport: proxies from the environment, TLS 1.2 or newer, and bounded
// waits for the connection, the TLS handshake and the server's answer, so a
// host that does not answer fails in seconds instead of hanging. What a
// client does with a redirect is chosen per use, and a request that gets no
// answer comes back as an *Error that says why in one line.
package httpx

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DialTimeout bounds the DNS lookup and TCP connect together.
	DialTimeout = 10 * time.Second
	// TLSHandshakeTimeout bounds the TLS handshake once connected.
	TLSHandshakeTimeout = 10 * time.Second
	// HeaderTimeout is how long the server may take to start answering
	// once the request is sent.
	HeaderTimeout = 30 * time.Second
	// DefaultTimeout bounds a whole request, the body of the answer
	// included.
	DefaultTimeout = 60 * time.Second
	// maxRedirects is where a client that follows redirects gives up.
	maxRedirects = 10
)

// Options chooses how a client behaves. The zero value is the API client:
// the default limits and no redirects.
type Options struct {
	// Timeout bounds a whole request; zero means DefaultTimeout. A request
	// whose context has a deadline of its own is bounded by that deadline
	// instead (see Client.Do).
	Timeout time.Duration
	// HeaderTimeout bounds the wait for the server to start answering;
	// zero means HeaderTimeout. A caller that lets the user choose a longer
	// Timeout sets this too, or a slow answer is still cut off at 30 s.
	HeaderTimeout time.Duration
	// FollowHTTPS follows redirects, each hop only to an https URL. Without
	// it a 3xx answer is returned as it is. API calls must not follow: Go
	// keeps the Authorization header on a redirect to the same host name,
	// even from https to plain http, so the bearer token would travel
	// unencrypted.
	FollowHTTPS bool
}

// Client sends requests and explains the ones that get no answer. It has
// only Do, so no request can go around that.
type Client struct {
	hc *http.Client
	// open is hc without the limit for the whole request, for requests
	// whose context sets their own.
	open          *http.Client
	headerTimeout time.Duration
}

// New returns a client with the given options.
func New(o Options) *Client {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.HeaderTimeout <= 0 {
		o.HeaderTimeout = HeaderTimeout
	}
	return newClient(o, transport(o.HeaderTimeout))
}

// newClient puts a client together around rt; tests pass a transport that
// trusts their own TLS server.
func newClient(o Options, rt http.RoundTripper) *Client {
	hc := &http.Client{Transport: rt, Timeout: o.Timeout, CheckRedirect: noRedirects}
	if o.FollowHTTPS {
		hc.CheckRedirect = httpsRedirects
	}
	open := *hc
	open.Timeout = 0
	return &Client{hc: hc, open: &open, headerTimeout: o.HeaderTimeout}
}

// Do sends req. A request that gets no answer returns an *Error, or an
// *InsecureRedirectError for a redirect away from https; a request whose
// context was cancelled or ran out of time returns the context's error,
// since stopping is not a connection problem.
//
// A request whose context has a deadline is bounded by that deadline, not
// by Timeout: the caller knows how long its answer may take (a chat
// answer streams in over minutes), and Timeout would cut it off at 60 s
// whatever the caller asked for. The waits for the connection, the TLS
// handshake and the start of the answer still apply.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	p := &progress{start: time.Now()}
	hc := c.hc
	if _, ok := req.Context().Deadline(); ok {
		hc, p.open = c.open, true
	}
	resp, err := hc.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), p.trace())))
	if err != nil {
		// A refused redirect comes with the redirect answer, its body
		// already closed; the caller gets the error only.
		return nil, c.explain(req, err, p)
	}
	return resp, nil
}

// progress is how far a request got. Go reports its timeouts as text
// only, so this is what tells a host that never let us connect from a
// server that is slow to answer.
type progress struct {
	start time.Time
	// open is a request that the caller's context bounds instead of
	// Timeout.
	open      bool
	handshake atomic.Bool // a TLS handshake started on the latest hop
	connected atomic.Bool // the latest hop has its connection
}

func (p *progress) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		// Each hop of a redirect starts over.
		GetConn: func(string) {
			p.handshake.Store(false)
			p.connected.Store(false)
		},
		TLSHandshakeStart: func() { p.handshake.Store(true) },
		GotConn:           func(httptrace.GotConnInfo) { p.connected.Store(true) },
	}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// InsecureRedirectError is a redirect to anything but https, which a client
// that follows redirects refuses.
type InsecureRedirectError struct{ To string }

func (e *InsecureRedirectError) Error() string {
	return fmt.Sprintf("refused a redirect to %s: only https is followed", e.To)
}

func httpsRedirects(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return &InsecureRedirectError{To: req.URL.Redacted()}
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// The transports are shared between clients with the same wait for an
// answer, so a connection the version check opened serves the command's
// own requests too.
var (
	transportsMu sync.Mutex
	transports   = map[time.Duration]*http.Transport{}
)

func transport(headerTimeout time.Duration) *http.Transport {
	transportsMu.Lock()
	defer transportsMu.Unlock()
	if t, ok := transports[headerTimeout]; ok {
		return t
	}
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ResponseHeaderTimeout: headerTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	transports[headerTimeout] = t
	return t
}

// ProxyFor is the proxy the requests to target go through, or nil for a
// direct connection. It asks http.ProxyFromEnvironment, as the transports
// do: HTTPS_PROXY for an https URL, HTTP_PROXY for plain http, NO_PROXY for
// the hosts to leave out, and never a proxy for this machine. net/http
// reads those variables once per process, so the answer holds for every
// request this run makes.
func ProxyFor(target string) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	return http.ProxyFromEnvironment(&http.Request{URL: u})
}
