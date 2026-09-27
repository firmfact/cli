package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/ui"
)

// newConfigCommand is `firmfact config`: the profiles, each a host and a
// default workspace, and which of them commands use.
func newConfigCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Show or change CLI settings and profiles"}
	cmd.AddCommand(
		newConfigShowCommand(app),
		newConfigGetCommand(app),
		newConfigSetCommand(app),
		newConfigUnsetCommand(app),
		newSetHostCommand(app),
		newUseProfileCommand(app),
		newProfilesCommand(app),
		newConfigResetCommand(app),
	)
	return cmd
}

// profileJSON is a profile as the config commands print it with --json.
type profileJSON struct {
	Profile   string `json:"profile"`
	Host      string `json:"host"`
	Workspace string `json:"workspace"`
}

func newConfigShowCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the active profile",
		Long: "Show the profile, host and workspace commands use: each from its flag, else\n" +
			"its environment variable, else the profile (see `" + app.Name + " help environment`).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, err := app.Host()
			if err != nil {
				return err
			}
			workspace := app.DefaultWorkspace()
			if app.JSONOutput {
				return app.PrintJSON(profileJSON{app.profileName(), host, workspace})
			}
			// A variable set long ago in a shell profile is easy to forget,
			// so a setting it supplies says so.
			fmt.Fprintf(app.Out, "profile:   %s%s\nhost:      %s%s\nworkspace: %s%s\n",
				ui.SafeLine(app.profileName()), fromEnvNote(app.ProfileName, envProfile),
				host, fromEnvNote(app.HostFlag, envHost),
				ui.SafeLine(workspace), fromEnvNote(app.Workspace, envWorkspace))
			return nil
		},
	}
}

func newSetHostCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "set-host <host>",
		Short: "Point the active profile at another host",
		Long: "Point the active profile at another host. It must use https, except on this\n" +
			"machine (localhost:<port> means plain http); add --insecure-http to allow plain\n" +
			"http to another host, which sends your token unencrypted. The same as\n" +
			"`" + app.Name + " config set host <host>`.",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{makesProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.setHost(cmd, args[0])
		},
	}
}

// setHost points the profile in use at value, as --insecure-http allows,
// and says so. The default workspace belongs to the old host, so it goes
// when the host changes.
func (a *App) setHost(cmd *cobra.Command, value string) error {
	parse := config.ParseHost
	if a.InsecureHTTP {
		parse = config.ParseHostAllowHTTP
	}
	host, err := parse(value)
	if err != nil {
		return withExit(ExitUsage, err)
	}
	p, err := a.profileToChange(cmd)
	if err != nil {
		return err
	}
	if old, err := p.ParsedHost(true); err != nil || old != host {
		p.Workspace = ""
	}
	rememberHost(p, host)
	if err := a.saveConfig(); err != nil {
		return err
	}
	if a.JSONOutput {
		if err := a.PrintJSON(profileJSON{a.profileName(), host, p.Workspace}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.Out, "Profile now uses %s. Run `%s login` to sign in there.\n", host, a.Name)
	}
	if config.NeedsInsecureHTTP(host) {
		fmt.Fprintln(a.Err, "warning: this host is plain http, so your token will travel unencrypted.")
	}
	a.envOverride(envHost, "host")
	return nil
}

// profileToChange is the profile in use, for cmd to change: every command
// that saves a setting of it, a sign-in's host and workspace among them,
// gets it here. It must exist, as for any command (see requireProfile),
// unless cmd is one that makes it (makesProfileAnnotation): config set and
// set-host make the profile --profile or FIRMFACT_PROFILE names, so that
// `--profile ci config set host <host>` sets one up in one go. A name for
// a new profile must pass CheckProfileName.
func (a *App) profileToChange(cmd *cobra.Command) (*config.Profile, error) {
	name := a.profileName()
	if _, ok := a.Config.Lookup(name); !ok {
		if cmd.Annotations[makesProfileAnnotation] == "" {
			return nil, a.unknownProfile(a.ProfileName)
		}
		if err := config.CheckProfileName(name); err != nil {
			return nil, withExit(ExitUsage, err)
		}
	}
	return a.Config.Profile(name), nil
}

