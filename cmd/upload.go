package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// firmfact upload sends documents to a workspace for its intake to read
// (section 15 of the MCP and CLI parity plan), in three steps:
//
//  1. find the files the arguments name and hash each once;
//  2. ask the server what it would do with them (the preflight, which
//     sends no bytes) and print that plan: what is new, what is already
//     in the workspace, what it refuses and why, the target workspace and
//     the allowance left;
//  3. send one request at a time, a file or a group of related files, and
//     wait until the server has read each document, then print what it
//     read.
//
// Nothing is booked: an upload creates documents, and records appear only
// when someone publishes a document on its review page. Every string the
// server sends goes through ui.SafeLine or ui.SafeText before it is shown.

// uploadFlags are the flags of firmfact upload.
type uploadFlags struct {
	recursive   bool
	related     bool
	name        string
	newVersion  bool
	noWait      bool
	waitTimeout time.Duration
	yes         bool
}

// defaultUploadWait is how long an upload waits for firmfact to read what
// it sent. A page of an invoice takes seconds; a queue of other tenants'
// documents, or a long contract, takes minutes.
const defaultUploadWait = 15 * time.Minute

func newUploadCommand(app *App) *cobra.Command {
	var f uploadFlags
	cmd := &cobra.Command{
		Use:   "upload <file, folder or pattern>... | -",
		Short: "Upload invoices, contracts and other documents for firmfact to read",
		Long: `Upload invoices, contracts and other documents to a workspace for firmfact
to read, and see what it read: the vendor, the amounts, the contract an
invoice matches, a preview of how it compares with that contract, and what
needs a person on the document's review page. For a spreadsheet or an HR
file, a line for each type of record says how many rows publishing would
add, change, leave as they are, or leave for a person to match. Nothing is
booked until someone publishes it there.

Name files, folders (with --recursive) or patterns such as '*.pdf', which
the CLI expands where the shell did not (on Windows, whatever the case).
Hidden files, and the ~$ lock files Office keeps beside an open document,
are left out, and so is a file in a folder or a pattern that firmfact does
not read, before it is read. A link in a folder counts only when it leads
to a file in that folder. The argument - reads one file from standard
input, and --name names it. Firmfact reads PDFs, images, Word and Excel
files, CSV, text, email (.eml) and ZIP files, each under 50 MB.

Before it sends anything, the CLI asks firmfact which files are new, which
are already in the workspace and how much of this month's allowance of
documents is left, and prints that plan. It then sends the new files one
at a time, and waits (up to --wait-timeout) until firmfact has read them.
A file already in the workspace is not sent again, so running the same
command twice is safe; --new-version sends it all the same, as a new
version.

--related sends the files as one group of related documents, such as an
invoice and its usage report, which counts once against the allowance: at
most 10 files and 100 MB together.

A new sign-up's default workspace is Demo, which is rebuilt from sample
data from time to time, and a rebuild removes what you upload there. So
when neither --workspace nor FIRMFACT_WORKSPACE names the workspace and the
upload would go to Demo, the CLI asks first; off a terminal, where there is
no one to ask, it needs --yes, and exits with status 2 without it.

Exit status: 0 when every file was uploaded or was already there, and was
read (or, with --no-wait, sent); 1 when a file was refused or could not be
read; 2 for a mistake on the command line, such as a pattern or folders
with no files to upload, or an unnamed Demo workspace off a terminal; 3
when not signed in; 4 when the workspace does not exist; 5 when the wait
ran out, or firmfact was busy or rate-limited (run the command again:
files already there are skipped); 6 when the host does not offer uploads
yet. A file called status is ./status, as upload status is the command
below.`,
		Example: fmt.Sprintf(`  %[1]s upload LSEG-2026-09.pdf --workspace Acme
  %[1]s upload ~/Invoices/2026-09 --recursive --workspace Acme
  %[1]s upload invoice.pdf usage-report.xlsx --related
  %[1]s upload '*.pdf' --no-wait --json
  scanimage --format=pdf | %[1]s upload - --name scan-0034.pdf --workspace Acme`, app.Name),
		Args: cobra.ArbitraryArgs,
		// Files, folders and patterns: the shell's own completion.
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.check(args); err != nil {
				return err
			}
			return runUpload(cmd.Context(), app, f, args)
		},
	}
	cmd.Flags().BoolVarP(&f.recursive, "recursive", "r", false, "upload the files in the folders named, and in their folders")
	cmd.Flags().BoolVar(&f.related, "related", false, "send the files as one group of related documents, counted once against the allowance (at most 10 files and 100 MB)")
	cmd.Flags().StringVar(&f.name, "name", "", "the file name, with its extension, for the file read from standard input (-)")
	cmd.Flags().BoolVar(&f.newVersion, "new-version", false, "send files that are already in the workspace again, as new versions")
	cmd.Flags().BoolVar(&f.noWait, "no-wait", false, "do not wait for firmfact to read the documents")
	cmd.Flags().DurationVar(&f.waitTimeout, "wait-timeout", defaultUploadWait, "how long to wait for firmfact to read the documents")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "upload to the Demo workspace without asking, when no workspace is named")
	_ = cmd.RegisterFlagCompletionFunc("name", cobra.NoFileCompletions)
	_ = cmd.RegisterFlagCompletionFunc("wait-timeout", completeWaitTimeout)
	cmd.AddCommand(newUploadStatusCommand(app))
	return cmd
}

