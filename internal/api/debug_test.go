package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// The secrets of the debug run below. None of them may reach the log.
const (
	dbgAuthCode  = "authcode-5d1e7c9a"
	dbgVerifier  = "pkce-verifier-0b8e61f4d2"
	dbgFirstAT   = "access-first-91c2e4"
	dbgFirstRT   = "refresh-first-44ab07"
	dbgSecondAT  = "access-second-d3f5a8"
	dbgSecondRT  = "refresh-second-6c29b1"
	dbgPassword  = "Correct-Horse-9 battery"
	dbgCode      = "480913"
	dbgSignupAT  = "access-signup-7e1f30"
	dbgSessionID = "mcp-session-5a7d0e"
)

// A debug run through sign-in, signup, a renewal, an MCP call, a server
// error and a revocation shows each request and what came of it, and why
// the client renewed the token, but no token, code or password: not in
// headers, not in bodies, not even where the server echoes one back.
func TestDebugLogShowsNoSecrets(t *testing.T) {
	useFileStore(t)
	var meCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-"+strings.Trim(strings.ReplaceAll(r.URL.Path, "/", "-"), "-"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			_ = r.ParseForm()
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				if r.Form.Get("code") != dbgAuthCode || r.Form.Get("code_verifier") != dbgVerifier {
					t.Errorf("token form = %v", r.Form)
				}
				io.WriteString(w, `{"access_token":"`+dbgFirstAT+`","refresh_token":"`+dbgFirstRT+`","expires_in":3600}`)
			case "refresh_token":
				io.WriteString(w, `{"access_token":"`+dbgSecondAT+`","refresh_token":"`+dbgSecondRT+`","expires_in":3600}`)
			}
		case "/oauth/revoke":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/signup":
			// A server that echoes the password in its refusal.
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"error":"`+dbgPassword+` is too common"}`)
		case "/api/v1/signup/confirm":
			io.WriteString(w, `{"status":"confirmed","access_token":"`+dbgSignupAT+`","refresh_token":"rt","expires_in":3600}`)
		case "/api/v1/cli/me":
			// The first token is refused once, which makes the client renew.
			if meCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"Token expired"}`)
				return
			}
			io.WriteString(w, `{"data":{"user":{"email":"jan@yourfirm.example"}}}`)
		case "/mcp":
			w.Header().Set("Mcp-Session-Id", dbgSessionID)
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case "/api/v1/cli/boom":
			// A development server's error page that shows the request's
			// headers, the token with them.
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"boom","request":{"headers":"Authorization: `+r.Header.Get("Authorization")+`"}}`)
		}
	}))
	defer srv.Close()

	var log bytes.Buffer
	c := New(srv.URL)
	c.Debug = NewDebugLog(&log)
	ctx := context.Background()

	tr, err := c.ExchangeToken(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {dbgAuthCode}, "code_verifier": {dbgVerifier}, "redirect_uri": {"http://127.0.0.1:1/callback"}})
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	if err := c.SetToken(tr.Token()); err != nil {
		t.Fatal(err)
	}
	signup := map[string]any{"user": map[string]any{"email": "jan@yourfirm.example", "password": dbgPassword}}
	if err := c.JSON(ctx, http.MethodPost, "/api/v1/signup", signup, nil, false); err == nil {
		t.Fatal("signup: want the refusal")
	}
	confirm := map[string]any{"user": map[string]any{"email": "jan@yourfirm.example", "confirmation_code": dbgCode}}
	if err := c.JSON(ctx, http.MethodPost, "/api/v1/signup/confirm", confirm, &struct{}{}, false); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/v1/cli/me", nil, &struct{}{}, true); err != nil {
		t.Fatalf("me: %v", err)
	}
	call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_vendors", "arguments": map[string]any{"password": dbgPassword, "currency_code": "EUR"}}}
	payload, _ := json.Marshal(call)
	resp, err := c.Send(ctx, http.MethodPost, "/mcp", payload, map[string]string{"Content-Type": "application/json", "mcp-session-id": dbgSessionID}, true)
	if err != nil {
		t.Fatalf("mcp: %v", err)
	}
	resp.Body.Close()
	err = c.JSON(ctx, http.MethodGet, "/api/v1/cli/boom", nil, nil, true)
	if err == nil || !strings.HasSuffix(err.Error(), "(request id: req-api-v1-cli-boom)") {
		t.Errorf("server error = %v, want it to end with the request id", err)
	}
	if err := c.Revoke(ctx, dbgSecondRT, HintRefreshToken); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got := log.String()
	for _, secret := range []string{dbgAuthCode, dbgVerifier, dbgFirstAT, dbgFirstRT, dbgSecondAT, dbgSecondRT, dbgPassword, dbgCode, dbgSignupAT, dbgSessionID} {
		if strings.Contains(got, secret) {
			t.Errorf("the debug log shows %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{
		"debug: POST " + srv.URL + "/oauth/token\n",
		"debug: POST /oauth/token: 200 OK in ",
		", request id req-oauth-token\n",
		"debug:   (", // a body not shown
		"not shown: the body of /oauth/token holds secrets)",
		"not shown: the body of /api/v1/signup holds secrets)",
		"not shown: the body of /api/v1/signup/confirm holds secrets)",
		"not shown: the body of /oauth/revoke holds secrets)",
		"debug:   Authorization: Bearer [redacted]\n",
		"debug:   Mcp-Session-Id: [redacted]\n",
		"debug: GET /api/v1/cli/me: 401 Unauthorized in ",
		"debug: 401: renewing the access token, then sending the request again\n",
		"debug: renewed the access token\n",
		"debug: GET /api/v1/cli/me: 200 OK in ",
		`"arguments":{"currency_code":"EUR","password":"[redacted]"}`,
		"debug: GET /api/v1/cli/boom: 500 Internal Server Error in ",
		`debug:   {"error":"boom","request":{"headers":"Authorization: Bearer [redacted]"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the debug log lacks %q:\n%s", want, got)
		}
	}
}