func newUseProfileCommand(app *App) *cobra.Command {
	var create bool
	cmd := &cobra.Command{
		Use:   "use-profile <name>",
		Short: "Switch the current profile (--create makes a new one)",
		Long: "Switch the current profile, which commands use unless --profile or\n" +
			"FIRMFACT_PROFILE names another. The profile must exist, so that a typo cannot\n" +
			"switch to a new profile on the default host; add --create to make a new one,\n" +
			"then point it at its host with `" + app.Name + " config set host <host>`.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeProfiles(1),
		Annotations:       map[string]string{noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			_, exists := app.Config.Lookup(name)
			if !exists {
				if !create {
					return app.noProfile(name, true)
				}
				if err := config.CheckProfileName(name); err != nil {
					return withExit(ExitUsage, err)
				}
			}
			app.Config.CurrentProfile = name
			p := app.Config.Profile(name)
			if err := app.saveConfig(); err != nil {
				return err
			}
			if app.JSONOutput {
				err := app.PrintJSON(profileJSON{name, p.Host, p.Workspace})
				app.profileEnvNote()
				return err
			}
			if !exists {
				fmt.Fprintf(app.Out, "Created profile %s, on %s; point it elsewhere with `%s config set host <host>`.\n", name, p.Host, app.Name)
			}
			fmt.Fprintf(app.Out, "Current profile is now %s.\n", ui.SafeLine(name))
			app.profileEnvNote()
			return nil
		},
	}
	cmd.Flags().BoolVar(&create, "create", false, "make the profile, on the default host, if there is none by that name")
	return cmd
}

// configKey is a setting of a profile that get, set and unset know.
type configKey struct {
	name string
	// env is the variable that overrides the setting.
	env string
	get func(*config.Profile) string
}

var configKeys = []configKey{
	{"host", envHost, func(p *config.Profile) string { return p.Host }},
	{"workspace", envWorkspace, func(p *config.Profile) string { return p.Workspace }},
}

func lookupConfigKey(name string) (configKey, error) {
	names := make([]string, len(configKeys))
	for i, k := range configKeys {
		if k.name == name {
			return k, nil
		}
		names[i] = k.name
	}
	return configKey{}, usageErrorf("no setting %q; the settings are %s and %s", name, strings.Join(names[:len(names)-1], ", "), names[len(names)-1])
}

func configKeyNames() []string {
	names := make([]string, len(configKeys))
	for i, k := range configKeys {
		names[i] = k.name
	}
	return names
}

