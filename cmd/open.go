package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/auth"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/ui"
)

// openResult is what open prints with --json.
type openResult struct {
	URL string `json:"url"`
}

// newOpenCommand is `firmfact open`: the web app, for what the CLI does not
// do, on the host this invocation uses, so --host and --profile take the
// browser where they take the other commands. A page of that host, such as
// the review link an upload prints, opens that page.
func newOpenCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "open [link]",
		Short: "Open firmfact in your browser",
		Long: `Open firmfact in your browser: the host you use, which is --host, else
FIRMFACT_HOST, else the profile's (https://firmfact.com unless you chose
another). Name a page of that host to open the page: its link, such as
the review link of a document you uploaded, or its path.`,
		Example: fmt.Sprintf(`  %[1]s open
  %[1]s open https://firmfact.com/accounts/<workspace>/documents/<document>`, app.Name),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(_ *cobra.Command, args []string) error {
			host, err := app.Host()
			if err != nil {
				return err
			}
			page := ""
			if len(args) == 1 {
				page = args[0]
			}
			target, err := openTarget(host, page)
			if err != nil {
				return err
			}
			// The browser opens it the way login's link opens: handed to
			// the platform's opener, never to a shell.
			if err := auth.LaunchBrowser(target); err != nil {
				return fmt.Errorf("could not open your browser (%w); open %s yourself", err, ui.SafeLine(target))
			}
			if app.JSONOutput {
				return app.PrintJSON(openResult{URL: target})
			}
			fmt.Fprintf(app.Out, "Opened %s in your browser.\n", ui.SafeLine(target))
			return nil
		},
	}
}

// openTarget is what open hands to the browser for page: the root of host
// when page is empty, else that page of host, named by its link or its
// path. The platform's opener would take a file or another scheme just as
// well, and xdg-open reads a word that starts with - as an option, so
// nothing but a page of host gets through: the target is always host, then
// the page's path, query and fragment.
func openTarget(host, page string) (string, error) {
	if page == "" {
		return host + "/", nil
	}
	u, err := url.Parse(page)
	if err != nil {
		return "", usageErrorf("%s is not a link or a path", ui.SafeLine(page))
	}
	switch {
	case u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/"):
		// A path of host.
	case u.Scheme == "http" || u.Scheme == "https":
		// ParseHost spells a host one way, as host is spelt: lower case,
		// no default port.
		origin, err := config.ParseHostAllowHTTP(u.Scheme + "://" + u.Host)
		if err != nil || u.User != nil {
			return "", usageErrorf("%s is not a link to a page of %s", ui.SafeLine(page), host)
		}
		if origin != host {
			return "", usageErrorf("%s is a page of %s, not of %s, the host you use; add --host %s to open it", ui.SafeLine(page), origin, host, origin)
		}
	default:
		return "", usageErrorf("open takes a page of %s: its link, or its path such as /accounts/...; %s is neither", host, ui.SafeLine(page))
	}
	target := host + u.EscapedPath()
	if u.Path == "" {
		target += "/"
	}
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		target += "#" + u.EscapedFragment()
	}
	return target, nil
}