// completeWaitTimeout offers a few waits for --wait-timeout.
func completeWaitTimeout(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return []cobra.Completion{"5m", "15m", "30m", "1h"}, cobra.ShellCompDirectiveNoFileComp
}

// check refuses a command line that cannot run as typed, before anything
// is read or sent.
func (f uploadFlags) check(args []string) error {
	if len(args) == 0 {
		return usageErrorf("name the files to upload: files, folders with --recursive, patterns such as '*.pdf', or - for standard input")
	}
	stdin := 0
	for _, arg := range args {
		if arg == upload.Stdin {
			stdin++
		}
	}
	switch {
	case stdin > 1:
		return usageErrorf("- reads standard input, which holds one file; name it once")
	case stdin == 1 && f.name == "":
		return usageErrorf("- needs --name with the file's name and extension, such as --name invoice.pdf: firmfact tells a file's type by its extension")
	case stdin == 0 && f.name != "":
		return usageErrorf("--name names the file read from standard input; add - to read one, or leave --name out")
	case f.waitTimeout <= 0:
		return usageErrorf("--wait-timeout must be more than 0; use --no-wait not to wait")
	}
	return nil
}

// runUpload is firmfact upload after its command line has been checked.
func runUpload(ctx context.Context, app *App, f uploadFlags, args []string) error {
	found, err := upload.Expand(args, f.recursive)
	if err != nil {
		return expandError(app, err)
	}
	c, err := app.Client()
	if err != nil {
		return err
	}
	r := &uploadRun{app: app, flags: f, uc: &upload.Client{API: c}, found: found, out: app.Out}
	if app.JSONOutput {
		r.progress = app.Err
	} else {
		r.progress = app.Out
	}
	r.live = liveLine{w: r.progress, on: app.attended() && ui.Escapes(r.progress)}
	// Standard input's copy goes once the run is over, however it ends.
	defer r.cleanup()
	if err := r.target(ctx, c); err != nil {
		return err
	}
	if err := r.read(ctx); err != nil {
		return err
	}
	if err := r.send(ctx); err != nil {
		if errors.Is(err, errDeclined) {
			fmt.Fprintln(r.out, "Nothing was uploaded.")
			return nil
		}
		return r.interrupted(ctx, err)
	}
	if err := r.finish(ctx); err != nil {
		return r.interrupted(ctx, err)
	}
	if err := r.print(); err != nil {
		return err
	}
	return r.result()
}

// expandError says what is wrong with the files an upload names; each is
// the command line's mistake, as nothing has been read or sent.
func expandError(app *App, err error) error {
	var (
		folder *upload.FolderError
		none   *upload.NoMatchError
		path   *fs.PathError
	)
	switch {
	case errors.As(err, &folder):
		return usageErrorf("%s is a folder; add --recursive to upload the files in it", ui.SafeLine(folder.Path))
	case errors.As(err, &none):
		return usageErrorf("no files match %s", ui.SafeLine(none.Pattern))
	case errors.As(err, &path) && errors.Is(err, fs.ErrNotExist):
		if path.Path == "status" {
			// upload status with a typo in a flag would be read as a file
			// of that name, but status itself never is.
			return usageErrorf("no such file: %s; for the status of your uploads, run `%s upload status`", ui.SafeLine(path.Path), app.Name)
		}
		return usageErrorf("no such file: %s", ui.SafeLine(path.Path))
	}
	return err
}

