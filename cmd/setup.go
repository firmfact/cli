package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
)

// SetupState is how far the server has got preparing a workspace: what
// /api/v1/cli/workspaces/:id/setup_progress says, the payload of the web
// app's progress overlay.
type SetupState struct {
	// State is the server's verdict: running, complete, failed or stalled
	// (nothing is running and the setup did not finish).
	State      string  `json:"state"`
	Percentage float64 `json:"percentage"`
	// Activity is what the setup is doing now, as the overlay headlines it.
	Activity string `json:"activity,omitempty"`
	Message  string `json:"message,omitempty"`
}

const (
	setupRunning  = "running"
	setupComplete = "complete"
	setupFailed   = "failed"
	setupStalled  = "stalled"
)

func fetchSetup(ctx context.Context, c *api.Client, workspaceID string) (*SetupState, error) {
	var p struct {
		Percentage      float64 `json:"percentage"`
		Message         string  `json:"message"`
		CurrentActivity string  `json:"current_activity"`
		ProgressDetails string  `json:"progress_details"`
		Liveness        string  `json:"liveness"`
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/v1/cli/workspaces/"+url.PathEscape(workspaceID)+"/setup_progress", nil, &p, true); err != nil {
		return nil, err
	}
	// current_activity is what the web overlay headlines; message stays
	// "Creating account Demo 2/2" for most of the run.
	activity := p.CurrentActivity
	if activity == "" {
		activity = p.ProgressDetails
	}
	if activity == "" {
		activity = p.Message
	}
	// The verdict and the progress figures come from different places on
	// the server, and a finished setup can still carry the last figures
	// cached before it finished ("99%, Processing demo documents"). The
	// verdict is what counts.
	if p.Liveness == setupComplete {
		p.Percentage, activity = 100, ""
	}
	return &SetupState{State: p.Liveness, Percentage: p.Percentage, Activity: activity, Message: p.Message}, nil
}

// label is the state in a few words, for the SETUP column of `workspaces
// list`; nil is a state that could not be read.
func (s *SetupState) label() string {
	if s == nil || s.State == "" {
		return "unknown"
	}
	switch s.State {
	case setupComplete:
		return "ready"
	case setupRunning:
		return fmt.Sprintf("preparing, %.0f%%", s.Percentage)
	}
	return ui.SafeLine(s.State)
}

// A Demo workspace takes a few minutes to prepare. The first check comes
// setupPollInterval (2 s) after the one before, and the pauses grow to 2.5
// times that (5 s): at one check every 2 s, a 20-minute wait would use up
// the CLI's rate limit on the server (60 requests at once, 1000 an hour).
// A check that failed in a way that may pass (see transientError) is
// retried, pausing twice as long each time up to 15 times the interval
// (30 s), until the wait runs out. Everything scales with the one variable,
// which tests set to a millisecond.
var setupPollInterval = 2 * time.Second

// nextPollGap is the pause after gap: a quarter longer, up to 2.5 times
// setupPollInterval.
func nextPollGap(gap time.Duration) time.Duration {
	return min(gap+gap/4, setupPollInterval*5/2)
}

// nextRetryGap is the pause before the next retry of a failed check: the
// usual pause at first, then twice the one before, up to 15 times
// setupPollInterval.
func nextRetryGap(retry, gap time.Duration) time.Duration {
	return min(max(retry*2, gap), setupPollInterval*15)
}

// transientError reports whether err may pass by itself, so a long wait
// should try again rather than give up: no answer from the server (unless
// its certificate did not verify, which a retry will not change), a server
// error, such as the 503 the progress endpoint answers when it cannot read
// the state, or a rate limit the client did not wait out.
func transientError(err error) bool {
	var netErr *httpx.Error
	if errors.As(err, &netErr) {
		return netErr.Kind != httpx.Untrusted
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || (apiErr.Status >= 500 && apiErr.Status != http.StatusNotImplemented)
	}
	return false
}

