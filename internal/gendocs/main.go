// Command gendocs writes the shell completion scripts and man pages that
// the release archives carry, made from the CLI's own command tree:
//
//	go run ./internal/gendocs    # into completions/ and manpages/
//
// The release workflow runs it before GoReleaser, as a step of its own: a
// GoReleaser before hook would run it with the release token and signing
// key in its environment. What it writes depends on the commit alone, so
// the archives stay reproducible: the man pages are dated by the commit
// (SOURCE_DATE_EPOCH, else git's commit time), never by the clock.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
	"github.com/spf13/pflag"

	"github.com/firmfact/cli/cmd"
)

func main() {
	out := flag.String("out", ".", "the directory to write completions/ and manpages/ in")
	flag.Parse()
	date, err := sourceDate(os.Getenv("SOURCE_DATE_EPOCH"), commitTime)
	if err == nil {
		err = generate(*out, date)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

// sourceDate is the date the man pages give: SOURCE_DATE_EPOCH, as
// reproducible builds set it, else the time of the commit being built.
func sourceDate(epoch string, commit func() (string, error)) (time.Time, error) {
	if epoch == "" {
		var err error
		if epoch, err = commit(); err != nil {
			return time.Time{}, fmt.Errorf("no SOURCE_DATE_EPOCH, and git could not say when the commit was made: %w", err)
		}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(epoch), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q is not a number of seconds", epoch)
	}
	// UTC, so the month does not depend on the build machine's time zone.
	return time.Unix(secs, 0).UTC(), nil
}

// commitTime is the committer time of HEAD, in seconds since 1970.
func commitTime() (string, error) {
	out, err := exec.CommandContext(context.Background(), "git", "log", "-1", "--format=%ct").Output()
	return string(out), err
}

// The scripts are named as each shell looks for them: bash-completion and
// fish by the command's name, zsh by the name with an underscore in front.
// The Homebrew cask links them into place; see .goreleaser.yaml.
var completions = map[string]func(root *cobra.Command, w io.Writer) error{
	"firmfact.bash": func(root *cobra.Command, w io.Writer) error { return root.GenBashCompletionV2(w, true) },
	"_firmfact":     (*cobra.Command).GenZshCompletion,
	"firmfact.fish": func(root *cobra.Command, w io.Writer) error { return root.GenFishCompletion(w, true) },
	"firmfact.ps1":  (*cobra.Command).GenPowerShellCompletionWithDesc,
}

// generate writes completions/ and manpages/ under out, replacing what
// was there, so a command that is gone leaves no page behind.
func generate(out string, date time.Time) error {
	completionDir := filepath.Join(out, "completions")
	manDir := filepath.Join(out, "manpages")
	for _, dir := range []string{completionDir, manDir} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}

	root := cmd.NewReferenceCommand("dev")
	for name, gen := range completions {
		var buf bytes.Buffer
		if err := gen(root, &buf); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := writeFile(filepath.Join(completionDir, name), buf.Bytes()); err != nil {
			return err
		}
	}
	return writeManPages(root, manDir, date)
}

// writeManPages writes a page for every command, firmfact-workspaces-use.1
// for `firmfact workspaces use`, and one for each help topic, such as
// firmfact-environment.1.
func writeManPages(root *cobra.Command, dir string, date time.Time) error {
	// cobra adds --version when the command runs, which it does not here.
	root.InitDefaultVersionFlag()
	root.Long += "\n\nThe workspace commands, such as `firmfact vendors list`, are not in these " +
		"pages: they come from the firmfact service once you sign in, which also keeps them " +
		"up to date. `firmfact --help` lists them and `firmfact <command> --help` describes one. " +
		"**firmfact-environment(1)** lists the environment variables."
	var topics []*cobra.Command
	flags := map[*pflag.Flag]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		// The pages would otherwise end with the day they were made.
		c.DisableAutoGenTag = true
		if name, args, ok := strings.Cut(c.Use, " "); ok {
			c.Use = name + " " + markdownText(args)
		}
		c.Short = markdownText(c.Short)
		c.Long = markdownLong(c.Long)
		for _, set := range []*pflag.FlagSet{c.LocalNonPersistentFlags(), c.PersistentFlags()} {
			set.VisitAll(func(f *pflag.Flag) {
				if !flags[f] {
					flags[f] = true
					f.Usage = markdownText(f.Usage)
				}
			})
		}
		if c.IsAdditionalHelpTopicCommand() {
			topics = append(topics, c)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	header := &doc.GenManHeader{Section: "1", Date: &date, Source: "firmfact", Manual: "Firmfact manual"}
	if err := doc.GenManTree(root, header, dir); err != nil {
		return err
	}
	for _, topic := range topics {
		if err := writeTopicPage(root, topic, *header, dir); err != nil {
			return err
		}
	}
	return nil
}

// writeTopicPage writes the page of a help topic, which cobra's tree
// leaves out. It is text to read, not a command to run, so the page is
// made from a copy of the topic without the flags every command takes.
func writeTopicPage(root, topic *cobra.Command, header doc.GenManHeader, dir string) error {
	page := &cobra.Command{Use: topic.Use, Short: topic.Short, Long: topic.Long, DisableAutoGenTag: true}
	page.Flags().BoolP("help", "h", false, "")
	_ = page.Flags().MarkHidden("help") // the flag was just defined
	(&cobra.Command{Use: root.Name(), DisableAutoGenTag: true}).AddCommand(page)
	var buf bytes.Buffer
	if err := doc.GenMan(page, &header, &buf); err != nil {
		return err
	}
	name := strings.ReplaceAll(page.CommandPath(), " ", "-") + "." + header.Section
	return writeFile(filepath.Join(dir, name), buf.Bytes())
}

// markdownLong makes a command's long help Markdown that shows as it does
// in the terminal. A line indented by two spaces is part of a block laid
// out by hand, such as a list of variables or an example, and becomes a
// code block, which the page keeps as it is instead of running it into a
// paragraph; a tab already makes one. Other lines are text.
func markdownLong(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "\t"):
		case strings.HasPrefix(line, "  "):
			lines[i] = "    " + line
		default:
			lines[i] = markdownText(line)
		}
	}
	return strings.Join(lines, "\n")
}

// markdownText escapes a placeholder such as <name>, which Markdown would
// take for HTML and drop, except in `code`, where it shows as it is.
func markdownText(text string) string {
	var b strings.Builder
	code := false
	for _, r := range text {
		switch {
		case r == '`':
			code = !code
		case r == '<' && !code:
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func writeFile(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New(path + " came out empty")
	}
	// Published in the archives, so readable by all.
	return os.WriteFile(path, data, 0o644) //nolint:gosec // G306, see above
}
