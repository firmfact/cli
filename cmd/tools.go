package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// The server's MCP tools become CLI commands: list_vendors is
// `firmfact vendors list`, analyze_cost_trends is `firmfact analyze cost-trends`,
// anything else keeps its name with hyphens (`firmfact chat-with-workspace`).
// Input schema properties become flags. Only tools with plain names that
// are not the CLI's own get a command (see toolnames.go). The tool list is
// cached per host so startup needs no network; login, signup and `firmfact
// tools refresh` update it, and so do workspace commands once it is out of
// date (see toolcache.go).
//
// Everything the server says that reaches the terminal (titles,
// descriptions, results, notes, errors) goes through ui.SafeText or
// ui.SafeLine first; see internal/ui/safe.go for why.

type toolCache struct {
	// Format is toolCacheFormat when this CLI wrote the file.
	Format    int        `json:"format"`
	Host      string     `json:"host"`
	FetchedAt time.Time  `json:"fetched_at"`
	Tools     []mcp.Tool `json:"tools"`
}

// toolCacheFormat numbers the layout of the tool cache, so that a CLI never
// reads one that means something else to it. The first, which had no number,
// came from a CLI that did not keep the tools' annotations. Read now, every
// tool in it would count as one that may delete data (see
// mcp.Tool.Destructive): scripts would need --yes to list vendors, and
// nothing would ever be retried. A cache of any other format counts as none,
// so the list is fetched again instead. Raise the number whenever a cached
// tool list comes to mean something new.
const toolCacheFormat = 2

func toolCachePath(host string) (string, error) {
	dir, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir, "tools-"+hex.EncodeToString(sum[:6])+".json"), nil
}

// loadToolCache is host's cached tool list, or nil when there is none that
// this CLI can read.
func loadToolCache(host string) *toolCache {
	path, err := toolCachePath(host)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var tc toolCache
	if json.Unmarshal(raw, &tc) != nil || tc.Format != toolCacheFormat || tc.Host != host {
		return nil
	}
	return &tc
}

// saveToolCache replaces the cache whole: a refresh in the background may
// be cut short when its command ends, and half a file would lose every
// workspace command until the next refresh.
func saveToolCache(host string, tools []mcp.Tool) error {
	path, err := toolCachePath(host)
	if err != nil {
		return err
	}
	return config.WriteJSON(path, toolCache{Format: toolCacheFormat, Host: host, FetchedAt: time.Now(), Tools: tools}, 0o600)
}

func refreshTools(ctx context.Context, app *App, c *api.Client) ([]mcp.Tool, error) {
	return fetchTools(ctx, app.MCP(c), c.Host)
}

// fetchTools asks m's server for its tools and caches them for host.
func fetchTools(ctx context.Context, m *mcp.Client, host string) ([]mcp.Tool, error) {
	tools, err := m.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	return tools, saveToolCache(host, tools)
}

// refreshToolsQuietly fetches the tool list after a sign-in, where a
// failure is only a note: the sign-in worked. announce prints how many
// commands it loaded; a caller that reports that itself, as login does in
// JSON, gets the count and whether the fetch worked.
func refreshToolsQuietly(ctx context.Context, app *App, c *api.Client, announce bool) (loaded int, ok bool) {
	tools, err := refreshTools(ctx, app, c)
	switch {
	case ctx.Err() != nil:
		// Interrupted: the command is ending, and saying why is not news.
		return 0, false
	case err != nil:
		fmt.Fprintf(app.Err, "note: could not load the workspace commands yet (%s); run `%s tools refresh` later.\n", ui.SafeLine(err.Error()), app.Name)
		return 0, false
	}
	loaded = commandCount(planToolCommands(tools, app.builtins))
	if announce {
		fmt.Fprintf(app.Out, "Loaded %d workspace commands; see `%s --help`.\n", loaded, app.Name)
	}
	return loaded, true
}

func newToolsCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "tools", Short: "List or refresh the workspace commands"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the server's tools and their commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, err := app.Host()
			if err != nil {
				return err
			}
			tc := loadToolCache(host)
			if tc == nil {
				return fmt.Errorf("no tools cached for this host; run `%s tools refresh`", app.Name)
			}
			return printTools(app, tc.Tools)
		},
	}
	refresh := &cobra.Command{
		Use:   "refresh",
		Short: "Fetch the tool list from the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := app.Client()
			if err != nil {
				return err
			}
			tools, err := refreshTools(cmd.Context(), app, c)
			if err != nil {
				return err
			}
			// The same list `tools list --json` prints, fresh.
			plans := planToolCommands(tools, app.builtins)
			if app.JSONOutput {
				return app.PrintJSON(listedTools(plans))
			}
			fmt.Fprintf(app.Out, "Loaded %d workspace commands.\n", commandCount(plans))
			if skipped := len(plans) - commandCount(plans); skipped > 0 {
				fmt.Fprintf(app.Out, "%d tools have no command; `%s tools list` says why.\n", skipped, app.Name)
			}
			return nil
		},
	}
	cmd.AddCommand(list, refresh)
	return cmd
}

