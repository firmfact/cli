package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/ui"
)

// `ask` puts a question to a workspace: the question is its argument and
// the answer prints as text. It calls the server's chat tool, which the
// generated `chat-with-workspace --message` command also runs; that stays
// for the scripts that use it. Each answer names its chat thread, and ask
// keeps the last one per host and workspace, so --continue follows up
// without the id being copied out of JSON.

// chatTool is the server tool ask calls.
const chatTool = "chat_with_workspace"

// callsAnnotation names the server tool a built-in command calls, as
// toolAnnotation does for a generated one; the tests that check the next
// steps and the README's examples run the command against that tool.
const callsAnnotation = "firmfact/calls"

// maxQuestionChars is how much of a question the server reads: it cuts a
// longer one at 10,000 characters (sanitize_args in tool_registry.rb),
// without saying so.
const maxQuestionChars = 10_000

// maxStdinQuestion is the most `ask -` reads. It is far more than the
// server reads, and only keeps a mistaken pipe (a large file, a device
// that never ends) from filling memory.
const maxStdinQuestion = 1 << 20

func newAskCommand(app *App) *cobra.Command {
	var follow, yes bool
	var thread string
	cmd := &cobra.Command{
		Use:   `ask "question"`,
		Short: "Ask a question about a workspace in plain English",
		Long: `Ask a question about a workspace in plain English and get the answer as
text, markdown and all. The question is one argument, in quotes; with - it is
read from standard input, so a longer question can come from a file or a
pipe.

The question and its answer are saved in the workspace as a chat thread,
which the next question can follow up in. --continue follows up in the thread
of your last ask on this host and workspace, which the CLI keeps in its cache
directory until logout; --thread follows up in the thread with that id. With
--json, the answer's thread is data.thread_id.

A chat can take a minute or more: the CLI waits up to five minutes for the
answer (see --timeout), and on a terminal counts the seconds meanwhile. ask
calls the workspace's chat_with_workspace tool, as chat-with-workspace does.`,
		Example: fmt.Sprintf(`  %[1]s ask "Which contracts renew in the next 90 days?"
  %[1]s ask --continue "Which of those are with LSEG?"
  %[1]s ask - < question.txt
  %[1]s ask --workspace Demo --json "What is our total spend?"`, app.Name),
		Args:              askArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		Annotations:       map[string]string{callsAnnotation: chatTool},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			host, err := app.Host()
			if err != nil {
				return err
			}
			tool, err := toolFor(ctx, app, chatTool)
			if err != nil {
				return err
			}
			workspace := app.toolWorkspace(tool)
			asked := thread
			if follow {
				if asked = loadThread(host, workspace); asked == "" {
					return withExit(ExitNotFound, fmt.Errorf("there is no earlier question %s to continue; ask one without --continue first", threadPlace(host, workspace)))
				}
			}
			question, err := app.question(ctx, args[0])
			if err != nil {
				return err
			}

			toolArgs := map[string]any{"message": question}
			if workspace != "" {
				toolArgs["workspace"] = workspace
			}
			if asked != "" {
				toolArgs["thread_id"] = asked
			}
			var opts renderOptions
			if err := (listFlags{}).apply(app, tool, toolArgs, false, &opts); err != nil {
				return err
			}
			opts.continueWith = func(threadID string) string { return askToContinue(cmd, cmd.CommandPath(), threadID) }
			opts.answered = func(out toolOutput) { keepThread(app, host, workspace, asked, out) }
			if ok, err := confirmTool(ctx, app, tool, yes, cmd.CommandPath(), "--yes", toolArgs); !ok {
				return err
			}
			return runTool(ctx, app, tool, toolArgs, opts)
		},
	}
	cmd.Flags().BoolVarP(&follow, "continue", "c", false, "follow up in the thread of your last ask on this host and workspace")
	cmd.Flags().StringVar(&thread, "thread", "", "follow up in the chat thread with this id")
	cmd.MarkFlagsMutuallyExclusive("continue", "thread")
	// The chat only adds to a workspace, so it is not asked about. Should
	// the server ever say otherwise, a script can still go ahead, as with
	// any workspace command (see addYesFlag).
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, yesFlagUsage)
	_ = cmd.Flags().MarkHidden("yes")
	return cmd
}

// askArgs takes the question as one argument. Unquoted, it arrives as
// several, and a shell may already have expanded a ? or * in it.
func askArgs(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return fmt.Errorf(`ask needs a question: %s "...", or %s - to read it from standard input`, cmd.CommandPath(), cmd.CommandPath())
	case len(args) > 1:
		return fmt.Errorf(`the question arrived as %d separate arguments; put it in quotes: %s "..."`, len(args), cmd.CommandPath())
	case strings.TrimSpace(args[0]) == "":
		return errors.New("the question is empty")
	}
	return nil
}

