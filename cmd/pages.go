package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/ui"
)

// allPageSize is how many rows --all asks for at a time: the most the
// server's list tools send in one page.
const allPageSize = 200

// pageSize is allPageSize, or less when the tool's schema says its limit
// goes no higher.
func pageSize(tool mcp.Tool) int {
	props, _ := tool.InputSchema["properties"].(map[string]any)
	spec, _ := props["limit"].(map[string]any)
	if most, ok := spec["maximum"].(float64); ok && most >= 1 && most < allPageSize {
		return int(most)
	}
	return allPageSize
}

// runAllPages is --all: it calls tool for page 1, 2 and on until the
// paging meta says there are no more, and prints the rows of every page as
// one list. Each page is a call like any other, so callTool's retries
// apply to each: a busy server is waited out, and a read-only tool is sent
// again after a proxy's error, which keeps one bad moment from ending a
// long run. With --json the rows print as NDJSON, a line per row as each
// page arrives, so a script can work through a long list as it comes.
func runAllPages(ctx context.Context, app *App, c *api.Client, tool mcp.Tool, args map[string]any, opts renderOptions, bg *toolRefresh) error {
	args = maps.Clone(args)
	if _, set := args["limit"]; !set && argType(tool, "limit") != "" {
		args["limit"] = pageSize(tool)
	}
	opts.nextPage = nil // every page is here
	var rows []any
	noted := map[string]bool{}
	for page := 1; ; page++ {
		args["page"] = page
		out, err := callForOutput(ctx, app, c, tool, args, bg)
		if err != nil {
			if page > 1 {
				return fmt.Errorf("page %d: %w", page, err)
			}
			return err
		}
		// A note such as the Demo notice comes with every page; once is
		// enough.
		for _, note := range out.shown {
			if !noted[note] {
				noted[note] = true
				fmt.Fprintln(app.Err, ui.SafeText(note))
			}
		}
		list, isList := out.Data.([]any)
		switch {
		case !isList && page == 1:
			// Not a list after all, so there is nothing to page through.
			return printToolOutput(app, tool, out, opts)
		case !isList:
			return fmt.Errorf("page %d did not answer with rows, as the pages before it did", page)
		}
		if sent, ok := out.Meta["page"].(float64); ok && int(sent) != page {
			// A server that ignores the page would repeat page 1 for ever.
			return fmt.Errorf("asked for page %d, the server sent page %d", page, int(sent))
		}
		if app.JSONOutput {
			if err := app.writeRows(list); err != nil {
				return err
			}
		} else {
			rows = append(rows, list...)
		}
		pages, _ := out.Meta["total_pages"].(float64)
		if len(list) == 0 || float64(page) >= pages {
			break
		}
	}
	if app.JSONOutput {
		return nil
	}
	// The count is of the rows printed: the list may have changed between
	// pages.
	merged := toolOutput{Data: rows, Meta: map[string]any{"total_count": float64(len(rows))}}
	if rows == nil {
		merged.Data = []any{}
	}
	return printToolOutput(app, tool, merged, opts)
}

// writeRows prints the rows of a page as --all --json does: a line of JSON
// each, or with --jq what the expression makes of each row, as it would of
// the rows piped from --json to jq.
func (a *App) writeRows(rows []any) error {
	if a.jq == nil {
		return writeNDJSON(a.Out, rows)
	}
	for _, row := range rows {
		if err := a.printFiltered(row); err != nil {
			return err
		}
	}
	return nil
}

// writeNDJSON writes each row as one line of JSON, with the terminal
// controls encoding/json leaves raw escaped as PrintJSON escapes them.
func writeNDJSON(w io.Writer, rows []any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	_, err := w.Write(ui.SafeJSON(buf.Bytes()))
	return err
}