// setupWait is one wait for a workspace's setup to finish.
type setupWait struct {
	// workspace is the one being set up; its ID and Name are used.
	workspace Workspace
	timeout   time.Duration
	// out takes the progress: stdout, or stderr when stdout is for --json.
	out io.Writer
	// signup is the wait that ends signup, for the Demo workspace the user
	// has just been signed in to.
	signup bool
}

// subject names the workspace in messages.
func (w setupWait) subject() string {
	switch {
	case w.signup:
		return "your Demo workspace"
	case w.workspace.Name != "":
		return ui.SafeLine(w.workspace.Name)
	}
	return ui.SafeLine(w.workspace.ID)
}

// statusCommand is the command that checks on this workspace later. The ID
// is left out when the workspace is the one `workspaces status` picks
// anyway: the one commands use, or the token's default when none is set.
func (w setupWait) statusCommand(app *App) string {
	command := app.Name + " workspaces status"
	current := app.DefaultWorkspace()
	if w.workspace.ID != current && (current != "" || !w.workspace.Default) {
		command += " " + argWord(ui.SafeLine(w.workspace.ID), "<workspace-id>")
	}
	return command
}

// waitForSetup follows the setup of a workspace until it is complete, has
// failed or stalled, or the wait runs out, and returns the last state it
// read (nil when none was).
func waitForSetup(ctx context.Context, app *App, c *api.Client, w setupWait) (*SetupState, error) {
	mode := app.Mode()
	out := w.out
	// A person watches one line redrawn in place, coloured or not; scripts,
	// logs and --json get a line per change, as does a terminal that cannot
	// redraw one (TERM=dumb).
	live := app.attended() && ui.Escapes(out)
	if w.signup {
		fmt.Fprintln(out, "Preparing your Demo workspace (this takes a few minutes)...")
	} else {
		fmt.Fprintf(out, "Waiting for %s to be ready (setup takes a few minutes)...\n", w.subject())
	}
	deadline := time.Now().Add(w.timeout)
	shown := ""
	show := func(percentage float64, activity string) {
		// The line is redrawn in place, so it must stay one line and carry
		// no escape sequences of the server's own.
		activity = ui.SafeLine(activity)
		if live {
			fmt.Fprint(out, "\r\x1b[K"+progressLine(mode, ui.Width(out), percentage, activity))
		} else if line := fmt.Sprintf("  %3.0f%%  %s", percentage, activity); line != shown {
			fmt.Fprintln(out, line)
			shown = line
		}
	}
	endLine := func() {
		if live {
			fmt.Fprintln(out)
		}
	}
	// Ctrl-C stops the wait, not the setup: that runs on the server, and
	// after signup the user is already signed in.
	interrupted := func() error {
		ui.EndInterruptedLine(out)
		if w.signup {
			fmt.Fprintf(out, "You are signed in; setup continues on the server. Check with `%s`.\n", w.statusCommand(app))
		} else {
			fmt.Fprintf(out, "Setup continues on the server. Check again with `%s`.\n", w.statusCommand(app))
		}
		return ctx.Err()
	}

	var (
		last    *SetupState
		lastErr error
		gap     = setupPollInterval
		retry   time.Duration
	)
	for {
		st, err := fetchSetup(ctx, c, w.workspace.ID)
		if ctx.Err() != nil {
			return last, interrupted()
		}
		pause := gap
		switch {
		case err != nil && !transientError(err):
			endLine()
			return last, err
		case err != nil:
			// The server's trouble, or the network's, need not end a wait
			// of minutes; the setup goes on meanwhile.
			lastErr, retry = err, nextRetryGap(retry, gap)
			pause = retry
			percentage := 0.0
			if last != nil {
				percentage = last.Percentage
			}
			if live {
				show(percentage, "Could not check progress; trying again")
			} else {
				show(percentage, "Could not check progress ("+err.Error()+"); trying again")
			}
		default:
			last, lastErr, retry = st, nil, 0
			switch st.State {
			case setupComplete:
				if live {
					fmt.Fprint(out, "\r\x1b[K"+progressLine(mode, ui.Width(out), 100, "Ready")+"\n")
				} else {
					fmt.Fprintln(out, "  100%  Ready.")
				}
				if w.signup {
					fmt.Fprintf(out, "%s Open %s in your browser, or stay here.\n", mode.Rainbow("Your Demo workspace is ready."), c.Host)
				} else {
					fmt.Fprintln(out, mode.Rainbow(w.subject()+" is ready."))
				}
				return st, nil
			case setupFailed, setupStalled:
				endLine()
				return st, setupDidNotFinish(w.subject(), st, c.Host)
			}
			show(st.Percentage, st.Activity)
			gap = nextPollGap(gap)
		}
		if time.Now().After(deadline) {
			endLine()
			if lastErr != nil {
				return last, withExit(ExitUnavailable, fmt.Errorf("gave up waiting for %s after %s; the last check failed: %w. Setup continues on the server; check with `%s --wait`",
					w.subject(), w.timeout, lastErr, w.statusCommand(app)))
			}
			return last, withExit(ExitUnavailable, fmt.Errorf("%s is still being set up after %s; setup continues on the server. Check with `%s --wait`",
				w.subject(), w.timeout, w.statusCommand(app)))
		}
		// The last check comes when the wait runs out, not a pause later.
		pause = min(pause, time.Until(deadline))
		if !ui.Pause(ctx, pause) {
			return last, interrupted()
		}
	}
}

