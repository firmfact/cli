package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/firmfact/cli/internal/mcp"
)

// A tool's name is the server's to choose, and it becomes words on the
// user's command line and in help. So only a plain name becomes a command,
// and never one that would take the place of the CLI's own: a cached tool
// named help used to take over `firmfact help`. A tool that gets no command
// stays callable with `call`, and `tools list` says why it has none.

// toolNamePattern is the names that may become commands: lower-case words
// joined by underscores, no longer than any real tool's.
var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// reservedWords are names no tool command may have at any level. help and
// completion are cobra's commands, added after the tool commands; version
// is what people type to ask which CLI they have.
var reservedWords = map[string]bool{"help": true, "completion": true, "version": true}

// Why a tool has no command, as `tools list --json` names it.
const (
	skipInvalid  = "invalid_name" // not a plain name
	skipReserved = "reserved"     // the CLI's own command, or a reserved word
	skipTaken    = "taken"        // another tool's command, or its group
)

// toolCommand is where a tool's command goes, or why it has none.
type toolCommand struct {
	tool  mcp.Tool
	group string // empty for a command directly under the root
	verb  string
	// skipped is one of the skip reasons; empty when the tool has a command.
	skipped string
	// clash is the command a reserved or taken tool would have needed,
	// such as "help" or "config", for `tools list` to name.
	clash string
}

// path is the command's words after the CLI's name.
func (p toolCommand) path() string { return strings.TrimSpace(p.group + " " + p.verb) }

// commandWord reports whether w can be a command's name: cobra would read
// one that starts with a hyphen as a flag (list__x makes "-x").
func commandWord(w string) bool { return w != "" && !strings.HasPrefix(w, "-") }

// planToolCommands decides each tool's command, in the order of tools.
// builtins are the names of the CLI's own top-level commands. When two
// tools want the same command, the first by name gets it, and a group
// (vendors, analyze) wins over a lone command of the same name; the order
// the server lists its tools in decides nothing.
func planToolCommands(tools []mcp.Tool, builtins map[string]bool) []toolCommand {
	reserved := func(w string) bool { return builtins[w] || reservedWords[w] }
	plans := make([]toolCommand, len(tools))
	groups := map[string]bool{}
	for i, t := range tools {
		p := toolCommand{tool: t}
		p.group, p.verb = commandPath(t.Name)
		grouped := strings.HasPrefix(t.Name, "list_") || strings.HasPrefix(t.Name, "analyze_")
		switch {
		case !toolNamePattern.MatchString(t.Name) || !commandWord(p.verb) || grouped && !commandWord(p.group):
			p.group, p.verb, p.skipped = "", "", skipInvalid
		case p.group != "" && reserved(p.group):
			p.skipped, p.clash = skipReserved, p.group
		case p.group == "" && reserved(p.verb), reservedWords[p.verb]:
			p.skipped, p.clash = skipReserved, p.path()
		case p.group != "":
			groups[p.group] = true
		}
		plans[i] = p
	}

	order := make([]int, len(plans))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return plans[order[a]].tool.Name < plans[order[b]].tool.Name })
	taken := map[string]bool{}
	for _, i := range order {
		p := &plans[i]
		if p.skipped != "" {
			continue
		}
		path := p.path()
		if taken[path] || p.group == "" && groups[p.verb] {
			p.skipped, p.clash = skipTaken, path
			continue
		}
		taken[path] = true
	}
	return plans
}

// skipReason says in words why p has no command.
func skipReason(p toolCommand, name string) string {
	switch p.skipped {
	case skipInvalid:
		return "its name cannot be a command name"
	case skipReserved:
		return fmt.Sprintf("`%s %s` is reserved for the CLI", name, p.clash)
	case skipTaken:
		return fmt.Sprintf("`%s %s` is taken by another tool", name, p.clash)
	}
	return ""
}

// listedTool is a tool as `tools list --json` prints it: the server's
// description of it, with the command it has or why it has none.
type listedTool struct {
	mcp.Tool
	// Command is the command's words after the CLI's name, such as
	// "vendors list"; empty when the tool has none.
	Command string `json:"command,omitempty"`
	// Skipped is why the tool has no command: invalid_name, reserved or
	// taken.
	Skipped string `json:"skipped,omitempty"`
	// Writes and Destructive are what the CLI makes of the tool's
	// annotations, with MCP's defaults for a hint left out: whether it may
	// change the workspace, and whether it may delete or overwrite data, so
	// that its command asks first or needs --yes.
	Writes      bool `json:"writes"`
	Destructive bool `json:"destructive"`
}

func listedTools(plans []toolCommand) []listedTool {
	out := make([]listedTool, len(plans))
	for i, p := range plans {
		out[i] = listedTool{Tool: p.tool, Skipped: p.skipped, Writes: !p.tool.ReadOnly(), Destructive: p.tool.Destructive()}
		if p.skipped == "" {
			out[i].Command = p.path()
		}
	}
	return out
}

// commandCount is how many of plans have a command.
func commandCount(plans []toolCommand) int {
	n := 0
	for _, p := range plans {
		if p.skipped == "" {
			n++
		}
	}
	return n
}
