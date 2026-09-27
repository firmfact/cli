package cmd

import (
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/update"
)

// Build is what the running binary knows of its own build. main fills it
// in from what the release build stamped into the binary (the ldflags in
// .goreleaser.yaml), or else from what Go recorded.
type Build struct {
	Version string
	// Commit is the commit the binary was built from: in full when the
	// release build stamped it or Go recorded a checkout's, only the start
	// of it when all Go had was a pseudo-version, and "" when nothing
	// recorded it, as for go install ...@v0.4.0.
	Commit string
	// Date is when Commit was made, in RFC 3339.
	Date string
	// Modified marks a build from a checkout whose files differed from
	// Commit.
	Modified bool
}

// buildReport is the build as version and doctor report it. The Go
// version, system and architecture are the ones the binary was built with
// and for; the install method is judged from where the binary is.
type buildReport struct {
	Version       string        `json:"version"`
	Commit        string        `json:"commit"`
	Date          string        `json:"date"`
	Modified      bool          `json:"modified"`
	GoVersion     string        `json:"go_version"`
	OS            string        `json:"os"`
	Arch          string        `json:"arch"`
	InstallMethod update.Method `json:"install_method"`
}

func (a *App) buildReport() buildReport {
	b := a.build
	return buildReport{
		Version:       b.Version,
		Commit:        b.Commit,
		Date:          b.Date,
		Modified:      b.Modified,
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		InstallMethod: update.InstallMethod(),
	}
}

// versionReport is what version prints with --json.
type versionReport struct {
	buildReport
	// Latest is the newest release the last check heard of (the daily
	// one, doctor's or update's), pre-releases included for a
	// pre-release; "" before any has.
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	// Upgrade is the command that upgrades this installation, when an
	// update is available.
	Upgrade string `json:"upgrade,omitempty"`
}

func newVersionCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show this CLI's version and build, and the latest release",
		Long: "Show this CLI's version, the commit it was built from and when that commit\n" +
			"was made, the Go version and platform it was built with and for, how it was\n" +
			"installed, and the latest release the daily check has heard of. It asks no\n" +
			"server; `" + app.Name + " doctor` looks the latest release up again.",
		Args: cobra.NoArgs,
		// It reads no profile, so a config file that cannot be used does
		// not stop it, nor does a profile that does not exist; the version
		// check lets it run too (versionExempt).
		Annotations: map[string]string{withoutConfigAnnotation: "true", noProfileAnnotation: "true"},
		RunE: func(_ *cobra.Command, _ []string) error {
			r := versionReport{buildReport: app.buildReport()}
			if cacheDir, err := config.CacheDir(); err == nil {
				r.Latest = update.LatestKnown(cacheDir, r.Version)
			}
			if r.Latest != "" && update.Offer(r.Latest, r.Version) {
				r.UpdateAvailable, r.Upgrade = true, upgradeTo(r.InstallMethod, app.Name, r.Latest)
			}
			if app.JSONOutput {
				return app.PrintJSON(r)
			}
			fmt.Fprintf(app.Out, "%s %s\n", app.Name, r.Version)
			writeBuild(app.Out, r.buildReport)
			latest := ui.SafeLine(r.Latest)
			switch {
			case r.Latest == "":
				latest = "not known yet; `" + app.Name + " doctor` looks it up"
			case r.UpdateAvailable:
				latest += " is available; upgrade with: " + r.Upgrade
			case r.Latest == r.Version:
				latest += " (this version)"
			}
			buildLine(app.Out, "latest", latest)
			return nil
		},
	}
}

// writeBuild writes the build's details under the line that names the
// version, as version and doctor show them.
func writeBuild(w io.Writer, r buildReport) {
	commit, date := r.Commit, commitDate(r.Date)
	if commit == "" {
		commit = "unknown"
	}
	if r.Modified {
		commit += ", with uncommitted changes"
	}
	buildLine(w, "commit", commit)
	buildLine(w, "committed", date)
	buildLine(w, "go", r.GoVersion)
	buildLine(w, "platform", r.OS+"/"+r.Arch)
	buildLine(w, "install", string(r.InstallMethod))
}

func buildLine(w io.Writer, label, value string) {
	fmt.Fprintf(w, "  %-9s %s\n", label, value)
}

// commitDate is the commit's date for people to read, in UTC, as the
// build recorded it: the same whichever machine shows it.
func commitDate(date string) string {
	if date == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return ui.SafeLine(date)
	}
	return t.UTC().Format("2 Jan 2006 15:04 MST")
}