// printTools is `tools list`: each tool's command, then the tools that
// have none and why, which `call` still reaches. A tool that writes says
// so after its title (see toolEffect).
func printTools(app *App, tools []mcp.Tool) error {
	plans := planToolCommands(tools, app.builtins)
	if app.JSONOutput {
		return app.PrintJSON(listedTools(plans))
	}
	tw := tabwriter.NewWriter(app.Out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "COMMAND\tTOOL\tTITLE")
	for _, p := range plans {
		if p.skipped == "" {
			fmt.Fprintf(tw, "%s %s\t%s\t%s\n", app.Name, p.path(), ui.SafeLine(p.tool.Name), markedTitle(ui.SafeLine(p.tool.Title), p.tool))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if commandCount(plans) == len(plans) {
		return nil
	}
	fmt.Fprintf(app.Out, "\nNo command; run these with `%s call <tool>`:\n", app.Name)
	tw = tabwriter.NewWriter(app.Out, 0, 2, 2, ' ', 0)
	for _, p := range plans {
		if p.skipped != "" {
			fmt.Fprintf(tw, "  %s\t%s\n", ui.SafeLine(p.tool.Name), markedTitle(skipReason(p, app.Name), p.tool))
		}
	}
	return tw.Flush()
}

func newCallCommand(app *App) *cobra.Command {
	var rawArgs []string
	var yes bool
	var list listFlags
	cmd := &cobra.Command{
		Use:   "call <tool>",
		Short: "Call any server tool by name, with --arg key=value for each argument",
		Long: fmt.Sprintf(`Call any server tool by name, including one that has no command of its own
(see `+"`%s tools list`"+`), with --arg key=value for each argument.

Each value is read as the type the tool's schema gives the argument, as the
tool's own command reads its flags: query=123 is the text 123, limit=10 the
number 10, and a boolean takes true or false. An array takes its items as the
command's flag does, commas separating them, and more each time the key is
repeated; a JSON array is the way to send an item with a comma of its own. An
object takes JSON. An argument the schema does not describe, or any argument
of a tool whose schema is not known, is read as JSON when it can be, and as
text otherwise.`, app.Name),
		Example: fmt.Sprintf(`  %[1]s call list_vendors --arg query=Bloomberg --arg limit=10
  %[1]s call list_vendors --arg fields=name,id
  %[1]s call analyze_cost_trends --arg entity_type=vendor --arg months_back=12`, app.Name),
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{rowsAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// A malformed --arg fails before any tool list is fetched.
			pairs, err := splitCallArgs(rawArgs)
			if err != nil {
				return err
			}
			tool, err := toolFor(cmd.Context(), app, args[0])
			if err != nil {
				return err
			}
			toolArgs, err := callArgs(tool, pairs)
			if err != nil {
				return err
			}
			if ws := app.DefaultWorkspace(); ws != "" {
				if _, set := toolArgs["workspace"]; !set {
					toolArgs["workspace"] = ws
				}
			}
			opts := renderOptions{continueWith: func(threadID string) string {
				return cmd.CommandPath() + " " + argWord(ui.SafeLine(args[0]), "<tool>") + globalFlagArgs(cmd) +
					" --arg " + shellWord("thread_id="+threadID, "thread_id=<thread-id>") + ` --arg message="..."`
			}}
			_, pageGiven := toolArgs["page"]
			if err := list.apply(app, tool, toolArgs, pageGiven, &opts); err != nil {
				return err
			}
			if opts.all && argType(tool, "page") == "" {
				if tool.InputSchema == nil {
					return usageErrorf("the tool list does not say what %s takes, so --all cannot tell how it pages; `%s tools refresh` fetches the list again", ui.SafeLine(tool.Name), app.Name)
				}
				return usageErrorf("%s takes no page, so --all has no pages to fetch", ui.SafeLine(tool.Name))
			}
			opts.nextPage = func(n string) string { return "--arg page=" + n + " or --all" }
			what := cmd.CommandPath() + " " + argWord(ui.SafeLine(args[0]), "<tool>")
			if ok, err := confirmTool(cmd.Context(), app, tool, yes, what, "--yes", toolArgs); !ok {
				return err
			}
			return runTool(cmd.Context(), app, tool, toolArgs, opts)
		},
	}
	cmd.Flags().StringArrayVar(&rawArgs, "arg", nil, "tool argument as key=value (repeatable)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "go ahead without asking first when the tool may delete or overwrite data")
	list = addListFlags(cmd, true)
	return cmd
}

// splitCallArgs splits call's --arg values at their first "=".
func splitCallArgs(raw []string) ([][2]string, error) {
	pairs := make([][2]string, 0, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, usageErrorf("--arg %q is not key=value", kv)
		}
		pairs = append(pairs, [2]string{k, v})
	}
	return pairs, nil
}

// callArgs makes call's key=value pairs the tool's arguments, each value
// read as the type the tool's schema gives it. Read as JSON regardless,
// query=123 went to the server as the number 123, where `vendors list
// --query 123` sends the text; a search for an id or a year failed on its
// type. An array's key may come again, each time with one more item.
func callArgs(tool mcp.Tool, pairs [][2]string) (map[string]any, error) {
	props, _ := tool.InputSchema["properties"].(map[string]any)
	args := map[string]any{}
	for _, kv := range pairs {
		key, raw := kv[0], kv[1]
		spec, _ := props[key].(map[string]any)
		value, err := argValue(spec, raw)
		if err != nil {
			return nil, usageErrorf("--arg %s: %s %v", ui.SafeLine(key+"="+raw), ui.SafeLine(key), err)
		}
		if items, isList := value.([]any); isList && spec["type"] == "array" {
			if held, ok := args[key].([]any); ok {
				value = append(held, items...)
			}
		}
		args[key] = value
	}
	return args, nil
}

// argValue reads raw as the type spec gives it. A type the CLI does not
// read itself, such as null or a list of types, or no schema at all, leaves
// raw to JSON, as call always read it: a value that is not JSON is text.
func argValue(spec map[string]any, raw string) (any, error) {
	kind, _ := spec["type"].(string)
	switch kind {
	case "string":
		// A value in double quotes is the text inside them, so a script
		// that quoted its text for JSON, as call once needed, still works.
		var s string
		if strings.HasPrefix(raw, `"`) && json.Unmarshal([]byte(raw), &s) == nil {
			return s, nil
		}
		return raw, nil
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, errors.New("takes a whole number")
		}
		return n, nil
	case "number":
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, errors.New("takes a number")
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, errors.New("takes true or false")
		}
		return b, nil
	case "array":
		var list []any
		if strings.HasPrefix(strings.TrimSpace(raw), "[") && json.Unmarshal([]byte(raw), &list) == nil {
			return list, nil
		}
		// Otherwise raw holds items as the list's flag takes them (see
		// listItems), but for one item in double quotes, which is that
		// item, commas and all, as for text above.
		items, _ := spec["items"].(map[string]any)
		parts := []string{raw}
		var quoted string
		if !strings.HasPrefix(raw, `"`) || json.Unmarshal([]byte(raw), &quoted) != nil {
			if split, ok := listItems(items, raw); ok {
				if len(split) == 0 {
					return nil, errors.New("names no item")
				}
				parts = split
			}
		}
		values := make([]any, len(parts))
		for i, part := range parts {
			item, err := argValue(items, part)
			if err != nil {
				return nil, fmt.Errorf("%w for each item", err)
			}
			values[i] = item
		}
		return values, nil
	case "object":
		var obj map[string]any
		if json.Unmarshal([]byte(raw), &obj) != nil || obj == nil {
			return nil, errors.New(`takes a JSON object, such as {"key":"value"}`)
		}
		return obj, nil
	}
	var parsed any
	if json.Unmarshal([]byte(raw), &parsed) == nil {
		return parsed, nil
	}
	return raw, nil
}

