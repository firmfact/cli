package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/update"
)

// firmfact update replaces a binary that was downloaded directly with a
// release from GitHub: the latest release, which is never a pre-release;
// with --pre, the newest release, pre-releases included; with --version,
// the release it names. Whichever it is, it is installed only once its
// signature, its checksum and a trial run of the new binary check out
// (see update.SelfUpdate). A copy that a package manager installed is
// left to that package manager.

// updateFlags are the flags of firmfact update.
type updateFlags struct {
	pre     bool
	version string
	yes     bool
}

func newUpdateCommand(app *App) *cobra.Command {
	var f updateFlags
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update this CLI to the latest release",
		Long: `Update this CLI to the latest release, which is never a pre-release. Before
anything is replaced, the release's checksums.txt must carry a valid
signature by firmfact's release key, the archive must match its checksum
there, and the new binary must run here and report the version it was
downloaded as. Should the swap fail, the old binary stays.

--pre takes the newest release, pre-releases such as 0.3.0-rc.1 included.
--version installs the release it names, such as 0.2.0 or v0.2.0, with the
same checks; one older than the version you have is installed once you
confirm, or off a terminal with --yes.

A copy that Homebrew, Scoop or winget installed is left to that package
manager: update says how to upgrade it, and with --pre or --version, what
the package manager can do instead.`,
		Example: fmt.Sprintf(`  %[1]s update
  %[1]s update --pre
  %[1]s update --version 0.2.0
  %[1]s update --version 0.1.0 --yes   # back to an older release, in a script`, app.Name),
		Args: cobra.NoArgs,
		// Neither a broken config file nor a profile that does not exist
		// should stand between a user and a fixed release. What it learns
		// of the releases goes with the daily check's findings for the
		// profile's host, or the default one.
		Annotations: map[string]string{withoutConfigAnnotation: "true", noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.missingProfileOK = true
			if cmd.Flags().Changed("version") {
				return updateToVersion(cmd.Context(), app, f)
			}
			return updateToNewest(cmd.Context(), app, f.pre)
		},
	}
	cmd.Flags().BoolVar(&f.pre, "pre", false, "take the newest release, pre-releases included")
	cmd.Flags().StringVar(&f.version, "version", "", "install release `version`, such as 0.2.0 or 0.3.0-rc.1")
	_ = cmd.RegisterFlagCompletionFunc("version", cobra.NoFileCompletions) // the flag was just defined
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "with --version, install a release older than this one without asking")
	cmd.MarkFlagsMutuallyExclusive("pre", "version")
	return cmd
}

// updateResult is what update prints with --json.
type updateResult struct {
	// Version is the version that ran the command.
	Version string `json:"version"`
	// Latest is the latest release, never a pre-release: what update
	// installs without --pre or --version. "" when none is published yet,
	// or when update did not ask (--pre, --version).
	Latest string `json:"latest"`
	// Newest is the newest release, pre-releases included, when update
	// read GitHub's list of releases: with --pre, or when no release is
	// published yet.
	Newest string `json:"newest,omitempty"`
	// Target is the release update installs, or would: one newer than
	// Version, or the one --version names.
	Target  string `json:"target,omitempty"`
	Updated bool   `json:"updated"`
	// Path is the binary that was replaced, once updated.
	Path string `json:"path,omitempty"`
	// Upgrade is the command to upgrade a copy that a package manager
	// installed, which update leaves to it.
	Upgrade string `json:"upgrade,omitempty"`
	// Install is that package manager's command for the release --version
	// names, where it has one.
	Install string `json:"install,omitempty"`
	// Pin is that package manager's command to keep the copy at its
	// version, where it has one, with --pre and --version.
	Pin string `json:"pin,omitempty"`
}

