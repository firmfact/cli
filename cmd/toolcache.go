package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// The cached tool list goes out of date when the server gains, renames or
// drops a tool. Two things bring it up to date without a `tools refresh`:
// on a terminal, a workspace command fetches it alongside its own call
// once it is a day old; and a call the server answers with "no such tool"
// or "not those arguments" fetches it and, when the tool is still there,
// tries once more. Scripts get no refresh they did not ask for, only the
// one after such an answer: a list that changes under a script mid-run
// would be harder to follow than one it refreshes itself.

// toolCacheTTL is how old the cached tool list may get before a command on
// a terminal fetches it again.
const toolCacheTTL = 24 * time.Hour

// refreshGrace is how long a command waits, once its own work is done, for
// a refresh in the background to finish. The refresh is one small request
// that usually beats the command's own; one that takes longer is left for
// the next command rather than holding up the prompt.
const refreshGrace = 2 * time.Second

// toolCacheStale reports whether host's tool list should be fetched again:
// none is cached, or it was fetched more than toolCacheTTL ago. A time in
// the future is a clock that has since been put right.
func toolCacheStale(host string, now time.Time) bool {
	tc := loadToolCache(host)
	if tc == nil {
		return true
	}
	age := now.Sub(tc.FetchedAt)
	return age > toolCacheTTL || age < 0
}

// toolRefresh is a fetch of the tool list running beside a command.
type toolRefresh struct {
	done  chan struct{}
	tools []mcp.Tool
	err   error
}

// refreshStaleTools starts fetching the tool list in the background when
// this is a terminal and the cached list is stale; otherwise it returns
// nil, which the methods of toolRefresh take as no refresh.
func refreshStaleTools(ctx context.Context, app *App) *toolRefresh {
	if !app.Interactive() {
		return nil
	}
	host, err := app.Host()
	if err != nil || !toolCacheStale(host, time.Now()) {
		return nil
	}
	return startToolRefresh(ctx, app)
}

// startToolRefresh fetches the tool list on a goroutine, with a client of
// its own: the command's is not safe to share while it calls the tool. The
// client is made here, as reading the profile can write to the config.
func startToolRefresh(ctx context.Context, app *App) *toolRefresh {
	r := &toolRefresh{done: make(chan struct{})}
	c, err := app.Client()
	if err != nil {
		r.err = err
		close(r.done)
		return r
	}
	c.Busy = nil // a wait for a busy server is the command's to mention, not this
	m := app.MCP(c)
	go func() {
		defer close(r.done)
		r.tools, r.err = fetchTools(ctx, m, c.Host)
	}()
	return r
}

// result waits for the refresh, or for ctx to end.
func (r *toolRefresh) result(ctx context.Context) ([]mcp.Tool, error) {
	select {
	case <-r.done:
		return r.tools, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// settle gives the refresh up to refreshGrace to finish once the command is
// done. A refresh that is left behind ends with the process; the cache is
// replaced whole or not at all (see saveToolCache).
func (r *toolRefresh) settle(ctx context.Context) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, refreshGrace)
	defer cancel()
	_, _ = r.result(ctx)
}

// callTool calls tool. When the server says it has no such tool, or not
// with those arguments, the cached list is out of date: it is fetched once,
// the background refresh standing in when there is one, and a tool that is
// still there is called once more as the fresh list describes it. Neither
// answer means the tool ran, so the second call cannot make it act twice.
// A tool the fresh list says may delete or overwrite data, where the old
// one did not, is not called again: the command did not ask first.
func callTool(ctx context.Context, app *App, c *api.Client, tool mcp.Tool, args map[string]any, bg *toolRefresh) (*mcp.CallResult, error) {
	m := app.MCP(c)
	res, err := m.CallTool(ctx, tool, args)
	if !unknownTool(tool.Name, res, err) || ctx.Err() != nil {
		return res, err
	}
	var tools []mcp.Tool
	var ferr error
	if bg != nil {
		tools, ferr = bg.result(ctx)
	} else {
		tools, ferr = fetchTools(ctx, m, c.Host)
	}
	if ferr != nil {
		// The server's own answer says more than a failed refresh.
		return res, err
	}
	fresh, ok := findTool(tools, tool.Name)
	if !ok {
		return nil, noSuchTool(app, tool.Name)
	}
	// The command asked first, or not, as the old list described the tool.
	if fresh.Destructive() && !tool.Destructive() {
		return nil, fmt.Errorf("%s did not run: the server now says it may delete or overwrite data; run the command again to be asked first", ui.SafeLine(tool.Name))
	}
	return m.CallTool(ctx, fresh, args)
}

// unknownTool reports whether the server answered a call of tool with
// "no such tool" or "not those arguments": JSON-RPC's method not found or
// invalid params, or firmfact's tool error "Unknown tool: <name>", which it
// sends when the tool is missing from the workspace it ran in. Other tool
// errors can mention an unknown tool too, such as a chat's, so only this
// tool's name counts.
func unknownTool(name string, res *mcp.CallResult, err error) bool {
	var rpcErr *mcp.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Method == "tools/call" && (rpcErr.Code == mcp.ErrMethodNotFound || rpcErr.Code == mcp.ErrInvalidParams)
	}
	if err != nil || res == nil || !res.IsError {
		return false
	}
	text := res.Text()
	i := strings.Index(text, "Unknown tool: "+name)
	if i < 0 {
		return false
	}
	// The name must end there: list_vendors is not list_vendors_v2.
	rest := text[i+len("Unknown tool: "+name):]
	return rest == "" || !isNameChar(rest[0])
}

func isNameChar(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