// uploadRun is one run of firmfact upload.
type uploadRun struct {
	app   *App
	flags uploadFlags
	uc    *upload.Client
	found *upload.Found

	// ref is what requests name the workspace by: what the command line
	// gave, until the first preflight says its id.
	ref string
	// named is set when the command line named the workspace (--workspace
	// or FIRMFACT_WORKSPACE); otherwise the profile's or the sign-in's
	// default applies, and Demo needs a yes.
	named bool
	// demo is set when the target is a Demo workspace, as far as the CLI
	// knows before the first preflight.
	demo      bool
	workspace upload.Workspace
	// allowance is the monthly allowance as the first preflight found it.
	allowance *upload.Allowance

	files []*uploadFile
	// spooled is standard input's copy, removed once the run is over.
	spooled *upload.Spooled
	// stop is why sending stopped before every file went, if it did.
	stop *uploadStop
	// timedOut is set when the wait ran out with documents still being
	// read.
	timedOut bool
	// sent is whether any request went out, which an interrupt then
	// reports.
	sent bool
	// sendTotal and sendDone count the files to send and those sent, for
	// the live line.
	sendTotal, sendDone int
	// asked is set once confirm has had its say, before the first request.
	asked bool
	// unread is why what firmfact made of the documents could not be read
	// back after they were sent, if it could not; the results then show
	// what the upload's own answers said.
	unread error
	// cut is set when an interrupt stopped the upload.
	cut bool
	// schema is the version of the document entries, as the server named
	// it in its answers.
	schema string

	out      io.Writer // results: stdout
	progress io.Writer // the plan's wait messages: stdout, or stderr with --json
	live     liveLine
}

// target settles which workspace the upload goes to. Named on the command
// line, it is the server's to find, in the first preflight. Otherwise it
// is the profile's default or the sign-in's, which /api/v1/cli/me says is
// a Demo workspace or not: a sign-up's default is Demo, which a rebuild
// wipes, so an upload there needs a yes, and off a terminal the refusal
// comes before any file is read.
func (r *uploadRun) target(ctx context.Context, c *api.Client) error {
	r.ref = r.app.ChosenWorkspace()
	r.named = r.ref != ""
	if r.named {
		return nil
	}
	me, err := fetchMe(ctx, c)
	if err != nil {
		return err
	}
	ref := r.app.DefaultWorkspace()
	w, ok := findWorkspace(me, ref)
	if !ok {
		if ref == "" {
			return withExit(ExitNotFound, fmt.Errorf("this sign-in has no default workspace; name one with --workspace, see `%s workspaces list`", r.app.Name))
		}
		return withExit(ExitNotFound, fmt.Errorf("no workspace %q; see `%s workspaces list`", ui.SafeLine(ref), r.app.Name))
	}
	r.ref, r.demo = w.ID, w.Demo
	r.workspace = upload.Workspace{ID: w.ID, Name: w.Name, Demo: w.Demo}
	if w.Demo && !r.flags.yes && !r.app.attended() {
		return usageErrorf("without --workspace, this upload would go to %s, a Demo workspace, which is rebuilt from sample data from time to time and loses what you upload; "+
			"name the workspace with --workspace (or FIRMFACT_WORKSPACE), or add --yes to upload to %s all the same", ui.SafeLine(w.Name), ui.SafeLine(w.Name))
	}
	return nil
}

