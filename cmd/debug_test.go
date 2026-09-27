package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
)

// --debug and FIRMFACT_DEBUG=1 log a sign-in and a workspace command on
// stderr: the requests to /oauth/token and /mcp, the MCP method and tool,
// the renewal of the token and the server's request ids, but no token,
// code or session id. Without either, and with FIRMFACT_DEBUG=0, nothing
// is logged.
func TestDebugLogsRequestsWithoutSecrets(t *testing.T) {
	isolate(t)
	const (
		authCode   = "authcode-3e9f71b2"
		firstAT    = "access-first-5c0d8e"
		firstRT    = "refresh-first-a17b44"
		secondAT   = "access-second-0f9e2d"
		secondRT   = "refresh-second-6b3c91"
		sessionID  = "mcp-session-c4d2a8"
		requestIDs = "req-"
	)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", fmt.Sprintf("%s%d", requestIDs, n.Add(1)))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "authorization_code" {
				if r.Form.Get("code") != authCode {
					t.Errorf("code = %q", r.Form.Get("code"))
				}
				io.WriteString(w, `{"access_token":"`+firstAT+`","refresh_token":"`+firstRT+`","expires_in":3600}`)
				return
			}
			io.WriteString(w, `{"access_token":"`+secondAT+`","refresh_token":"`+secondRT+`","expires_in":3600}`)
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"email":"jan@yourfirm.example"},"workspaces":[{"id":"demo-1","name":"Demo","default":true}]}}`)
		case "/mcp":
			if a := r.Header.Get("Authorization"); a != "Bearer "+firstAT && a != "Bearer "+secondAT {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"Token expired"}`)
				return
			}
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req.Method {
			case "initialize":
				w.Header().Set("Mcp-Session-Id", sessionID)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
			case "tools/list":
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"list_vendors","title":"List vendors","description":"Vendors.","inputSchema":{"type":"object","properties":{}},"annotations":{"readOnlyHint":true}}]}}`, req.ID)
			default:
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{\"data\":[{\"name\":\"Acme\"}]}"}]}}`, req.ID)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	useBrowser(t, func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		go func() {
			cb := url.Values{"code": {authCode}, "state": {q.Get("state")}}
			if resp, err := http.Get(q.Get("redirect_uri") + "?" + cb.Encode()); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	})

	_, loginLog, err := run("test", "--debug", "--host", srv.URL, "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// Expired by this machine's clock, so the next command renews it first.
	tok, err := config.LoadToken(srv.URL)
	if err != nil || tok == nil {
		t.Fatalf("stored token = %v, %v", tok, err)
	}
	tok.ExpiresAt = time.Now().Add(-time.Minute)
	if err := config.SaveToken(srv.URL, tok); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRMFACT_DEBUG", "1")
	stdout, callLog, err := run("test", "--host", srv.URL, "vendors", "list")
	if err != nil {
		t.Fatalf("vendors list: %v", err)
	}
	if !strings.Contains(stdout, "Acme") {
		t.Errorf("stdout = %q", stdout)
	}

	log := loginLog + callLog
	for _, secret := range []string{authCode, firstAT, firstRT, secondAT, secondRT, sessionID} {
		if strings.Contains(log, secret) {
			t.Errorf("the debug log shows %q:\n%s", secret, log)
		}
	}
	for _, want := range []string{
		"debug: POST " + srv.URL + "/oauth/token\n",
		"not shown: the body of /oauth/token holds secrets)",
		"debug: mcp initialize\n",
		"debug: mcp tools/list\n",
		"debug: mcp tools/call list_vendors\n",
		"debug:   Authorization: Bearer [redacted]\n",
		"debug: the access token has expired by this machine's clock; renewing it first\n",
		"debug: renewed the access token\n",
		"debug: the MCP session belongs to the old token, so a new one is needed\n",
		"debug: POST /mcp: 200 OK in ",
		", request id " + requestIDs,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the debug log lacks %q:\n%s", want, log)
		}
	}

	for _, env := range []string{"0", ""} {
		t.Setenv("FIRMFACT_DEBUG", env)
		_, stderr, err := run("test", "--host", srv.URL, "vendors", "list")
		if err != nil {
			t.Fatalf("FIRMFACT_DEBUG=%q: %v", env, err)
		}
		if strings.Contains(stderr, "debug:") {
			t.Errorf("FIRMFACT_DEBUG=%q logged:\n%s", env, stderr)
		}
	}
}
