package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
)

// The debug log names each MCP request's method and tool and says why the
// client renews the token and opens a new session, with the server's id for
// each request; the tokens and session ids stay out of it. An error that is
// the server's own carries the request id, one about the request does not.
func TestDebugLogNamesMethodsAndTools(t *testing.T) {
	setup(t)
	const (
		oldAT, oldRT     = "mcp-access-old-1a2b3c", "mcp-refresh-old-7c8d9e"
		newAT, newRT     = "mcp-access-new-2f8c1d", "mcp-refresh-new-9e0b7a"
		oldSess, newSess = "mcp-sess-old-000111", "mcp-sess-new-4d5e6f"
	)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", fmt.Sprintf("req-%d", n.Add(1)))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth/token" {
			io.WriteString(w, `{"access_token":"`+newAT+`","refresh_token":"`+newRT+`","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+newAT {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Token expired"}`)
			return
		}
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case req.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", newSess)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
		case req.Params.Name == "broken":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32603,"message":"Internal error"}}`, req.ID)
		case req.Params.Name == "unknown":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Unknown tool: unknown"}}`, req.ID)
		case req.Params.Name == "cut":
			// A stream that ends before the answer.
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, ": ping\n\n")
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{}"}]}}`, req.ID)
		}
	}))
	defer srv.Close()

	var log bytes.Buffer
	c := api.New(srv.URL)
	c.Debug = api.NewDebugLog(&log)
	if err := c.SetToken(&config.Token{AccessToken: oldAT, RefreshToken: oldRT, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	saveSession(srv.URL, oldSess) // an earlier command's
	m := New(c)
	ctx := context.Background()
	if _, err := m.CallTool(ctx, Tool{Name: "list_vendors"}, map[string]any{"q": "Acme"}); err != nil {
		t.Fatalf("list_vendors: %v", err)
	}
	if _, err := m.CallTool(ctx, Tool{Name: "broken"}, nil); err == nil || err.Error() != "Internal error (request id: req-5)" {
		t.Errorf("internal error = %v, want the message with the request id", err)
	}
	if _, err := m.CallTool(ctx, Tool{Name: "unknown"}, nil); err == nil || err.Error() != "Unknown tool: unknown" {
		t.Errorf("unknown tool = %v, want the message alone", err)
	}
	if _, err := m.CallTool(ctx, Tool{Name: "cut"}, nil); err == nil || !strings.HasSuffix(err.Error(), "(request id: req-7)") {
		t.Errorf("broken-off answer = %v, want it to end with the request id", err)
	}

	got := log.String()
	for _, secret := range []string{oldAT, oldRT, newAT, newRT, oldSess, newSess} {
		if strings.Contains(got, secret) {
			t.Errorf("the debug log shows %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{
		"debug: mcp: reusing the session an earlier command opened\n",
		"debug: mcp tools/call list_vendors\n",
		"debug: POST /mcp: 401 Unauthorized in ",
		"debug: 401: renewing the access token, then sending the request again\n",
		"debug: the MCP session belongs to the old token, so a new one is needed\n",
		"debug: mcp: opening a new session and sending tools/call again\n",
		"debug: mcp initialize\n",
		"debug:   Mcp-Session-Id: [redacted]\n",
		`"method":"tools/call","params":{"arguments":{"q":"Acme"},"name":"list_vendors"}`,
		"debug: POST /mcp: 200 OK in ",
		", request id req-5\n",
		"debug: mcp tools/call broken: error -32603: Internal error\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the debug log lacks %q:\n%s", want, got)
		}
	}
}
