package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/update"
)

// Commands that must keep working on an outdated version: the ones that
// upgrade or diagnose it, and the ones that only undo or adjust things on
// this machine. Signing out and `claim --undo` must never wait for an
// upgrade, and neither must repairing the host or the command list.
var versionExempt = map[string]bool{
	"update": true, "doctor": true, "help": true, "completion": true, "version": true,
	"logout": true, "claim": true, "config": true, "tools": true,
	// It only hands the host to the browser; the web app does not care
	// which CLI asked.
	"open": true,
	// A Tab asks through these: its answer must come at once, and an
	// error in place of it would only leave the shell without one.
	cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true,
}

// isVersionExempt judges a subcommand by the command directly under the
// root, so `config show` counts as config and `claim --undo` as claim. A
// command generated from a server tool is never exempt, whichever group the
// server's naming puts it in.
func isVersionExempt(cmd *cobra.Command) bool {
	if _, generated := cmd.Annotations[toolAnnotation]; generated {
		return false
	}
	if _, group := cmd.Annotations[groupAnnotation]; group {
		return true // it only shows its help
	}
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	return versionExempt[cmd.Name()]
}

// checkVersion runs before every command and acts on what the last check
// learned: below the server's minimum version the command is refused with
// the upgrade command, and on a terminal a newer release gets one line on
// stderr. Once a day it starts the next check, which runs beside the
// command, so a slow or unreachable GitHub or server adds nothing to its
// time; what it learns applies from the next run. In CI, or with stdout
// not on a terminal, GitHub is not asked, as nobody would see the notice;
// the server's minimum still is. Scripts and --json never see the notice,
// and FIRMFACT_NO_UPDATE_CHECK=1 turns the whole check off. Local and
// snapshot builds skip it (see update.IsRelease).
func checkVersion(app *App, cmd *cobra.Command) error {
	if !update.IsRelease(api.Version) || updateCheckOff() || isVersionExempt(cmd) {
		return nil
	}
	cacheDir, err := config.CacheDir()
	if err != nil {
		return nil
	}
	// No usable host: the command reports that itself if it needs one, and
	// config set-host, which repairs it, must still run.
	host, err := app.Host()
	if err != nil {
		return nil
	}
	ctx := cmd.Context()
	tooOld := func(s update.Status) bool { return s.Minimum != "" && update.Newer(s.Minimum, api.Version) }
	github := askGitHub(app)
	s, due := update.Cached(host, cacheDir, github)
	// A refusal on an answer that is due for renewal is renewed first, in
	// case the server has lowered its minimum since. The command would not
	// run anyway, so the wait costs it nothing, and a refused command ends
	// too soon for a check beside it to finish.
	if due && tooOld(s) {
		s, due = update.Check(ctx, host, cacheDir, false), false
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	hint := update.UpgradeHint(update.InstallMethod(), app.Name)
	if tooOld(s) {
		return withExit(ExitUnsupported, fmt.Errorf("this is %s %s; %s needs %s or newer. Upgrade with: %s", app.Name, api.Version, host, ui.SafeLine(s.Minimum), hint))
	}
	if due {
		stopWithCommand(update.Start(ctx, host, cacheDir, github))
	}
	if s.Latest != "" && update.Offer(s.Latest, api.Version) && app.Interactive() && !app.JSONOutput {
		fmt.Fprintf(app.Err, "%s %s is available (you have %s). Upgrade with: %s\n", app.Name, ui.SafeLine(s.Latest), api.Version, hint)
	}
	return nil
}

// updateCheckOff reports whether FIRMFACT_NO_UPDATE_CHECK turns the daily
// check off (see envOn), so that =0 or =false leaves it on. doctor then
// leaves GitHub out too, so the variable keeps the CLI to the firmfact host
// (see the README's Security and privacy).
func updateCheckOff() bool { return envOn(envNoUpdateCheck) }

// askGitHub says whether the daily check asks GitHub for a newer release:
// only where someone may read the notice. A CI job (CI set, as CI services
// do, to true or 1; see envOn) or output to a pipe or a file has nobody to
// read it, and a build agent that may not reach GitHub at all has no
// reason to try.
func askGitHub(app *App) bool {
	return !envOn(envCI) && ui.IsTerminal(app.Out)
}

// backgroundChecks are the version checks running beside commands. Each is
// stopped when its command's Execute returns, failed or not, by a
// finalizer, which cobra runs at that point: in the program that is just
// before it exits, and in tests before the directories the check writes to
// are removed. cobra keeps finalizers for the whole process, so one is
// registered, once, and it stops every check started since it last ran.
var backgroundChecks struct {
	register sync.Once
	mu       sync.Mutex
	stops    []func()
}

// stopWithCommand has stop called when the running command ends.
func stopWithCommand(stop func()) {
	backgroundChecks.register.Do(func() { cobra.OnFinalize(stopBackgroundChecks) })
	backgroundChecks.mu.Lock()
	defer backgroundChecks.mu.Unlock()
	backgroundChecks.stops = append(backgroundChecks.stops, stop)
}

func stopBackgroundChecks() {
	backgroundChecks.mu.Lock()
	stops := backgroundChecks.stops
	backgroundChecks.stops = nil
	backgroundChecks.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

func newUpdateCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update this CLI to the latest release",
		Args:  cobra.NoArgs,
		// Neither a broken config file nor a profile that does not exist
		// should stand between a user and a fixed release. The host it asks
		// for the oldest version it supports is the profile's, or the
		// default one.
		Annotations: map[string]string{withoutConfigAnnotation: "true", noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.missingProfileOK = true
			cacheDir, err := config.CacheDir()
			if err != nil {
				return err
			}
			host, err := app.Host()
			if err != nil {
				return err
			}
			s := update.Check(cmd.Context(), host, cacheDir, true)
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if s.Latest == "" {
				return withExit(ExitUnavailable, errors.New("could not find the latest release; check your connection or "+"https://github.com/"+update.Repo+"/releases"))
			}
			result := updateResult{Version: api.Version, Latest: s.Latest}
			if !update.Offer(s.Latest, api.Version) {
				if app.JSONOutput {
					return app.PrintJSON(result)
				}
				fmt.Fprintf(app.Out, "You have the latest version (%s).\n", api.Version)
				return nil
			}
			if m := update.InstallMethod(); m != update.Direct {
				if app.JSONOutput {
					result.Upgrade = update.UpgradeHint(m, app.Name)
					return app.PrintJSON(result)
				}
				fmt.Fprintf(app.Out, "%s %s is available. This copy was installed with %s; upgrade with:\n\n  %s\n", app.Name, ui.SafeLine(s.Latest), m, update.UpgradeHint(m, app.Name))
				return nil
			}
			progress := app.Out
			if app.JSONOutput {
				progress = app.Err
			}
			fmt.Fprintf(progress, "Updating %s %s to %s...\n", app.Name, api.Version, ui.SafeLine(s.Latest))
			path, err := update.SelfUpdate(cmd.Context(), s.Latest)
			if err != nil {
				return err
			}
			if app.JSONOutput {
				result.Updated, result.Path = true, path
				return app.PrintJSON(result)
			}
			fmt.Fprintf(app.Out, "%s Updated %s.\n", app.Mode().Rainbow("Done."), path)
			return nil
		},
	}
}