// progressLine is the live line of a setup for a terminal width columns
// wide: the bar, the percentage and as much of the activity as fits. It
// stays within one row, less the last column, because a line that wraps
// cannot be redrawn in place: every update would leave a row behind. On a
// narrow terminal the bar gives up room first, then the activity.
func progressLine(mode ui.ColorMode, width int, percentage float64, activity string) string {
	// The bar is 30 columns wide where the terminal leaves 40 beside it,
	// for the margins, the percentage and some of the activity; on a
	// narrower one it shrinks, to 10 at the least.
	const bar, minBar = 30, 10
	head := fmt.Sprintf("  %s %3.0f%%", mode.Bar(percentage/100, min(bar, max(minBar, width-40))), percentage)
	room := width - 1 - ui.Columns(head) - 2
	if room < 1 || activity == "" {
		return head
	}
	return head + "  " + cutCell(activity, room)
}

// setupDidNotFinish is the error for a setup that failed or stalled.
func setupDidNotFinish(subject string, st *SetupState, host string) error {
	return fmt.Errorf("the setup of %s did not finish (%s): open %s to retry it", subject, ui.SafeLine(st.Message), host)
}

// findWorkspace is the workspace ref names, by id or by name as `workspaces
// use` matches it; an empty ref is the token's default workspace.
func findWorkspace(me *Me, ref string) (Workspace, bool) {
	for _, w := range me.Workspaces {
		if (ref == "" && w.Default) || (ref != "" && (w.ID == ref || equalFold(w.Name, ref))) {
			return w, true
		}
	}
	return Workspace{}, false
}

