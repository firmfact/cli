package httpx

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Kind is what kept a request from getting an answer.
type Kind int

const (
	// Unreachable is any other failure: a reset connection, no route to
	// the host, an answer that was not HTTP.
	Unreachable Kind = iota
	// NotFound is a host name that does not resolve, or a DNS lookup that
	// failed.
	NotFound
	// Refused is a host that turned the connection away: nothing listens
	// on that port.
	Refused
	// ConnectTimeout is a connection or TLS handshake that did not finish
	// in time.
	ConnectTimeout
	// AnswerTimeout is a server that took longer to answer than allowed.
	AnswerTimeout
	// Untrusted is a certificate that did not verify.
	Untrusted
)

// Error is a request that got no answer, said in one line the user can act
// on (Go's own text reads like `Get "https://…": dial tcp: lookup …`). Err
// is what the transport reported.
type Error struct {
	Kind Kind
	// Host is the host[:port] the request went to.
	Host string
	Err  error
	msg  string
}

func (e *Error) Error() string { return e.msg }
func (e *Error) Unwrap() error { return e.Err }

// Broken reports whether err is a request whose connection broke: the other
// end reset it, or closed it before an answer came. The request may or may
// not have reached the server, so only one that changes nothing is safe to
// send again. A reset while the answer was being read counts too, when the
// error that says so is wrapped with %w.
func Broken(err error) bool {
	if reset(err) {
		return true
	}
	var e *Error
	return errors.As(err, &e) && (errors.Is(e.Err, io.EOF) || errors.Is(e.Err, io.ErrUnexpectedEOF))
}

// TooSlow is a request that the caller's own deadline of limit ended before
// the answer from host was complete. Do returns such a request as the
// context's error, as it cannot tell a deadline from Ctrl-C; a caller that
// can tell them apart explains it with this, so it reads like any other
// wait that ran out.
func TooSlow(host string, limit time.Duration, err error) *Error {
	return &Error{Kind: AnswerTimeout, Host: host, Err: err,
		msg: fmt.Sprintf("%s did not finish answering within %s", host, span(limit))}
}

// explain turns the error of a request that got as far as p into an
// *Error.
func (c *Client) explain(req *http.Request, err error, p *progress) error {
	// Ctrl-C, or a deadline of the caller's own: the request was stopped,
	// which says nothing about the connection.
	if ctxErr := req.Context().Err(); ctxErr != nil {
		return ctxErr
	}
	var redirect *InsecureRedirectError
	if errors.As(err, &redirect) {
		return redirect
	}

	// After a redirect, the hop that failed may be on another host.
	failed := req.URL
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if u, perr := url.Parse(urlErr.URL); perr == nil && u.Host != "" {
			failed = u
		}
	}
	e := &Error{Host: failed.Host, Err: err}
	// A failure on the way to a proxy (HTTPS_PROXY) is the proxy's, not
	// the host's.
	who := e.Host
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "proxyconnect" {
		who = "the proxy for " + e.Host
	}
	var (
		dnsErr    *net.DNSError
		verifyErr *tls.CertificateVerificationError
		netErr    net.Error
	)
	switch {
	case errors.As(err, &dnsErr):
		e.Kind = NotFound
		name := dnsErr.Name
		if name == "" {
			name = failed.Hostname()
		}
		switch {
		case dnsErr.IsNotFound:
			e.msg = fmt.Sprintf("cannot find %s: no such host name", name)
		case dnsErr.IsTimeout:
			e.msg = fmt.Sprintf("cannot find %s: the DNS lookup timed out", name)
		default:
			e.msg = fmt.Sprintf("cannot find %s: the DNS lookup failed (%s)", name, dnsErr.Err)
		}
	case refused(err):
		e.Kind = Refused
		e.msg = who + " refused the connection"
	case errors.As(err, &verifyErr):
		e.Kind = Untrusted
		e.msg = fmt.Sprintf("cannot verify the certificate of %s: %s", e.Host, strings.TrimPrefix(verifyErr.Err.Error(), "x509: "))
	case errors.As(err, &netErr) && netErr.Timeout():
		e.Kind, e.msg = c.timedOut(p, who)
	default:
		e.msg = fmt.Sprintf("could not reach %s: %s", who, cause(err))
	}
	return e
}

// timedOut says what a timed-out request was waiting for, and which limit
// it ran into: the one for that step, or the one for the whole request
// when that came first.
func (c *Client) timedOut(p *progress, who string) (Kind, string) {
	limit := func(step time.Duration) string {
		if !p.open && time.Since(p.start) >= c.hc.Timeout {
			return span(c.hc.Timeout)
		}
		return span(step)
	}
	switch {
	case p.connected.Load():
		return AnswerTimeout, fmt.Sprintf("%s did not answer within %s", who, limit(c.headerTimeout))
	case p.handshake.Load():
		return ConnectTimeout, fmt.Sprintf("%s did not complete the TLS handshake within %s", who, limit(TLSHandshakeTimeout))
	}
	return ConnectTimeout, fmt.Sprintf("%s did not accept a connection within %s", who, limit(DialTimeout))
}

// cause is the part of err that says what happened, without Go's
// `Get "url": dial tcp 1.2.3.4:443: connect:` prefixes.
func cause(err error) string {
	var (
		urlErr *url.Error
		opErr  *net.OpError
		sysErr *os.SyscallError
	)
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.As(err, &opErr) {
		err = opErr.Err
	}
	if errors.As(err, &sysErr) {
		err = sysErr.Err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "the connection closed before an answer came"
	}
	return err.Error()
}

// span writes a limit the way the flags take it: 30s, 90s, 5m, 500ms, 1.5s.
func span(d time.Duration) string {
	switch {
	case d >= 2*time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
