package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// groupAnnotation marks a command that only holds others, such as config
// or vendors. Run on its own it shows its help, which needs no version
// check.
const groupAnnotation = "firmfact/group"

// guardCommandLine makes every mistake in a command line end with
// ExitUsage. cobra's own refusals are plain errors, and a group without a
// Run of its own takes any argument, prints its help and succeeds: `vendors
// lst` and `config sho` passed silently in CI. So each group refuses an
// unknown subcommand, with suggestions, and the refusals of arguments and
// flags are marked as usage errors. It runs once the tree is complete,
// cobra's help and completion commands included.
func guardCommandLine(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		// A flag that a group does not know most likely belongs to a
		// subcommand that is not there: `vendors list --limit 2` after a
		// profile that does not exist, or on a host whose tool list is
		// missing or of an older format, and `vendors lst --limit 2`. The
		// group's own check of what follows it says what is wrong, as it
		// does without the flag, where cobra would only name the flag.
		if c.Annotations[groupAnnotation] != "" && c.Args != nil {
			if argErr := c.Args(c, c.Flags().Args()); argErr != nil {
				return argErr
			}
		}
		return withExit(ExitUsage, err)
	})
	// A group is runnable now, only to refuse what follows it; its usage
	// line should not suggest that it runs on its own. A command that runs
	// and has commands below it, such as upload, keeps its line.
	root.SetUsageTemplate(strings.Replace(root.UsageTemplate(), "{{if .Runnable}}", `{{if and .Runnable (not (index .Annotations "`+groupAnnotation+`"))}}`, 1))

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		switch {
		case c.Name() == "help" && c.HasParent() && c.Run != nil: // cobra's; commands of ours use RunE
			guardHelp(c)
		case c.HasSubCommands() && c.Run == nil && c.RunE == nil:
			c.Args = unknownSubcommand
			c.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[groupAnnotation] = "true"
		case c.Args != nil:
			check := c.Args
			c.Args = func(c *cobra.Command, args []string) error { return withExit(ExitUsage, check(c, args)) }
		}
	}
	walk(root)
}

// unknownSubcommand is a group's argument check: anything after the group
// is a subcommand it does not have.
func unknownSubcommand(c *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	path := c.CommandPath()
	if c.SuggestionsMinimumDistance <= 0 {
		c.SuggestionsMinimumDistance = 2 // cobra's own default
	}
	if names := c.SuggestionsFor(args[0]); len(names) > 0 {
		for i, name := range names {
			names[i] = "`" + path + " " + name + "`"
		}
		return usageErrorf("unknown command %q for %q; did you mean %s?", args[0], path, orList(names))
	}
	switch {
	case !c.HasParent() && c.Annotations[noToolsAnnotation] != "":
		// No workspace commands at all for this host: on a new machine,
		// or in a CI job that signs in with FIRMFACT_TOKEN, `vendors list`
		// is not a typo.
		return usageErrorf("unknown command %q for %q; workspace commands appear after `%s login`; in scripts run `%s tools refresh` first or use `%s call <tool>`", args[0], path, path, path, path)
	case !c.HasParent():
		// The workspace commands come from the server; one that is not
		// here yet may only need the list fetched.
		return usageErrorf("unknown command %q for %q; see `%s --help`, or run `%s tools refresh` if it is a workspace command", args[0], path, path, path)
	}
	return usageErrorf("unknown command %q for %q; see `%s --help`", args[0], path, path)
}

// guardHelp makes `help <typo>` refuse the typo the way the command itself
// would, instead of showing some other command's help and succeeding.
func guardHelp(help *cobra.Command) {
	show := help.Run
	help.Run = nil
	help.RunE = func(c *cobra.Command, args []string) error {
		target, rest, err := c.Root().Find(args)
		if err == nil && len(rest) > 0 && target.Annotations[groupAnnotation] != "" {
			return unknownSubcommand(target, rest)
		}
		show(c, args)
		return nil
	}
}

// orList joins words as "a", "a or b", "a, b or c".
func orList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}
