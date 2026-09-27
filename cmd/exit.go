package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// Exit statuses. Scripts and CI branch on them, so they are part of the
// CLI's interface: README.md lists them, and a number keeps its meaning
// once given.
const (
	// ExitFailed is any failure that none of the others describes.
	ExitFailed = 1
	// ExitUsage is a command line that cannot run as typed: an unknown
	// command or flag, a missing argument or required flag.
	ExitUsage = 2
	// ExitSignedOut is a command that needs a sign-in without one: never
	// signed in, or the session has ended or was revoked.
	ExitSignedOut = 3
	// ExitNotFound is a workspace, tool or record that does not exist.
	ExitNotFound = 4
	// ExitUnavailable is a request that may work later: rate-limited, the
	// server failing, or no answer at all.
	ExitUnavailable = 5
	// ExitUnsupported is a host that cannot serve this CLI: it is too old,
	// not firmfact, or needs a newer version of the CLI.
	ExitUnsupported = 6
	// 7 and 8 are kept for a request the sign-in is not allowed to make
	// and for a conflict with a change made meanwhile, which are 1 until
	// the CLI tells them apart.

	// ExitVariance is not a failure of the command but a finding, for
	// pipelines: an upload, or upload status, with --fail-on-variance
	// whose documents were all read, and an invoice's variance preview is
	// over the threshold. Anything else that went wrong has its own
	// status, which wins, as that run has not checked everything.
	ExitVariance = 9
	// ExitInterrupted is Ctrl-C or SIGTERM: 128 + SIGINT, what a shell
	// reports for a command that Ctrl-C ended.
	ExitInterrupted = 130
)

// exitStatus names each exit status in the --json error, so a script can
// branch on a word instead of a number.
var exitStatus = map[int]string{
	ExitFailed:      "failed",
	ExitUsage:       "usage",
	ExitSignedOut:   "not_signed_in",
	ExitNotFound:    "not_found",
	ExitUnavailable: "unavailable",
	ExitUnsupported: "unsupported",
	ExitVariance:    "variance_exceeded",
	ExitInterrupted: "interrupted",
}

// ExitError is an error that says which exit status it ends the process
// with. Most errors are classified by their type (see Classify); a command
// wraps one in ExitError when only it knows, such as a workspace name that
// matched nothing.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ErrInterrupted is what main reports for Ctrl-C or SIGTERM, whatever the
// command returned: it is the interrupt's doing.
var ErrInterrupted error = &ExitError{Code: ExitInterrupted, Err: errors.New("interrupted")}

// withExit gives err the exit status code; nil stays nil.
func withExit(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Err: err}
}

// usageErrorf is an error in how the command was typed (ExitUsage).
func usageErrorf(format string, a ...any) error {
	return withExit(ExitUsage, fmt.Errorf(format, a...))
}

// Classify returns the exit status for err and its name for --json.
func Classify(err error) (code int, status string) {
	code = exitCode(err)
	return code, exitStatus[code]
}

func exitCode(err error) int {
	var (
		exit   *ExitError
		ended  *api.SessionEndedError
		apiErr *api.Error
		rpcErr *mcp.RPCError
		netErr *httpx.Error
	)
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.Code
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	case errors.Is(err, api.ErrNotSignedIn), errors.As(err, &ended):
		return ExitSignedOut
	case errors.As(err, &apiErr):
		return httpExitCode(apiErr)
	case errors.As(err, &rpcErr):
		return rpcExitCode(rpcErr)
	case errors.As(err, &netErr), errors.Is(err, config.ErrKeyringTimeout):
		return ExitUnavailable
	}
	return ExitFailed
}

// httpExitCode classifies an answer from the server by its status.
func httpExitCode(e *api.Error) int {
	switch s := e.Status; {
	case e.Unsupported:
		// A CLI endpoint the host does not have, whatever page it sent.
		return ExitUnsupported
	case s == http.StatusUnauthorized:
		return ExitSignedOut
	case s == http.StatusNotFound && (e.Message != "" || e.Code != ""):
		// The API said what it did not find, in JSON.
		return ExitNotFound
	case s == http.StatusNotFound, s == http.StatusUpgradeRequired, s == http.StatusNotImplemented:
		// A bare 404 (an HTML error page) is an endpoint the host does not
		// have: an older firmfact, or something else at that address.
		return ExitUnsupported
	case s == http.StatusTooManyRequests, s >= 500:
		return ExitUnavailable
	}
	return ExitFailed
}