// updateResult is what update prints with --json.
type updateResult struct {
	// Version is the version that ran the command.
	Version string `json:"version"`
	Latest  string `json:"latest"`
	Updated bool   `json:"updated"`
	// Path is the binary that was replaced, once updated.
	Path string `json:"path,omitempty"`
	// Upgrade is the command to upgrade a copy that a package manager
	// installed, which update leaves to it.
	Upgrade string `json:"upgrade,omitempty"`
}

// doctorReport is what doctor prints with --json.
type doctorReport struct {
	Version string        `json:"version"`
	Build   buildReport   `json:"build"`
	Host    string        `json:"host"`
	OK      bool          `json:"ok"`
	Checks  []doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func newDoctorCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this installation, the connection and your sign-in",
		Args:  cobra.NoArgs,
		// A config file that cannot be read, or a profile in use that does
		// not exist, is one of the things it reports (see configHealth);
		// the other checks then run on the default host.
		Annotations: map[string]string{withoutConfigAnnotation: "true", noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			app.missingProfileOK = true
			ctx := cmd.Context()
			m := app.Mode()
			failed := 0
			var checks []doctorCheck
			report := func(ok bool, label, detail string) {
				// Interrupted, the checks still to come fail at once; they
				// are not failures worth listing.
				if ctx.Err() != nil {
					return
				}
				if !ok {
					failed++
				}
				if app.JSONOutput {
					checks = append(checks, doctorCheck{Name: label, OK: ok, Detail: detail})
					return
				}
				mark := m.Rainbow("ok  ")
				if !ok {
					mark = m.Orange("FAIL")
				}
				fmt.Fprintf(app.Out, "  %s  %-14s %s\n", mark, label, ui.SafeLine(detail))
			}
			c, err := app.Client()
			if err != nil {
				return err
			}
			// The build heads the report, as a bug report needs it.
			build := app.buildReport()
			if !app.JSONOutput {
				fmt.Fprintf(app.Out, "%s %s on %s\n", app.Name, api.Version, c.Host)
				writeBuild(app.Out, build)
				fmt.Fprintln(app.Out)
			}

			// One request to the host serves three checks: whether it
			// answers, its clock, and the oldest version it supports. A host
			// that does not answer then costs one wait, not two.
			start := time.Now()
			server, serverErr := getVersion(ctx, c)
			took := time.Since(start)
			var latest string
			if !updateCheckOff() {
				latest, _ = update.LatestRelease(ctx)
			}
			s := update.Status{Host: c.Host, Latest: latest, Minimum: server.minimum}
			if cacheDir, err := config.CacheDir(); err == nil {
				s = update.Record(ctx, c.Host, cacheDir, latest, server.minimum, !updateCheckOff())
			}
			switch {
			// A local or snapshot build is not held to the minimum, just as
			// checkVersion lets it run every command.
			case !update.IsRelease(api.Version):
				report(true, "version", api.Version+" is a development build; the minimum and latest versions do not apply")
			case s.Minimum != "" && update.Newer(s.Minimum, api.Version):
				report(false, "version", fmt.Sprintf("%s is below the minimum %s; run: %s", api.Version, s.Minimum, update.UpgradeHint(update.InstallMethod(), app.Name)))
			case s.Latest != "" && update.Offer(s.Latest, api.Version):
				report(true, "version", fmt.Sprintf("%s works; %s is available (%s)", api.Version, s.Latest, update.UpgradeHint(update.InstallMethod(), app.Name)))
			case s.Latest == "" && updateCheckOff():
				report(true, "version", api.Version+" (FIRMFACT_NO_UPDATE_CHECK is set, so GitHub was not asked for a newer release)")
			case s.Latest == "":
				report(true, "version", api.Version+" (could not check for a newer release)")
			default:
				report(true, "version", api.Version+" is the latest")
			}

			// Before the connection, as it is the way there.
			proxyOK, proxyDetail := proxyCheck(c.Host, os.Getenv)
			report(proxyOK, "proxy", proxyDetail)
			if serverErr != nil {
				report(false, "connection", serverErr.Error())
			} else {
				detail := fmt.Sprintf("%s answered %d in %s", c.Host, server.status, took.Round(time.Millisecond))
				switch {
				case server.redirect != "":
					detail += " (a redirect to " + server.redirect + ", which the CLI does not follow)"
				case server.status == http.StatusNotFound:
					// Every firmfact with CLI support has this endpoint.
					detail += "; the server predates the CLI (or the host is wrong)"
				}
				report(server.status == http.StatusOK, "connection", detail)
				if date, err := http.ParseTime(server.date); err == nil {
					skew := time.Since(date).Round(time.Second)
					if skew < 0 {
						skew = -skew
					}
					report(skew < 2*time.Minute, "clock", fmt.Sprintf("%s off the server's clock", skew))
				}
			}

			tok, err := c.Token()
			if err == nil {
				var where string
				if where, err = config.DescribeStore(c.Host); err == nil {
					report(true, "token store", where)
				}
			}
			if err != nil {
				report(false, "token store", err.Error())
			} else if tok == nil {
				report(false, "sign-in", "not signed in; run: "+app.Name+" login")
			} else if serverErr != nil {
				// Asking again would only wait for the same silence.
				report(false, "sign-in", "not checked, as the connection failed")
			} else if me, err := fetchMe(ctx, c); err != nil {
				report(false, "sign-in", err.Error())
			} else {
				report(true, "sign-in", fmt.Sprintf("%s, %d workspace(s)", me.User.Email, len(me.Workspaces)))
			}

			configOK, configDetail := app.configHealth()
			report(configOK, "config", configDetail)
			if err := ctx.Err(); err != nil {
				return err
			}

			if tc := loadToolCache(c.Host); tc == nil {
				report(false, "commands", "none cached; run: "+app.Name+" tools refresh")
			} else {
				report(true, "commands", fmt.Sprintf("%d, fetched %s", len(tc.Tools), tc.FetchedAt.Local().Format("2 Jan 2006 15:04")))
			}

			if app.JSONOutput {
				// The report goes out either way, so a script can see
				// which checks failed; the exit status says whether any.
				if err := app.PrintJSON(doctorReport{Version: api.Version, Build: build, Host: c.Host, OK: failed == 0, Checks: checks}); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(app.Out)
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			if !app.JSONOutput {
				fmt.Fprintln(app.Out, "All good.")
			}
			return nil
		},
	}
}