// commandPath maps a tool name to its command group and verb. The names come
// from the server and are shown in help and `tools list`, so they are made
// safe to print; the tool is still called by its own name.
func commandPath(tool string) (group, verb string) {
	hyphen := func(s string) string { return ui.SafeLine(strings.ReplaceAll(s, "_", "-")) }
	switch {
	case strings.HasPrefix(tool, "list_"):
		return hyphen(strings.TrimPrefix(tool, "list_")), "list"
	case strings.HasPrefix(tool, "analyze_"):
		return "analyze", hyphen(strings.TrimPrefix(tool, "analyze_"))
	default:
		return "", hyphen(tool)
	}
}

// noToolsAnnotation marks the root when the host has no tool list cached,
// so an unknown command there is most likely a workspace command that has
// not been fetched yet (see unknownSubcommand).
const noToolsAnnotation = "firmfact/no-tools"

// addToolCommands registers the cached tools of the host this invocation
// targets. Flags are not parsed yet, so --host, --profile and
// --insecure-http are read from the raw arguments; FIRMFACT_HOST and
// FIRMFACT_PROFILE apply as they do once the flags are parsed. An invalid
// host, or a profile that does not exist, adds none; the command then fails
// with the reason.
func addToolCommands(root *cobra.Command, app *App, args []string) {
	host, err := hostFromArgs(app, args)
	if err != nil {
		return
	}
	tc := loadToolCache(host)
	if tc == nil {
		if root.Annotations == nil {
			root.Annotations = map[string]string{}
		}
		root.Annotations[noToolsAnnotation] = "true"
		// Help says where the rest of the commands are, before anyone
		// types one and is told it is unknown.
		root.AddGroup(&cobra.Group{ID: workspaceGroup, Title: fmt.Sprintf(
			"Workspace commands:\n  none for this host yet; `%s login` or `%s tools refresh` fetches them", app.Name, app.Name)})
		return
	}
	plans := planToolCommands(tc.Tools, app.builtins)
	if commandCount(plans) > 0 {
		root.AddGroup(&cobra.Group{ID: workspaceGroup, Title: "Workspace commands:"})
	}
	for _, p := range plans {
		if p.skipped != "" {
			continue // `tools list` says why
		}
		cmd := newToolCommand(app, p)
		if p.group == "" {
			cmd.GroupID = workspaceGroup
			root.AddCommand(cmd)
			continue
		}
		findOrAddGroup(root, p.group).AddCommand(cmd)
	}
}

// commandNames are the names of root's commands so far: the built-in ones,
// when called before the tools add theirs.
func commandNames(root *cobra.Command) map[string]bool {
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
		for _, alias := range c.Aliases {
			names[alias] = true
		}
	}
	return names
}

func hostFromArgs(app *App, args []string) (string, error) {
	var profile, host string
	insecure := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break // the rest are arguments, not flags
		}
		if arg == "--insecure-http" {
			insecure = true
			continue
		}
		if v, ok := strings.CutPrefix(arg, "--insecure-http="); ok {
			insecure, _ = strconv.ParseBool(v)
			continue
		}
		for _, name := range []string{"--host", "--profile"} {
			value := ""
			switch {
			case arg == name && i+1 < len(args):
				i++
				value = args[i]
			case strings.HasPrefix(arg, name+"="):
				value = strings.TrimPrefix(arg, name+"=")
			default:
				continue
			}
			if name == "--host" {
				host = value
			} else {
				profile = value
			}
		}
	}
	return app.resolveHost(profile, host, insecure)
}

func findOrAddGroup(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	group := &cobra.Command{Use: name, Short: "Workspace " + strings.ReplaceAll(name, "-", " "), GroupID: workspaceGroup}
	if name == "analyze" {
		group.Short = "Analyse spend, utilisation and allocations"
	}
	root.AddCommand(group)
	return group
}

type flagBinding struct {
	flag string
	kind string
	str  *string
	num  *float64
	i    *int
	b    *bool
	arr  *[]string
	// items is the schema of a list flag's items.
	items map[string]any
}

