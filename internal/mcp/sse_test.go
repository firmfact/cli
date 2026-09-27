package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/httpx"
)

// streamServer answers initialize with JSON and tools/call with an event
// stream, as firmfact's MCP controller does for a chat: send writes the
// events, with the id of the request to answer. accept records the Accept
// header of the tools/call.
func streamServer(t *testing.T, accept *atomic.Value, send func(w io.Writer, flush func(), r *http.Request, id int)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("mcp-session-id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
			return
		}
		if accept != nil {
			accept.Store(r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flush := func() { w.(http.Flusher).Flush() }
		flush()
		send(w, flush, r, req.ID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func answer(id int, text string) string {
	result, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false},
	})
	return string(result)
}

// A chat answered as a stream: the started event, comments and pings come
// first, the response last. The request says it takes an event stream, or
// the server would hold the whole answer back until it is done.
func TestStreamedAnswerAfterPings(t *testing.T) {
	setup(t)
	var accept atomic.Value
	srv := streamServer(t, &accept, func(w io.Writer, flush func(), _ *http.Request, id int) {
		fmt.Fprint(w, "retry: 3000\nevent: message\ndata: {\"type\":\"started\",\"tool\":\"chat_with_workspace\"}\n\n")
		flush()
		for i := 0; i < 3; i++ {
			fmt.Fprint(w, ": keepalive\n\n")
			fmt.Fprint(w, "event: message\ndata: {\"type\":\"ping\",\"timestamp\":\"2026-09-27T10:00:00Z\"}\n\n")
			flush()
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", answer(id, `{"response":"Three contracts renew in October."}`))
	})

	res, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "chat_with_workspace"}, map[string]any{"message": "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Text(); got != `{"response":"Three contracts renew in October."}` {
		t.Errorf("result = %q", got)
	}
	got, _ := accept.Load().(string)
	if !strings.Contains(got, "application/json") || !strings.Contains(got, "text/event-stream") {
		t.Errorf("Accept = %q, want both application/json and text/event-stream", got)
	}
}

// Only the response to this request ends the wait: a notification, the
// response to another request, a request from the server that happens to
// carry the same id, and events of another type are passed over. The
// response itself may be split over data lines, with CRLF line ends.
func TestStreamSkipsOtherMessages(t *testing.T) {
	setup(t)
	srv := streamServer(t, nil, func(w io.Writer, flush func(), _ *http.Request, id int) {
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n")
		fmt.Fprintf(w, "data: %s\n\n", answer(id+100, "for someone else"))
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"sampling/createMessage\",\"params\":{}}\n\n", id)
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", answer(id, "not a message event"))
		fmt.Fprint(w, "data: not json at all\n\n")
		flush()
		whole := answer(id, "the answer")
		cut := strings.Index(whole, `"result"`)
		fmt.Fprintf(w, "event: message\r\ndata: %s\r\ndata: %s\r\n\r\n", whole[:cut], whole[cut:])
		fmt.Fprintf(w, "data: %s\n\n", answer(id, "too late"))
	})

	res, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "chat_with_workspace"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Text(); got != "the answer" {
		t.Errorf("result = %q, want the answer", got)
	}
}

// An error response in the stream is the call's error.
func TestStreamedError(t *testing.T) {
	setup(t)
	srv := streamServer(t, nil, func(w io.Writer, _ func(), _ *http.Request, id int) {
		fmt.Fprint(w, "data: {\"type\":\"ping\"}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-32603,\"message\":\"Internal error\"}}\n\n", id)
	})
	_, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "chat_with_workspace"}, nil)
	if err == nil || err.Error() != "Internal error" {
		t.Fatalf("err = %v, want the server's error", err)
	}
}

// A stream that stops sending ends at the limit for the call, with a
// message that names it, not after the CLI's usual 60 s and not never.
func TestStalledStreamStopsAtTheLimit(t *testing.T) {
	setup(t)
	srv := streamServer(t, nil, func(w io.Writer, flush func(), r *http.Request, _ int) {
		fmt.Fprint(w, "data: {\"type\":\"started\"}\n\ndata: {\"type\":\"ping\"}\n\n")
		flush()
		<-r.Context().Done() // stalls until the client gives up
	})

	m := New(newClient(t, srv.URL))
	m.Limit = 300 * time.Millisecond
	start := time.Now()
	_, err := m.CallTool(context.Background(), Tool{Name: "chat_with_workspace"}, nil)
	took := time.Since(start)

	var e *httpx.Error
	if !errors.As(err, &e) || e.Kind != httpx.AnswerTimeout {
		t.Fatalf("want an AnswerTimeout *httpx.Error, got %#v", err)
	}
	if want := strings.TrimPrefix(srv.URL, "http://") + " did not finish answering within 300ms"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if took > 3*time.Second {
		t.Errorf("gave up after %s", took)
	}
}

