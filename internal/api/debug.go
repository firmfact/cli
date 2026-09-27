package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/firmfact/cli/internal/ui"
)

// The debug log (--debug, or FIRMFACT_DEBUG=1) shows on stderr what the
// client does: each request and the answer to it, how long that took and
// the id the server gave it, and why the client waits and retries, renews
// the token or opens a new MCP session. A user may paste it into a
// support request, so it must not give the sign-in away: secrets are
// redacted wherever they could appear, and the bodies of the sign-in and
// signup requests, which are mostly secrets, are never shown at all.

// DebugLog is where clients write their debug lines. A command's own
// client and the one that refreshes the tool list in the background share
// one, which keeps their lines whole.
type DebugLog struct {
	mu sync.Mutex
	w  io.Writer
	// terminal is w being a terminal, where a live line may be under way:
	// the thinking count.
	terminal bool
	// hidden are the secrets the clients hold (their tokens). They are
	// taken out of every line, whatever put them there: a server that
	// echoes the Authorization header in an error page, say.
	hidden []string
}

// NewDebugLog returns a log that writes to w.
func NewDebugLog(w io.Writer) *DebugLog { return &DebugLog{w: w, terminal: ui.IsTerminal(w)} }

// redacted stands in for a secret.
const redacted = "[redacted]"

// Printf writes one line. A nil log writes nothing, so callers need not
// check whether debugging is on.
func (d *DebugLog) Printf(format string, args ...any) {
	if d == nil {
		return
	}
	d.write(fmt.Sprintf(format, args...))
}

// write writes lines together, so another client's lines do not land in
// the middle of a request's.
func (d *DebugLog) write(lines ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, line := range lines {
		for _, secret := range d.hidden {
			line = strings.ReplaceAll(line, secret, redacted)
		}
		// Much of a line is the server's text: a status, a header, an
		// error body. It must not drive the terminal (see ui.SafeText).
		line = "debug: " + ui.SafeLine(line)
		if d.terminal {
			ui.Interject(d.w, line)
			continue
		}
		fmt.Fprintln(d.w, line)
	}
}

// hide takes secrets out of every line from now on. Values shorter than
// a signup code are left alone, as they would take ordinary words with
// them.
func (d *DebugLog) hide(secrets ...string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range secrets {
		if len(s) >= 6 && !slices.Contains(d.hidden, s) {
			d.hidden = append(d.hidden, s)
		}
	}
}

// Debugf writes a line to the client's debug log, when it has one.
func (c *Client) Debugf(format string, args ...any) {
	c.Debug.Printf(format, args...)
}

// logRequest writes req, its headers and its body to the debug log: the
// payload of a small body, and only the size and description of a streamed
// one, whose bytes are a user's files.
func (c *Client) logRequest(req *http.Request, payload []byte, stream Body) {
	if c.Debug == nil {
		return
	}
	c.hideSecretFields(req.Header.Get("Content-Type"), payload)
	lines := []string{req.Method + " " + urlForLog(req.URL)}
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, "  "+name+": "+headerForLog(name, req.Header.Get(name)))
	}
	if len(payload) > 0 {
		lines = append(lines, "  "+bodyForLog(req.URL.Path, req.Header.Get("Content-Type"), payload))
	}
	if stream != nil {
		line := fmt.Sprintf("  (a body of %s, not shown)", ui.Bytes(stream.Size()))
		if d, ok := stream.(fmt.Stringer); ok {
			line += " " + clip(d.String())
		}
		lines = append(lines, line)
	}
	c.Debug.write(lines...)
}

// answerHeaders are the headers of an answer the debug log shows. The rest
// (security policies, caching) say nothing about why a command failed.
var answerHeaders = []string{"Content-Type", "Location", "Retry-After", "Www-Authenticate", "Mcp-Session-Id", "X-Ratelimit-Remaining"}

