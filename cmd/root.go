// Package cmd holds the `firmfact` commands.
package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// App is the state every command shares: the loaded config, the active
// profile and an API client for its host.
type App struct {
	// Name is how the CLI was invoked ("firmfact", or a user's own symlink).
	// Help texts and next-step hints use it.
	Name   string
	Config *config.Config
	// configErr is why the config file could not be used; Config then
	// holds the defaults, which are never saved (see requireConfig).
	configErr   error
	ProfileName string
	HostFlag    string
	// InsecureHTTP is --insecure-http: plain http to a host other than this
	// machine is allowed.
	InsecureHTTP bool
	// Timeout is --timeout; zero when not given.
	Timeout time.Duration
	// Debug is --debug (see debugLog).
	Debug      bool
	Workspace  string
	JSONOutput bool
	Out        io.Writer
	Err        io.Writer
	In         io.Reader
	stdin      *bufio.Reader
	// Format is --format; settleFormat reconciles it with --json.
	Format outputFormat
	// jq is --jq, nil when not given; PrintJSON prints through it.
	jq *jqFilter
	// builtins are the names of the CLI's own top-level commands, which no
	// server tool may take (see planToolCommands).
	builtins map[string]bool
	// debug is the log every client of this invocation shares.
	debug *api.DebugLog
	// build is the running binary's build, which version and doctor show.
	build Build
	// bannerShown is set once Banner has drawn the logo, which Celebrate
	// then looks for above the cursor.
	bannerShown bool
	// missingProfileOK is set by a command that runs without the profile in
	// use (update, doctor): a profile that does not exist then reads as a
	// new one would, on the default host, as the commands that run without
	// the config file read the defaults.
	missingProfileOK bool
}

// The environment can stand in for the global flags that decide where a
// command runs, for a shell or a CI job that always means the same place.
// Each setting is taken from its flag, else its variable, else the profile:
// FIRMFACT_HOST overrides the host of whichever profile is in use, even one
// chosen with --profile.
const (
	envHost      = "FIRMFACT_HOST"
	envProfile   = "FIRMFACT_PROFILE"
	envWorkspace = "FIRMFACT_WORKSPACE"
	// envDebug turns the debug log on, as --debug does.
	envDebug = "FIRMFACT_DEBUG"
	// envNoUpdateCheck turns the daily version check off.
	envNoUpdateCheck = "FIRMFACT_NO_UPDATE_CHECK"
	// envCI is set by CI services; the daily check then leaves GitHub out.
	envCI = "CI"
)

// fromEnv is the variable's value; blank counts as unset, so that
// `FIRMFACT_HOST= firmfact ...` is the way to ignore an exported one.
func fromEnv(name string) string { return strings.TrimSpace(os.Getenv(name)) }

// peekProfile is the profile this invocation uses, for reading. Only a
// command that uses no profile gets this far with one that does not exist
// (see requireProfile); it reads as a new one would, and is not added. A
// command that changes the profile gets it from profileToChange.
func (a *App) peekProfile() *config.Profile { return a.Config.Peek(a.profileName()) }

// profileName is the profile this invocation uses: --profile, else
// FIRMFACT_PROFILE, else the current one.
func (a *App) profileName() string { return resolveProfile(a.Config, a.ProfileName) }

func resolveProfile(cfg *config.Config, profileFlag string) string {
	if profileFlag != "" {
		return profileFlag
	}
	if env := fromEnv(envProfile); env != "" {
		return env
	}
	return cfg.CurrentProfile
}

// Host is --host when given, else FIRMFACT_HOST, else the profile's host,
// checked with config.ParseHost. An invalid host is an error for the
// command that needs it; commands that do not (config set-host, which
// repairs it) still run.
func (a *App) Host() (string, error) {
	return a.resolveHost(a.ProfileName, a.HostFlag, a.InsecureHTTP)
}

