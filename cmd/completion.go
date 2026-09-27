package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/ui"
)

// Tab completion. cobra completes the names of commands and flags, and
// `firmfact completion <shell>` prints the script that asks the CLI for the
// rest; the release archives carry those scripts too (see
// internal/gendocs). On top of that the CLI completes the values it knows
// without asking the server, as a Tab must answer at once, offline too:
// profile names, the values a workspace command's flag takes according to
// the tool's schema, and the names of the workspaces /api/v1/cli/me last
// listed for the host, which is kept in the cache for this.

// completeSchemaValues registers the values a tool's flag takes as its
// completion: the schema's enum; for a list, its items' enum, less the
// values earlier uses of the flag gave; and true or false for a boolean,
// which only a Tab after --flag= asks for. Any other flag completes to
// nothing rather than to file names, which no tool takes.
func completeSchemaValues(cmd *cobra.Command, flag, kind string, spec map[string]any) {
	values, list := schemaValues(kind, spec)
	complete := cobra.NoFileCompletions
	if len(values) > 0 {
		complete = valueCompletion(flag, values, list)
	}
	// Two properties may come out as the same flag; the first keeps its
	// completion, as it keeps the flag.
	_ = cmd.RegisterFlagCompletionFunc(flag, complete)
}

// schemaValues are the values the schema of a flag of this kind allows,
// and whether the flag takes a list of them. A value the terminal would
// not show as it is cannot be typed as it is either, so it is left out.
func schemaValues(kind string, spec map[string]any) (values []string, list bool) {
	enum, _ := spec["enum"].([]any)
	if kind == "array" {
		items, _ := spec["items"].(map[string]any)
		enum, _ = items["enum"].([]any)
		list = true
	}
	for _, v := range enum {
		s := fmt.Sprint(v)
		if s != "" && ui.SafeLine(s) == s {
			values = append(values, s)
		}
	}
	if len(values) == 0 && kind == "boolean" {
		values = []string{"true", "false"}
	}
	return values, list
}

// valueCompletion offers the values of flag. A list flag with values to
// offer takes one each time it is given, or several separated by commas
// (see listItems), so the values given already, which cobra has parsed by
// the time it asks, are not offered again.
func valueCompletion(flag string, values []string, list bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		chosen := map[string]bool{}
		if list {
			given, _ := cmd.Flags().GetStringArray(flag)
			enum := make([]any, len(values))
			for i, v := range values {
				enum[i] = v
			}
			for _, v := range given {
				chosen[v] = true
				parts, _ := listItems(map[string]any{"enum": enum}, v)
				for _, part := range parts {
					chosen[part] = true
				}
			}
		}
		var out []cobra.Completion
		for _, v := range values {
			if !chosen[v] {
				out = append(out, v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeFormatFlag completes --format.
func completeFormatFlag(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return []cobra.Completion{
		cobra.CompletionWithDesc(string(formatTable), "a table for people (the default)"),
		cobra.CompletionWithDesc(string(formatJSON), "JSON, as --json"),
		cobra.CompletionWithDesc(string(formatCSV), "comma-separated rows"),
		cobra.CompletionWithDesc(string(formatTSV), "tab-separated rows"),
	}, cobra.ShellCompDirectiveNoFileComp
}

// workspaceList is what the cache keeps of /api/v1/cli/me for a host: the
// id and name of each workspace, which is all completion needs. The rest,
// such as who is signed in, stays out of it.
type workspaceList struct {
	Host       string            `json:"host"`
	Workspaces []listedWorkspace `json:"workspaces"`
}

type listedWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func workspaceListPath(host string) (string, error) {
	dir, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir, "workspaces-"+hex.EncodeToString(sum[:6])+".json"), nil
}

// rememberWorkspaces keeps the workspaces /api/v1/cli/me just listed for
// host, for completion. Whichever command asked (login, whoami, workspaces
// list, doctor and more) brings the list up to date.
func rememberWorkspaces(host string, workspaces []Workspace) error {
	path, err := workspaceListPath(host)
	if err != nil {
		return err
	}
	list := workspaceList{Host: host, Workspaces: make([]listedWorkspace, 0, len(workspaces))}
	for _, w := range workspaces {
		list.Workspaces = append(list.Workspaces, listedWorkspace{ID: w.ID, Name: w.Name})
	}
	return config.WriteJSON(path, list, 0o600)
}

// forgetWorkspaces drops the list for host, on logout: the names belong to
// the sign-in that ended.
func forgetWorkspaces(host string) error {
	path, err := workspaceListPath(host)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func loadWorkspaces(host string) []listedWorkspace {
	path, err := workspaceListPath(host)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var list workspaceList
	if json.Unmarshal(raw, &list) != nil || list.Host != host {
		return nil
	}
	return list.Workspaces
}

// completeWorkspaceFlag completes --workspace with the names of the
// workspaces last listed for the host the command line aims at.
func (a *App) completeWorkspaceFlag(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	host, err := a.Host()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return workspaceCompletions(loadWorkspaces(host)), cobra.ShellCompDirectiveNoFileComp
}

// completeWorkspaceArg completes a command's first argument, when it is a
// workspace, as completeWorkspaceFlag does.
func (a *App) completeWorkspaceArg(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return a.completeWorkspaceFlag(cmd, args, toComplete)
}

// workspaceCompletions offers each workspace by name, with its id beside
// it. A name that others share, or one that cannot be typed as it is
// printed, does not say which workspace it means, so those are offered by
// id instead, with the name beside it.
func workspaceCompletions(workspaces []listedWorkspace) []cobra.Completion {
	named := map[string]int{}
	for _, w := range workspaces {
		named[strings.ToLower(strings.TrimSpace(w.Name))]++
	}
	var out []cobra.Completion
	for _, w := range workspaces {
		name, id := ui.SafeLine(w.Name), ui.SafeLine(w.ID)
		if strings.TrimSpace(w.Name) != "" && name == w.Name && named[strings.ToLower(strings.TrimSpace(w.Name))] == 1 {
			out = append(out, cobra.CompletionWithDesc(name, id))
			continue
		}
		switch {
		case w.ID == "" || id != w.ID:
		case strings.TrimSpace(name) == "":
			out = append(out, id)
		default:
			out = append(out, cobra.CompletionWithDesc(id, name))
		}
	}
	return out
}