// value is what the flag sends: its value as the JSON type the schema
// gives, as call reads an --arg. A list's items are read by their own
// type, one or more per use of the flag (see listItems); text items go as
// typed.
func (b *flagBinding) value() (any, error) {
	switch b.kind {
	case "integer":
		return *b.i, nil
	case "number":
		return *b.num, nil
	case "boolean":
		return *b.b, nil
	case "array":
		kind, _ := b.items["type"].(string)
		text := kind == "" || kind == "string"
		list := make([]any, 0, len(*b.arr))
		for _, raw := range *b.arr {
			parts, split := listItems(b.items, raw)
			switch {
			case !split:
				parts = []string{raw}
			case len(parts) == 0:
				return nil, b.refuse(raw, errors.New("names no item"))
			}
			for _, part := range parts {
				if text {
					list = append(list, part)
					continue
				}
				item, err := argValue(b.items, part)
				if err != nil {
					return nil, b.refuse(raw, fmt.Errorf("%w for each item", err))
				}
				list = append(list, item)
			}
		}
		return list, nil
	case "object":
		obj, err := argValue(map[string]any{"type": "object"}, *b.str)
		if err != nil {
			return nil, b.refuse(*b.str, err)
		}
		return obj, nil
	}
	return *b.str, nil
}

// commaList reports whether a list flag whose items have this schema also
// takes its items separated by commas: text with no fixed set of values,
// such as the fields a list command returns. `--fields name,temporal_id`
// asks for two fields, as --columns does, and no field name has a comma.
// Where the schema lists the values an item may take, an item may be one
// of them, commas and all, and completion offers them one per use of the
// flag; listItems still splits a value that is not one of them. Items of
// another type, or of none the schema names, go one per use too.
func commaList(items map[string]any) bool {
	kind, _ := items["type"].(string)
	enum, _ := items["enum"].([]any)
	return kind == "string" && len(enum) == 0
}

// listItems is raw as the items of a list whose items have this schema,
// or ok false when raw is one item as it is. Text with no fixed values is
// separated by commas (see commaList). Where the schema lists the values
// an item may take, so is a raw that is not one of them when each of its
// parts is: `--statuses active,archived` asks for two statuses, where one
// called "active,archived" would match none, while a value with a comma of
// its own stays one item. Anything else is one item, for the server to
// judge; the values the cached schema lists may be older than the
// server's.
func listItems(items map[string]any, raw string) (parts []string, ok bool) {
	if commaList(items) {
		return splitItems(raw), true
	}
	enum, _ := items["enum"].([]any)
	if len(enum) == 0 || !strings.Contains(raw, ",") {
		return nil, false
	}
	allowed := make(map[string]bool, len(enum))
	for _, v := range enum {
		allowed[fmt.Sprint(v)] = true
	}
	if allowed[raw] {
		return nil, false
	}
	parts = splitItems(raw)
	for _, part := range parts {
		if !allowed[part] {
			return nil, false
		}
	}
	return parts, len(parts) > 0
}