// resolveHost is Host for the given flags; hostFromArgs needs it before
// the flags are parsed, and FIRMFACT_HOST and FIRMFACT_PROFILE apply there
// too, or the tool cache of another host would be loaded.
func (a *App) resolveHost(profileFlag, hostFlag string, insecureHTTP bool) (string, error) {
	source, value := "--host", hostFlag
	if value == "" {
		source, value = envHost, fromEnv(envHost)
	}
	if value != "" {
		parse := config.ParseHost
		if insecureHTTP {
			parse = config.ParseHostAllowHTTP
		}
		host, err := parse(value)
		if err != nil {
			return "", usageErrorf("%s: %w", source, err)
		}
		return host, nil
	}
	profile := resolveProfile(a.Config, profileFlag)
	p, ok := a.Config.Lookup(profile)
	if !ok {
		// A profile that does not exist has no host; the default one would
		// be production (see requireProfile). When the config file cannot
		// be read, which profiles exist is not known, and the commands that
		// run without it use the defaults, as do those that run without
		// the profile.
		if a.configErr == nil && !a.missingProfileOK {
			return "", a.unknownProfile(profileFlag)
		}
		p = a.Config.Peek(profile)
	}
	host, err := p.ParsedHost(insecureHTTP)
	if err != nil {
		return "", fmt.Errorf("the host stored in profile %q is not usable: %w; set another with `%s config set-host <host>`", profile, err, a.Name)
	}
	return host, nil
}

// Client is an API client for Host, with the request limit --timeout or
// FIRMFACT_TIMEOUT asks for.
func (a *App) Client() (*api.Client, error) {
	host, err := a.Host()
	if err != nil {
		return nil, err
	}
	timeout, err := a.RequestTimeout()
	if err != nil {
		return nil, err
	}
	c := api.New(host)
	// A chosen limit covers the whole request, so the wait for the server
	// to start answering may take as long; zero keeps the defaults.
	c.HTTP = httpx.New(httpx.Options{Timeout: timeout, HeaderTimeout: timeout})
	c.Debug = a.debugLog()
	// A person sees why a command pauses for a busy server; a script's log
	// does not fill with it.
	if ui.IsTerminal(a.Err) {
		c.Busy = a.Err
	}
	return c, nil
}

// debugLog is where clients log their requests and answers: stderr, when
// --debug or FIRMFACT_DEBUG asks for it, else nowhere (nil). All clients
// of the invocation share it, so their lines do not run into each other.
func (a *App) debugLog() *api.DebugLog {
	if a.debug == nil && (a.Debug || debugFromEnv()) {
		a.debug = api.NewDebugLog(a.Err)
	}
	return a.debug
}

// debugFromEnv reports whether FIRMFACT_DEBUG turns the debug log on.
func debugFromEnv() bool { return envOn(envDebug) }

