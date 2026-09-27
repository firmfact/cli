package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
)

type fakeServer struct {
	inits, calls, refreshes, throttled atomic.Int32
	valid                              atomic.Value // the session id the server accepts
	throttleNext                       atomic.Bool
	// Like firmfact, the server binds a session to the access token that
	// opened it, and answers "Token expired" before it looks at the session.
	owner    atomic.Value // the Authorization header that opened the valid session
	expired  atomic.Value // an Authorization header the server no longer accepts
	rejected atomic.Int32 // "Invalid session" answers
}

func (f *fakeServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			f.refreshes.Add(1)
			io.WriteString(w, `{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`)
			return
		}
		if expired, _ := f.expired.Load().(string); expired != "" && r.Header.Get("Authorization") == expired {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Token expired"}`)
			return
		}
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if f.throttleNext.CompareAndSwap(true, false) {
			f.throttled.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if req.Method == "initialize" {
			f.inits.Add(1)
			id := "sess-" + string(rune('0'+f.inits.Load()))
			f.valid.Store(id)
			f.owner.Store(r.Header.Get("Authorization"))
			w.Header().Set("mcp-session-id", id)
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		valid, _ := f.valid.Load().(string)
		owner, _ := f.owner.Load().(string)
		if r.Header.Get("mcp-session-id") != valid || r.Header.Get("Authorization") != owner {
			f.rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Invalid session"}`)
			return
		}
		f.calls.Add(1)
		io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"data\":[]}"}],"isError":false}}`)
	}
}

func newClient(t *testing.T, url string) *api.Client {
	c := api.New(url)
	if err := c.SetToken(&config.Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return c
}

func setup(t *testing.T) {
	// The CLI creates its config directory, so its mode is not the umask's.
	t.Setenv("FIRMFACT_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("FIRMFACT_CACHE_DIR", t.TempDir())
	t.Setenv("FIRMFACT_TOKEN_STORE", "file")
}

// Two commands in a row open one session between them.
func TestSessionIsReusedAcrossCommands(t *testing.T) {
	setup(t)
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	for i := 0; i < 2; i++ {
		if _, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "list_vendors"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if f.inits.Load() != 1 || f.calls.Load() != 2 {
		t.Fatalf("inits=%d calls=%d, want 1 and 2", f.inits.Load(), f.calls.Load())
	}
}

// A session id is written to a file of its own and renamed into place, not
// through a symlink planted at its name.
func TestSessionIsNotWrittenThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a symbolic link takes a privilege on Windows")
	}
	setup(t)
	host := "https://a.example"
	path := sessionPath(host)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(victim, []byte("export KEEP=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	saveSession(host, "sess-1")
	if got, err := os.ReadFile(victim); err != nil || string(got) != "export KEEP=1\n" {
		t.Errorf("the link's target now holds %q, %v", got, err)
	}
	if got := loadSession(host); got != "sess-1" {
		t.Errorf("session = %q, want sess-1", got)
	}
}

// A session the server no longer accepts is replaced, without renewing the token.
func TestInvalidSessionReopensWithoutTokenRefresh(t *testing.T) {
	setup(t)
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	if _, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "list_vendors"}, nil); err != nil {
		t.Fatal(err)
	}
	f.valid.Store("something-else") // server forgot the session
	if _, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "list_vendors"}, nil); err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if f.inits.Load() != 2 || f.refreshes.Load() != 0 {
		t.Fatalf("inits=%d refreshes=%d, want 2 and 0", f.inits.Load(), f.refreshes.Load())
	}
}

// A token that expires under a remembered session: the renewed token cannot
// resume that session, so the call opens a new one rather than sending the
// old id with the new token and failing on "Invalid session".
func TestTokenExpiredOnTheServerOpensANewSession(t *testing.T) {
	setup(t)
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	// An earlier command opened sess-0 with the token that has now run out
	// on the server, though not yet by the CLI's clock.
	f.valid.Store("sess-0")
	f.owner.Store("Bearer at")
	f.expired.Store("Bearer at")
	saveSession(srv.URL, "sess-0")

	res, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "list_vendors"}, nil)
	if err != nil {
		t.Fatalf("expected the call to succeed, got %v", err)
	}
	if res.IsError {
		t.Fatalf("result is an error: %+v", res)
	}
	if f.inits.Load() != 1 || f.refreshes.Load() != 1 || f.calls.Load() != 1 {
		t.Fatalf("inits=%d refreshes=%d calls=%d, want 1, 1 and 1", f.inits.Load(), f.refreshes.Load(), f.calls.Load())
	}
	if f.rejected.Load() != 0 {
		t.Errorf("the old session id was sent with the new token %d times", f.rejected.Load())
	}
	if got := loadSession(srv.URL); got != "sess-1" {
		t.Errorf("remembered session = %q, want the new sess-1", got)
	}
}

// A token the CLI renews before sending, because by its own clock it has
// expired, drops the remembered session in the same way.
func TestRenewalBeforeSendingOpensANewSession(t *testing.T) {
	setup(t)
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	f.valid.Store("sess-0")
	f.owner.Store("Bearer at")
	f.expired.Store("Bearer at")
	saveSession(srv.URL, "sess-0")

	c := api.New(srv.URL)
	if err := c.SetToken(&config.Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(c).CallTool(context.Background(), Tool{Name: "list_vendors"}, nil); err != nil {
		t.Fatalf("expected the call to succeed, got %v", err)
	}
	if f.inits.Load() != 1 || f.refreshes.Load() != 1 || f.calls.Load() != 1 || f.rejected.Load() != 0 {
		t.Fatalf("inits=%d refreshes=%d calls=%d rejected=%d, want 1, 1, 1 and 0",
			f.inits.Load(), f.refreshes.Load(), f.calls.Load(), f.rejected.Load())
	}
}

// A 429 with a short Retry-After is waited out and retried.
func TestRateLimitIsRetried(t *testing.T) {
	setup(t)
	f := &fakeServer{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := New(newClient(t, srv.URL))
	if err := c.ensureSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.throttleNext.Store(true)
	if _, err := c.CallTool(context.Background(), Tool{Name: "list_vendors"}, nil); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if f.throttled.Load() != 1 || f.calls.Load() != 1 {
		t.Fatalf("throttled=%d calls=%d", f.throttled.Load(), f.calls.Load())
	}
}

// lossyServer opens sessions and answers tools/call, but loses the first
// `lose` calls on the way: with status and a proxy's HTML error page, or,
// when status is 0, by resetting the connection.
type lossyServer struct {
	status int
	lose   int32
	calls  atomic.Int32
}

func (s *lossyServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("mcp-session-id", "sess-1")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		if s.calls.Add(1) <= s.lose {
			if s.status != 0 {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(s.status)
				io.WriteString(w, "<html><body>Bad Gateway</body></html>")
				return
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.(*net.TCPConn).SetLinger(0) // the close sends a reset
			conn.Close()
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"[]"}],"isError":false}}`, req.ID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A tool that only reads is called once more after a gateway's error or a
// reset connection, since calling it twice does no harm; once only. Any
// other tool, or any other failure, fails at once: the lost call may have
// run, and a tool that changes something must not run twice.
func TestReadOnlyToolIsRetriedOnceWhenLost(t *testing.T) {
	yes, no := true, false
	reader := Tool{Name: "list_vendors", Annotations: &ToolAnnotations{ReadOnlyHint: &yes}}
	writer := Tool{Name: "chat_with_workspace", Annotations: &ToolAnnotations{ReadOnlyHint: &no}}
	unhinted := Tool{Name: "list_vendors"}
	cases := []struct {
		name   string
		tool   Tool
		status int // 0 resets the connection
		lose   int32
		ok     bool
		calls  int32
	}{
		{"read-only after a 502", reader, http.StatusBadGateway, 1, true, 2},
		{"read-only after a 504", reader, http.StatusGatewayTimeout, 1, true, 2},
		{"read-only after a reset", reader, 0, 1, true, 2},
		{"read-only lost twice", reader, http.StatusBadGateway, 2, false, 2},
		{"read-only after a 500", reader, http.StatusInternalServerError, 1, false, 1},
		{"not read-only after a 502", writer, http.StatusBadGateway, 1, false, 1},
		{"not read-only after a reset", writer, 0, 1, false, 1},
		{"no hint after a 504", unhinted, http.StatusGatewayTimeout, 1, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			s := &lossyServer{status: tc.status, lose: tc.lose}
			srv := s.start(t)
			_, err := New(newClient(t, srv.URL)).CallTool(context.Background(), tc.tool, nil)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v, want success %v", err, tc.ok)
			}
			if n := s.calls.Load(); n != tc.calls {
				t.Errorf("calls = %d, want %d", n, tc.calls)
			}
		})
	}
}