// splitItems is raw's items, separated by commas, without the spaces
// around them; empty ones are left out.
func splitItems(raw string) []string {
	var items []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// refuse is the usage error for a value raw the flag cannot send, worded
// as pflag words one for a number or a boolean (%q escapes what a terminal
// would act on).
func (b *flagBinding) refuse(raw string, err error) error {
	return usageErrorf("invalid argument %q for \"--%s\" flag: it %v", raw, b.flag, err)
}

// toolAnnotation marks a command generated from a server tool. The server
// names those; the mark keeps them out of what the CLI's own commands are
// allowed, such as running on an outdated version, should one ever land
// among them (planToolCommands keeps a tool such as list_config out of the
// config group).
const toolAnnotation = "firmfact/tool"

func newToolCommand(app *App, p toolCommand) *cobra.Command {
	tool := p.tool
	short := tool.Title
	if short == "" {
		short = tool.Name
	}
	cmd := &cobra.Command{
		Use:         p.verb,
		Short:       markedTitle(ui.SafeLine(short), tool),
		Args:        cobra.NoArgs,
		Annotations: map[string]string{toolAnnotation: tool.Name, rowsAnnotation: "true"},
	}

	props, _ := tool.InputSchema["properties"].(map[string]any)
	required := map[string]bool{}
	if req, ok := tool.InputSchema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}

	bindings := map[string]*flagBinding{}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "workspace" {
			continue // the global --workspace flag supplies it
		}
		spec, _ := props[name].(map[string]any)
		desc := flagUsage(spec, required[name])
		flagName := ui.SafeLine(strings.ReplaceAll(name, "_", "-"))
		if cmd.Flags().Lookup(flagName) != nil {
			// Two properties such as a_b and a-b make the same flag, and
			// pflag panics on the second, which would stop every command
			// for this host. The first by name keeps it; call reaches both.
			continue
		}
		b := &flagBinding{flag: flagName}
		kind, _ := spec["type"].(string)
		switch kind {
		case "integer":
			b.kind, b.i = kind, cmd.Flags().Int(flagName, 0, desc)
		case "number":
			b.kind, b.num = kind, cmd.Flags().Float64(flagName, 0, desc)
		case "boolean":
			b.kind, b.b = kind, cmd.Flags().Bool(flagName, false, desc)
		case "array":
			// One value per use of the flag, as typed: StringSlice would
			// split every list's items at commas, and read quotes as CSV.
			// value splits only where listItems says commas separate items.
			b.kind, b.arr = kind, cmd.Flags().StringArray(flagName, nil, desc)
			b.items, _ = spec["items"].(map[string]any)
		case "object":
			// JSON, read when the command runs (see value).
			b.kind, b.str = kind, cmd.Flags().String(flagName, "", desc)
		default:
			b.kind, b.str = "string", cmd.Flags().String(flagName, "", desc)
		}
		completeSchemaValues(cmd, flagName, b.kind, spec)
		bindings[name] = b
		if required[name] {
			_ = cmd.MarkFlagRequired(flagName)
		}
	}
	// An analysis can print its month-by-month totals too. That is a choice
	// of what to show rather than an argument for the server, so the flag is
	// the CLI's own, unless the tool has one by that name.
	var monthly *bool
	if strings.HasPrefix(tool.Name, "analyze_") && cmd.Flags().Lookup("monthly") == nil {
		monthly = cmd.Flags().Bool("monthly", false, "also print the month-by-month totals, where the analysis has them")
	}
	list := addListFlags(cmd, argType(tool, "page") != "")
	// Added after the tool's own flags, which win: see addYesFlag.
	yes := addYesFlag(cmd, tool)
	askFlag := "--yes"
	if yes == nil {
		askFlag = "`" + app.Name + " call " + tool.Name + " --yes`"
	}
	cmd.Long = ui.SafeText(tool.Description)
	if cmd.Long == "" {
		cmd.Long = ui.SafeLine(short)
	}
	if effect := effectHelp(tool, askFlag); effect != "" {
		cmd.Long += "\n\n" + effect
	}
	cmd.Example = toolExample(app.Name+" "+p.path(), props, names, required, bindings)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		args := map[string]any{}
		for _, name := range names {
			b, ok := bindings[name]
			if !ok || !cmd.Flags().Changed(b.flag) {
				continue
			}
			value, err := b.value()
			if err != nil {
				return err
			}
			args[name] = value
		}
		if ws := app.toolWorkspace(tool); ws != "" {
			args["workspace"] = ws
		}
		opts := renderOptions{monthly: monthly != nil && *monthly}
		page := bindings["page"]
		if err := list.apply(app, tool, args, page != nil && cmd.Flags().Changed(page.flag), &opts); err != nil {
			return err
		}
		if page != nil {
			opts.nextPage = func(n string) string {
				if list.all == nil {
					return "--" + page.flag + " " + n
				}
				return "--" + page.flag + " " + n + " or --all"
			}
		}
		// A tool that takes a thread can continue the one its answer names.
		// For a chat, ask does that in fewer words, and sends the same
		// workspace (see toolWorkspace).
		if thread, ok := bindings["thread_id"]; ok {
			opts.continueWith = func(threadID string) string {
				if tool.Name == chatTool {
					return askToContinue(cmd, app.Name+" ask", threadID)
				}
				line := cmd.CommandPath() + globalFlagArgs(cmd) + " --" + thread.flag + " " + shellWord(threadID, "<thread-id>")
				if message, ok := bindings["message"]; ok {
					line += " --" + message.flag + ` "..."`
				}
				return line
			}
		}
		// Asked last, so a command line that fails anyway is not asked
		// about first.
		if ok, err := confirmTool(cmd.Context(), app, tool, yes != nil && *yes, cmd.CommandPath(), askFlag, args); !ok {
			return err
		}
		if err := runTool(cmd.Context(), app, tool, args, opts); err != nil {
			return err
		}
		if step, ok := stepAfterTool(tool.Name); ok {
			app.printNextStep(step)
		}
		return nil
	}
	return cmd
}

// toolWorkspace is the workspace a call of tool is sent, or empty to leave
// it to the server: the sign-in's own workspace. A workspace this
// invocation names (--workspace or FIRMFACT_WORKSPACE) is sent even when
// the cached schema has no workspace argument, as it has not when the
// sign-in reaches one workspace. The server still checks a workspace it is
// sent and refuses one the sign-in does not reach; left out, the command
// would quietly run against the sign-in's own workspace instead of the one
// asked for. The profile's default was not asked for here and may belong
// to another sign-in (a FIRMFACT_TOKEN, say), so it only goes where the
// schema takes it.
func (a *App) toolWorkspace(tool mcp.Tool) string {
	if ws := a.ChosenWorkspace(); ws != "" {
		return ws
	}
	props, _ := tool.InputSchema["properties"].(map[string]any)
	if _, takesWorkspace := props["workspace"]; takesWorkspace {
		return a.profileWorkspace()
	}
	return ""
}

// flagUsage is the help text of a tool's flag: the server's description,
// the values the schema allows, for a flag of any type, its default and
// range, and whether the tool needs it. cobra only names a required flag
// once a command has failed without it; --help should say so before that.
// The flags themselves have no defaults: the server applies its own to an
// argument left out, and the CLI sends only the flags that are given.
func flagUsage(spec map[string]any, required bool) string {
	server, _ := spec["description"].(string)
	desc := ui.SafeText(server)
	if enum, ok := spec["enum"].([]any); ok && len(enum) > 0 {
		desc += " (one of: " + ui.SafeLine(joinAny(enum)) + ")"
	}
	// A list flag takes several values: from its items' enum when they
	// have one, and separated by commas as well when they are text without
	// one, or values of the enum (see listItems).
	if spec["type"] == "array" {
		items, _ := spec["items"].(map[string]any)
		enum, _ := items["enum"].([]any)
		if len(enum) > 0 {
			desc += " (any of: " + ui.SafeLine(joinAny(enum)) + ")"
		}
		if commaList(items) || len(enum) > 0 {
			desc += " (repeatable, or comma-separated)"
		} else {
			desc += " (repeatable)"
		}
	}
	if spec["type"] == "object" {
		desc += ` (a JSON object, such as '{"key":"value"}')`
	}
	// Some descriptions name the default already, as "(default: 6)".
	if def := schemaDefault(spec); def != "" && !strings.Contains(strings.ToLower(server), "default") {
		desc += " (default: " + def + ")"
	}
	if r := schemaRange(spec); r != "" {
		desc += " (" + r + ")"
	}
	if required {
		desc += " (required)"
	}
	return strings.TrimSpace(desc)
}

