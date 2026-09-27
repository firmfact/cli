package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/firmfact/cli/cmd"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/update"
)

// version, commit and date are set at build time, as .goreleaser.yaml
// sets them:
//
//	-ldflags "-X main.version=0.4.0 -X main.commit=<full commit> -X main.date=<commit time, RFC 3339>"
//
// A build that does not set them reports what Go recorded instead (see
// buildInfo).
var (
	version = "dev"
	commit  string
	date    string
)

func main() {
	// Before anything is printed: a Windows console shows colour only once
	// it has been asked to understand escape sequences.
	restore := ui.PrepareConsole(os.Stdout, os.Stderr)
	update.RemoveOldBinary()
	status := run(os.Args[1:], cmd.OSStreams())
	restore()
	os.Exit(status)
}

// run is the whole program but the exit, so tests can run it in a child
// process. It returns the exit status: 0, or one of the cmd.Exit* codes.
func run(args []string, streams cmd.IOStreams) int {
	// The first Ctrl-C or SIGTERM cancels the command's context: requests
	// and waits stop, a prompt puts the terminal back as it found it, and
	// the command returns. From then on the signals have their default
	// effect again, so a second Ctrl-C ends the process at once should
	// anything not be listening.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	stamped := cmd.Build{Version: version, Commit: commit, Date: date}
	root := cmd.NewRootCommand(buildInfo(stamped, debug.ReadBuildInfo), args, streams)
	err := root.ExecuteContext(ctx)
	if ctx.Err() != nil {
		// Whatever the command returned, it is the interrupt's doing.
		ui.RestoreTerminal(streams.Err, streams.Out)
		err = cmd.ErrInterrupted
	}
	if err == nil {
		return 0
	}
	return cmd.ReportError(streams.Err, err, cmd.JSONRequested(root, args, err))
}

// buildInfo is the build this binary reports: what the release build
// stamped into it, and for what it did not stamp, what Go recorded. A
// build from a checkout records the commit, its time and whether any file
// differed from it (the vcs.* settings); go install ...@<commit> records
// a pseudo-version, which holds the commit's first 12 characters and its
// time; go install ...@v0.3.0 records only the version, so its commit and
// date stay unknown. info is debug.ReadBuildInfo, passed in so tests can
// describe any build.
func buildInfo(stamped cmd.Build, info func() (*debug.BuildInfo, bool)) cmd.Build {
	b := stamped
	b.Version = buildVersion(stamped.Version, info)
	bi, ok := info()
	if !ok {
		return b
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	rev, when := settings["vcs.revision"], settings["vcs.time"]
	if rev == "" && module.IsPseudoVersion(bi.Main.Version) {
		rev, _ = module.PseudoVersionRev(bi.Main.Version)
		if t, err := module.PseudoVersionTime(bi.Main.Version); err == nil {
			when = t.UTC().Format(time.RFC3339)
		}
	}
	if b.Commit == "" {
		b.Commit = rev
	}
	// What Go recorded describes its own commit; it fills in the stamped
	// one only when that is the same commit (a pseudo-version names just
	// the start of it).
	if rev != "" && strings.HasPrefix(b.Commit, rev) {
		if b.Date == "" {
			b.Date = when
		}
		b.Modified = settings["vcs.modified"] == "true"
	}
	return b
}

// buildVersion is the version this binary reports: the one the release
// build stamped, or, when none was (go install ...@v0.3.0), the module
// version Go recorded, without its 'v', so that such a copy takes part in
// the daily check and says what it is. info is debug.ReadBuildInfo, passed
// in so tests can describe any build. A build from a checkout records
// "(devel)", which stays "dev", or a pseudo-version, which
// update.IsRelease does not count as a release.
func buildVersion(stamped string, info func() (*debug.BuildInfo, bool)) string {
	if stamped != "dev" {
		return stamped
	}
	if bi, ok := info(); ok && semver.IsValid(bi.Main.Version) {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return stamped
}