// updateToNewest installs the latest release or, with pre, the newest
// release, pre-releases included, when it is newer than this one.
func updateToNewest(ctx context.Context, app *App, pre bool) error {
	m := update.InstallMethod()
	if pre && m != update.Direct {
		return leaveToPackageManager(app, m, "", true)
	}
	github := update.FinalReleases
	if pre {
		github = update.WithPreReleases
	}
	found, err := update.Lookup(ctx, github)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return lookupFailed(err)
	}
	if host, err := app.Host(); err == nil {
		if cacheDir, err := config.CacheDir(); err == nil {
			update.Remember(host, cacheDir, found)
		}
	}
	result := updateResult{Version: api.Version, Latest: found.Latest, Newest: found.Newest}
	target := found.Latest
	if pre {
		target = found.Newest
	}

	if target == "" {
		if app.JSONOutput {
			return app.PrintJSON(result)
		}
		switch {
		case update.Newer(found.Newest, api.Version):
			fmt.Fprintf(app.Out, "No release is published yet, only pre-releases. To take the newest pre-release: %s update --pre\n", app.Name)
			return nil
		case found.Newest != "" && update.IsRelease(api.Version):
			// update --pre would find nothing newer to install.
			fmt.Fprintf(app.Out, "No release is published yet, only pre-releases, and you have the newest (%s).\n", api.Version)
			return nil
		case found.Newest != "":
			// A build of its own, which update --pre would not replace.
			fmt.Fprintln(app.Out, "No release is published yet, only pre-releases.")
			return nil
		}
		fmt.Fprintln(app.Out, "No release is published yet.")
		return nil
	}
	// Without --pre, someone on a release is never offered a
	// pre-release (see update.Offer); with it, anything newer goes.
	newer := update.Offer(target, api.Version)
	if pre {
		newer = update.Newer(target, api.Version)
	}
	if !newer {
		if app.JSONOutput {
			return app.PrintJSON(result)
		}
		switch {
		case pre:
			fmt.Fprintf(app.Out, "You have the newest version, pre-releases included (%s).\n", api.Version)
		case update.GitHubFor(api.Version) == update.WithPreReleases && update.Newer(api.Version, target):
			fmt.Fprintf(app.Out, "You have %s, newer than the latest release (%s). To take newer pre-releases too: %s update --pre\n", api.Version, ui.SafeLine(target), app.Name)
		default:
			fmt.Fprintf(app.Out, "You have the latest version (%s).\n", api.Version)
		}
		return nil
	}

	result.Target = target
	if m != update.Direct {
		result.Upgrade = update.UpgradeHint(m, app.Name)
		if app.JSONOutput {
			return app.PrintJSON(result)
		}
		fmt.Fprintf(app.Out, "%s %s is available. This copy was installed with %s; upgrade with:\n\n  %s\n", app.Name, ui.SafeLine(target), m, result.Upgrade)
		return nil
	}
	return selfUpdate(ctx, app, result, fmt.Sprintf("Updating %s %s to %s...", app.Name, api.Version, ui.SafeLine(target)))
}

// updateToVersion installs the release --version names. One older than
// this version needs a yes: on a terminal the question, off one --yes.
func updateToVersion(ctx context.Context, app *App, f updateFlags) error {
	target, ok := update.ReleaseVersion(strings.TrimSpace(f.version))
	if !ok {
		return usageErrorf("--version takes a release version such as 0.2.0 or 0.3.0-rc.1, not %q", ui.SafeLine(f.version))
	}
	if m := update.InstallMethod(); m != update.Direct {
		return leaveToPackageManager(app, m, target, false)
	}
	result := updateResult{Version: api.Version, Target: target}
	switch {
	case update.Newer(api.Version, target):
		if !f.yes {
			if !app.attended() {
				return usageErrorf("%s is older than the %s you have; nothing changed. Add --yes to install it all the same", target, api.Version)
			}
			ok, err := app.Confirm(ctx, fmt.Sprintf("%s is older than the %s you have. Install it all the same?", target, api.Version))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(app.Out, "Nothing changed.")
				return nil
			}
		}
	// Neither is newer: the same release, build metadata aside. A local
	// build, whose version compares with nothing, is replaced.
	case update.IsRelease(api.Version) && !update.Newer(target, api.Version):
		if app.JSONOutput {
			return app.PrintJSON(result)
		}
		fmt.Fprintf(app.Out, "You have %s already.\n", api.Version)
		return nil
	}
	err := selfUpdate(ctx, app, result, fmt.Sprintf("Installing %s %s in place of %s...", app.Name, target, api.Version))
	var missing *update.NotPublishedError
	if errors.As(err, &missing) {
		return withExit(ExitNotFound, fmt.Errorf("there is no release %s (%s was not found); the releases are at %s", target, missing.URL, update.ReleasesPage))
	}
	return err
}

