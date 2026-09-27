package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// A tool's annotations say what calling it does to the workspace. Every
// listed tool becomes a command that runs as soon as it is typed, so a tool
// that deletes or overwrites data would otherwise go live, unasked, on
// every installed CLI the day the server adds it. Help and `tools list`
// mark the tools that write, and one that may delete or overwrite data
// (see mcp.Tool.Destructive) runs only once the user says so: at a prompt,
// or with --yes. A hint the server leaves out takes MCP's default, which is
// the cautious one: the tool writes, and destructively.

// writesMarker is how help and `tools list` mark a tool that is not
// read-only, such as chat_with_workspace, which saves its thread.
const writesMarker = "writes to your workspace"

// toolEffect is what help and `tools list` say a tool does to the
// workspace; empty for a tool that only reads.
func toolEffect(t mcp.Tool) string {
	switch {
	case t.ReadOnly():
		return ""
	case t.Destructive():
		return writesMarker + ", may delete or overwrite data"
	}
	return writesMarker
}

// markedTitle is s, a tool's title or other short text about it, with its
// effect after it in brackets.
func markedTitle(s string, t mcp.Tool) string {
	effect := toolEffect(t)
	switch {
	case effect == "":
		return s
	case s == "":
		return "(" + effect + ")"
	}
	return s + " (" + effect + ")"
}

// effectHelp is the paragraph a tool command's help ends with; empty for a
// tool that only reads. askFlag is how the command goes ahead without the
// question, such as "--yes".
func effectHelp(t mcp.Tool, askFlag string) string {
	switch {
	case t.ReadOnly():
		return ""
	case t.Destructive():
		return "This command writes to your workspace and may delete or overwrite data, so it asks before it runs. " +
			askFlag + " goes ahead without asking; off a terminal, where there is no one to ask, it is needed."
	}
	return "This command writes to your workspace."
}

// yesFlagUsage is the help of --yes on a command whose tool may delete or
// overwrite data. On any other it is hidden, and changes nothing.
const yesFlagUsage = "go ahead without asking first; this command may delete or overwrite data"

// addYesFlag gives a tool command --yes (-y). Every one gets it, so a
// script that passes it keeps working when the server changes a tool's
// hints; help shows it only where the tool may delete or overwrite data.
// A tool with an argument named yes keeps --yes for that, and its command
// has no way to skip the question: nil, and `call` runs it instead.
func addYesFlag(cmd *cobra.Command, tool mcp.Tool) *bool {
	if cmd.Flags().Lookup("yes") != nil {
		return nil
	}
	yes := cmd.Flags().BoolP("yes", "y", false, yesFlagUsage)
	if !tool.Destructive() {
		_ = cmd.Flags().MarkHidden("yes")
	}
	return yes
}

// confirmTool reports whether tool may run: yes when it only reads or only
// adds, when yes (--yes) is set, or when the user answers yes on a
// terminal; a no says nothing changed. Off a terminal there is no one to
// ask, so the command fails and says how to go ahead: a script has to say
// so itself. what is the command as the user typed it, askFlag how to skip
// the question, and args the call's arguments, for the workspace it runs
// in.
func confirmTool(ctx context.Context, app *App, tool mcp.Tool, yes bool, what, askFlag string, args map[string]any) (bool, error) {
	if yes || !tool.Destructive() {
		return true, nil
	}
	where := "your workspace"
	if ws, ok := args["workspace"].(string); ok && ws != "" {
		where = "workspace " + nameInText(ui.SafeLine(ws))
	}
	if !app.Interactive() {
		return false, usageErrorf("`%s` may delete or overwrite data in %s, and there is no terminal to ask on; use %s to go ahead", what, where, askFlag)
	}
	ok, err := app.Confirm(ctx, fmt.Sprintf("`%s` may delete or overwrite data in %s. Go ahead?", what, where))
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(app.Out, "Nothing changed.")
	}
	return ok, nil
}
