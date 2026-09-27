package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// outputFormat is --format: how a command prints its answer. --json is
// --format json, kept because scripts already pass it.
type outputFormat string

const (
	formatTable outputFormat = "table"
	formatJSON  outputFormat = "json"
	formatCSV   outputFormat = "csv"
	formatTSV   outputFormat = "tsv"
)

// delimited reports whether f is CSV or TSV, which only a command that
// prints rows can write.
func (f outputFormat) delimited() bool { return f == formatCSV || f == formatTSV }

// formatFlag is --format. A value it does not know fails as the command
// line is parsed, with the values it does.
type formatFlag struct{ f *outputFormat }

func (v formatFlag) Set(s string) error {
	switch f := outputFormat(strings.ToLower(strings.TrimSpace(s))); f {
	case formatTable, formatJSON, formatCSV, formatTSV:
		*v.f = f
		return nil
	}
	return fmt.Errorf("%q is not a format; give table, json, csv or tsv", s)
}

// String is empty until the flag is given, so help shows no default and
// settleFormat can tell a table asked for from none.
func (v formatFlag) String() string {
	if v.f == nil {
		return ""
	}
	return string(*v.f)
}

func (v formatFlag) Type() string { return "format" }

// rowsAnnotation marks a command that prints a tool's answer, and so can
// print its rows as CSV or TSV.
const rowsAnnotation = "firmfact/rows"

// settleFormat makes --json, --format and --jq one setting before a
// command runs, so every command that knows --json knows --format json and
// --jq too, errors included.
func (a *App) settleFormat(cmd *cobra.Command) error {
	// --jq filters the JSON, so it asks for JSON, and for nothing else.
	if a.jq != nil {
		if a.Format != "" && a.Format != formatJSON {
			return usageErrorf("--jq filters JSON, so it cannot go with --format %s", a.Format)
		}
		a.JSONOutput = true
		a.jq.ctx = cmd.Context()
	}
	switch {
	case a.Format == "" && a.JSONOutput:
		a.Format = formatJSON
	case a.Format == "":
		a.Format = formatTable
	case a.JSONOutput && a.Format != formatJSON:
		return usageErrorf("--json is --format json; give one or the other")
	case a.Format == formatJSON:
		a.JSONOutput = true
	}
	// A group on its own only shows its help, whatever the format.
	if a.Format.delimited() && cmd.Annotations[rowsAnnotation] == "" && cmd.Annotations[groupAnnotation] == "" {
		return usageErrorf("--format %s is for workspace commands and `%s call`; use --json here", a.Format, a.Name)
	}
	return nil
}

// outputFormat is the format this invocation prints in; a table when
// settleFormat has not run, as for help.
func (a *App) outputFormat() outputFormat {
	if a.Format == "" {
		if a.JSONOutput {
			return formatJSON
		}
		return formatTable
	}
	return a.Format
}

// listFlags are the CLI's own flags on a command that prints a tool's
// answer: which columns, every page, and whether to fit the terminal. A
// tool argument of the same name keeps its flag, and the CLI's goes
// without; each field is nil when its flag is not there.
type listFlags struct {
	columns *[]string
	all     *bool
	wide    *bool
}

// addListFlags adds the list flags to cmd; --all only when paged, that is
// when the tool takes a page to fetch.
func addListFlags(cmd *cobra.Command, paged bool) listFlags {
	var f listFlags
	if cmd.Flags().Lookup("columns") == nil {
		f.columns = cmd.Flags().StringSlice("columns", nil, "the columns to print, in order (comma-separated); where the tool takes fields, also the fields it sends")
	}
	if paged && cmd.Flags().Lookup("all") == nil {
		f.all = cmd.Flags().Bool("all", false, fmt.Sprintf("fetch every page, %d rows at a time; with --json, one JSON row per line (NDJSON)", allPageSize))
	}
	if cmd.Flags().Lookup("wide") == nil {
		f.wide = cmd.Flags().Bool("wide", false, "print every cell whole instead of fitting the table to the terminal")
	}
	return f
}

// apply sets what the list flags ask for on opts, and the columns as the
// tool's fields in args where the tool takes them and they were not given
// already. The page argument is refused beside --all, which fetches them
// all itself.
func (f listFlags) apply(app *App, tool mcp.Tool, args map[string]any, pageGiven bool, opts *renderOptions) error {
	opts.format = app.outputFormat()
	if f.columns != nil {
		for _, c := range *f.columns {
			if c = strings.TrimSpace(c); c != "" {
				opts.table.columns = append(opts.table.columns, c)
			}
		}
		if len(*f.columns) > 0 && len(opts.table.columns) == 0 {
			return usageErrorf("--columns names no columns")
		}
	}
	if len(opts.table.columns) > 0 && argType(tool, "fields") == "array" {
		if _, given := args["fields"]; !given {
			args["fields"] = opts.table.columns
		}
	}
	opts.all = f.all != nil && *f.all
	if opts.all && pageGiven {
		return usageErrorf("--all fetches every page; leave out the page")
	}
	wide := f.wide != nil && *f.wide
	if opts.format == formatTable && !wide && ui.IsTerminal(app.Out) {
		opts.table.width = ui.Width(app.Out)
	}
	return nil
}

// argType is the JSON type the tool's input schema gives the argument, or
// "" when the tool does not take it (or the schema is not known).
func argType(tool mcp.Tool, name string) string {
	props, _ := tool.InputSchema["properties"].(map[string]any)
	spec, ok := props[name].(map[string]any)
	if !ok {
		return ""
	}
	if kind, _ := spec["type"].(string); kind != "" {
		return kind
	}
	return "any"
}