// read reads standard input, when an argument names it, and hashes every
// file once. A file with the same bytes as one before it goes once. A file
// a folder or a pattern found is not read at all when firmfact does not
// read its type, and no file is read past the most firmfact takes: neither
// goes into a preflight, so the name, size and checksum of whatever else
// a folder holds never leave the machine.
func (r *uploadRun) read(ctx context.Context) error {
	for _, s := range r.found.Sources {
		if s.Path != upload.Stdin {
			continue
		}
		if in, ok := r.app.In.(*os.File); ok && term.IsTerminal(int(in.Fd())) {
			return r.stdinFromTerminal()
		}
		spooled, err := upload.Spool(r.app.In, "", upload.MaxFileBytes)
		var tooLarge *upload.TooLargeError
		switch {
		case errors.As(err, &tooLarge):
			return fmt.Errorf("standard input holds more than %s, the most one file may be", ui.Bytes(tooLarge.Limit+1))
		case err != nil:
			return err
		}
		r.spooled = spooled
	}
	defer r.live.clear()
	bySum := map[string]*uploadFile{}
	for i, s := range r.found.Sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := s.Path
		if path == upload.Stdin {
			path = r.spooled.Path
		}
		f := &uploadFile{src: s}
		r.files = append(r.files, f)
		if !s.Named && !upload.Readable(path) {
			f.settle(path)
			f.skip("unsupported_type", "firmfact does not read files of this type")
			continue
		}
		if len(r.found.Sources) > 1 {
			r.live.show(fmt.Sprintf("Reading %d of %d files: %s", i+1, len(r.found.Sources), f.display()))
		}
		file, err := upload.Hash(path)
		var tooLarge *upload.TooLargeError
		switch {
		case errors.As(err, &tooLarge):
			f.settle(path)
			f.set(outcomeRefused, "file_too_large", tooLargeMessage(f), ExitFailed)
			continue
		case err == nil:
			err = s.Verify(file)
		}
		if err != nil {
			return fmt.Errorf("could not read %s: %w", f.display(), err)
		}
		if s.Path == upload.Stdin {
			file.Name = r.flags.name
		}
		f.file, f.name = file, file.Name
		if first, ok := bySum[file.SHA256]; ok {
			f.skip("same_contents", "the same as "+first.display())
			f.local = true
		} else {
			bySum[file.SHA256] = f
		}
	}
	if len(r.files) == 0 {
		return usageErrorf("%s", r.nothingFound())
	}
	return nil
}

// stdinFromTerminal is the mistake of - with nothing piped to it. The
// example redirects a file, which PowerShell cannot: there, the file is
// named instead.
func (r *uploadRun) stdinFromTerminal() error {
	if runtimeOS == "windows" {
		return usageErrorf("- reads the file from a pipe, not from the terminal; to upload a file, name it: such as `%s upload %s`", r.app.Name, shellWord(r.flags.name, "<file>"))
	}
	return usageErrorf("- reads the file from a pipe or a redirect, not from the terminal: such as `%s upload - --name %s < %s`", r.app.Name, shellWord(r.flags.name, "<name>"), shellWord(r.flags.name, "<file>"))
}

// runtimeOS is the system the CLI runs on; tests stand in for others.
var runtimeOS = runtime.GOOS

// settle records a file the CLI settles without reading it: its name and,
// from the file system, its size.
func (f *uploadFile) settle(path string) {
	f.file = upload.File{Path: path, Name: filepath.Base(path)}
	if info, err := os.Stat(path); err == nil {
		f.file.Size = info.Size()
	}
	f.name = f.file.Name
	f.local = true
}

// tooLargeMessage says why f, over the most one file may hold, is refused
// before it is read, as the service would.
func tooLargeMessage(f *uploadFile) string {
	limit := ui.Bytes(upload.MaxFileBytes + 1)
	if f.file.Size > upload.MaxFileBytes {
		return fmt.Sprintf("%s: files of %s or more are not supported, and this one is %s", f.label(), limit, ui.Bytes(f.file.Size))
	}
	return fmt.Sprintf("%s: files of %s or more are not supported, and this one holds more", f.label(), limit)
}

// nothingFound says why the arguments named no file to upload.
func (r *uploadRun) nothingFound() string {
	if n := len(r.found.Folders); n > 0 {
		return fmt.Sprintf("nothing to upload: the %s matched %s; add --recursive to upload the files in %s", plural(n, "pattern", "patterns"), plural(n, "a folder", "folders only"), plural(n, "it", "them"))
	}
	var but []string
	if r.found.Hidden > 0 {
		but = append(but, "hidden ones")
	}
	if r.found.Outside > 0 {
		but = append(but, "links that lead out of them")
	}
	if len(but) > 0 {
		return "nothing to upload: the folders and patterns named hold no files but " + strings.Join(but, " and ")
	}
	return "nothing to upload: the folders and patterns named hold no files"
}

// cleanup removes standard input's copy.
func (r *uploadRun) cleanup() {
	if r.spooled != nil {
		r.spooled.Remove()
	}
}