// Ctrl-C during a stream stops it as an interrupt, not as a slow server.
func TestInterruptedStreamIsNotATimeout(t *testing.T) {
	setup(t)
	srv := streamServer(t, nil, func(w io.Writer, flush func(), r *http.Request, _ int) {
		fmt.Fprint(w, "data: {\"type\":\"ping\"}\n\n")
		flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := New(newClient(t, srv.URL)).CallTool(ctx, Tool{Name: "chat_with_workspace"}, nil)
	var e *httpx.Error
	if !errors.Is(err, context.Canceled) || errors.As(err, &e) {
		t.Fatalf("want context.Canceled, got %#v", err)
	}
}

// A stream that ends without the response says so.
func TestStreamEndingWithoutAnswer(t *testing.T) {
	setup(t)
	srv := streamServer(t, nil, func(w io.Writer, _ func(), _ *http.Request, id int) {
		fmt.Fprint(w, "data: {\"type\":\"ping\"}\n\n")
		// An event cut off before its blank line does not count.
		fmt.Fprintf(w, "data: %s\n", answer(id, "incomplete"))
	})
	_, err := New(newClient(t, srv.URL)).CallTool(context.Background(), Tool{Name: "chat_with_workspace"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ended its answer before the result came") {
		t.Fatalf("err = %v", err)
	}
}

// A proxy's error page for 502, 503 or 504 becomes a sentence the user can
// act on, not "Gateway Timeout"; an error the server put in JSON is kept.
func TestGatewayErrorPages(t *testing.T) {
	setup(t)
	var status atomic.Int32
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("mcp-session-id", "sess-1")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			return
		}
		b := body.Load().(string)
		if strings.HasPrefix(b, "<") {
			w.Header().Set("Content-Type", "text/html")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(int(status.Load()))
		io.WriteString(w, b)
	}))
	defer srv.Close()

	page := "<html><head><title>504 Gateway Time-out</title></head><body><h1>Gateway Time-out</h1></body></html>"
	cases := []struct {
		status int
		body   string
		list   bool
		want   string
	}{
		{http.StatusGatewayTimeout, page, false, "the server took too long to answer; try a narrower question or retry"},
		{http.StatusBadGateway, page, false, "the server took too long to answer; try a narrower question or retry"},
		{http.StatusServiceUnavailable, "", false, "the server took too long to answer; try a narrower question or retry"},
		{http.StatusGatewayTimeout, page, true, "the server took too long to answer; retry"},
		{http.StatusServiceUnavailable, `{"error":"Down for maintenance until 14:00"}`, false, "Down for maintenance until 14:00"},
	}
	for _, tc := range cases {
		status.Store(int32(tc.status))
		body.Store(tc.body)
		m := New(newClient(t, srv.URL))
		var err error
		if tc.list {
			_, err = m.ListTools(context.Background())
		} else {
			_, err = m.CallTool(context.Background(), Tool{Name: "chat_with_workspace"}, nil)
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status || err.Error() != tc.want {
			t.Errorf("%d %.20q: err = %#v (%v), want %q", tc.status, tc.body, err, err, tc.want)
		}
	}
}

// A chat may take minutes; anything else gets 90 s, and --timeout sets
// both.
func TestLimits(t *testing.T) {
	m := &Client{}
	if got := m.limit("chat_with_workspace"); got != ChatLimit {
		t.Errorf("chat limit = %s", got)
	}
	if got := m.limit("list_vendors"); got != CallLimit {
		t.Errorf("list limit = %s", got)
	}
	if got := m.limit(""); got != CallLimit {
		t.Errorf("initialize limit = %s", got)
	}
	m.Limit = 10 * time.Second
	if got := m.limit("chat_with_workspace"); got != 10*time.Second {
		t.Errorf("chosen limit = %s", got)
	}
}

func TestIsEventStream(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/event-stream":                true,
		"text/event-stream; charset=utf-8": true,
		"Text/Event-Stream":                true,
		"application/json":                 false,
		"":                                 false,
	} {
		if got := isEventStream(ct); got != want {
			t.Errorf("isEventStream(%q) = %v", ct, got)
		}
	}
}
