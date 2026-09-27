package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// uploadID is the shape of a document's id.
var uploadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// defaultRecentUploads is how many recent uploads the server lists when
// asked for none in particular.
const defaultRecentUploads = 20

func newUploadStatusCommand(app *App) *cobra.Command {
	var (
		wait           bool
		waitTimeout    time.Duration
		limit          int
		failOnVariance varianceFlag
	)
	cmd := &cobra.Command{
		Use:   "status [id]...",
		Short: "Show what firmfact read from your uploads",
		Long: `Show what firmfact read from documents you uploaded to a workspace: the
vendor, the amounts, the contract an invoice matches, a preview of how it
compares with that contract, and what needs a person on the review page.

With ids, those documents, in full; --wait waits until firmfact has read
each of them, for up to --wait-timeout. Without, your most recent uploads,
newest first, with their ids (--limit of them, 20 by default). A document
someone else uploaded shows its state and link only.

` + varianceHelp("It takes the ids of the documents to check, and waits for them as --wait does.") + `

The workspace is the one commands use (--workspace, FIRMFACT_WORKSPACE or
the profile's), else the sign-in's default. Exit status: 4 when an id is not
a document in the workspace; with --wait, 1 when a document could not be
read and 5 when the wait ran out, or a document was still being read after
it; 9 when --fail-on-variance finds an invoice over its threshold.`,
		Example: fmt.Sprintf(`  %[1]s upload status
  %[1]s upload status 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d --wait
  %[1]s upload status 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d --fail-on-variance=50
  %[1]s upload status --workspace Acme --limit 50 --json`, app.Name),
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]string, len(args))
			for i, arg := range args {
				if !uploadID.MatchString(arg) {
					if hint := failOnVariance.thresholdHint(arg); hint != "" {
						return usageErrorf("%s is not a document id%s", ui.SafeLine(arg), hint)
					}
					return usageErrorf("%s is not a document id; ids look like 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d, and `%s upload status` lists yours", ui.SafeLine(arg), app.Name)
				}
				ids[i] = strings.ToLower(arg)
			}
			switch {
			case failOnVariance.on && len(ids) == 0:
				return usageErrorf("--fail-on-variance needs the ids of the documents to check; `%s upload status` lists yours", app.Name)
			case wait && len(ids) == 0:
				return usageErrorf("--wait needs the ids of the documents to wait for; `%s upload status` lists yours", app.Name)
			case cmd.Flags().Changed("limit") && len(ids) > 0:
				return usageErrorf("--limit is for the list of recent uploads; leave it out with ids")
			case limit < 1 || limit > upload.MaxIDs:
				return usageErrorf("--limit must be from 1 to %d", upload.MaxIDs)
			case waitTimeout <= 0:
				return usageErrorf("--wait-timeout must be more than 0")
			}
			// The variance is known once each document has been read.
			wait = wait || failOnVariance.on
			s := &uploadStatus{app: app, wait: wait, waitTimeout: waitTimeout, failOnVariance: failOnVariance}
			if len(ids) == 0 {
				return s.recent(cmd.Context(), limit)
			}
			return s.show(cmd.Context(), ids)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until firmfact has read each document named")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", defaultUploadWait, "how long --wait waits")
	cmd.Flags().IntVar(&limit, "limit", defaultRecentUploads, fmt.Sprintf("how many recent uploads to list, up to %d", upload.MaxIDs))
	addVarianceFlag(cmd, &failOnVariance)
	_ = cmd.RegisterFlagCompletionFunc("wait-timeout", completeWaitTimeout)
	_ = cmd.RegisterFlagCompletionFunc("limit", cobra.NoFileCompletions)
	return cmd
}

// uploadStatus is one run of firmfact upload status.
type uploadStatus struct {
	app            *App
	wait           bool
	waitTimeout    time.Duration
	failOnVariance varianceFlag
	uc             *upload.Client
	// ref is the workspace the documents are read from.
	ref string
}

// connect finds the workspace: the one commands use, else the sign-in's
// default, which the requests need by id.
func (s *uploadStatus) connect(ctx context.Context) error {
	c, err := s.app.Client()
	if err != nil {
		return err
	}
	s.uc = &upload.Client{API: c}
	s.ref = s.app.DefaultWorkspace()
	if s.ref != "" {
		return nil
	}
	me, err := fetchMe(ctx, c)
	if err != nil {
		return err
	}
	w, ok := findWorkspace(me, "")
	if !ok {
		return withExit(ExitNotFound, fmt.Errorf("this sign-in has no default workspace; name one with --workspace, see `%s workspaces list`", s.app.Name))
	}
	s.ref = w.ID
	return nil
}

// statusJSON is what upload status prints with --json: the envelope of
// the workspace commands, with the documents in data.results as the server
// sent them, in document_result/1.
type statusJSON struct {
	Data  statusJSONData `json:"data"`
	Meta  uploadJSONMeta `json:"meta"`
	Notes []string       `json:"notes"`
}

type statusJSONData struct {
	Results []json.RawMessage `json:"results"`
	// Missing are the ids asked for that are not documents in the
	// workspace; only with ids.
	Missing []string `json:"missing,omitempty"`
}

// documentsJSON is docs, and the ids that are not documents here, in the
// schema the server named for them (upload.Schema when it named none).
func documentsJSON(docs []*upload.Document, missing []string, schema string) statusJSON {
	out := statusJSON{Meta: uploadJSONMeta{Schema: orDefault(schema, upload.Schema)}, Notes: []string{}}
	out.Data.Results = make([]json.RawMessage, 0, len(docs))
	for _, d := range docs {
		out.Data.Results = append(out.Data.Results, documentJSON(d))
	}
	out.Data.Missing = missing
	return out
}