// selfUpdate replaces this binary with release result.Target, after
// saying so on the line progress: on stdout, or on stderr with --json,
// whose stdout is the result alone.
func selfUpdate(ctx context.Context, app *App, result updateResult, progress string) error {
	w := app.Out
	if app.JSONOutput {
		w = app.Err
	}
	fmt.Fprintln(w, progress)
	path, err := update.SelfUpdate(ctx, result.Target)
	if err != nil {
		return err
	}
	if app.JSONOutput {
		result.Updated, result.Path = true, path
		return app.PrintJSON(result)
	}
	fmt.Fprintln(app.Out, updatedLine(app, path))
	return nil
}

// updatedLine is what update says on app's stdout once it has replaced the
// binary at path, "Done." in green on a terminal that shows colour.
func updatedLine(app *App, path string) string {
	return app.Mode().Green("Done.") + " Updated " + path + "."
}

// leaveToPackageManager is update's answer to --pre, or to --version
// naming version, for a copy that the package manager m keeps: its
// upgrade, as without them, and what it can do instead, which is less.
// None of them is given pre-releases, only winget installs a version it is
// asked for, and Homebrew cannot keep a cask at a version.
func leaveToPackageManager(app *App, m update.Method, version string, pre bool) error {
	result := updateResult{Version: api.Version, Target: version, Upgrade: update.UpgradeHint(m, app.Name), Pin: update.PinHint(m)}
	if !pre {
		result.Install = update.InstallHint(m, version)
	}
	if app.JSONOutput {
		return app.PrintJSON(result)
	}
	switch {
	case pre:
		fmt.Fprintf(app.Out, "This copy was installed with %s, which has releases only, not pre-releases; upgrade with:\n\n  %s\n", m, result.Upgrade)
	case result.Install != "":
		fmt.Fprintf(app.Out, "This copy was installed with %s; upgrade with:\n\n  %s\n\nor install %s with:\n\n  %s\n", m, result.Upgrade, version, result.Install)
	default:
		fmt.Fprintf(app.Out, "This copy was installed with %s, which installs the latest release only; upgrade with:\n\n  %s\n", m, result.Upgrade)
	}
	if result.Pin != "" {
		fmt.Fprintf(app.Out, "\nTo keep %s from upgrading it: %s\n", m, result.Pin)
	} else {
		fmt.Fprintf(app.Out, "\nThere is no way to keep %s from upgrading it.\n", m)
	}
	return nil
}

// lookupFailed is update's error for a GitHub that could not say what has
// been released. Only one that gave no answer could not be reached; one
// that is busy is worth trying again later, like it. A connection failure
// is not wrapped: the hint every command adds to one (withDoctorHint)
// would offer --timeout, which this lookup does not take, after the hint
// here.
func lookupFailed(err error) error {
	var (
		netErr *httpx.Error
		answer *update.AnswerError
	)
	switch {
	case errors.As(err, &netErr):
		return withExit(ExitUnavailable, fmt.Errorf("could not reach GitHub: %v; check your connection or %s", err, update.ReleasesPage)) //nolint:errorlint // not wrapped on purpose, see above
	case errors.As(err, &answer) && answer.Busy():
		return withExit(ExitUnavailable, fmt.Errorf("GitHub cannot say which releases there are just now (%w); try again later", err))
	}
	return fmt.Errorf("could not tell which releases there are: %w", err)
}