// schemaDefault is the schema's default for an argument as help shows it,
// or empty when there is none worth showing: false for a boolean is what
// leaving its flag out means anyway.
func schemaDefault(spec map[string]any) string {
	switch def := spec["default"].(type) {
	case string:
		return ui.SafeLine(def)
	case float64:
		return formatNumber(def)
	case bool:
		if def {
			return "true"
		}
	case []any:
		return ui.SafeLine(joinAny(def))
	}
	return ""
}

// schemaRange is the range the schema allows a number in, such as "from 1
// to 200", or empty when it sets no bounds.
func schemaRange(spec map[string]any) string {
	least, hasLeast := spec["minimum"].(float64)
	most, hasMost := spec["maximum"].(float64)
	switch {
	case hasLeast && hasMost:
		return "from " + formatNumber(least) + " to " + formatNumber(most)
	case hasLeast:
		return "at least " + formatNumber(least)
	case hasMost:
		return "at most " + formatNumber(most)
	}
	return ""
}

// formatNumber prints a JSON number the way it would be typed: 1000000,
// not 1e+06.
func formatNumber(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// toolExample is the example in a tool command's help: line, the command
// as typed, with each flag its tool requires and a value the schema
// allows, so that it runs as shown once a "..." is filled in. names are the
// tool's properties in flag order; one without a flag of its own, such as
// workspace, which the global --workspace supplies, is left out.
func toolExample(line string, props map[string]any, names []string, required map[string]bool, bindings map[string]*flagBinding) string {
	for _, name := range names {
		b, ok := bindings[name]
		if !ok || !required[name] {
			continue
		}
		line += " --" + b.flag
		spec, _ := props[name].(map[string]any)
		if value, takesOne := exampleValue(b.kind, spec); takesOne {
			line += " " + value
		}
	}
	return "  " + line
}

// exampleValue is a value for a flag of this kind that its schema allows:
// the first of its enum, its default, its minimum, or "..." for text to be
// filled in. A boolean flag takes none: given, it is true.
func exampleValue(kind string, spec map[string]any) (value string, takesOne bool) {
	if kind == "boolean" {
		return "", false
	}
	if kind == "array" {
		// Each use of the flag is one item.
		items, _ := spec["items"].(map[string]any)
		itemKind, _ := items["type"].(string)
		if value, ok := exampleValue(orDefault(itemKind, "string"), items); ok {
			return value, true
		}
		return "true", true
	}
	values, _ := schemaValues(kind, spec)
	if len(values) > 0 {
		return shellWord(values[0], `"..."`), true
	}
	if def := schemaDefault(spec); def != "" {
		return shellWord(def, `"..."`), true
	}
	if least, ok := spec["minimum"].(float64); ok && (kind == "integer" || kind == "number") {
		return formatNumber(least), true
	}
	switch kind {
	case "integer", "number":
		return "1", true
	case "object":
		return "'{}'", true
	}
	return `"..."`, true
}

// toolFor is the tool called name as the server describes it, hints and
// all, so `call` asks first and retries just as a generated command would.
// The host's cached tool list has it, or else the list is fetched now:
// known by its name alone, a tool would count as one that may delete data
// (see mcp.Tool.Destructive), and a script with no list cached, such as a
// CI job, would need --yes even to list vendors. A tool the fresh list
// does not have is not found.
func toolFor(ctx context.Context, app *App, name string) (mcp.Tool, error) {
	host, err := app.Host()
	if err != nil {
		return mcp.Tool{}, err
	}
	if tc := loadToolCache(host); tc != nil {
		if t, ok := findTool(tc.Tools, name); ok {
			return t, nil
		}
	}
	c, err := app.Client()
	if err != nil {
		return mcp.Tool{}, err
	}
	tools, err := refreshTools(ctx, app, c)
	if err != nil {
		return mcp.Tool{}, err
	}
	if t, ok := findTool(tools, name); ok {
		return t, nil
	}
	return mcp.Tool{}, noSuchTool(app, name)
}

func findTool(tools []mcp.Tool, name string) (mcp.Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return mcp.Tool{}, false
}

// noSuchTool is the error for a tool the server's fresh list does not have.
func noSuchTool(app *App, name string) error {
	return withExit(ExitNotFound, fmt.Errorf("the server has no tool %q; `%s tools list` shows the ones it has", ui.SafeLine(name), app.Name))
}

func runTool(ctx context.Context, app *App, tool mcp.Tool, args map[string]any, opts renderOptions) error {
	c, err := app.Client()
	if err != nil {
		return err
	}
	bg := refreshStaleTools(ctx, app)
	defer bg.settle(ctx)
	if opts.all {
		return runAllPages(ctx, app, c, tool, args, opts, bg)
	}
	out, err := callForOutput(ctx, app, c, tool, args, bg)
	if err != nil {
		return err
	}
	// Plain-text notes (such as the Demo disclaimer) go to stderr in every
	// format, so they stay visible without breaking --json on stdout.
	for _, note := range out.shown {
		fmt.Fprintln(app.Err, ui.SafeText(note))
	}
	if opts.answered != nil {
		opts.answered(out)
	}
	return printToolOutput(app, tool, out, opts)
}

// callForOutput calls tool, showing the wait on a terminal, and sorts its
// answer (see parseToolResult). A tool error is the error.
func callForOutput(ctx context.Context, app *App, c *api.Client, tool mcp.Tool, args map[string]any, bg *toolRefresh) (toolOutput, error) {
	stop := ui.Thinking(ctx, app.Err)
	res, err := callTool(ctx, app, c, tool, args, bg)
	stop()
	if err != nil {
		return toolOutput{}, err
	}
	if res.IsError {
		return toolOutput{}, errors.New(ui.SafeText(strings.TrimSpace(res.Text())))
	}
	return parseToolResult(res.Content), nil
}

// printToolOutput prints a tool's answer in the format asked for, and on
// stderr how many rows there are in all and how to get the next page.
func printToolOutput(app *App, tool mcp.Tool, out toolOutput, opts renderOptions) error {
	if app.JSONOutput {
		return app.PrintJSON(out)
	}
	rows, isList := out.Data.([]any)
	empty := isList && len(rows) == 0
	if len(opts.table.columns) > 0 {
		noteMissingColumns(app, findRows(out.Data), opts.table.columns)
	}
	switch {
	case opts.format.delimited():
		if err := printDelimited(app, tool, out.Data, opts); err != nil {
			return err
		}
	case empty:
		fmt.Fprintln(app.Out, noneFound(tool.Name))
	case out.Data != nil: // nil when only notes came back
		if err := printHuman(app, out.Data, opts); err != nil {
			return err
		}
	}
	if total, ok := out.Meta["total_count"].(float64); ok && !(empty && total == 0) {
		fmt.Fprintf(app.Err, "%d total", int(total))
		if pages, ok := out.Meta["total_pages"].(float64); ok && pages > 1 {
			page := ui.SafeLine(fmt.Sprint(out.Meta["page"]))
			fmt.Fprintf(app.Err, " (page %s of %d", page, int(pages))
			if opts.nextPage != nil {
				next := "N"
				if n, ok := out.Meta["page"].(float64); ok && n >= 1 && n < pages {
					next = strconv.Itoa(int(n) + 1)
				}
				fmt.Fprintf(app.Err, "; use %s", opts.nextPage(next))
			}
			fmt.Fprint(app.Err, ")")
		}
		fmt.Fprintln(app.Err)
	}
	return nil
}

// printDelimited prints the rows of an answer as CSV or TSV. An empty list
// is the column names alone, when --columns gives them, and an answer with
// no rows at all, such as a chat, is an error: stdout is meant for a
// program that reads rows.
func printDelimited(app *App, tool mcp.Tool, data any, opts renderOptions) error {
	rows := findRows(data)
	if len(rows) == 0 {
		if list, isList := data.([]any); !(isList && len(list) == 0) && data != nil {
			return usageErrorf("this answer has no rows to print as %s; use --json or leave out --format", strings.ToUpper(string(opts.format)))
		}
		fmt.Fprintln(app.Err, noneFound(tool.Name))
	}
	if len(rows) == 0 && len(opts.table.columns) == 0 {
		return nil
	}
	return writeDelimited(app.Out, rows, opts.table.columns, opts.format == formatTSV)
}

// noteMissingColumns says which of the columns --columns asked for none of
// the rows has, which is most likely a typo. They still print, empty, so a
// script gets the columns it asked for.
func noteMissingColumns(app *App, rows []map[string]any, columns []string) {
	if len(rows) == 0 {
		return
	}
	var missing []string
	for _, col := range columns {
		found := false
		for _, row := range rows {
			if _, ok := row[col]; ok {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, ui.SafeLine(col))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(app.Err, "note: no row has %s %s.\n", plural(len(missing), "the column", "the columns"), strings.Join(missing, ", "))
	}
}

// globalFlagArgs repeats the global flags this invocation was given that
// decide where it ran, so a command built from them reaches the same host
// and workspace: a chat thread belongs to one workspace.
func globalFlagArgs(cmd *cobra.Command) string {
	var b strings.Builder
	for _, name := range []string{"profile", "host", "insecure-http", "workspace"} {
		f := cmd.Flags().Lookup(name)
		if f == nil || !f.Changed {
			continue
		}
		if f.Value.Type() == "bool" {
			if f.Value.String() == "true" {
				b.WriteString(" --" + name)
			}
			continue
		}
		b.WriteString(" --" + name + " " + shellWord(ui.SafeLine(f.Value.String()), "<"+name+">"))
	}
	return b.String()
}

// shellWord is s as one word of a command line printed for the user to
// run, safe to paste into bash, zsh, fish, PowerShell or cmd: as it is when
// it is plain, and in double quotes when it has spaces or punctuation that
// none of those shells reads inside them. No one quoting is literal in all
// of them. Inside double quotes each still expands something: $ and
// backticks in POSIX shells and PowerShell, a backslash in fish, ! in an
// interactive bash or zsh, % in cmd. Single quotes are literal to a POSIX
// shell, but fish reads a backslash in them and cmd does not know them at
// all. Many of these values come from the server (a chat's thread id, a
// workspace id, a schema's default), which must not be able to plant a
// command for the user to paste, so a value that would need more than
// double quotes is not printed: placeholder takes its place, for the user
// to fill in.
func shellWord(s, placeholder string) string {
	switch {
	case plainWord(s):
		return s
	case quotableWord(s):
		return `"` + s + `"`
	}
	return placeholder
}

// argWord is shellWord for a word in the place of an argument rather than
// a flag's value. There a word that starts with - is read as a flag,
// quoted or not, so a workspace id such as --host=https://evil.example
// would send the pasted command to another host; placeholder takes its
// place. After a flag that takes a value, the next word is that value
// whatever it starts with, so shellWord is enough there.
func argWord(s, placeholder string) string {
	if strings.HasPrefix(s, "-") {
		return placeholder
	}
	return shellWord(s, placeholder)
}

// plainWord reports whether s needs no quoting in any of those shells:
// letters, digits and -_.:/@+=, alone. A word that starts with = or @ is
// not plain, as zsh expands =word to a path and PowerShell @word to a
// variable's value.
func plainWord(s string) bool {
	return s != "" && !strings.ContainsRune("=@", rune(s[0])) && strings.IndexFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.:/@+=,", r)
	}) < 0
}