// proxyFor is httpx.ProxyFor. net/http reads the proxy variables once per
// process, so a test stands in a proxy here rather than setting them.
var proxyFor = httpx.ProxyFor

// proxyCheck is doctor's line on the proxy that the requests to host go
// through: the proxy, none, or why the one that is set is left out.
// getenv is os.Getenv, passed in so tests can describe any environment.
// A password in the proxy's address is masked.
func proxyCheck(host string, getenv func(string) string) (ok bool, detail string) {
	// An https host takes HTTPS_PROXY, a plain http one HTTP_PROXY, each
	// in capitals or not, as net/http reads them.
	variable := "HTTPS_PROXY"
	if strings.HasPrefix(host, "http://") {
		variable = "HTTP_PROXY"
	}
	value := getenv(variable)
	if lower := strings.ToLower(variable); value == "" && getenv(lower) != "" {
		variable, value = lower, getenv(lower)
	}
	proxy, err := proxyFor(host)
	switch {
	case err != nil:
		return false, fmt.Sprintf("%s is not usable: %v", variable, err)
	case proxy != nil:
		return true, proxy.Redacted() + " (from " + variable + ")"
	case value == "":
		return true, "none; connecting directly"
	case !readsAsProxy(value):
		// net/http ignores it without a word, which a user behind a
		// proxy that must be used finds out only as a failed connection.
		return false, variable + " is set but is not a proxy address, so the CLI connects directly"
	}
	// Set but not used: net/http never sends a request for this machine
	// through a proxy, and NO_PROXY can leave out any other host.
	name := host
	if u, err := url.Parse(host); err == nil {
		name = u.Hostname()
	}
	if ip := net.ParseIP(name); name == "localhost" || ip != nil && ip.IsLoopback() {
		return true, "none; " + variable + " is not used for this machine"
	}
	return true, "none; NO_PROXY leaves out " + name
}

