package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/auth"
)

// openResult is what open prints with --json.
type openResult struct {
	URL string `json:"url"`
}

// newOpenCommand is `firmfact open`: the web app, for what the CLI does not
// do, on the host this invocation uses, so --host and --profile take the
// browser where they take the other commands.
func newOpenCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "open",
		Short: "Open firmfact in your browser",
		Long: `Open firmfact in your browser: the host you use, which is --host, else
FIRMFACT_HOST, else the profile's (https://firmfact.com unless you chose
another).`,
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(_ *cobra.Command, _ []string) error {
			host, err := app.Host()
			if err != nil {
				return err
			}
			target := host + "/"
			// The browser opens it the way login's link opens: handed to
			// the platform's opener, never to a shell.
			if err := auth.LaunchBrowser(target); err != nil {
				return fmt.Errorf("could not open your browser (%w); open %s yourself", err, target)
			}
			if app.JSONOutput {
				return app.PrintJSON(openResult{URL: target})
			}
			fmt.Fprintf(app.Out, "Opened %s in your browser.\n", target)
			return nil
		},
	}
}