// logAnswer writes what came of req to the debug log: the answer's status,
// how long it took to come, and the server's id for the request, or why
// there was none.
func (c *Client) logAnswer(req *http.Request, resp *http.Response, err error, took time.Duration) {
	if c.Debug == nil {
		return
	}
	span := took.Round(time.Millisecond).String()
	if took < time.Millisecond {
		span = "under 1ms"
	}
	what := req.Method + " " + pathForLog(req.URL)
	if err != nil {
		c.Debug.write(fmt.Sprintf("%s: no answer after %s: %v", what, span, err))
		return
	}
	line := fmt.Sprintf("%s: %s in %s", what, resp.Status, span)
	if id := RequestID(resp); id != "" {
		line += ", request id " + id
	}
	lines := []string{line}
	for _, name := range answerHeaders {
		if v := resp.Header.Get(name); v != "" {
			if name == "Location" {
				v = locationForLog(v)
			}
			lines = append(lines, "  "+name+": "+headerForLog(name, v))
		}
	}
	c.Debug.write(lines...)
}

// logErrorBody writes the body of an answer that was not a success.
func (c *Client) logErrorBody(resp *http.Response, raw []byte) {
	if c.Debug == nil || len(raw) == 0 || resp.Request == nil {
		return
	}
	c.Debug.write("  " + bodyForLog(resp.Request.URL.Path, resp.Header.Get("Content-Type"), raw))
}

// RequestID is the id the server gave the request in the answer's
// X-Request-Id header, which its logs know the request by: quoted in a
// support request, it finds what went wrong. It is empty when there is
// none, or when the header holds something no id looks like (an id is at
// most 255 letters, digits and -_.:@, as the server's own are).
func RequestID(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	id := strings.TrimSpace(resp.Header.Get("X-Request-Id"))
	if id == "" || len(id) > 255 {
		return ""
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:@", r)) {
			return ""
		}
	}
	return id
}

// RequestIDError is an error about an answer the server gave that it did
// not explain, with the id of the request (see RequestID) for a support
// request.
type RequestIDError struct {
	Err       error
	RequestID string
}

func (e *RequestIDError) Error() string {
	return e.Err.Error() + " (request id: " + e.RequestID + ")"
}

func (e *RequestIDError) Unwrap() error { return e.Err }

// WithRequestID gives err the request id of resp, when it has one. An
// interrupt is left as it is: the user stopped the request, and no server
// log explains that.
func WithRequestID(err error, resp *http.Response) error {
	id := RequestID(resp)
	if err == nil || id == "" || errors.Is(err, context.Canceled) {
		return err
	}
	return &RequestIDError{Err: err, RequestID: id}
}

// secretBody reports whether path is an endpoint whose bodies, both ways,
// are mostly secrets: the OAuth endpoints (tokens, an authorisation code
// and its verifier) and signup (a password, the emailed code, the first
// tokens). The debug log never shows them, redacted or not, so a field the
// redaction does not know about cannot leak.
func secretBody(path string) bool {
	return strings.HasPrefix(path, "/oauth/") || path == "/api/v1/signup" || strings.HasPrefix(path, "/api/v1/signup/")
}

