// Package api is the CLI's HTTP client: JSON requests against one firmfact
// host, with the stored OAuth token attached and refreshed when it expires.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
)

// ClientID is the CLI's public OAuth client id (a PKCE client; not a secret).
const ClientID = "firmfact-cli"

var Version = "dev"

// Commit is the commit this binary was built from, or "" when nothing
// recorded it; see UserAgent.
var Commit string

// Name is how the CLI was invoked, for the commands its messages suggest.
var Name = "firmfact"

type Client struct {
	Host string
	// HTTP follows no redirects: the token must reach Host and nothing
	// else. A 3xx answer is an error that names where it pointed.
	HTTP *httpx.Client
	// Busy, when set, is where the client says that it waits for a busy
	// server before a retry ("Server busy; retrying in 5s..."). The CLI
	// sets it to stderr on a terminal only: two waits of up to 30 s in
	// silence look like a hang, but a script's log needs no commentary.
	Busy io.Writer
	// Debug, when set, is where the client logs its requests and answers
	// and the reasons for its retries and renewals (--debug). Nil logs
	// nothing.
	Debug *DebugLog
	// Pace bounds the requests that stream their body (SendBody). A field
	// left at zero takes DefaultPace's value.
	Pace Pace
	// Token is loaded lazily; nil means "not signed in". mu guards it, as
	// requests may be sent at once (workspaces list reads each workspace's
	// setup that way), and any of them may renew it.
	mu        sync.Mutex
	token     *config.Token
	tokenRead bool
}

// New returns a client for host, which must come from config.ParseHost (or
// ParseHostAllowHTTP): the token goes wherever Host points, so the client
// does not guess at a half-given host itself. Requests get the default
// limits; set HTTP for others.
func New(host string) *Client {
	return &Client{Host: host, HTTP: httpx.New(httpx.Options{})}
}

// Error is a non-2xx answer. Code and Details come from the server's
// {error, code, details} body when it sends one. Error() escapes terminal
// control characters, because the message is the server's own text and
// ends up on the user's terminal.
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
	// Unsupported marks a 404 for one of the CLI's own endpoints that the
	// host does not have: a firmfact that predates this version of the CLI,
	// or a host that is not firmfact at all. Message then says so in place
	// of the host's page-not-found text.
	Unsupported bool
	// RequestID is the id the server gave the request (see RequestID).
	// The message ends with it when the server did not explain the error:
	// a 5xx, or an answer that is not the API's JSON error.
	RequestID string
	// unexplained is an answer without the API's JSON error in it.
	unexplained bool
}