// show prints the documents with ids, in full, after waiting for them
// with --wait.
func (s *uploadStatus) show(ctx context.Context, ids []string) error {
	if err := s.connect(ctx); err != nil {
		return err
	}
	list, err := s.uc.Documents(ctx, s.ref, ids, upload.Full)
	if err != nil {
		return err
	}
	docs := make([]*upload.Document, len(list.Results))
	for i := range list.Results {
		docs[i] = &list.Results[i]
	}
	timedOut := false
	if s.wait {
		progress := s.app.Out
		if s.app.JSONOutput {
			progress = s.app.Err
		}
		live := liveLine{w: progress, on: s.app.attended() && ui.Escapes(progress)}
		w := documentWait{uc: s.uc, workspace: s.ref, timeout: s.waitTimeout, progress: progress, live: &live}
		if timedOut, err = w.wait(ctx, docs); err != nil {
			return err
		}
		var schema string
		if schema, err = readResults(ctx, s.uc, s.ref, docs); err != nil {
			return err
		}
		list.Schema = orDefault(schema, list.Schema)
	}
	gate := s.varianceGate(docs)
	if s.app.JSONOutput {
		out := documentsJSON(docs, list.Missing, list.Schema)
		gate.setMeta(&out.Meta)
		if err := s.app.PrintJSON(out); err != nil {
			return err
		}
	} else {
		s.print(docs, list.Missing)
	}
	return gate.result(s.result(docs, list.Missing, timedOut))
}

// varianceGate is --fail-on-variance's verdict on docs, each named by its
// file and listed by its id; nil without the flag.
func (s *uploadStatus) varianceGate(docs []*upload.Document) *varianceGate {
	if !s.failOnVariance.on {
		return nil
	}
	subjects := make([]varianceSubject, len(docs))
	for i, d := range docs {
		name := ui.SafeLine(d.Filename)
		if name == "" {
			name = "document " + ui.SafeLine(d.ID)
		}
		subjects[i] = varianceSubject{name: name, key: d.ID, doc: d}
	}
	return gateVariance(s.failOnVariance.threshold, subjects)
}

// print shows docs as blocks, and the ids that are not documents here.
func (s *uploadStatus) print(docs []*upload.Document, missing []string) {
	w := s.app.Out
	width := wrapWidth(w)
	for i, d := range docs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		name := ui.SafeLine(d.Filename)
		if name == "" {
			name = "Document " + ui.SafeLine(d.ID)
		}
		printDocument(w, name, d, false, width)
	}
	if len(docs) > 1 {
		fmt.Fprintf(w, "\n%s: %s.\n", countDocuments(len(docs)), stateCounts(docs))
	}
	if waitingForReview(docs) {
		fmt.Fprintln(w, nothingBooked)
	}
}

// result is the error show ends with: an id that is not a document here;
// with --wait, a document that could not be read, or one still being read
// when the wait ran out or after it (see uploadRun.failure).
func (s *uploadStatus) result(docs []*upload.Document, missing []string, timedOut bool) error {
	if len(missing) > 0 {
		for i, id := range missing {
			missing[i] = ui.SafeLine(id)
		}
		return withExit(ExitNotFound, fmt.Errorf("no %s %s in this workspace", plural(len(missing), "document", "documents"), strings.Join(missing, ", ")))
	}
	if !s.wait {
		return nil
	}
	unread, reading := 0, 0
	for _, d := range docs {
		switch {
		case d.State == upload.StateFailed, d.State == stateMissing, d.State == upload.StateSkipped && d.Reason == "over_quota":
			unread++
		case upload.InProgress(d.State):
			reading++
		}
	}
	switch {
	case unread > 0:
		return withExit(ExitFailed, fmt.Errorf("%d %s not be read", unread, plural(unread, "document could", "documents could")))
	case reading > 0:
		after := ""
		if timedOut {
			after = " after " + httpx.Span(s.waitTimeout)
		}
		return withExit(ExitUnavailable, fmt.Errorf("%d %s still being read%s; firmfact goes on reading, so check again later", reading, plural(reading, "document was", "documents were"), after))
	}
	return nil
}

// recent lists the caller's most recent uploads to the workspace.
func (s *uploadStatus) recent(ctx context.Context, limit int) error {
	if err := s.connect(ctx); err != nil {
		return err
	}
	list, err := s.uc.Recent(ctx, s.ref, limit, upload.StateOnly)
	if err != nil {
		return err
	}
	docs := make([]*upload.Document, len(list.Results))
	for i := range list.Results {
		docs[i] = &list.Results[i]
	}
	if s.app.JSONOutput {
		return s.app.PrintJSON(documentsJSON(docs, nil, list.Schema))
	}
	w := s.app.Out
	if len(docs) == 0 {
		fmt.Fprintln(w, "No uploads of yours in this workspace yet.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "UPLOADED\tFILE\tSTATE\tID")
	for _, d := range docs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", uploadedAt(d.CreatedAt), ui.SafeLine(orDefault(d.Filename, "-")), stateWords(d), ui.SafeLine(d.ID))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "Show one in full with `%s upload status <id>`.\n", s.app.Name)
	return nil
}

// uploadedAt is when a document was uploaded, in local time: 27 Sep 2026
// 18:59.
func uploadedAt(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return orDefault(ui.SafeLine(s), "-")
	}
	return t.Local().Format("2 Jan 2006 15:04")
}