// secretName reports whether a header, form field, query parameter or JSON
// key of that name may hold a secret: a token, a password, a code sent by
// email or given to the sign-in, a session id, a cookie.
func secretName(name string) bool {
	n := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	if n == "code" || n == "otp" || strings.HasSuffix(n, "_code") {
		return true
	}
	for _, part := range []string{"token", "password", "passwd", "secret", "verifier", "session", "authorization", "cookie", "api_key", "apikey", "private_key", "credential"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// plainCode reports whether value, under a key named like a code, names
// something rather than being one: capitals and underscores only, such as
// an error code (NOT_FOUND) or a currency (EUR). A code sent by email or
// handed to the sign-in has digits or small letters.
func plainCode(key string, value any) bool {
	k := strings.ToLower(key)
	if k != "code" && !strings.HasSuffix(k, "_code") {
		return false
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r == '_') {
			return false
		}
	}
	return true
}

// headerForLog is a header's value as the log shows it. The scheme of an
// Authorization header stays, as whether one was sent at all is often the
// question.
func headerForLog(name, value string) string {
	if !secretName(name) || value == "" {
		return value
	}
	if scheme, _, ok := strings.Cut(value, " "); ok && strings.Contains(strings.ToLower(name), "authorization") {
		return scheme + " " + redacted
	}
	return redacted
}

// urlForLog is u with the values of secret query parameters redacted.
func urlForLog(u *url.URL) string {
	c := *u
	c.User = nil
	c.RawQuery = ""
	s := c.String()
	if q := queryForLog(u.RawQuery); q != "" {
		s += "?" + q
	}
	return s
}

// pathForLog is urlForLog without the scheme and host.
func pathForLog(u *url.URL) string {
	s := u.EscapedPath()
	if q := queryForLog(u.RawQuery); q != "" {
		s += "?" + q
	}
	return s
}

// locationForLog is a Location header's value with secret query
// parameters redacted: a redirect may carry a token back.
func locationForLog(v string) string {
	u, err := url.Parse(v)
	if err != nil {
		return redacted
	}
	return urlForLog(u)
}

// queryForLog redacts the secret parameters of a raw query, keeping the
// rest as they were sent.
func queryForLog(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	for i, pair := range pairs {
		key, _, _ := strings.Cut(pair, "=")
		if k, err := url.QueryUnescape(key); err != nil || secretName(k) {
			pairs[i] = key + "=" + redacted
		}
	}
	return strings.Join(pairs, "&")
}

// maxLoggedBody is where the log cuts a body short: enough for an error
// or a tool call's arguments, not a page of HTML.
const maxLoggedBody = 2 << 10

// bodyForLog is a body as the log shows it: nothing of the endpoints that
// deal in secrets, JSON and forms with their secret fields redacted, other
// text as it is; all of it cut short.
func bodyForLog(path, contentType string, body []byte) string {
	if secretBody(path) {
		return fmt.Sprintf("(%d bytes, not shown: the body of %s holds secrets)", len(body), path)
	}
	text := string(body)
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "application/x-www-form-urlencoded":
		text = queryForLog(text)
	case json.Valid(body):
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			break
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(redactJSON(v)) == nil {
			text = strings.TrimSuffix(buf.String(), "\n")
		}
	}
	return clip(text)
}

// redactJSON replaces the values of secret keys in v, at any depth. An
// empty value stays: that nothing was sent can be the clue.
func redactJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch {
			case val == nil || val == "":
			case secretName(k) && !plainCode(k, val):
				x[k] = redacted
			default:
				x[k] = redactJSON(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = redactJSON(x[i])
		}
	}
	return v
}

// clip cuts s short at maxLoggedBody, on a character boundary.
func clip(s string) string {
	if len(s) <= maxLoggedBody {
		return s
	}
	cut := maxLoggedBody
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s... (%d bytes in all)", s[:cut], len(s))
}

// hideSecretFields hides the values of the secret fields of a request
// body (see hide), so that one the server echoes back, in an error say,
// is not shown either.
func (c *Client) hideSecretFields(contentType string, body []byte) {
	if c.Debug == nil || len(body) == 0 {
		return
	}
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return
		}
		for k, vs := range form {
			if secretName(k) {
				c.Debug.hide(vs...)
			}
		}
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		c.Debug.hide(secretValues(v)...)
	}
}

// secretValues are the values of the secret keys in v, at any depth.
func secretValues(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if !secretName(k) || plainCode(k, val) {
				out = append(out, secretValues(val)...)
				continue
			}
			switch s := val.(type) {
			case string:
				out = append(out, s)
			case json.Number:
				out = append(out, s.String())
			default:
				out = append(out, secretValues(val)...)
			}
		}
	case []any:
		for _, e := range x {
			out = append(out, secretValues(e)...)
		}
	}
	return out
}