// configValueJSON is what config get prints with --json.
type configValueJSON struct {
	Profile string `json:"profile"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

const configKeysHelp = "The settings are host, the firmfact host (as `config set-host` takes it), and\n" +
	"workspace, the default workspace for commands that take one. They belong to\n" +
	"the profile in use: --profile, else FIRMFACT_PROFILE, else the current one."

func newConfigGetCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:       "get <key>",
		Short:     "Print one setting of the profile in use",
		Long:      "Print one setting as the profile stores it; `config show` shows what\ncommands use once flags and variables are applied.\n\n" + configKeysHelp,
		Args:      cobra.ExactArgs(1),
		ValidArgs: configKeyNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := lookupConfigKey(args[0])
			if err != nil {
				return err
			}
			name := app.profileName()
			p, ok := app.Config.Lookup(name)
			if !ok {
				return app.noProfile(name, false)
			}
			value := key.get(p)
			if app.JSONOutput {
				err = app.PrintJSON(configValueJSON{name, key.name, value})
			} else if value != "" {
				fmt.Fprintln(app.Out, ui.SafeLine(value))
			}
			app.envOverride(key.env, key.name)
			return err
		},
	}
}

func newConfigSetCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change one setting of the profile in use",
		Long: "Change one setting of the profile in use, making the profile if --profile or\n" +
			"FIRMFACT_PROFILE names one that does not exist yet. A host is checked as\n" +
			"`config set-host` checks it, and changing it clears the default workspace,\n" +
			"which belongs to the old host. A workspace is not checked against the server;\n" +
			"`" + app.Name + " workspaces use` does that.\n\n" + configKeysHelp,
		Args:        cobra.ExactArgs(2),
		Annotations: map[string]string{makesProfileAnnotation: "true"},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch {
			case len(args) == 0:
				return configKeyNames(), cobra.ShellCompDirectiveNoFileComp
			case len(args) == 1 && args[0] == "workspace":
				return app.completeWorkspaceFlag(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := lookupConfigKey(args[0])
			if err != nil {
				return err
			}
			if key.name == "host" {
				return app.setHost(cmd, args[1])
			}
			value := strings.TrimSpace(args[1])
			switch {
			case value == "":
				return usageErrorf("no workspace given; `%s config unset workspace` clears it", app.Name)
			case strings.IndexFunc(value, unicode.IsControl) >= 0:
				return usageErrorf("a workspace cannot contain control characters")
			}
			p, err := app.profileToChange(cmd)
			if err != nil {
				return err
			}
			p.Workspace = value
			if err := app.saveConfig(); err != nil {
				return err
			}
			if app.JSONOutput {
				err = app.PrintJSON(profileJSON{app.profileName(), p.Host, p.Workspace})
			} else {
				fmt.Fprintf(app.Out, "Default workspace for profile %s is now %s.\n", ui.SafeLine(app.profileName()), ui.SafeLine(value))
			}
			app.envOverride(envWorkspace, "workspace")
			return err
		},
	}
}

func newConfigUnsetCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "Put one setting of the profile in use back to its default",
		Long: "Put one setting of the profile in use back to its default: the host to\n" +
			config.DefaultHost + ", which clears the default workspace too, and the\n" +
			"workspace to none.\n\n" + configKeysHelp,
		Args:      cobra.ExactArgs(1),
		ValidArgs: configKeyNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := lookupConfigKey(args[0])
			if err != nil {
				return err
			}
			name := app.profileName()
			p, err := app.profileToChange(cmd)
			if err != nil {
				return err
			}
			switch key.name {
			case "host":
				if p.Host != config.DefaultHost {
					p.Workspace = ""
				}
				p.Host, p.InsecureHTTP = config.DefaultHost, false
			case "workspace":
				p.Workspace = ""
			}
			if err := app.saveConfig(); err != nil {
				return err
			}
			if app.JSONOutput {
				err = app.PrintJSON(profileJSON{name, p.Host, p.Workspace})
			} else if key.name == "host" {
				fmt.Fprintf(app.Out, "Profile %s now uses %s.\n", ui.SafeLine(name), p.Host)
			} else {
				fmt.Fprintf(app.Out, "Profile %s has no default workspace now.\n", ui.SafeLine(name))
			}
			app.envOverride(key.env, key.name)
			return err
		},
	}
}

// profileEntry is one row of `config profiles list --json`.
type profileEntry struct {
	Profile   string `json:"profile"`
	Host      string `json:"host"`
	Workspace string `json:"workspace"`
	// Current is the profile use-profile chose, which commands use unless
	// --profile or FIRMFACT_PROFILE names another.
	Current bool `json:"current"`
}

func newProfilesCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "profiles",
		Aliases: []string{"profile"},
		Short:   "List, rename or delete profiles",
		Long: "List, rename or delete profiles. A profile is a host and a default workspace;\n" +
			"`config use-profile --create <name>` makes one. Sign-ins are kept by host, so\n" +
			"renaming or deleting a profile does not sign you out.",
	}
	list := &cobra.Command{
		Use:         "list",
		Short:       "List the profiles; * marks the current one",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var entries []profileEntry
			for _, name := range app.Config.Names() {
				p := app.Config.Peek(name)
				entries = append(entries, profileEntry{name, p.Host, p.Workspace, name == app.Config.CurrentProfile})
			}
			if app.JSONOutput {
				err := app.PrintJSON(entries)
				app.profileEnvNote()
				return err
			}
			tw := tabwriter.NewWriter(app.Out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "\tNAME\tHOST\tWORKSPACE")
			for _, e := range entries {
				marker := ""
				if e.Current {
					marker = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, ui.SafeLine(e.Profile), ui.SafeLine(e.Host), ui.SafeLine(e.Workspace))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			app.profileEnvNote()
			return nil
		},
	}
	del := &cobra.Command{
		Use:               "delete <name>",
		Aliases:           []string{"remove", "rm"},
		Short:             "Delete a profile",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeProfiles(1),
		Annotations:       map[string]string{noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			p, ok := app.Config.Lookup(name)
			switch {
			case !ok:
				return app.noProfile(name, false)
			case name == app.Config.CurrentProfile:
				return fmt.Errorf("%s is the current profile; switch to another first with `%s config use-profile <name>`", name, app.Name)
			case name == config.DefaultProfile:
				return fmt.Errorf("the default profile is always there; `%s --profile %s config unset host` puts it back to %s", app.Name, name, config.DefaultHost)
			}
			app.Config.Delete(name)
			if err := app.saveConfig(); err != nil {
				return err
			}
			if app.JSONOutput {
				err := app.PrintJSON(profileJSON{name, p.Host, p.Workspace})
				app.staleProfileNote(name, "")
				return err
			}
			fmt.Fprintf(app.Out, "Deleted profile %s, which used %s.\n", ui.SafeLine(name), ui.SafeLine(p.Host))
			app.staleProfileNote(name, "")
			return nil
		},
	}
	rename := &cobra.Command{
		Use:               "rename <old> <new>",
		Short:             "Rename a profile, keeping its host and workspace",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: app.completeProfiles(1),
		Annotations:       map[string]string{noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			from, to := args[0], args[1]
			if _, ok := app.Config.Lookup(from); !ok {
				return app.noProfile(from, false)
			}
			if _, taken := app.Config.Lookup(to); taken {
				return fmt.Errorf("there is a profile %q already; delete it first, or choose another name", to)
			}
			if err := config.CheckProfileName(to); err != nil {
				return withExit(ExitUsage, err)
			}
			app.Config.Rename(from, to)
			if err := app.saveConfig(); err != nil {
				return err
			}
			p := app.Config.Peek(to)
			if app.JSONOutput {
				err := app.PrintJSON(profileJSON{to, p.Host, p.Workspace})
				app.staleProfileNote(from, to)
				return err
			}
			fmt.Fprintf(app.Out, "Renamed profile %s to %s.\n", ui.SafeLine(from), to)
			app.staleProfileNote(from, to)
			return nil
		},
	}
	cmd.AddCommand(list, rename, del)
	return cmd
}

// profileEnvNote is envOverride for FIRMFACT_PROFILE, after a command
// that lists or chooses profiles: the profile it names is the one commands
// use, unless there is no such profile, when they stop at it instead.
func (a *App) profileEnvNote() {
	name := fromEnv(envProfile)
	if name == "" {
		return
	}
	if _, ok := a.Config.Lookup(name); !ok {
		fmt.Fprintf(a.Err, "note: FIRMFACT_PROFILE names %s, which is no profile, so commands will not run until it changes or is unset.\n", ui.SafeLine(name))
		return
	}
	a.envOverride(envProfile, "profile")
}

// staleProfileNote says that FIRMFACT_PROFILE still names a profile that
// was deleted, or renamed to renamedTo, so commands stop at it until it
// changes.
func (a *App) staleProfileNote(gone, renamedTo string) {
	if fromEnv(envProfile) != gone {
		return
	}
	what := "which no longer exists"
	if renamedTo != "" {
		what = "which is now called " + renamedTo
	}
	fmt.Fprintf(a.Err, "note: FIRMFACT_PROFILE still names %s, %s, so commands will not run until it changes.\n", ui.SafeLine(gone), what)
}

// resetResult is what config reset prints with --json.
type resetResult struct {
	Path string `json:"path"`
	// MovedTo is where the file went; empty when there was none.
	MovedTo string `json:"moved_to"`
}

func newConfigResetCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Move the config file aside and start again from the defaults",
		Long: "Move config.json aside, to a copy named after the time, so that commands start\n" +
			"again from the default profile on " + config.DefaultHost + ". This is the way out\n" +
			"when the file cannot be read and you would rather not repair it by hand; the\n" +
			"copy keeps its profiles. Sign-ins are kept by host, outside this file, so\n" +
			"they stay.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{withoutConfigAnnotation: "true", noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			backup, err := config.Reset(time.Now())
			if err != nil {
				return err
			}
			if app.JSONOutput {
				return app.PrintJSON(resetResult{Path: path, MovedTo: backup})
			}
			if backup == "" {
				fmt.Fprintf(app.Out, "There is no config file at %s, so commands already use the defaults.\n", path)
				return nil
			}
			fmt.Fprintf(app.Out, "Moved %s to %s.\nCommands now use the default profile, on %s; your sign-ins are kept.\n",
				path, filepath.Base(backup), config.DefaultHost)
			return nil
		},
	}
}

// Commands run on the profile --profile or FIRMFACT_PROFILE names, else on
// the current one, and that profile must exist. Read as a new one, a name
// with no profile would mean the default host, production, and a typo
// would run there with the sign-in stored for it. These annotations mark
// the commands that go ahead all the same.
const (
	// noProfileAnnotation marks a command that does not use the profile in
	// use: it names its own (config use-profile, config profiles) or needs
	// none (version, claim, config reset).
	noProfileAnnotation = "firmfact/no-profile"
	// makesProfileAnnotation marks a command that makes the profile in use
	// when it does not exist yet (see profileToChange).
	makesProfileAnnotation = "firmfact/makes-profile"
)

// requireProfile stops cmd when the profile in use does not exist, before
// it can reach the default host. Help, completion and the commands
// annotated above run all the same.
func (a *App) requireProfile(cmd *cobra.Command) error {
	if cmd.Annotations[noProfileAnnotation] != "" || cmd.Annotations[makesProfileAnnotation] != "" ||
		cmd.Annotations[groupAnnotation] != "" || helpOrCompletion(cmd) {
		return nil
	}
	return a.checkProfile()
}

// checkProfile is nil when the profile in use exists, and otherwise the
// error that says so. When the config file cannot be read, which profiles
// exist is not known, and requireConfig has had its say.
func (a *App) checkProfile() error {
	if a.configErr != nil {
		return nil
	}
	if _, ok := a.Config.Lookup(a.profileName()); ok {
		return nil
	}
	return a.unknownProfile(a.ProfileName)
}

// unknownProfile is the error for a profile in use that does not exist,
// named by profileFlag, else by FIRMFACT_PROFILE: the current profile
// always exists. It says which of the two named it, with the names meant.
func (a *App) unknownProfile(profileFlag string) error {
	source := "--profile"
	if profileFlag == "" {
		source = envProfile
	}
	return fmt.Errorf("%s: %w", source, a.noProfile(resolveProfile(a.Config, profileFlag), false))
}

// noProfile is the error for a name that no profile has. A typo is the
// likely cause, so the nearest names are suggested; create says the
// command can make the profile instead.
func (a *App) noProfile(name string, create bool) error {
	if name == "" {
		return usageErrorf("a profile needs a name")
	}
	msg := fmt.Sprintf("no profile %q", name)
	near := nearNames(name, a.Config.Names())
	switch {
	case len(near) > 0 && create:
		msg += "; did you mean " + orList(near) + "? Add --create to make a new profile"
	case len(near) > 0:
		msg += "; did you mean " + orList(near) + "?"
	case create:
		msg += fmt.Sprintf("; add --create to make a new one, or see `%s config profiles list`", a.Name)
	default:
		msg += fmt.Sprintf("; see `%s config profiles list`", a.Name)
	}
	return withExit(ExitNotFound, errors.New(msg))
}

// nearNames are the names within two edits of name, or starting with it,
// quoted: the rule cobra uses to suggest commands.
func nearNames(name string, names []string) []string {
	name = strings.ToLower(name)
	var near []string
	for _, n := range names {
		lower := strings.ToLower(n)
		if editDistance(name, lower) <= 2 || strings.HasPrefix(lower, name) {
			near = append(near, strconv.Quote(n))
		}
	}
	return near
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// completeProfiles completes the first n arguments with profile names.
func (a *App) completeProfiles(n int) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) >= n {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return a.Config.Names(), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeProfileFlag completes --profile with profile names.
func (a *App) completeProfileFlag(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return a.Config.Names(), cobra.ShellCompDirectiveNoFileComp
}

// configHealth is doctor's config check: the file can be used, the
// profile in use exists, the directory takes a write, and every profile's
// host passes the checks a command applies to it before sending a token
// there.
func (a *App) configHealth() (ok bool, detail string) {
	if a.configErr != nil {
		return false, a.configFileHint(a.configErr).Error()
	}
	if err := a.checkProfile(); err != nil {
		return false, err.Error()
	}
	dir, err := config.Dir()
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err == nil {
		// A file of its own, as the token and cache files are written: at a
		// fixed name, a symlink that someone who can write to the directory
		// put there would have the probe empty the file it points at.
		var probe *os.File
		if probe, err = os.CreateTemp(dir, ".doctor-*"); err == nil {
			_ = probe.Close()
			_ = os.Remove(probe.Name())
		}
	}
	if err != nil {
		return false, "cannot write to the config directory: " + err.Error()
	}
	names := a.Config.Names()
	var bad []string
	for _, name := range names {
		if _, err := a.Config.Peek(name).ParsedHost(false); err != nil {
			bad = append(bad, fmt.Sprintf("profile %q: %v", name, err))
		}
	}
	if len(bad) > 0 {
		return false, strings.Join(bad, "; ") + fmt.Sprintf("; fix with `%s --profile <name> config set host <host>`", a.Name)
	}
	path, err := config.Path()
	if err != nil {
		return false, err.Error()
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return true, dir + " (no config file yet, so the defaults apply)"
	}
	return true, fmt.Sprintf("%s: %d profile(s), current %s", path, len(names), a.Config.CurrentProfile)
}
