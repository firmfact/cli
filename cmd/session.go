package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/auth"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// loginTimeout is how long login waits for the browser (a variable so tests
// can give up sooner).
var loginTimeout = auth.DefaultTimeout

func newLoginCommand(app *App) *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through your browser",
		Long: "Sign in through your browser. The consent page lets you choose the default\n" +
			"workspace and whether the CLI may reach your other workspaces.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := app.profileToChange(cmd)
			if err != nil {
				return err
			}
			c, err := app.Client()
			if err != nil {
				return err
			}
			below := app.Banner()
			// With --json, stdout carries the result alone; the link and
			// the wait go to stderr, where the person signing in sees them.
			progress := app.Out
			if app.JSONOutput {
				progress = app.Err
			}
			// Count the rows login prints, so the logo can be found again.
			rows := &ui.RowCounter{W: progress, Width: ui.Width(progress)}
			tr, err := auth.Login(cmd.Context(), c, auth.Options{OpenBrowser: !noBrowser, Out: rows, Timeout: loginTimeout})
			if err != nil {
				return err
			}
			if err := c.SetToken(tr.Token()); err != nil {
				return err
			}
			keep := app.keepsSignIn(p, c.Host)
			if keep {
				rememberHost(p, c.Host)
				app.SaveConfig()
			}
			me, err := fetchMe(cmd.Context(), c)
			if err != nil {
				return err
			}
			// The default workspace commands use here: the profile's, or,
			// when the profile keeps another host, the sign-in's own.
			workspace := defaultWorkspaceOf(me)
			if keep {
				rememberDefaultWorkspace(app, p, me)
				workspace = p.Workspace
			} else {
				app.noteSignInNotKept(p, c.Host)
			}
			app.Celebrate(cmd.Context(), below+rows.Rows())
			if !app.JSONOutput {
				fmt.Fprintf(app.Out, "You're in: %s on %s.\n", ui.SafeLine(me.User.Email), c.Host)
			}
			loaded, ok := refreshToolsQuietly(cmd.Context(), app, c, !app.JSONOutput)
			if app.JSONOutput {
				result := loginResult{Host: c.Host, User: me.User, Workspace: workspace, Workspaces: me.Workspaces}
				if ok {
					result.Commands = &loaded
				}
				return app.PrintJSON(result)
			}
			app.printNextStep(stepVendors)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in link instead of opening a browser")
	return cmd
}

// loginResult is what login prints with --json.
type loginResult struct {
	Host       string      `json:"host"`
	User       MeUser      `json:"user"`
	Workspace  string      `json:"workspace"` // the default workspace commands use on Host now
	Workspaces []Workspace `json:"workspaces"`
	// Commands is how many workspace commands were loaded; null when the
	// list could not be fetched, which `tools refresh` can retry.
	Commands *int `json:"commands"`
}

// logoutResult is what logout prints with --json.
type logoutResult struct {
	Host string `json:"host"`
	// WasSignedIn is false when there was no sign-in to end.
	WasSignedIn bool `json:"was_signed_in"`
}

func newLogoutCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out: end the session on the server and forget it here",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A token from the environment was never stored, so there is
			// nothing to forget, and whoever handed it over decides when it
			// ends. Going on would sign out of the stored session instead,
			// which is not what the user is looking at.
			if config.TokenFromEnv() {
				return errors.New("FIRMFACT_TOKEN is set; a token from the environment is not stored, so there is nothing to sign out of. Unset FIRMFACT_TOKEN to sign out of the sign-in stored on this machine")
			}
			c, err := app.Client()
			if err != nil {
				return err
			}
			// A sign-in that cannot be read, from a keyring that did not
			// answer say, cannot be revoked, and is not known to be gone:
			// it is no "not signed in".
			t, err := c.Token()
			if err != nil {
				return err
			}
			var revokeErr error
			if t == nil {
				mcp.ForgetSession(c.Host)
				_ = forgetWorkspaces(c.Host)
				_ = forgetThreads(c.Host)
				if app.JSONOutput {
					return app.PrintJSON(logoutResult{Host: c.Host})
				}
				fmt.Fprintf(app.Out, "Not signed in to %s.\n", c.Host)
				return nil
			}
			revokeErr = revokeSession(cmd.Context(), c, t)
			// Ctrl-C stops here, with the local copy kept: whether the session
			// ended on the server is not known, and a second logout can make
			// sure.
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			// Otherwise the local copy goes either way: the user asked for it
			// gone, and a copy that cannot be revoked is no safer for being
			// kept.
			if err := config.DeleteToken(c.Host); err != nil {
				return err
			}
			mcp.ForgetSession(c.Host)
			// Best effort, like the session: a name left behind only
			// completes, and the next sign-in replaces the list. A chat
			// thread left behind only starts a new thread when continued.
			_ = forgetWorkspaces(c.Host)
			_ = forgetThreads(c.Host)
			if revokeErr != nil {
				fmt.Fprintln(app.Err, "warning: removed from this machine; the session is still valid on the server.")
				// The exit status is the cause's: a server out of reach
				// is worth a retry, a refusal is not.
				return withExit(exitCode(revokeErr), fmt.Errorf("could not revoke the session on %s: %v. A workspace admin can end it in the web app, "+
					"under Connected AI clients on the MCP page", c.Host, revokeErr))
			}
			if app.JSONOutput {
				return app.PrintJSON(logoutResult{Host: c.Host, WasSignedIn: true})
			}
			fmt.Fprintf(app.Out, "Signed out of %s.\n", c.Host)
			return nil
		},
	}
}

// revokeSession ends the sign-in on the server. The refresh token goes
// first: it is what keeps the session alive, and revoking it revokes the
// access token issued with it. The server will not revoke an access token
// that has already expired (they last an hour), so revoking only that, as
// logout once did, usually left the session alive. A live access token is
// then revoked too, for a server that keeps the two apart.
func revokeSession(ctx context.Context, c *api.Client, t *config.Token) error {
	if t.RefreshToken != "" {
		if err := c.Revoke(ctx, t.RefreshToken, api.HintRefreshToken); err != nil {
			return err
		}
	}
	if t.AccessToken != "" && !t.Expired() {
		return c.Revoke(ctx, t.AccessToken, api.HintAccessToken)
	}
	return nil
}

// Workspace is one row of /api/v1/cli/me.
type Workspace struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Default      bool   `json:"default"`
	Demo         bool   `json:"demo"`
	MCPAvailable bool   `json:"mcp_available"`
	// Setup is read separately, by `workspaces list` and `workspaces
	// status`; nil when it was not, or could not be.
	Setup *SetupState `json:"setup,omitempty"`
}

type Me struct {
	User       MeUser      `json:"user"`
	Workspaces []Workspace `json:"workspaces"`
}

// MeUser is the signed-in user in /api/v1/cli/me.
type MeUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func fetchMe(ctx context.Context, c *api.Client) (*Me, error) {
	var body struct {
		Data Me `json:"data"`
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/v1/cli/me", nil, &body, true); err != nil {
		return nil, err
	}
	// Only completion reads the copy, so a copy that cannot be written
	// costs a command nothing.
	if err := rememberWorkspaces(c.Host, body.Data.Workspaces); err != nil {
		c.Debugf("could not keep the workspace names for completion: %v", err)
	}
	return &body.Data, nil
}

// A fresh sign-in keeps the profile's workspace if the token still reaches
// it, and otherwise falls back to the token's own default.
func rememberDefaultWorkspace(app *App, p *config.Profile, me *Me) {
	for _, w := range me.Workspaces {
		if w.ID == p.Workspace {
			return
		}
	}
	if ws := defaultWorkspaceOf(me); ws != "" {
		p.Workspace = ws
		app.SaveConfig()
	}
}

// defaultWorkspaceOf is the id of the sign-in's own default workspace, or
// empty when it has none.
func defaultWorkspaceOf(me *Me) string {
	for _, w := range me.Workspaces {
		if w.Default {
			return w.ID
		}
	}
	return ""
}

func newWhoamiCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are signed in as",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := app.Client()
			if err != nil {
				return err
			}
			me, err := fetchMe(cmd.Context(), c)
			if err != nil {
				return err
			}
			if app.JSONOutput {
				return app.PrintJSON(me)
			}
			fmt.Fprintf(app.Out, "%s <%s> on %s\n", ui.SafeLine(me.User.Name), ui.SafeLine(me.User.Email), c.Host)
			if t, _ := c.Token(); t != nil && !t.ExpiresAt.IsZero() {
				fmt.Fprintf(app.Out, "Token renews automatically; current one expires %s.\n", t.ExpiresAt.Local().Format(time.RFC1123))
			}
			return nil
		},
	}
}

func newWorkspacesCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspaces",
		Aliases: []string{"workspace"},
		Short:   "List workspaces, choose the default one or check on a setup",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the workspaces you can reach",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := app.Client()
			if err != nil {
				return err
			}
			me, err := fetchMe(cmd.Context(), c)
			if err != nil {
				return err
			}
			if err := addSetupStates(cmd.Context(), app, c, me.Workspaces); err != nil {
				return err
			}
			if app.JSONOutput {
				return app.PrintJSON(me.Workspaces)
			}
			current := app.DefaultWorkspace()
			tw := tabwriter.NewWriter(app.Out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "\tNAME\tID\tKIND\tSETUP\tDATA ACCESS")
			for _, w := range me.Workspaces {
				marker := ""
				if w.ID == current {
					marker = "*"
				}
				kind := "production"
				if w.Demo {
					kind = "demo"
				}
				access := "yes"
				if !w.MCPAvailable {
					access = "not on this plan"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", marker, ui.SafeLine(w.Name), ui.SafeLine(w.ID), kind, w.Setup.label(), access)
			}
			return tw.Flush()
		},
	}
	use := &cobra.Command{
		Use:               "use <id or name>",
		Short:             "Set the default workspace for this profile",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeWorkspaceArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := app.profileToChange(cmd)
			if err != nil {
				return err
			}
			host, err := app.Host()
			if err != nil {
				return err
			}
			// The workspace is one of host's, and the profile's default
			// applies on the profile's own host only (see profileWorkspace).
			// So a sign-in's rule decides where it goes: --host points the
			// profile at the host it names, as `login --host` does, and
			// FIRMFACT_HOST never changes the profile. Saved into a profile
			// on another host, the id would be ignored while the variable
			// is set, and sent to a host it does not belong to once it is
			// not.
			if !app.keepsSignIn(p, host) {
				return app.workspaceNotKept(p, host, args[0])
			}
			own, err := p.ParsedHost(true)
			moves := err != nil || own != host
			c, err := app.Client()
			if err != nil {
				return err
			}
			me, err := fetchMe(cmd.Context(), c)
			if err != nil {
				return err
			}
			for _, w := range me.Workspaces {
				if w.ID == args[0] || equalFold(w.Name, args[0]) {
					if moves {
						rememberHost(p, host)
					}
					p.Workspace = w.ID
					// The change is the command's work, so a config that
					// cannot be saved is its failure, not a warning.
					if err := app.saveConfig(); err != nil {
						return err
					}
					if app.JSONOutput {
						err := app.PrintJSON(w)
						app.envOverride(envWorkspace, "workspace")
						return err
					}
					if moves {
						fmt.Fprintf(app.Out, "Profile %s now uses %s.\n", ui.SafeLine(app.profileName()), host)
					}
					fmt.Fprintf(app.Out, "Default workspace is now %s.\n", ui.SafeLine(w.Name))
					app.envOverride(envWorkspace, "workspace")
					app.printNextStep(stepVendors)
					return nil
				}
			}
			return withExit(ExitNotFound, fmt.Errorf("no workspace %q; see `%s workspaces list`", args[0], app.Name))
		},
	}
	cmd.AddCommand(list, use, newWorkspaceStatusCommand(app))
	return cmd
}