// interrupted tidies up after err stopped the run, and says, for a person
// who pressed Ctrl-C, what was sent before that.
func (r *uploadRun) interrupted(ctx context.Context, err error) error {
	r.live.clear()
	if ctx.Err() == nil || !r.sent {
		return err
	}
	r.cut = true
	if r.app.JSONOutput {
		// A script learns what was stored before the interrupt, and what
		// was not sent, as from a run that ended.
		if printErr := r.app.PrintJSON(r.json()); printErr != nil {
			return printErr
		}
		return err
	}
	ui.EndInterruptedLine(r.out)
	stored := 0
	for _, f := range r.files {
		if f.outcome == outcomeCreated {
			stored++
		}
	}
	switch {
	case r.pending() == 0:
		fmt.Fprintf(r.out, "Stopped waiting. Firmfact reads the documents all the same; see what it read with `%s`.\n", r.statusCommand(r.documentIDs()))
	case stored > 0:
		fmt.Fprintf(r.out, "Stopped. %d %s uploaded before that, and firmfact reads %s all the same; see what it read with `%s`.\n",
			stored, plural(stored, "file was", "files were"), plural(stored, "it", "them"), r.statusCommand(r.documentIDs()))
		fallthrough
	default:
		fmt.Fprintln(r.out, "Run the same command again to send the rest: files already there are skipped.")
	}
	return err
}

// statusCommand is the command that shows the documents with ids, or the
// recent uploads when there are more than a line holds.
func (r *uploadRun) statusCommand(ids []string) string {
	command := r.app.Name + " upload status"
	if r.named && r.workspace.ID != "" {
		command += " --workspace " + shellWord(ui.SafeLine(r.workspace.ID), "<workspace>")
	}
	if len(ids) > 0 && len(ids) <= 3 {
		for _, id := range ids {
			command += " " + argWord(ui.SafeLine(id), "<id>")
		}
	}
	return command
}

// documentIDs are the ids of the documents this run follows, each once.
func (r *uploadRun) documentIDs() []string {
	var ids []string
	for _, d := range r.documents() {
		ids = append(ids, d.ID)
	}
	return ids
}

// pending is how many files are still to be sent.
func (r *uploadRun) pending() int {
	n := 0
	for _, f := range r.files {
		if f.outcome == "" || f.outcome == outcomeNotSent {
			n++
		}
	}
	return n
}

// finish waits for the documents to be read, unless --no-wait, and reads
// back what firmfact made of the caller's own finished ones. Once files
// are stored, the report of them must not be lost: a read that fails is
// kept in unread, for the results to say, and only an interrupt ends the
// run here.
func (r *uploadRun) finish(ctx context.Context) error {
	// An upload stopped by its sign-in or its workspace could not read
	// the documents back either.
	if r.stop != nil && r.stop.err != nil {
		return nil
	}
	docs := r.documents()
	if !r.flags.noWait {
		w := documentWait{uc: r.uc, workspace: r.ref, timeout: r.flags.waitTimeout, progress: r.progress, live: &r.live}
		timedOut, err := w.wait(ctx, docs)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			r.unread = err
			return nil
		}
		r.timedOut = timedOut
	}
	schema, err := readResults(ctx, r.uc, r.ref, docs)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		r.unread = err
	}
	r.schema = orDefault(schema, r.schema)
	return nil
}

// documents are the documents this run follows, each once. Files that
// turned out to be the same document share one copy of it from here on,
// so that what the wait and the reads find reaches each of them.
func (r *uploadRun) documents() []*upload.Document {
	var docs []*upload.Document
	seen := map[string]*upload.Document{}
	for _, f := range r.files {
		switch d, ok := seen[docID(f.doc)]; {
		case f.doc == nil:
		case ok:
			f.doc = d
		default:
			seen[f.doc.ID] = f.doc
			docs = append(docs, f.doc)
		}
	}
	return docs
}

func docID(d *upload.Document) string {
	if d == nil {
		return ""
	}
	return d.ID
}

// print shows the results: JSON with --json, else the documents as
// blocks, or as a table when there are more than a few.
func (r *uploadRun) print() error {
	if r.app.JSONOutput {
		return r.app.PrintJSON(r.json())
	}
	r.printResults()
	return nil
}