// The hints come with tools/list and survive a round trip through JSON, as
// the tool cache takes them. A hint left out takes MCP's default: a tool
// without them is not read-only and counts as destructive, and only
// destructiveHint false makes a tool that writes additive.
func TestToolAnnotations(t *testing.T) {
	var tools []Tool
	raw := `[{"name":"list_vendors","annotations":{"readOnlyHint":true,"destructiveHint":false}},
		{"name":"chat_with_workspace","annotations":{"readOnlyHint":false}},
		{"name":"old_tool"},
		{"name":"add_note","annotations":{"readOnlyHint":false,"destructiveHint":false}},
		{"name":"delete_vendor","annotations":{"destructiveHint":true}},
		{"name":"read_with_odd_hint","annotations":{"readOnlyHint":true,"destructiveHint":true}}]`
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(tools)
	tools = nil
	if err := json.Unmarshal(again, &tools); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ readOnly, destructive bool }{
		{true, false}, {false, true}, {false, true}, {false, false}, {false, true}, {true, false},
	} {
		if got := tools[i].ReadOnly(); got != want.readOnly {
			t.Errorf("%s: ReadOnly() = %v, want %v", tools[i].Name, got, want.readOnly)
		}
		if got := tools[i].Destructive(); got != want.destructive {
			t.Errorf("%s: Destructive() = %v, want %v", tools[i].Name, got, want.destructive)
		}
	}
	if d := tools[0].Annotations.DestructiveHint; d == nil || *d {
		t.Errorf("destructiveHint = %v, want false kept", d)
	}
}