// quotableWord reports whether s reads the same inside double quotes in
// every one of those shells: printable text (so no control, bidirectional
// or invisible characters, see ui.SafeText) without a double quote, $, a
// backtick, a backslash, ! or %, nor the curly double quotes PowerShell
// takes for straight ones.
func quotableWord(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool {
		return !unicode.IsPrint(r) || strings.ContainsRune("\"$`\\!%\u201c\u201d\u201e", r)
	}) < 0
}

// nameInText is s as it reads inside a sentence rather than on a command
// line: as it is when it is plain, and otherwise in double quotes, so that
// a name with spaces reads as one. s is already safe to print (see
// ui.SafeLine).
func nameInText(s string) string {
	if plainWord(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// noneFound is the human answer to an empty list: list_cost_centers finds
// no cost centers.
func noneFound(tool string) string {
	if what, ok := strings.CutPrefix(tool, "list_"); ok && what != "" {
		return "No " + ui.SafeLine(strings.ReplaceAll(what, "_", " ")) + " found."
	}
	return "No results found."
}

// toolOutput is a tool result as --json prints it, with the same three keys
// for every workspace command and every kind of workspace. The server does
// not send one shape: a list from a Demo workspace is a notice, then
// {"workspace_data_source": ..., "data": [rows]}, then the paging block; the
// same list from any other workspace is the rows as a bare array, then the
// paging block; chat and analysis answer with one object. Scripts should
// not have to know which kind of workspace they are talking to.
type toolOutput struct {
	Data  any            `json:"data"`
	Meta  map[string]any `json:"meta"`
	Notes []string       `json:"notes"`

	shown []string // the notes that came as plain text, printed on stderr
}

// pagingKeys are the fields of the block the server appends after the data
// to say how complete it is (successful_tool_content in the server's MCP
// controller).
var pagingKeys = map[string]bool{"meta": true, "total_count": true, "truncated": true, "note": true}

// demoMarker is the field the server stamps on a Demo workspace's data.
// It moves to meta, so a script can still tell sample data from real data.
const demoMarker = "workspace_data_source"

// parseToolResult sorts the text blocks of a result into data, meta and
// notes. Nothing the server sent is dropped: a result that fits no known
// shape keeps all its values under data.
func parseToolResult(content []mcp.Content) toolOutput {
	out := toolOutput{Meta: map[string]any{}, Notes: []string{}}
	merged := map[string]any{}
	var other []any
	for _, c := range content {
		if c.Type != "text" {
			continue
		}
		var parsed any
		if json.Unmarshal([]byte(c.Text), &parsed) != nil {
			if note := strings.TrimSpace(c.Text); note != "" {
				out.Notes = append(out.Notes, note)
				out.shown = append(out.shown, note)
			}
			continue
		}
		obj, isObject := parsed.(map[string]any)
		switch {
		case isObject && isPagingBlock(obj):
			out.addPaging(obj)
		case isObject:
			for k, v := range obj {
				merged[k] = v
			}
		default:
			other = append(other, parsed)
		}
	}

	if source, ok := merged[demoMarker]; ok {
		out.Meta[demoMarker] = source
		delete(merged, demoMarker)
	}
	data, hasData := merged["data"]
	if !hasData && len(merged) > 0 && len(other) == 1 {
		merged["data"], other = other[0], nil
	}
	switch {
	case len(merged) == 0 && len(other) == 1:
		out.Data = other[0]
	case len(merged) == 0 && len(other) > 1:
		out.Data = other
	case len(other) > 0:
		out.Data = append([]any{merged}, other...)
	case hasData && len(merged) == 1:
		out.Data = data
	case len(merged) > 0:
		out.Data = merged
	}
	return out
}

// isPagingBlock recognises the paging block by its fields alone, so a data
// object that merely has a "meta" or "note" among others stays data.
func isPagingBlock(obj map[string]any) bool {
	if len(obj) == 0 {
		return false
	}
	for k := range obj {
		if !pagingKeys[k] {
			return false
		}
	}
	if meta, ok := obj["meta"]; ok {
		if _, isObject := meta.(map[string]any); !isObject {
			return false
		}
	}
	if note, ok := obj["note"]; ok {
		if _, isString := note.(string); !isString {
			return false
		}
	}
	return true
}

// addPaging folds the paging block into meta and notes. Its note is written
// for a language model ("Use page to retrieve other pages"), so it is kept
// for --json but not shown on stderr, where the total line says the same.
func (o *toolOutput) addPaging(block map[string]any) {
	if meta, ok := block["meta"].(map[string]any); ok {
		for k, v := range meta {
			o.Meta[k] = v
		}
	}
	for _, k := range []string{"total_count", "truncated"} {
		if v, ok := block[k]; ok {
			if _, set := o.Meta[k]; !set {
				o.Meta[k] = v
			}
		}
	}
	if note, _ := block["note"].(string); strings.TrimSpace(note) != "" {
		o.Notes = append(o.Notes, strings.TrimSpace(note))
	}
}

func joinAny(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}

func equalFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