// askToContinue is the command line that follows up in thread with ask,
// where what is how ask is typed; it keeps the flags that chose the host
// and workspace, since the thread lives there.
func askToContinue(cmd *cobra.Command, what, thread string) string {
	return what + globalFlagArgs(cmd) + " --thread " + shellWord(thread, "<thread-id>") + ` "..."`
}

// question is the question ask sends: its argument, or with -, all of
// standard input. A question longer than the server reads is still sent,
// with a note that only its start will be read.
func (a *App) question(ctx context.Context, arg string) (string, error) {
	q := arg
	if arg == "-" {
		var err error
		if q, err = a.readQuestion(ctx); err != nil {
			return "", err
		}
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return "", usageErrorf("standard input held no question")
	}
	if n := utf8.RuneCountInString(q); n > maxQuestionChars {
		fmt.Fprintf(a.Err, "note: the question is %s characters long; the service reads the first %s.\n",
			groupThousands(strconv.Itoa(n)), groupThousands(strconv.Itoa(maxQuestionChars)))
	}
	return q, nil
}

// readQuestion reads standard input to its end, so a question can have
// several lines. On a terminal the user types it and then ends the input,
// which is said first: otherwise the command seems to hang.
func (a *App) readQuestion(ctx context.Context) (string, error) {
	if f, ok := a.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		end := "Ctrl-D"
		if runtime.GOOS == "windows" {
			end = "Ctrl-Z and Enter"
		}
		fmt.Fprintf(a.Err, "Type your question, then press %s on a line of its own.\n", end)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err := awaitRead(ctx, a.Err, func() (string, error) {
		raw, err := io.ReadAll(io.LimitReader(a.reader(), maxStdinQuestion+1))
		return string(raw), err
	}, nil)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", fmt.Errorf("could not read the question from standard input: %w", err)
	case len(text) > maxStdinQuestion:
		return "", usageErrorf("standard input holds more than 1 MiB; the service reads the first %s characters of a question", groupThousands(strconv.Itoa(maxQuestionChars)))
	}
	return text, nil
}

// keepThread keeps the thread an answer names for --continue. When it is
// not the thread asked for, that is said: the server starts a new thread,
// without an error, for an id it cannot find in the workspace, such as one
// that was deleted or belongs to another user.
func keepThread(app *App, host, workspace, asked string, out toolOutput) {
	_, id, ok := chatResponse(out.Data)
	if !ok || id == "" {
		return
	}
	if asked != "" && !strings.EqualFold(id, asked) {
		fmt.Fprintf(app.Err, "note: thread %s was not found %s, so this answer starts a new thread.\n", ui.SafeLine(asked), threadPlace(host, workspace))
	}
	if err := saveThread(host, workspace, id); err != nil {
		fmt.Fprintf(app.Err, "warning: could not keep the thread for --continue: %v\n", err)
	}
}

// threadPlace says where a thread lives, for messages: "on <host> in
// workspace Demo", or in the sign-in's own workspace when none was named.
func threadPlace(host, workspace string) string {
	if workspace == "" {
		return "on " + host + " in your default workspace"
	}
	return "on " + host + " in workspace " + nameInText(ui.SafeLine(workspace))
}

// savedThread is the chat thread of the last answer on a host and
// workspace. The workspace is as the command named it, by name or id, or
// empty for the sign-in's own; a name and an id of one workspace keep
// threads apart, which at worst starts a new thread.
type savedThread struct {
	Host      string    `json:"host"`
	Workspace string    `json:"workspace"`
	ThreadID  string    `json:"thread_id"`
	SavedAt   time.Time `json:"saved_at"`
}

// threadPath is the file for host and workspace. The name starts with the
// host's part (see threadFilePrefix), so logout finds every thread of a
// host without reading the files.
func threadPath(host, workspace string) (string, error) {
	dir, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, threadFilePrefix(host)+shortHash(workspace)+".json"), nil
}

func threadFilePrefix(host string) string { return "chat-thread-" + shortHash(host) + "-" }

// shortHash names a cache file after s without putting s, a host or a
// workspace name, in the name.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// loadThread is the thread kept for host and workspace, or empty.
func loadThread(host, workspace string) string {
	path, err := threadPath(host, workspace)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var t savedThread
	if json.Unmarshal(raw, &t) != nil || t.Host != host || t.Workspace != workspace {
		return ""
	}
	return t.ThreadID
}

func saveThread(host, workspace, id string) error {
	path, err := threadPath(host, workspace)
	if err != nil {
		return err
	}
	return config.WriteJSON(path, savedThread{Host: host, Workspace: workspace, ThreadID: id, SavedAt: time.Now().UTC()}, 0o600)
}

// forgetThreads drops the threads kept for host, on logout: they belong to
// the sign-in that ended, and another user's would only start new ones.
func forgetThreads(host string) error {
	dir, err := config.CacheDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	prefix := threadFilePrefix(host)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