// Without a debug log the client writes nothing, and works as before.
func TestNoDebugLogByDefault(t *testing.T) {
	c := New("http://127.0.0.1:1")
	c.Debugf("nothing %s", "here") // must not panic
	if c.Debug != nil {
		t.Error("a new client must not log")
	}
}

// An error the server did not explain ends with the request id: a 5xx,
// whatever its body, an answer that is not the API's JSON error, and a
// success whose body the CLI cannot read. One the API explains, a rate
// limit, and a header that holds no id look as they did.
func TestUnexplainedErrorsCarryTheRequestID(t *testing.T) {
	useFileStore(t)
	type answer struct {
		status      int
		contentType string
		body, id    string
	}
	cases := []struct {
		name   string
		answer answer
		want   string
	}{
		{"5xx with JSON", answer{500, "application/json", `{"error":"Something went wrong"}`, "req-1"}, "Something went wrong (request id: req-1)"},
		{"5xx page", answer{502, "text/html", `<h1>Bad gateway</h1>`, "req-2"}, "Bad Gateway (request id: req-2)"},
		{"unexpected 4xx page", answer{400, "text/html", `<h1>Bad request</h1>`, "req-3"}, "Bad Request (request id: req-3)"},
		{"unreadable success", answer{200, "application/json", `<html>`, "req-4"}, "unexpected answer from the server: invalid character '<' looking for beginning of value (request id: req-4)"},
		{"empty success", answer{200, "application/json", ``, "req-5"}, "unexpected answer from the server: EOF (request id: req-5)"},
		{"explained 4xx", answer{422, "application/json", `{"error":"Name is taken","code":"TAKEN"}`, "req-6"}, "Name is taken"},
		{"rate limit", answer{429, "text/plain", `slow down`, "req-7"}, "too many requests; wait a while and try again"},
		{"5xx without an id", answer{500, "application/json", `{"error":"Something went wrong"}`, ""}, "Something went wrong"},
		{"not an id", answer{500, "application/json", `{"error":"Something went wrong"}`, "req 8; rm -rf"}, "Something went wrong"},
	}
	var current atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := current.Load().(answer)
		if a.id != "" {
			w.Header().Set("X-Request-Id", a.id)
		}
		w.Header().Set("Content-Type", a.contentType)
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}))
	defer srv.Close()
	c := New(srv.URL)
	for _, tc := range cases {
		current.Store(tc.answer)
		err := c.JSON(context.Background(), http.MethodGet, "/api/v1/cli/me", nil, &struct{}{}, false)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

// The request id stays out of an interrupted request's error, and the
// error still says what it is.
func TestRequestIDKeepsTheErrorItWraps(t *testing.T) {
	resp := &http.Response{Header: http.Header{"X-Request-Id": {"req-9"}}}
	if err := WithRequestID(context.Canceled, resp); err != context.Canceled { //nolint:errorlint // the very same error
		t.Errorf("an interrupt got %v", err)
	}
	err := WithRequestID(io.ErrUnexpectedEOF, resp)
	if !errors.Is(err, io.ErrUnexpectedEOF) || err.Error() != "unexpected EOF (request id: req-9)" {
		t.Errorf("got %v", err)
	}
	if WithRequestID(nil, resp) != nil {
		t.Error("no error must stay no error")
	}
}

// Secret fields are redacted wherever they are in a body, while the names
// a code field can hold (an error code, a currency) stay readable.
func TestBodyForLogRedactsSecretFields(t *testing.T) {
	body := `{"jsonrpc":"2.0","params":{"arguments":{"api_key":"k-123456","nested":[{"refresh_token":"r-123456"}],"code":"123456","currency_code":"EUR","error_code":"NOT_FOUND","vendor":"Acme <S&P>","limit":20,"password":""}}}`
	got := bodyForLog("/mcp", "application/json", []byte(body))
	want := `{"jsonrpc":"2.0","params":{"arguments":{"api_key":"[redacted]","code":"[redacted]","currency_code":"EUR","error_code":"NOT_FOUND","limit":20,"nested":[{"refresh_token":"[redacted]"}],"password":"","vendor":"Acme <S&P>"}}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := bodyForLog("/other", "application/x-www-form-urlencoded", []byte("token=abc123456&a=b&client_secret=xyz")); got != "token=[redacted]&a=b&client_secret=[redacted]" {
		t.Errorf("form: got %s", got)
	}
	long := strings.Repeat("é", maxLoggedBody)
	if got := bodyForLog("/other", "text/plain", []byte(long)); !strings.HasSuffix(got, "... (4096 bytes in all)") || !strings.HasPrefix(got, "éé") || len(got) > maxLoggedBody+30 {
		t.Errorf("long body: %d bytes, ends %q", len(got), got[len(got)-30:])
	}
	for _, path := range []string{"/oauth/token", "/oauth/revoke", "/api/v1/signup", "/api/v1/signup/confirm"} {
		if got := bodyForLog(path, "application/json", []byte(`{"a":1}`)); !strings.Contains(got, "not shown") {
			t.Errorf("%s: got %s", path, got)
		}
	}
}

// Secret query parameters, and the credential of an Authorization header,
// are redacted; the rest is kept as sent.
func TestURLAndHeadersForLog(t *testing.T) {
	u, _ := url.Parse("https://firmfact.example/api/v1/x?locale=en&access_token=abc&code=123456&q=a%20b")
	if got := urlForLog(u); got != "https://firmfact.example/api/v1/x?locale=en&access_token=[redacted]&code=[redacted]&q=a%20b" {
		t.Errorf("url: got %s", got)
	}
	if got := pathForLog(u); got != "/api/v1/x?locale=en&access_token=[redacted]&code=[redacted]&q=a%20b" {
		t.Errorf("path: got %s", got)
	}
	headers := map[string]string{
		"Authorization":  "Bearer abc.def",
		"Mcp-Session-Id": "s-1",
		"Cookie":         "a=b",
		"Content-Type":   "application/json",
		"User-Agent":     "firmfact-cli/dev",
	}
	want := map[string]string{
		"Authorization":  "Bearer [redacted]",
		"Mcp-Session-Id": "[redacted]",
		"Cookie":         "[redacted]",
		"Content-Type":   "application/json",
		"User-Agent":     "firmfact-cli/dev",
	}
	for name, v := range headers {
		if got := headerForLog(name, v); got != want[name] {
			t.Errorf("%s: got %q, want %q", name, got, want[name])
		}
	}
	if got := locationForLog("https://evil.example/cb?token=abc&next=/"); got != "https://evil.example/cb?token=[redacted]&next=/" {
		t.Errorf("location: got %s", got)
	}
}

// Lines are written whole, even from clients on several goroutines, and a
// server's terminal controls are escaped.
func TestDebugLogLines(t *testing.T) {
	var buf bytes.Buffer
	d := NewDebugLog(&buf)
	d.hide("abc", "secret-value")
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				d.Printf("line with secret-value and abc\x1b]52;c;x\x07")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 200 {
		t.Fatalf("got %d lines", len(lines))
	}
	for _, line := range lines {
		if line != `debug: line with [redacted] and abc\u001b]52;c;x\u0007` {
			t.Fatalf("line = %q", line)
		}
	}
	var nilLog *DebugLog
	nilLog.Printf("nothing")
	nilLog.hide("x")
}