// rpcExitCode classifies an MCP server's JSON-RPC error.
func rpcExitCode(e *mcp.RPCError) int {
	switch e.Code {
	case mcp.ErrMethodNotFound:
		// For tools/call, the server's word for a tool it does not have;
		// for anything else, a server without that part of MCP.
		if e.Method == "tools/call" {
			return ExitNotFound
		}
		return ExitUnsupported
	case mcp.ErrInvalidParams:
		return ExitUsage
	}
	return ExitFailed
}

// ReportError tells the user about err on w and returns the exit status.
// With --json it writes {"error":{"message","code","status"}} on one line,
// so a script reads the reason without parsing prose; otherwise one
// "error:" line. An interrupt gets no line of its own: the user knows about
// that.
func ReportError(w io.Writer, err error, asJSON bool) int {
	code, status := Classify(err)
	// Errors often carry the server's own text (a tool error, a login
	// failure description), so terminal controls in it are escaped, in
	// JSON too: a script may print the message.
	msg := ui.SafeText(err.Error())
	switch {
	case asJSON:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		var envelope struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		envelope.Error.Message, envelope.Error.Code, envelope.Error.Status = msg, code, status
		if enc.Encode(envelope) == nil {
			_, _ = w.Write(ui.SafeJSON(buf.Bytes()))
		}
	case code != ExitInterrupted:
		fmt.Fprintln(w, "error:", msg)
	}
	return code
}

// JSONRequested reports whether the invocation asked for --json, --format
// json or --jq. The parsed flags decide when parsing got that far; a
// command line that failed to parse may not have reached them, so then the
// arguments are read again, stepping over the values of flags that take
// one, as the parser does: in `--profile --json`, --json is a profile's
// name.
func JSONRequested(root *cobra.Command, args []string, err error) bool {
	if on, _ := root.PersistentFlags().GetBool("json"); on {
		return true
	}
	format := root.PersistentFlags().Lookup("format")
	if format != nil && format.Value.String() == string(formatJSON) {
		return true
	}
	// --jq asks for JSON, errors included.
	if jq := root.PersistentFlags().Lookup("jq"); jq != nil && jq.Value.String() != "" {
		return true
	}
	if exitCode(err) != ExitUsage {
		return false
	}
	target, _, findErr := root.Find(args)
	if findErr != nil {
		target = root
	}
	lookup := func(name string) *pflag.Flag {
		if f := target.Flags().Lookup(name); f != nil {
			return f
		}
		return root.PersistentFlags().Lookup(name)
	}
	takesValue := func(name string) bool {
		f := lookup(name)
		return f != nil && f.NoOptDefVal == ""
	}
	// A tool with an argument named format has a --format of its own.
	ours := lookup("format") == format
	isJSON := func(v string) bool { return ours && strings.EqualFold(strings.TrimSpace(v), string(formatJSON)) }
	on := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return on // the rest are arguments, not flags
		case arg == "--json":
			on = true
		case strings.HasPrefix(arg, "--json="):
			on, _ = strconv.ParseBool(strings.TrimPrefix(arg, "--json="))
		case arg == "--format" && i+1 < len(args):
			i++
			on = on || isJSON(args[i])
		case arg == "--jq" && i+1 < len(args) && lookup("jq") == root.PersistentFlags().Lookup("jq"):
			i++
			on = true
		case strings.HasPrefix(arg, "--jq=") && lookup("jq") == root.PersistentFlags().Lookup("jq"):
			on = true
		case strings.HasPrefix(arg, "--format="):
			on = on || isJSON(strings.TrimPrefix(arg, "--format="))
		case strings.HasPrefix(arg, "--") && !strings.Contains(arg, "=") && takesValue(arg[2:]):
			i++ // its value
		}
	}
	return on
}