// envOn reports whether the variable name, one that turns something on or
// off, is on: set to any value but blank, 0, false, no or off, in any
// case. So FIRMFACT_NO_UPDATE_CHECK=0 leaves the check on, as CI=false
// leaves a run out of CI.
func envOn(name string) bool {
	switch strings.ToLower(fromEnv(name)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// MCP is an MCP client that talks through c. A limit the user chose is the
// limit for each of its requests too; without one, a chat gets 5 minutes
// and anything else 90 s (see mcp.ChatLimit).
func (a *App) MCP(c *api.Client) *mcp.Client {
	m := mcp.New(c)
	// Client already refused a FIRMFACT_TIMEOUT it could not read.
	m.Limit, _ = a.RequestTimeout()
	return m
}

// RequestTimeout is --timeout, else FIRMFACT_TIMEOUT, else zero: the
// defaults of 30 s for the server to start answering and 60 s in all (90 s
// for a workspace command, 5 minutes for a chat).
func (a *App) RequestTimeout() (time.Duration, error) {
	if a.Timeout > 0 {
		return a.Timeout, nil
	}
	env := os.Getenv("FIRMFACT_TIMEOUT")
	if env == "" {
		return 0, nil
	}
	d, err := parseTimeout(env)
	if err != nil {
		return 0, usageErrorf("FIRMFACT_TIMEOUT: %w", err)
	}
	return d, nil
}

// keyringUnlockWait is how much longer than usual a command with a person
// at the terminal waits for the system keyring (see
// config.SetKeyringPatience): a locked one holds each call until its
// password is typed into the unlock prompt.
var keyringUnlockWait = 2 * time.Minute

// keyringPatience is the extra wait for the keyring this command gets:
// keyringUnlockWait when a person is at the terminal, on stdin and stderr,
// where the CLI says what it waits for; none for a script, which has
// nobody to unlock the keyring.
func (a *App) keyringPatience() time.Duration {
	in, ok := a.In.(*os.File)
	if ok && term.IsTerminal(int(in.Fd())) && ui.IsTerminal(a.Err) {
		return keyringUnlockWait
	}
	return 0
}

// rememberHost makes host p's host, as after a sign-in there. A host only
// --insecure-http allows keeps that permission in the profile, so the next
// command can reach it too.
func rememberHost(p *config.Profile, host string) {
	p.Host = host
	p.InsecureHTTP = config.NeedsInsecureHTTP(host)
}

// keepsSignIn reports whether a sign-in on host goes into profile p, its
// host and default workspace, as login and signup do: when --host named
// the host, or it is p's own. FIRMFACT_HOST overrides the profile only for
// as long as it is set, so a sign-in on the host it names leaves the
// profile as it was. `export FIRMFACT_HOST=localhost:5000` and a login
// would otherwise point the profile at localhost for good. The sign-in
// itself is kept either way, by host.
func (a *App) keepsSignIn(p *config.Profile, host string) bool {
	if a.HostFlag != "" {
		return true
	}
	own, err := p.ParsedHost(true)
	return err == nil && own == host
}

// noteSignInNotKept tells the user, after a sign-in on host that
// keepsSignIn left out of profile p, where commands go once FIRMFACT_HOST is
// unset.
func (a *App) noteSignInNotKept(p *config.Profile, host string) {
	fmt.Fprintf(a.Err, "note: %s is set, so profile %s still points at %s; commands use %s until it is unset.\n",
		envHost, ui.SafeLine(a.profileName()), ui.SafeLine(p.Host), host)
}

// workspaceNotKept is why `workspaces use` refuses workspace on host, which
// FIRMFACT_HOST names and profile p does not keep (see keepsSignIn): p's
// default workspace belongs to p's own host, and the variable does not
// change p. It says how to name the workspace on host instead, or to point
// p at host as well.
func (a *App) workspaceNotKept(p *config.Profile, host, workspace string) error {
	return usageErrorf("%s is set, so this runs on %s, while profile %s stays on %s and its default workspace belongs there; "+
		"name the workspace with --workspace or FIRMFACT_WORKSPACE instead, or point the profile at %s too with `%s --host %s workspaces use %s`",
		envHost, host, ui.SafeLine(a.profileName()), ui.SafeLine(p.Host), host, a.Name, shellWord(host, "<host>"), argWord(ui.SafeLine(workspace), "<workspace>"))
}

// Mode is the colour mode for stdout; --json always means plain. It only
// says whether to colour. The logo, the next-step hints and the live
// progress line are for a person (see attended), who gets them in plain
// text with NO_COLOR set.
func (a *App) Mode() ui.ColorMode {
	if a.JSONOutput {
		return ui.NoColor
	}
	return ui.Detect(a.Out)
}

// attended reports whether a person is there to see what is shown only for
// one: the logo, the next-step hints, the live progress line. Scripts,
// pipes and --json get the output alone.
func (a *App) attended() bool { return a.Interactive() && !a.JSONOutput }

// Banner shows the logo when a person is at a terminal wide enough for it
// (nothing in scripts) and returns how many lines below the first logo row
// the cursor is, 0 when it showed none.
func (a *App) Banner() int {
	if !a.attended() {
		return 0
	}
	up := ui.Banner(a.Out, a.Mode(), ui.Width(a.Out))
	a.bannerShown = a.bannerShown || up > 0
	return up
}

// Celebrate runs the rainbow once over the logo `up` lines above the cursor,
// or over a fresh logo when that one has scrolled out of reach or none was
// shown, as on a terminal widened since.
func (a *App) Celebrate(ctx context.Context, up int) {
	m := a.Mode()
	if m == ui.NoColor || !a.attended() || ctx.Err() != nil {
		return
	}
	width, height := ui.Width(a.Out), ui.Height(a.Out)
	if a.bannerShown && ui.AnimateBanner(ctx, a.Out, m, width, height, up) {
		return
	}
	ui.AnimateBanner(ctx, a.Out, m, width, height, a.Banner())
}

// DefaultWorkspace is the workspace commands use: the one this invocation
// names (see ChosenWorkspace), else the profile's (see profileWorkspace).
func (a *App) DefaultWorkspace() string {
	if ws := a.ChosenWorkspace(); ws != "" {
		return ws
	}
	return a.profileWorkspace()
}

// profileWorkspace is the profile's default workspace when the command runs
// on the profile's host, and empty on any other that --host or
// FIRMFACT_HOST names: a workspace id belongs to one host, and elsewhere it
// names nothing, or something else. There the sign-in's own default
// workspace applies, as for a profile without one.
func (a *App) profileWorkspace() string {
	p := a.peekProfile()
	if p.Workspace == "" {
		return ""
	}
	host, err := a.Host()
	if err != nil {
		// The command reports the host itself if it needs one.
		return p.Workspace
	}
	if own, err := p.ParsedHost(true); err != nil || own != host {
		return ""
	}
	return p.Workspace
}

// ChosenWorkspace is the workspace this invocation names: --workspace, else
// FIRMFACT_WORKSPACE. It is empty when neither does and the profile's
// default applies.
func (a *App) ChosenWorkspace() string {
	if a.Workspace != "" {
		return a.Workspace
	}
	return fromEnv(envWorkspace)
}

// fromEnvNote marks a setting config show took from the variable name:
// its flag was not given and the variable is set.
func fromEnvNote(flagValue, name string) string {
	if flagValue == "" && fromEnv(name) != "" {
		return " (from " + name + ")"
	}
	return ""
}

// envOverride is the note that a variable, not the setting a command just
// changed, decides what the next commands use.
func (a *App) envOverride(name, what string) {
	if value := fromEnv(name); value != "" {
		fmt.Fprintf(a.Err, "note: %s is set, so commands use %s %s until it is unset.\n", name, what, ui.SafeLine(value))
	}
}

// Interactive reports whether we may prompt: stdin and stdout are terminals.
func (a *App) Interactive() bool {
	in, ok := a.In.(*os.File)
	out, ok2 := a.Out.(*os.File)
	return ok && ok2 && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func (a *App) reader() *bufio.Reader {
	if a.stdin == nil {
		a.stdin = bufio.NewReader(a.In)
	}
	return a.stdin
}

// Prompt asks for a line; an empty answer returns def. Cancelling ctx
// (Ctrl-C) ends the wait for an answer.
func (a *App) Prompt(ctx context.Context, label, def string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if def != "" {
		fmt.Fprintf(a.Out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(a.Out, "%s: ", label)
	}
	line, err := awaitRead(ctx, a.Out, func() (string, error) { return a.reader().ReadString('\n') }, nil)
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// PromptSecret reads a line without echo. Cancelling ctx (Ctrl-C) ends the
// wait with echo back on.
func (a *App) PromptSecret(ctx context.Context, label string) (string, error) {
	f, ok := a.In.(*os.File)
	if !ok {
		return "", errors.New("cannot read a hidden value without a terminal")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(f.Fd())
	// ReadPassword turns echo off and back on when the read returns, but an
	// interrupted read never does. The state is saved before it changes so
	// the cancel can put it back; without that the shell is left not
	// echoing what the user types.
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(a.Out, "%s: ", label)
	secret, err := awaitRead(ctx, a.Out, func() (string, error) {
		raw, err := term.ReadPassword(fd)
		return string(raw), err
	}, func() { _ = term.Restore(fd, state) })
	if !errors.Is(err, context.Canceled) {
		fmt.Fprintln(a.Out) // the Enter was not echoed either
	}
	return strings.TrimSpace(secret), err
}

// ReadStdinLine reads one line of piped standard input for a flag such as
// --password-stdin, the way `docker login --password-stdin` does: the value
// never becomes an argument, which other users of the machine can see (ps,
// /proc) and which lands in shell history and CI logs. Only the line ending
// is removed, so a password arrives exactly as given. A terminal is refused
// rather than read with echo on; there the prompt asks without echo. what
// names the value in errors, which never include what was read.
func (a *App) ReadStdinLine(ctx context.Context, flag, what string) (string, error) {
	if f, ok := a.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return "", usageErrorf("--%s reads the %s from a pipe, not a terminal; leave the flag out to be asked for it", flag, what)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	line, err := awaitRead(ctx, a.Out, func() (string, error) { return a.reader().ReadString('\n') }, nil)
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return "", usageErrorf("--%s found no %s on standard input", flag, what)
		}
		if ctx.Err() != nil {
			return "", err
		}
		return "", fmt.Errorf("--%s could not read standard input: %w", flag, err)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return "", usageErrorf("--%s read an empty %s", flag, what)
	}
	return line, nil
}

// awaitRead runs read, a blocking read of the terminal, and waits for it or
// for ctx to be cancelled. A read in progress cannot be interrupted, so on
// cancel it is left behind on its goroutine (the process exits soon after)
// and undo, when given, puts back what it changed. The prompt's line then
// gets the newline the user's Enter would have given it.
func awaitRead(ctx context.Context, out io.Writer, read func() (string, error), undo func()) (string, error) {
	type result struct {
		s   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := read()
		done <- result{s, err}
	}()
	select {
	case r := <-done:
		return r.s, r.err
	case <-ctx.Done():
		if undo != nil {
			undo()
		}
		ui.EndInterruptedLine(out)
		return "", ctx.Err()
	}
}

// Confirm asks a yes/no question, defaulting to no.
func (a *App) Confirm(ctx context.Context, question string) (bool, error) {
	answer, err := a.Prompt(ctx, question+" (y/N)", "")
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", nil
}

// PrintJSON writes v indented, or what the --jq expression makes of it.
// The values are mostly the server's, so the few terminal controls
// encoding/json leaves raw are escaped too (see ui.SafeJSON); the JSON
// still decodes to the same value.
func (a *App) PrintJSON(v any) error {
	if a.jq != nil {
		return a.printFiltered(v)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The HTML escaping encoding/json does by default is for JSON placed in
	// a web page. Scripts and people read this, and "S&P" should not arrive
	// as "S&P".
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := a.Out.Write(ui.SafeJSON(buf.Bytes()))
	return err
}

// SaveConfig persists the config, reporting (not failing on) errors: for
// a command whose work is done, such as a sign-in, which a profile that
// could not be updated does not undo.
func (a *App) SaveConfig() {
	if err := a.saveConfig(); err != nil {
		fmt.Fprintf(a.Err, "warning: could not save config: %v\n", err)
	}
}

// saveConfig persists the config, for a command whose work is the change.
func (a *App) saveConfig() error {
	return a.configFileHint(a.Config.Save())
}

// requireConfig refuses to run a command on the defaults when the config
// file cannot be used: it would run against the wrong host and, on its
// first change, replace the file and every profile in it. Only the
// commands that repair or report on the file, and those that need no
// profile, run without it (see worksWithoutConfig).
func (a *App) requireConfig(cmd *cobra.Command) error {
	if a.configErr == nil || worksWithoutConfig(cmd) {
		return nil
	}
	return a.configFileHint(a.configErr)
}

// configFileHint adds the way out to an error about a config file that
// cannot be used; other errors pass as they are.
func (a *App) configFileHint(err error) error {
	var fileErr *config.FileError
	if !errors.As(err, &fileErr) {
		return err
	}
	return fmt.Errorf("%w; fix the file, or run `%s config reset` to move it aside and start again from the defaults", err, a.Name)
}

// withoutConfigAnnotation marks a command that runs even when the config
// file cannot be used: it repairs the file (config reset) or reports on it
// (doctor), or has nothing to do with profiles (update).
const withoutConfigAnnotation = "firmfact/without-config"

func worksWithoutConfig(cmd *cobra.Command) bool {
	return cmd.Annotations[withoutConfigAnnotation] != "" || cmd.Annotations[groupAnnotation] != "" || helpOrCompletion(cmd)
}

// helpOrCompletion reports whether cmd is cobra's help or completion, or
// one of their subcommands: they read no settings.
func helpOrCompletion(cmd *cobra.Command) bool {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	switch cmd.Name() {
	case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	}
	return false
}

// invokedName is the name the binary was called by, so a symlink such as
// ~/.local/bin/fct gets hints that say "fct". Test binaries and odd names
// fall back to "firmfact".
func invokedName() string {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if base == "" || strings.HasSuffix(base, ".test") || strings.ContainsAny(base, " \t") {
		return "firmfact"
	}
	return base
}

// IOStreams are what the commands read from and write to. main passes the
// process's own; tests pass buffers, which also means nothing is a terminal,
// so no prompts, colours or banners get in the way.
type IOStreams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// OSStreams are the process's standard input, output and error.
func OSStreams() IOStreams { return IOStreams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr} }

// NewRootCommand builds the command tree for one invocation of build b.
// args are the arguments after the program name: the generated workspace
// commands depend on the host they name, so they are needed before Execute
// parses them.
func NewRootCommand(b Build, args []string, streams IOStreams) *cobra.Command {
	return newRootCommand(invokedName(), b, args, streams, true)
}

// NewReferenceCommand is the command tree the completion scripts and man
// pages in the release archives are made from (see internal/gendocs): the
// CLI's own commands, named firmfact whatever the program building them is
// called. The workspace commands are left out. They come from the server,
// and the tool list the machine building a release happens to have cached
// has no place in the release.
func NewReferenceCommand(version string) *cobra.Command {
	return newRootCommand("firmfact", Build{Version: version}, nil, IOStreams{}, false)
}

// Root help lists the CLI's own commands apart from the workspace commands,
// which come from the server and differ from host to host (see
// addToolCommands). A help topic such as environment has its own place,
// under "Additional help topics".
const (
	builtinGroup   = "builtin"
	workspaceGroup = "workspace"
)

// newRootCommand is NewRootCommand for a program called name, with the
// workspace commands of the host args aim at when withTools is set.
func newRootCommand(name string, b Build, args []string, streams IOStreams, withTools bool) *cobra.Command {
	api.Version, api.Commit = b.Version, b.Commit
	if streams.In == nil {
		streams.In = strings.NewReader("")
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	// cobra falls back to os.Args for nil args, which in a test binary are
	// the test flags.
	if args == nil {
		args = []string{}
	}
	app := &App{Name: name, Out: streams.Out, Err: streams.Err, In: streams.In, build: b}
	api.Name = app.Name
	config.SetWarningOutput(streams.Err)

	root := &cobra.Command{
		Use:           app.Name,
		Short:         "Firmfact from the command line",
		Long:          "Sign up, sign in and query your firmfact workspaces from the command line.",
		Version:       b.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			config.SetKeyringPatience(cmd.Context(), app.keyringPatience())
			// cobra checks these too, but only after this hook, and with
			// errors that do not say the command line is at fault.
			if err := cmd.ValidateRequiredFlags(); err != nil {
				return withExit(ExitUsage, err)
			}
			if err := cmd.ValidateFlagGroups(); err != nil {
				return withExit(ExitUsage, err)
			}
			if err := app.settleFormat(cmd); err != nil {
				return err
			}
			if err := app.requireConfig(cmd); err != nil {
				return err
			}
			if err := app.requireProfile(cmd); err != nil {
				return err
			}
			return checkVersion(app, cmd)
		},
	}
	root.SetArgs(args)
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.PersistentFlags().StringVar(&app.ProfileName, "profile", "", "profile to use, one that exists (default: FIRMFACT_PROFILE, else the current profile)")
	_ = root.RegisterFlagCompletionFunc("profile", app.completeProfileFlag) // the flag was just defined
	root.PersistentFlags().StringVar(&app.HostFlag, "host", "", "Firmfact host (default: FIRMFACT_HOST, else the profile's, else https://firmfact.com); localhost:<port> for local development")
	root.PersistentFlags().BoolVar(&app.InsecureHTTP, "insecure-http", false, "allow plain http to a host other than this machine (your token travels unencrypted)")
	root.PersistentFlags().Var(timeoutFlag{&app.Timeout}, "timeout", "limit for each request, such as 90s (default: 60s, 90s for workspace commands and 5m for a chat, and 30s for the server to start answering; or set FIRMFACT_TIMEOUT)")
	root.PersistentFlags().BoolVar(&app.Debug, "debug", false, "log each request and answer on stderr, with secrets redacted (or set FIRMFACT_DEBUG=1)")
	root.PersistentFlags().StringVar(&app.Workspace, "workspace", "", "workspace id or name (default: FIRMFACT_WORKSPACE, else the profile's workspace)")
	_ = root.RegisterFlagCompletionFunc("workspace", app.completeWorkspaceFlag)
	root.PersistentFlags().BoolVar(&app.JSONOutput, "json", false, "print machine-readable JSON (the same as --format json)")
	root.PersistentFlags().Var(formatFlag{&app.Format}, "format", "output format: table, json, csv or tsv (default: table; csv and tsv print a workspace command's rows)")
	_ = root.RegisterFlagCompletionFunc("format", completeFormatFlag)
	root.PersistentFlags().Var(jqFlag{&app.jq}, "jq", "filter the JSON output with a jq expression, such as '.data[].name'; implies --json")
	_ = root.RegisterFlagCompletionFunc("jq", cobra.NoFileCompletions)

	// A config file that cannot be used stops every command that needs it
	// (see requireConfig); the defaults stand in until then. Without a
	// config directory at all (no home directory, say) there is no file to
	// protect, and commands run on the defaults, as a CI job that passes
	// FIRMFACT_TOKEN and FIRMFACT_HOST can.
	cfg, err := config.Load()
	var fileErr *config.FileError
	switch {
	case errors.As(err, &fileErr):
		app.configErr = err
		cfg = config.Placeholder(err)
	case err != nil:
		fmt.Fprintf(app.Err, "warning: %v; using defaults\n", err)
		cfg = &config.Config{CurrentProfile: config.DefaultProfile, Profiles: map[string]*config.Profile{}}
	}
	app.Config = cfg

	doctor := newDoctorCommand(app)
	root.AddCommand(
		newSignupCommand(app),
		newLoginCommand(app),
		newLogoutCommand(app),
		newWhoamiCommand(app),
		newWorkspacesCommand(app),
		newToolsCommand(app),
		newCallCommand(app),
		newAskCommand(app),
		newUploadCommand(app),
		newConfigCommand(app),
		newUpdateCommand(app),
		newVersionCommand(app),
		newOpenCommand(app),
		newClaimCommand(app),
		doctor,
		newEnvironmentTopic(app.Name),
	)
	root.AddGroup(&cobra.Group{ID: builtinGroup, Title: "Commands:"})
	for _, c := range root.Commands() {
		c.GroupID = builtinGroup
	}
	root.SetHelpCommandGroupID(builtinGroup)
	root.SetCompletionCommandGroupID(builtinGroup)
	app.builtins = commandNames(root)
	if withTools {
		addToolCommands(root, app, args)
	}
	pointAtDoctor(root, doctor, app.Name)
	guardCommandLine(root)
	// `--profile stagin vendors list` names a profile that does not exist,
	// so no workspace commands were added (see addToolCommands); that is
	// the mistake to report, not an unknown command that sends the user
	// off to sign in.
	unknownCommand := root.Args
	root.Args = func(c *cobra.Command, args []string) error {
		if len(args) > 0 {
			if err := app.checkProfile(); err != nil {
				return err
			}
		}
		return unknownCommand(c, args)
	}
	return root
}
