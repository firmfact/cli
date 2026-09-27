package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// Waiting for firmfact to read what was uploaded, which upload and
// upload status --wait share: the states are read every few seconds until
// no document is in progress, and the caller's own finished documents are
// then read back in full.

// liveLine is a line of progress redrawn in place, for a person at a
// terminal that can redraw one; elsewhere it shows nothing.
type liveLine struct {
	w     io.Writer
	on    bool
	shown bool
}

func (l *liveLine) show(s string) {
	if !l.on {
		return
	}
	// One row, less the last column: a line that wraps cannot be redrawn.
	fmt.Fprint(l.w, "\r\x1b[K"+cutCell(s, ui.Width(l.w)-1))
	l.shown = true
}

// clear takes the line away, before anything else is printed.
func (l *liveLine) clear() {
	if l.shown {
		fmt.Fprint(l.w, "\r\x1b[K")
		l.shown = false
	}
}

// uploadPollInterval is the first pause between two reads of the
// documents' states. The pauses grow by a quarter each time, up to 10 s
// (see nextUploadGap), so a document read in seconds is shown in seconds,
// and a long wait asks about once every 10 s, well inside the CLI's rate
// limit. Tests set it to a millisecond.
var uploadPollInterval = 3 * time.Second

func nextUploadGap(gap time.Duration) time.Duration {
	return min(gap+gap/4, uploadPollInterval*10/3)
}

// nextUploadRetry is the pause before reading again after a read failed in
// a way that may pass: the usual pause, then twice the one before, up to
// 10 times uploadPollInterval (30 s).
func nextUploadRetry(retry, gap time.Duration) time.Duration {
	return min(max(retry*2, gap), uploadPollInterval*10)
}

// documentWait is one wait for documents to be read.
type documentWait struct {
	uc        *upload.Client
	workspace string
	timeout   time.Duration
	// progress takes the line that says what the wait is for, and live
	// the redrawn count.
	progress io.Writer
	live     *liveLine
}

// wait reads the states of docs until none is in progress, updating them
// in place, and reports whether the time ran out first. A read that fails
// in a way that may pass (no answer, a server error, a rate limit) is
// tried again, until the time runs out.
func (w documentWait) wait(ctx context.Context, docs []*upload.Document) (timedOut bool, err error) {
	if len(inProgress(docs)) == 0 {
		return false, nil
	}
	defer w.live.clear()
	n := len(inProgress(docs))
	if !w.live.on {
		fmt.Fprintf(w.progress, "Waiting for %s to be read (at most %s)...\n", countDocuments(n), httpx.Span(w.timeout))
	}
	start := time.Now()
	deadline := start.Add(w.timeout)
	gap, retry := uploadPollInterval, time.Duration(0)
	pause := gap
	for {
		waiting := inProgress(docs)
		if len(waiting) == 0 {
			return false, nil
		}
		if !time.Now().Before(deadline) {
			return true, nil
		}
		w.live.show(fmt.Sprintf("Waiting for %d of %s to be read (%s)", len(waiting), countDocuments(len(docs)), elapsed(time.Since(start))))
		if !ui.Pause(ctx, min(pause, time.Until(deadline))) {
			return false, ctx.Err()
		}
		ids := make([]string, len(waiting))
		for i, d := range waiting {
			ids[i] = d.ID
		}
		list, err := w.uc.Documents(ctx, w.workspace, ids, upload.StateOnly)
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil && !transientError(err):
			return false, err
		case err != nil:
			retry = nextUploadRetry(retry, gap)
			pause = retry
			continue
		}
		retry = 0
		updateStates(waiting, list)
		gap = nextUploadGap(gap)
		pause = gap
	}
}

// inProgress are the documents of docs that are still being read.
func inProgress(docs []*upload.Document) []*upload.Document {
	var out []*upload.Document
	for _, d := range docs {
		if upload.InProgress(d.State) {
			out = append(out, d)
		}
	}
	return out
}

// stateMissing marks a document the workspace no longer has: someone
// deleted it while the CLI waited.
const stateMissing = "missing"

// updateStates takes each document from list, a state-only read, in
// place of what was known of it, which was no more than its state either.
func updateStates(docs []*upload.Document, list *upload.DocumentList) {
	byID := map[string]upload.Document{}
	for _, d := range list.Results {
		byID[d.ID] = d
	}
	for _, d := range docs {
		if got, ok := byID[d.ID]; ok {
			*d = got
		}
	}
	markMissing(docs, list.Missing)
}

// markMissing marks the documents of docs whose ids a read listed as
// missing: the workspace no longer has them.
func markMissing(docs []*upload.Document, missing []string) {
	for _, id := range missing {
		for _, d := range docs {
			if d.ID == id {
				d.State = stateMissing
			}
		}
	}
}

// fullReadBatch is how many documents one full read asks for. The server
// works out a variance preview and a review for each, and a proxy on the
// way gives up on an answer after 30 s, so a read asks for a few at a
// time rather than the 100 a read may name.
const fullReadBatch = 20

// fullReadAttempts is how often a full read is tried when it fails in a way
// that may pass.
const fullReadAttempts = 3

// readResults reads back, in full, the documents of docs that are the
// caller's own and finished: what was read, the contract match, the
// variance preview and what needs review. Someone else's document is its
// state and link only, which docs has already. A document deleted since
// the wait is marked missing, as the wait marks one, rather than left as
// the state it had: a finished document with nothing read from it would
// pass for one there is nothing to check. It returns the schema the
// server named; after an error, the documents read before it are in docs.
func readResults(ctx context.Context, uc *upload.Client, workspace string, docs []*upload.Document) (schema string, err error) {
	var ids []string
	for _, d := range docs {
		if d.Own && !upload.InProgress(d.State) && d.State != stateMissing {
			ids = append(ids, d.ID)
		}
	}
	for len(ids) > 0 {
		batch := ids[:min(len(ids), fullReadBatch)]
		ids = ids[len(batch):]
		list, err := readFull(ctx, uc, workspace, batch)
		if err != nil {
			return schema, err
		}
		schema = orDefault(list.Schema, schema)
		byID := map[string]upload.Document{}
		for _, d := range list.Results {
			byID[d.ID] = d
		}
		for _, d := range docs {
			if got, ok := byID[d.ID]; ok {
				*d = got
			}
		}
		markMissing(docs, list.Missing)
	}
	return schema, nil
}

// readFull reads the documents with ids in full, and again after a failure
// that may pass (no answer, a server error, a rate limit), up to
// fullReadAttempts times.
func readFull(ctx context.Context, uc *upload.Client, workspace string, ids []string) (*upload.DocumentList, error) {
	var retry time.Duration
	for attempt := 1; ; attempt++ {
		list, err := uc.Documents(ctx, workspace, ids, upload.Full)
		if err == nil || ctx.Err() != nil || !transientError(err) || attempt == fullReadAttempts {
			return list, err
		}
		retry = nextUploadRetry(retry, uploadPollInterval)
		if !ui.Pause(ctx, retry) {
			return nil, ctx.Err()
		}
	}
}

// countDocuments is "1 document" or "n documents".
func countDocuments(n int) string {
	return fmt.Sprintf("%d %s", n, plural(n, "document", "documents"))
}

// elapsed is a wait so far, as 45s or 3m05s.
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return fmt.Sprintf("%dm%02ds", d/time.Minute, d%time.Minute/time.Second)
}
