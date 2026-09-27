// Package mcp speaks firmfact's MCP endpoint (Streamable HTTP, JSON-RPC over
// POST /mcp): initialize once for a session id, then tools/list and
// tools/call. An answer comes as one JSON body, or as an event stream for a
// slow tool such as a chat.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
)

const ProtocolVersion = "2025-03-26"

// The limits for one MCP request: sending it, the server's work and the
// whole answer. Each request gets its own as a context deadline, which
// takes the place of the CLI's usual 60 s for a request (see
// httpx.Client.Do); the waits for the connection and for the server to
// start answering still apply.
const (
	// CallLimit is the limit for a request that is not a chat.
	CallLimit = 90 * time.Second
	// ChatLimit is the limit for a chat. The server streams the answer
	// while a model works on it, which can take minutes.
	ChatLimit = 5 * time.Minute
)

// chatTools are the tools that get ChatLimit.
var chatTools = map[string]bool{"chat_with_workspace": true}

type Client struct {
	API *api.Client
	// Limit, when set, is the limit for every request in place of
	// CallLimit and ChatLimit: the user's own --timeout.
	Limit     time.Duration
	sessionID string
	nextID    int
}

// New returns a client that reuses the MCP session of an earlier command on
// the same host while it is fresh, so a command costs one request instead of
// two against the server's rate limit.
func New(c *api.Client) *Client {
	m := &Client{API: c}
	m.sessionID = loadSession(c.Host)
	if m.sessionID != "" {
		c.Debugf("mcp: reusing the session an earlier command opened")
	}
	return m
}

// sessionTTL stays under the server's one-hour session lifetime.
const sessionTTL = 55 * time.Minute

type savedSession struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
}

func sessionPath(host string) string {
	dir, err := config.CacheDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir, "mcp-session-"+hex.EncodeToString(sum[:6])+".json")
}

func loadSession(host string) string {
	path := sessionPath(host)
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s savedSession
	if json.Unmarshal(raw, &s) != nil || time.Now().After(s.Expires) {
		return ""
	}
	return s.ID
}

// ForgetSession drops the remembered session for host (used by logout).
func ForgetSession(host string) { saveSession(host, "") }

func saveSession(host, id string) {
	path := sessionPath(host)
	if path == "" {
		return
	}
	if id == "" {
		_ = os.Remove(path)
		return
	}
	// Renamed into place, as the CLI's other caches are, rather than written
	// through whatever is at the name.
	_ = config.WriteJSON(path, savedSession{ID: id, Expires: time.Now().Add(sessionTTL)}, 0o600)
}