// readsAsProxy reports whether net/http takes value as a proxy's address:
// a URL with a scheme it knows, or one that reads as a URL once http:// is
// put in front, as in proxy.example:3128.
func readsAsProxy(value string) bool {
	if u, err := url.Parse(value); err == nil {
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
			return true
		}
	}
	_, err := url.Parse("http://" + value)
	return err == nil
}

// versionAnswer is what doctor reads from the host's version endpoint.
type versionAnswer struct {
	status   int
	date     string // the Date header
	minimum  string // the oldest CLI version the host supports
	redirect string // where a 3xx answer points
}

// getVersion asks c's host for its version endpoint. Doctor wants the
// status and Date header too, not only the decoded JSON the API client's
// JSON method gives, but it goes through the same client, so the same
// limits, redirect rule and debug log apply.
func getVersion(ctx context.Context, c *api.Client) (versionAnswer, error) {
	resp, err := c.Raw(ctx, http.MethodGet, "/api/v1/cli/version")
	if err != nil {
		return versionAnswer{}, err
	}
	defer resp.Body.Close()
	a := versionAnswer{status: resp.StatusCode, date: resp.Header.Get("Date")}
	if loc, err := resp.Location(); err == nil {
		a.redirect = loc.Scheme + "://" + loc.Host
	}
	if resp.StatusCode == http.StatusOK {
		var body struct {
			MinimumVersion string `json:"minimum_version"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body) == nil {
			a.minimum = body.MinimumVersion
		}
	}
	return a, nil
}
