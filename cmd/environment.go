package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// newEnvironmentTopic is `firmfact help environment`: a help topic, not a
// command (it has no Run), so cobra lists it under "Additional help topics"
// and `firmfact environment` shows the same text. The variables are read
// all over the CLI; this is the one place that names them all. The
// commands it shows say {cli} where the name the CLI was invoked by goes.
func newEnvironmentTopic(name string) *cobra.Command {
	return &cobra.Command{
		Use:   "environment",
		Short: "Environment variables that set the host, profile, workspace and more",
		Long: strings.ReplaceAll(`Environment variables change what the CLI does without a flag on every
command, for a shell or a CI job that always means the same thing.

Where commands run. Each setting comes from its flag, else its variable, else
the profile; FIRMFACT_HOST overrides the host of any profile, even one chosen
with --profile. Set a variable to an empty value to ignore it for one command.
`+"`{cli} config show`"+` shows what is in use and which variables supplied it.

A variable never changes the profile. `+"`{cli} login`"+` and `+"`{cli} signup`"+` on the
host FIRMFACT_HOST names keep that sign-in, but not in the profile, which
keeps its own host and default workspace for when the variable is unset. On a
host other than the profile's, the profile's default workspace, which belongs
to its host, is not used; the sign-in's own default is. For the same reason
`+"`{cli} workspaces use`"+` refuses to run where FIRMFACT_HOST names a host
other than the profile's; with --host it points the profile at that host too,
as login does.

  FIRMFACT_HOST             the firmfact host, as --host takes it
  FIRMFACT_PROFILE          the profile to use, as --profile takes it
  FIRMFACT_WORKSPACE        the workspace, by id or name, as --workspace takes it

Signing in:

  FIRMFACT_TOKEN            an access token to use instead of the stored sign-in,
                            for scripts and CI. It is only sent to FIRMFACT_HOST,
                            or to https://firmfact.com when that is not set; a
                            command aimed at another host refuses to run.
  FIRMFACT_TOKEN_STORE      "file" keeps tokens in credentials.json in the
                            config directory without trying the system keyring
  FIRMFACT_SIGNUP_CODE      the 6-digit code from the email, for a scripted
                            signup

Everything else:

  FIRMFACT_TIMEOUT          the limit for each request, as --timeout takes it
  FIRMFACT_NO_UPDATE_CHECK  "1" turns the daily check for a newer release off,
                            and keeps `+"`{cli} doctor`"+` off GitHub too
  CI                        when set, as CI services set it, the daily check asks
                            only the server for the oldest version it supports,
                            not GitHub for a newer release
  FIRMFACT_DEBUG            "1" logs each request and answer on stderr, as --debug
                            does, with tokens, codes and passwords redacted
  FIRMFACT_CONFIG_DIR       where the profiles are kept, and the tokens when
                            there is no system keyring
  FIRMFACT_CACHE_DIR        where the cached command lists, workspace names,
                            sessions and release checks are kept
  HTTPS_PROXY, NO_PROXY     reach firmfact and GitHub through a proxy, but the
                            hosts NO_PROXY lists (HTTP_PROXY for a plain http
                            host); `+"`{cli} doctor`"+` shows the proxy in use
  SSL_CERT_FILE,            on Linux, a certificate bundle and directory to
  SSL_CERT_DIR              trust in place of the system's; macOS and Windows
                            use the system's certificate store
  NO_COLOR                  no colour; on a terminal the logo, next-step hints
                            and progress still show, in plain text

FIRMFACT_NO_UPDATE_CHECK, CI and FIRMFACT_DEBUG are on for any value but 0,
false, no or off (or blank), so FIRMFACT_NO_UPDATE_CHECK=0 leaves the check on
and CI=false is no CI job.

Examples:

  # Every command in this shell goes to a local development server, and
  # the profile still points at its own host once the variable is unset
  export FIRMFACT_HOST=localhost:5000
  {cli} login

  # A CI job with a token of its own: fetch the commands, then query a workspace
  export FIRMFACT_TOKEN=... FIRMFACT_WORKSPACE="Acme Bank"
  {cli} tools refresh
  {cli} vendors list --json`, "{cli}", name),
	}
}