// Tool is one entry of tools/list.
type Tool struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description"`
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are the hints a tool comes with (MCP 2025-03-26). They
// are the server's word, not a guarantee. The CLI uses readOnlyHint to
// decide that a failed call may be sent again and to mark the tools that
// write, and destructiveHint to ask before a call. A hint the server leaves
// out stays nil, as its default differs from hint to hint.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly reports whether the server says the tool changes nothing. A tool
// without the hint may: the default is false.
func (t Tool) ReadOnly() bool {
	return t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// Destructive reports whether the tool may delete or overwrite data rather
// than only add to it. That is any tool that writes, unless the server says
// its changes are additive (destructiveHint false): the default is true, so
// a tool the server says nothing about counts as destructive.
func (t Tool) Destructive() bool {
	if t.ReadOnly() {
		return false
	}
	return t.Annotations == nil || t.Annotations.DestructiveHint == nil || *t.Annotations.DestructiveHint
}

// Content is one block of a tools/call result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type CallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

// Text joins the result's text blocks.
func (r *CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes the server uses (JSON-RPC 2.0, section 5.1).
const (
	// ErrMethodNotFound is also the server's answer to tools/call for a
	// tool it does not have.
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
)

// RPCError is a JSON-RPC error the server answered a request with.
type RPCError struct {
	// Method is the request's method, such as tools/call.
	Method  string
	Code    int
	Message string
	// RequestID is the id the server gave the request (see api.RequestID).
	RequestID string
}

// Error is the server's message. It ends with the request id, when there
// is one, for an error that is the server's rather than the request's:
// anything but a tool or arguments it does not know, such as an internal
// error, is its equivalent of a 5xx.
func (e *RPCError) Error() string {
	if e.RequestID != "" && e.Code != ErrMethodNotFound && e.Code != ErrInvalidParams {
		return e.Message + " (request id: " + e.RequestID + ")"
	}
	return e.Message
}

// limit is how long a request for tool (empty for anything but
// tools/call) may take.
func (c *Client) limit(tool string) time.Duration {
	switch {
	case c.Limit > 0:
		return c.Limit
	case chatTools[tool]:
		return ChatLimit
	}
	return CallLimit
}

// call sends one JSON-RPC request, which may take up to limit. When the
// server no longer accepts a reused session, it opens a new one and
// retries once.
func (c *Client) call(ctx context.Context, limit time.Duration, method string, params any, out any) error {
	err := c.callOnce(ctx, limit, method, params, out)
	if errors.Is(err, api.ErrInvalidSession) && method != "initialize" {
		c.API.Debugf("mcp: opening a new session and sending %s again", method)
		c.sessionID = ""
		saveSession(c.API.Host, "")
		if err := c.ensureSession(ctx); err != nil {
			return err
		}
		return c.callOnce(ctx, limit, method, params, out)
	}
	return err
}

func (c *Client) callOnce(ctx context.Context, limit time.Duration, method string, params any, out any) error {
	c.nextID++
	id := c.nextID
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	headers := map[string]string{
		"Content-Type": "application/json",
		// Streamable HTTP clients must accept both. Without the event
		// stream the server answers a chat as one body at the end, and a
		// proxy on the way may give up on it long before that.
		"Accept":               "application/json, text/event-stream",
		"MCP-Protocol-Version": ProtocolVersion,
	}
	if c.sessionID != "" {
		headers["mcp-session-id"] = c.sessionID
	}
	// The debug log names the tool a call is for.
	label := method
	if p, ok := params.(map[string]any); ok && method == "tools/call" {
		if name, ok := p["name"].(string); ok {
			label += " " + name
		}
	}
	c.API.Debugf("mcp %s", label)
	callCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	resp, err := c.API.Send(callCtx, http.MethodPost, "/mcp", payload, headers, true)
	if err != nil {
		return c.explain(ctx, callCtx, limit, method, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("mcp-session-id"); sid != "" && sid != c.sessionID {
		c.sessionID = sid
		saveSession(c.API.Host, sid)
	}
	var msg *response
	if isEventStream(resp.Header.Get("Content-Type")) {
		msg, err = readEventStream(resp.Body, id)
	} else {
		msg = &response{}
		if err = json.NewDecoder(resp.Body).Decode(msg); err != nil {
			err = fmt.Errorf("unexpected MCP answer: %w", err)
		}
	}
	if err != nil {
		// The server answered, and then the answer went wrong: its logs
		// know the request by this id.
		return api.WithRequestID(c.explain(ctx, callCtx, limit, method, err), resp)
	}
	if msg.Error != nil {
		c.API.Debugf("mcp %s: error %d: %s", label, msg.Error.Code, msg.Error.Message)
		return &RPCError{Method: method, Code: msg.Error.Code, Message: msg.Error.Message, RequestID: api.RequestID(resp)}
	}
	if out != nil {
		if err := json.Unmarshal(msg.Result, out); err != nil {
			return api.WithRequestID(fmt.Errorf("unexpected MCP answer: %w", err), resp)
		}
	}
	return nil
}

// slowAnswer replaces the bare status of a proxy's error page: a proxy that
// gave up on a slow answer, for which the CLI used to print only "error:
// Gateway Timeout".
const slowAnswer = "the server took too long to answer"

// explain says what went wrong with a request made with callCtx, which is
// ctx with limit on it.
func (c *Client) explain(ctx, callCtx context.Context, limit time.Duration, method string, err error) error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// A message means the server said what is wrong, in JSON; an
			// HTML error page leaves it empty.
			if apiErr.Message != "" {
				return err
			}
			msg := slowAnswer + "; retry"
			if method == "tools/call" {
				msg = slowAnswer + "; try a narrower question or retry"
			}
			return &api.Error{Status: apiErr.Status, Message: msg, RequestID: apiErr.RequestID}
		}
		return err
	}
	// The deadline was ours, not a Ctrl-C: the server took longer than the
	// limit, which is worth saying as such (and --timeout can raise it).
	if ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		host := c.API.Host
		if u, perr := url.Parse(host); perr == nil && u.Host != "" {
			host = u.Host
		}
		return httpx.TooSlow(host, limit, err)
	}
	return err
}

func (c *Client) ensureSession(ctx context.Context) error {
	if c.sessionID != "" {
		return nil
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "firmfact-cli", "version": api.Version},
	}
	if err := c.call(ctx, c.limit(""), "initialize", params, nil); err != nil {
		return err
	}
	if c.sessionID == "" {
		return fmt.Errorf("the server did not open an MCP session")
	}
	return nil
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, c.limit(""), "tools/list", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool calls tool with args. A tool that only reads (see
// Tool.ReadOnly) is called once more when the call was lost on the way: a
// gateway's error (502, 504) or a connection that broke. Whether the first
// call ran is not known, which a tool that changes nothing makes harmless;
// any other tool could act twice, and fails instead.
func (c *Client) CallTool(ctx context.Context, tool Tool, args map[string]any) (*CallResult, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	params := map[string]any{"name": tool.Name, "arguments": args}
	var out CallResult
	err := c.call(ctx, c.limit(tool.Name), "tools/call", params, &out)
	if err != nil && tool.ReadOnly() && ctx.Err() == nil && lostOnTheWay(err) {
		c.API.Debugf("mcp: the call was lost on the way (%v); %s only reads, so calling it once more", err, tool.Name)
		out = CallResult{}
		err = c.call(ctx, c.limit(tool.Name), "tools/call", params, &out)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// lostOnTheWay reports whether err is a call that failed between the CLI
// and the server rather than in it: a proxy's 502 or 504, or a connection
// that broke.
func lostOnTheWay(err error) bool {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusBadGateway || apiErr.Status == http.StatusGatewayTimeout
	}
	return httpx.Broken(err)
}
