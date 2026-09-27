package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/claim"
)

func newClaimCommand(app *App) *cobra.Command {
	var keepAs, shell string
	var yes, undo bool
	cmd := &cobra.Command{
		Use:   "claim [name]",
		Short: "Make a short name such as ff run this CLI",
		Long: fmt.Sprintf(`Make a short command name (ff by default) run firmfact.

Adds ~/.local/bin/<name> pointing at this binary (on Windows a <name>.cmd shim).
If your shell already defines <name> as an alias or function, as Omarchy does
with ff for fzf, a clearly marked block at the end of your shell's rc file keeps
that definition under another name (--keep-as) and lets firmfact have <name>.

Nothing changes until you confirm, and "%[1]s claim <name> --undo" removes
exactly what was added.`, app.Name),
		Example: fmt.Sprintf(`  %[1]s claim            # claim ff
  %[1]s claim ff --keep-as fzff
  %[1]s claim ff --undo`, app.Name),
		Args: cobra.MaximumNArgs(1),
		// It edits this machine's shell files, whatever the profile.
		Annotations: map[string]string{noProfileAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "ff"
			if len(args) == 1 {
				name = args[0]
			}
			env, err := claim.DefaultEnv()
			if err != nil {
				return err
			}
			if shell != "" {
				env.Shell = shell
			}

			if undo {
				done, err := claim.Undo(env, name)
				for _, d := range done {
					fmt.Fprintln(app.Out, "  "+d)
				}
				if err != nil {
					return err
				}
				if len(done) == 0 {
					fmt.Fprintf(app.Out, "Nothing to undo for %s.\n", name)
				} else {
					fmt.Fprintf(app.Out, "Done. Open a new terminal for %s to go back to what it was.\n", name)
				}
				return nil
			}

			plan, err := claim.Prepare(env, name, keepAs)
			if err != nil {
				return err
			}
			if plan.NothingToDo() {
				fmt.Fprintf(app.Out, "%s already runs firmfact.\n", name)
				return nil
			}

			m := app.Mode()
			fmt.Fprintf(app.Out, "To make %s run firmfact:\n\n", m.Orange(name))
			if plan.Existing != "" {
				fmt.Fprintf(app.Out, "  Now: %s.\n\n", plan.Existing)
			}
			step := 0
			if !plan.LinkExists {
				step++
				if plan.LinkBody != "" {
					fmt.Fprintf(app.Out, "  %d. create %s\n", step, plan.LinkPath)
				} else {
					fmt.Fprintf(app.Out, "  %d. link %s -> %s\n", step, plan.LinkPath, env.Self)
				}
			}
			if plan.RCFile != "" {
				step++
				if plan.RCManual != "" {
					fmt.Fprintf(app.Out, "  %d. add this block to the end of %s yourself; firmfact leaves that file alone because %s:\n\n", step, plan.RCFile, plan.RCManual)
				} else {
					fmt.Fprintf(app.Out, "  %d. add this block to the end of %s:\n\n", step, plan.RCFile)
				}
				for _, line := range strings.Split(strings.TrimRight(plan.RCBlock, "\n"), "\n") {
					fmt.Fprintln(app.Out, "     "+m.Dim(line))
				}
				if plan.RCManual == "" {
					fmt.Fprintln(app.Out, "\n     (a dated copy of the current file is kept next to it)")
				}
			}
			for _, note := range plan.Notes {
				fmt.Fprintln(app.Out, "\n  "+note)
			}
			fmt.Fprintln(app.Out)
			if plan.LinkExists && plan.RCManual != "" {
				// Only the step for the user is left; there is nothing to confirm.
				fmt.Fprintf(app.Out, "Nothing changed. Add the block above to %s, then open a new terminal and try: %s whoami\n", plan.RCFile, name)
				return nil
			}

			if !yes {
				if !app.Interactive() {
					return usageErrorf("nothing changed; run again with --yes to apply")
				}
				ok, err := app.Confirm(cmd.Context(), "Go ahead?")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(app.Out, "Nothing changed.")
					return nil
				}
			}
			if err := claim.Apply(env, plan); err != nil {
				return err
			}
			if plan.RCManual != "" {
				fmt.Fprintf(app.Out, "%s now links to firmfact. Add the block above to the end of %s yourself, then open a new terminal and try: %s whoami\n", plan.LinkPath, plan.RCFile, name)
				return nil
			}
			fmt.Fprintf(app.Out, "%s Open a new terminal (or run: source %s), then try: %s whoami\n",
				m.Rainbow(name+" is yours."), orDefault(plan.RCFile, "~/.bashrc"), name)
			if plan.Backup != "" {
				fmt.Fprintf(app.Out, "The previous %s is saved as %s.\n", plan.RCFile, plan.Backup)
			}
			// Only a block that took the name over kept a previous one; a
			// free name had none.
			if plan.TakesOver {
				fmt.Fprintf(app.Out, "%s on its own still runs your previous %s (also available as %s). ", name, name, plan.KeepAs)
			}
			fmt.Fprintf(app.Out, "Undo everything with: %s claim %s --undo\n", app.Name, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&keepAs, "keep-as", "", "name for your existing alias or function (default <name>-previous)")
	cmd.Flags().StringVar(&shell, "shell", "", "shell to configure: bash, zsh or fish (default: from $SHELL)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "apply without asking")
	cmd.Flags().BoolVar(&undo, "undo", false, "remove the link and the shell block")
	return cmd
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