// showsRequestID reports whether the message ends with the request id: a
// server error, or an answer the API did not explain, but for the ones
// the CLI explains itself (a rate limit, a host without the CLI's
// endpoints).
func (e *Error) showsRequestID() bool {
	if e.RequestID == "" {
		return false
	}
	return e.Status >= 500 || e.unexplained && !e.Unsupported && e.Status != http.StatusTooManyRequests
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if len(e.Details) > 0 {
		var parts []string
		for field, v := range e.Details {
			parts = append(parts, fmt.Sprintf("%s %v", field, v))
		}
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	if e.showsRequestID() {
		msg += " (request id: " + e.RequestID + ")"
	}
	return ui.SafeText(msg)
}

func (c *Client) Token() (*config.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tokenRead {
		t, err := config.LoadToken(c.Host)
		if err != nil {
			return nil, err
		}
		c.useTokenLocked(t)
	}
	return c.token, nil
}

func (c *Client) SetToken(t *config.Token) error {
	c.useToken(t)
	return config.SaveToken(c.Host, t)
}

// useToken makes t the token the client sends, and keeps it out of the
// debug log.
func (c *Client) useToken(t *config.Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.useTokenLocked(t)
}

func (c *Client) useTokenLocked(t *config.Token) {
	c.token, c.tokenRead = t, true
	if t != nil {
		c.Debug.hide(t.AccessToken, t.RefreshToken)
	}
}

// JSON sends body (if any) as JSON and decodes the answer into out (if any).
// With auth, the stored token is sent and refreshed once on expiry or 401.
// Cancelling ctx (Ctrl-C) abandons the request and any wait before a retry.
func (c *Client) JSON(ctx context.Context, method, path string, body, out any, auth bool) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	resp, err := c.Send(ctx, method, path, payload, map[string]string{"Content-Type": "application/json"}, auth)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

// Send performs one request and returns the raw response for 2xx answers.
//
// A request that carries an MCP session id is never sent with a token Send
// has just renewed; it gets ErrInvalidSession instead. The server binds a
// session to the access token that opened it, so the renewed token could
// only earn "Invalid session" with it, at the cost of a request against
// the rate limit. The MCP client then opens a new session.
func (c *Client) Send(ctx context.Context, method, path string, payload []byte, headers map[string]string, auth bool) (*http.Response, error) {
	return c.send(ctx, headers, auth, 0, func(ctx context.Context) (*http.Response, error) {
		return c.do(ctx, method, path, payload, headers, auth)
	})
}

// send runs once, which makes one attempt at a request, as often as the
// request needs: again after a busy answer, and again after a 401 once the
// token is renewed. With auth, the token must be good for at least fresh
// (see ensureFreshToken).
func (c *Client) send(ctx context.Context, headers map[string]string, auth bool, fresh time.Duration, once func(context.Context) (*http.Response, error)) (*http.Response, error) {
	mcpSession := headers["mcp-session-id"] != ""
	if auth {
		renewed, err := c.ensureFreshToken(ctx, fresh)
		if err != nil {
			return nil, err
		}
		if renewed && mcpSession {
			c.Debugf("the MCP session belongs to the old token, so a new one is needed")
			return nil, ErrInvalidSession
		}
	}
	resp, err := c.attempt(ctx, headers, once)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && auth && c.canRefresh() {
		resp.Body.Close()
		c.Debugf("401: renewing the access token, then sending the request again")
		if err := c.refresh(ctx); err != nil {
			return nil, err
		}
		if mcpSession {
			c.Debugf("the MCP session belongs to the old token, so a new one is needed")
			return nil, ErrInvalidSession
		}
		if resp, err = c.attempt(ctx, headers, once); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		err := c.readError(resp)
		if resp.StatusCode == http.StatusUnauthorized && auth && !c.canRefresh() {
			c.Debugf("401: no refresh token to renew the access token with, so not retrying")
		}
		return nil, err
	}
	return resp, nil
}

// attempt sends the request, waiting out a busy server, and turns the
// MCP server's refusal of the session id into ErrInvalidSession. send runs
// it for the retry after a renewal too, so no answer reaches the user as a
// bare "Invalid session".
func (c *Client) attempt(ctx context.Context, headers map[string]string, once func(context.Context) (*http.Response, error)) (*http.Response, error) {
	resp, err := c.doRetrying(ctx, once)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || headers["mcp-session-id"] == "" {
		return resp, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if bytes.Contains(raw, []byte("Invalid session")) {
		c.Debugf("the server no longer accepts the MCP session")
		return nil, ErrInvalidSession
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

// doRetrying waits and retries when the server says it is busy and when
// to come back: a 429 (a rate limit) or a 503 (the MCP endpoint's capacity
// guard, "Unable to acquire capacity, please retry"; an upload's virus
// scanner, busy with others), with a Retry-After of up to 30 s. The server
// turned the request away before doing anything with it, so sending it
// again is safe. A 503 without Retry-After is a server that is down rather
// than busy, and is not retried.
func (c *Client) doRetrying(ctx context.Context, once func(context.Context) (*http.Response, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := once(ctx)
		if err != nil || !busy(resp.StatusCode) {
			return resp, err
		}
		if attempt >= maxBusyRetries {
			c.Debugf("%d: still busy after %d retries, so giving up", resp.StatusCode, maxBusyRetries)
			return resp, nil
		}
		wait, ok := retryAfter(resp.Header, time.Now())
		switch {
		case !ok:
			c.Debugf("%d without a Retry-After the CLI can read, so not retrying", resp.StatusCode)
			return resp, nil
		case wait > maxRetryAfter:
			c.Debugf("%d: Retry-After asks for %s, more than the %s the CLI waits, so not retrying", resp.StatusCode, seconds(wait), seconds(maxRetryAfter))
			return resp, nil
		}
		c.Debugf("%d: the server is busy; retry %d of %d in %s", resp.StatusCode, attempt+1, maxBusyRetries, seconds(wait))
		resp.Body.Close()
		if c.Busy != nil && wait > 0 {
			ui.Interject(c.Busy, fmt.Sprintf("Server busy; retrying in %s...", seconds(wait)))
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// busy reports whether status is a server's way of saying "not now".
func busy(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// sleep waits for d, or less when ctx is cancelled first: a Retry-After of
// up to half a minute must not hold up a Ctrl-C.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAfter is how long the Retry-After header of h asks the client to
// wait (RFC 9110 section 10.2.3): a number of seconds, or an HTTP-date. A
// date is measured from the answer's own Date header when it has one, so
// a clock that is off on either side does not stretch or cut short the
// wait; now stands in without it. A date that has passed means now.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 || secs > int64(math.MaxInt64/time.Second) {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	if date, err := http.ParseTime(h.Get("Date")); err == nil {
		now = date
	}
	return max(at.Sub(now), 0), true
}

// seconds writes d in whole seconds, rounded up: a wait of 4.2 s is "5s".
func seconds(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10) + "s"
}

// UserAgent is how the CLI names itself to the server: its version and,
// when known, the commit it was built from, so that a request in the
// server's logs leads to the code that sent it. Twelve characters of the
// commit are enough to find it, as they are in Go's pseudo-versions.
func UserAgent() string {
	ua := "firmfact-cli/" + Version
	if c := Commit; c != "" {
		ua += " (commit " + c[:min(len(c), 12)] + ")"
	}
	return ua
}

func (c *Client) do(ctx context.Context, method, path string, payload []byte, headers map[string]string, auth bool) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := c.newRequest(ctx, method, path, body, headers, auth)
	if err != nil {
		return nil, err
	}
	c.logRequest(req, payload, nil)
	start := time.Now()
	// Unanswered requests come back as one line that says why (an
	// *httpx.Error), and interrupted ones as the context's error: stopping
	// is not a connection problem.
	resp, err := c.HTTP.Do(req)
	c.logAnswer(req, resp, err, time.Since(start))
	return resp, err
}

// newRequest is a request for path on the client's host, with the CLI's
// own headers, then headers, then with auth the token.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, headers map[string]string, auth bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Host+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if auth {
		t, err := c.Token()
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, ErrNotSignedIn
		}
		req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	}
	return req, nil
}

// Raw sends one request without a token and returns whatever answer comes,
// for a caller that reads the status and headers itself (doctor). The
// caller closes the body.
func (c *Client) Raw(ctx context.Context, method, path string) (*http.Response, error) {
	return c.do(ctx, method, path, nil, nil, false)
}

// ErrNotSignedIn is a request that needs a sign-in when there is none. Its
// text is made when it is shown, from Name: the CLI sets that once it
// knows how it was invoked, and text made at start-up would say firmfact
// even to someone who runs the CLI as ff.
var ErrNotSignedIn error = notSignedIn{}

type notSignedIn struct{}

func (notSignedIn) Error() string {
	return "not signed in: run `" + Name + " login` (or `" + Name + " signup`)"
}

// ErrInvalidSession is the MCP server's answer to a session id it no longer
// accepts (expired, or opened with a token that has since been renewed), and
// Send's own verdict on a session id once it has renewed the token. It is
// not a token problem, so no refresh is attempted; the MCP client opens a
// new session instead.
var ErrInvalidSession = errors.New("mcp session no longer valid")

const (
	maxBusyRetries = 2
	maxRetryAfter  = 30 * time.Second
)

// ensureFreshToken renews a token that has expired by the CLI's clock, or
// that runs out within fresh, and says whether it did.
//
// fresh is for a request that takes long to send, such as an upload. The
// server reads the token once it has the whole body, so a token that runs
// out on the way is refused only then, and the whole body would have to be
// sent again. Renewing it first costs one small request.
func (c *Client) ensureFreshToken(ctx context.Context, fresh time.Duration) (renewed bool, err error) {
	t, err := c.Token()
	if err != nil {
		return false, err
	}
	if t == nil {
		return false, ErrNotSignedIn
	}
	if !c.canRefresh() {
		return false, nil
	}
	switch {
	case t.Expired():
		c.Debugf("the access token has expired by this machine's clock; renewing it first")
	case fresh > 0 && !t.ExpiresAt.IsZero() && time.Now().Add(fresh).After(t.ExpiresAt):
		c.Debugf("the access token runs out within the %s this request may take; renewing it first", fresh.Round(time.Second))
	default:
		return false, nil
	}
	if err := c.refresh(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) canRefresh() bool {
	t, _ := c.Token()
	return t != nil && t.RefreshToken != ""
}

// TokenResponse is the OAuth token endpoint's answer (RFC 6749 section 5.1).
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (r *TokenResponse) Token() *config.Token {
	t := &config.Token{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ClientID: ClientID}
	if r.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return t
}

// ExchangeToken posts a form to /oauth/token (authorization_code or refresh_token grant).
func (c *Client) ExchangeToken(ctx context.Context, form url.Values) (*TokenResponse, error) {
	form.Set("client_id", ClientID)
	resp, err := c.do(ctx, http.MethodPost, "/oauth/token", []byte(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, c.readError(resp)
	}
	var tr TokenResponse
	if err := decode(resp, &tr); err != nil {
		return nil, err
	}
	c.Debug.hide(tr.AccessToken, tr.RefreshToken)
	return &tr, nil
}

// Token type hints for Revoke (RFC 7009 section 2.1).
const (
	HintRefreshToken = "refresh_token"
	HintAccessToken  = "access_token"
)

// Revoke asks the server to revoke token (RFC 7009). The CLI is a public
// client, so it names itself with client_id alone and sends no bearer
// token. The server answers 200 for a token it does not know, as the RFC
// asks; anything but a 2xx, or no answer at all, is an error.
func (c *Client) Revoke(ctx context.Context, token, hint string) error {
	form := url.Values{"token": {token}, "token_type_hint": {hint}, "client_id": {ClientID}}
	resp, err := c.Send(ctx, http.MethodPost, "/oauth/revoke", []byte(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, false)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// refresh renews the token with its refresh token. It goes through
// config.RenewToken, which serialises renewals across processes: the server
// rotates refresh tokens, so a second command renewing with the same one
// would be refused. Such a command finds the first one's token instead.
func (c *Client) refresh(ctx context.Context) error {
	held, err := c.Token()
	if err != nil {
		return err
	}
	if held == nil {
		return ErrNotSignedIn
	}
	exchanged := false
	t, err := config.RenewToken(ctx, c.Host, held, func(stored *config.Token) (*config.Token, error) {
		exchanged = true
		c.Debug.hide(stored.AccessToken, stored.RefreshToken)
		tr, err := c.ExchangeToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {stored.RefreshToken}})
		if err != nil {
			c.Debugf("the renewal failed: %v", err)
			return nil, renewalError(ctx, err)
		}
		return tr.Token(), nil
	})
	if err != nil {
		return err
	}
	c.useToken(t)
	switch {
	case t == nil:
		c.Debugf("the sign-in was removed while renewing it")
		return ErrNotSignedIn
	case !exchanged:
		c.Debugf("another command renewed the token meanwhile; using its token")
	default:
		c.Debugf("renewed the access token")
	}
	return nil
}

// renewalError is what the user hears about a renewal that failed.
func renewalError(ctx context.Context, err error) error {
	// An interrupted renewal, or one that never reached the server, says
	// nothing about the session, so it is no reason to tell the user to
	// sign in again.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var netErr *httpx.Error
	if errors.As(err, &netErr) {
		return err
	}
	// The server's answer to the renewal is not an answer to the request
	// the caller made, and must not be read as one, so neither error wraps
	// it. A refused grant (400 invalid_grant, 401) ends the session; any
	// other answer is the server's trouble, and a retry may do.
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
		return &SessionEndedError{Name: Name}
	}
	return fmt.Errorf("the server could not renew your session (%v); try again", err) //nolint:errorlint // see above
}

// SessionEndedError is a renewal the server refused: the refresh token was
// revoked or has run out, and only a new sign-in helps.
type SessionEndedError struct {
	// Name is the command to sign in with.
	Name string
}

func (e *SessionEndedError) Error() string {
	return "Your session has ended. Run " + e.Name + " login."
}

// decode reads a successful answer into out. One that is not the JSON
// the CLI expects is an error with the request id: the server said all
// was well, and only its logs can say what it sent instead.
func decode(resp *http.Response, out any) error {
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	err := json.NewDecoder(resp.Body).Decode(out)
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return WithRequestID(fmt.Errorf("unexpected answer from the server: %w", err), resp)
	}
	return err
}

func (c *Client) readError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	c.logErrorBody(resp, raw)
	e := &Error{Status: resp.StatusCode, RequestID: RequestID(resp)}
	var body struct {
		Error            any            `json:"error"`
		ErrorDescription string         `json:"error_description"`
		Code             string         `json:"code"`
		Details          map[string]any `json:"details"`
	}
	if json.Unmarshal(raw, &body) == nil {
		switch v := body.Error.(type) {
		case string:
			e.Message = v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				e.Message = m
			}
		}
		if body.ErrorDescription != "" {
			e.Message = body.ErrorDescription
		}
		e.Code, e.Details = body.Code, body.Details
	}
	e.unexplained = e.Message == "" && e.Code == ""
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		e.Message = redirectMessage(resp)
	}
	// The API's own 404s name what they did not find and carry a code
	// (NOT_FOUND). One without a code is the router's: an HTML page, or the
	// JSON page-not-found text ("The page you're looking for doesn't
	// exist."), which would only puzzle the user.
	if resp.StatusCode == http.StatusNotFound && e.Code == "" && resp.Request != nil && cliEndpoint(resp.Request.URL.Path) {
		e.Message = origin(resp.Request.URL) + " does not support this version of the CLI yet (or --host is wrong)"
		e.Details, e.Unsupported = nil, true
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		e.Message = "too many requests; wait a while and try again"
		if wait, ok := retryAfter(resp.Header, time.Now()); ok {
			e.Message += " (retry after " + seconds(wait) + ")"
		}
	}
	return e
}

// cliEndpoints are the paths that exist for the CLI alone. The host takes
// no path (config.ParseHost), so a request's URL path is the API path.
var cliEndpoints = []string{"/api/v1/cli", "/api/v1/signup", "/mcp"}

// cliEndpoint reports whether path is one of cliEndpoints or below one.
func cliEndpoint(path string) bool {
	for _, p := range cliEndpoints {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// origin is u's scheme and host: the host as the user gave it.
func origin(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// redirectMessage explains a 3xx answer. The client does not follow
// redirects, so the token stays with the host the user chose; the answer
// is most likely a host given with the wrong name or scheme.
func redirectMessage(resp *http.Response) string {
	host := origin(resp.Request.URL)
	to, err := resp.Location()
	if err != nil {
		return fmt.Sprintf("%s answered with a redirect (%d), which the CLI does not follow; check the host", host, resp.StatusCode)
	}
	target := origin(to)
	if target == host {
		target = to.EscapedPath()
	}
	return fmt.Sprintf("%s redirects to %s, which the CLI does not follow; check the host", host, target)
}