func newWorkspaceStatusCommand(app *App) *cobra.Command {
	var (
		wait        bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status [id or name]",
		Short: "Show how far a workspace's setup is, or wait for it to finish",
		Long: `Show how far the server has got preparing a workspace. Signup prepares the
Demo workspace, which takes a few minutes; this picks up where signup's own
wait left off, after --no-wait, Ctrl-C or --wait-timeout.

The workspace is the one named, else the one commands use (--workspace,
FIRMFACT_WORKSPACE or the profile's), else the sign-in's default. With
--wait, the command follows the setup until it is ready, and fails if the
setup fails or stalls, or if it is still running when --wait-timeout runs out
(exit status 5).`,
		Example: fmt.Sprintf(`  %[1]s workspaces status
  %[1]s workspaces status Demo --wait
  %[1]s workspaces status --wait --json`, app.Name),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeWorkspaceArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := app.Client()
			if err != nil {
				return err
			}
			me, err := fetchMe(ctx, c)
			if err != nil {
				return err
			}
			ref := app.DefaultWorkspace()
			if len(args) == 1 {
				ref = args[0]
			}
			w, ok := findWorkspace(me, ref)
			if !ok {
				if ref == "" {
					return withExit(ExitNotFound, fmt.Errorf("this sign-in has no default workspace; name one, see `%s workspaces list`", app.Name))
				}
				return withExit(ExitNotFound, fmt.Errorf("no workspace %q; see `%s workspaces list`", ref, app.Name))
			}

			if !wait {
				st, err := fetchSetup(ctx, c, w.ID)
				if err != nil {
					return err
				}
				w.Setup = st
				if app.JSONOutput {
					return app.PrintJSON(w)
				}
				printSetupState(app, c, w)
				return nil
			}

			// With --json, stdout carries the result alone; the progress
			// goes to stderr.
			out := app.Out
			if app.JSONOutput {
				out = app.Err
			}
			st, err := waitForSetup(ctx, app, c, setupWait{workspace: w, timeout: waitTimeout, out: out})
			// A wait that ended on the setup's account still says where it
			// ended; an interrupted one says nothing more.
			if app.JSONOutput && st != nil && ctx.Err() == nil {
				w.Setup = st
				if jsonErr := app.PrintJSON(w); err == nil {
					err = jsonErr
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the setup is done")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 20*time.Minute, "how long --wait waits")
	return cmd
}

// printSetupState says where the setup of w stands, for a person.
func printSetupState(app *App, c *api.Client, w Workspace) {
	name, st := setupWait{workspace: w}.subject(), w.Setup
	switch st.State {
	case setupComplete:
		fmt.Fprintf(app.Out, "%s is ready.\n", name)
	case setupRunning:
		line := fmt.Sprintf("%s is being set up (%.0f%%)", name, st.Percentage)
		if st.Activity != "" {
			line += ": " + ui.SafeLine(st.Activity)
		}
		fmt.Fprintln(app.Out, line)
		command := setupWait{workspace: w}.statusCommand(app)
		fmt.Fprintf(app.Out, "Follow it with `%s --wait`.\n", command)
	case setupFailed, setupStalled:
		fmt.Fprintf(app.Out, "The setup of %s did not finish (%s): open %s to retry it.\n", name, ui.SafeLine(st.Message), c.Host)
	default:
		fmt.Fprintf(app.Out, "The setup state of %s is unknown.\n", name)
	}
}

// setupReads is how many setup states `workspaces list` reads at once. One
// after another, a user with many workspaces waited a round trip for each;
// a few at a time keeps the wait short without a burst of requests.
const setupReads = 4

// addSetupStates reads the setup state of each workspace for `workspaces
// list`, one request each. A workspace the server does not find keeps no
// state; any other failure (an older server without the endpoint, a rate
// limit, no answer) would befall the rest too, so the reading stops there
// with a note, and the list still prints. The first workspace is read
// alone, so a server that fails them all is asked once, and the others
// setupReads at a time.
func addSetupStates(ctx context.Context, app *App, c *api.Client, workspaces []Workspace) error {
	// read fills in workspace i, and returns the failure that stops the
	// reading.
	read := func(ctx context.Context, i int) error {
		st, err := fetchSetup(ctx, c, workspaces[i].ID)
		if err != nil && ctx.Err() == nil && exitCode(err) == ExitNotFound {
			return nil
		}
		workspaces[i].Setup = st
		return err
	}
	if len(workspaces) == 0 {
		return nil
	}
	failed := read(ctx, 0)
	if failed == nil && len(workspaces) > 1 {
		failed = readAtOnce(ctx, len(workspaces)-1, func(ctx context.Context, i int) error { return read(ctx, i+1) })
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed != nil {
		fmt.Fprintf(app.Err, "note: could not read the setup states: %s\n", failed)
	}
	return nil
}

// readAtOnce runs read for 0 to n-1, setupReads at a time, and returns the
// first failure, which cancels the reads still going and starts no more.
func readAtOnce(ctx context.Context, n int, read func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed error
	)
	slots := make(chan struct{}, setupReads)
	for i := range n {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := read(ctx, i); err != nil && ctx.Err() == nil {
				mu.Lock()
				if failed == nil {
					failed = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return failed
}
