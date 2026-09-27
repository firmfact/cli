package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// What an upload prints for a person: the plan before anything is sent,
// then what firmfact read, as a block per document, or as a table when
// there are more than maxBlocks of them, and a line that sums up a batch.
// Every string from the server goes through ui.SafeLine or ui.SafeText.

// maxBlocks is how many documents are shown in full; more make a table.
const maxBlocks = 3

// printPlan prints what the server says it would do with chunk, the files
// of one preflight: what is new, what is already in the workspace, what it
// refuses and why, which files go together, and on the first chunk what
// the CLI left out or refused itself, the allowance and a warning about a
// Demo workspace. With no chunk, the CLI settled every file itself.
func (r *uploadRun) printPlan(chunk []*uploadFile, groups []sendGroup, start, total int) {
	w := r.out
	var fresh, dup, busy, refused int
	counted := chunk
	if start == 0 {
		for _, f := range r.files {
			if f.local {
				counted = append(counted[:len(counted):len(counted)], f)
			}
		}
	}
	for _, f := range counted {
		switch f.outcome {
		case "":
			fresh++
		case outcomeDuplicate:
			dup++
		case outcomeInProgress:
			busy++
		case outcomeRefused, outcomeNotSent:
			refused++
		}
	}
	where := r.workspaceName()
	if total > upload.MaxPreflightFiles {
		where += fmt.Sprintf(" (files %d to %d of %d)", start+1, start+len(chunk), total)
	}
	var parts []string
	if fresh > 0 {
		parts = append(parts, fmt.Sprintf("%d new", fresh))
	}
	if dup > 0 {
		parts = append(parts, fmt.Sprintf("%d already in firmfact", dup))
	}
	if busy > 0 {
		parts = append(parts, fmt.Sprintf("%d being uploaded by someone else", busy))
	}
	if refused > 0 {
		parts = append(parts, fmt.Sprintf("%d refused", refused))
	}
	switch {
	case fresh > 0:
		fmt.Fprintf(w, "Uploading to %s: %s\n", where, strings.Join(parts, ", "))
	case len(parts) > 0:
		fmt.Fprintf(w, "Nothing to send to %s: %s\n", where, strings.Join(parts, ", "))
	default:
		fmt.Fprintf(w, "Nothing to send to %s\n", where)
	}

	printRefusals(w, withOutcome(chunk, outcomeRefused, outcomeNotSent))
	for _, f := range chunk {
		if f.outcome == outcomeInProgress {
			fmt.Fprintf(w, "  %s: someone is uploading the same file right now; run the command again in a minute to see it\n", f.label())
			f.listed = true
		}
	}
	for _, g := range groups {
		r.printGroup(w, g)
	}
	// Found files the server left out by their names, such as a Data
	// License delivery, which only the web app takes: its sentences say
	// why.
	printRefusals(w, withOutcome(chunk, outcomeSkipped))
	if start > 0 {
		return
	}
	r.printSettled(w)
	if n := len(r.found.Folders); n > 0 {
		folders := make([]string, n)
		for i, folder := range r.found.Folders {
			folders[i] = ui.SafeLine(folder)
		}
		fmt.Fprintf(w, "  Left out the %s %s; add --recursive to upload the files in %s\n",
			plural(n, "folder", "folders"), someOf(folders, 5), plural(n, "it", "them"))
	}
	if n := r.found.Hidden; n > 0 {
		fmt.Fprintf(w, "  Left out %d hidden %s\n", n, plural(n, "file", "files"))
	}
	if n := r.found.Outside; n > 0 {
		fmt.Fprintf(w, "  Left out %d %s that %s out of the folders named\n", n, plural(n, "link", "links"), plural(n, "leads", "lead"))
	}
	if a := r.allowance; a != nil && a.Limit != nil && a.Remaining != nil {
		line := fmt.Sprintf("Allowance: %d of %d documents left this month", *a.Remaining, *a.Limit)
		if a.ResetsOn != "" {
			line += " (it resets on " + dateWords(a.ResetsOn) + ")"
		}
		fmt.Fprintln(w, line+".")
	}
	if r.demo {
		fmt.Fprintln(w, r.demoNote())
	}
}

// printGroup says which files g sends together, when it sends more than
// one, and warns of a request that may be too large to get through.
func (r *uploadRun) printGroup(w io.Writer, g sendGroup) {
	if len(g.members) < 2 {
		return
	}
	names := make([]string, len(g.members))
	for i, f := range g.members {
		names[i] = f.label()
	}
	if g.related {
		fmt.Fprintf(w, "  Sent together as related documents, counted once against the allowance: %s\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintf(w, "  Sent together as one %s: %s\n", setTitle(g), strings.Join(names, ", "))
	}
	if g.guidance != nil {
		for _, s := range []string{g.guidance.Message, g.guidance.Suggestion} {
			if strings.TrimSpace(s) != "" {
				fmt.Fprintf(w, "    %s\n", ui.SafeLine(s))
			}
		}
	}
	opts := upload.Options{Related: g.related, NewVersion: r.flags.newVersion}
	if size := upload.RequestSize(filesOf(g.members), opts); size > upload.NearRequestLimit {
		fmt.Fprintf(w, "    Together %s, close to the most one request can carry; if it is refused as too large, upload fewer of these files at a time.\n", ui.Bytes(size))
	}
}

// setTitle is what the plan calls a set of files the server recognised by
// their names: the upload screen's title for it, such as "Bloomberg SID
// report set".
func setTitle(g sendGroup) string {
	if g.guidance != nil && strings.TrimSpace(g.guidance.Title) != "" {
		return ui.SafeLine(g.guidance.Title)
	}
	return "set of related reports"
}

// printSettled prints what the CLI settled itself, before asking: files
// too large to send, those of a type firmfact does not read, and those
// with the same bytes as another.
func (r *uploadRun) printSettled(w io.Writer) {
	var refused []*uploadFile
	var unread []string
	for _, f := range r.files {
		switch {
		case !f.local:
		case f.code == "same_contents":
		case f.outcome == outcomeSkipped:
			unread = append(unread, f.label())
		default:
			refused = append(refused, f)
		}
	}
	printRefusals(w, refused)
	if len(unread) > 0 {
		fmt.Fprintf(w, "  Left out, as firmfact does not read files of their type: %s\n", someOf(unread, 5))
	}
	for _, f := range r.files {
		if f.code == "same_contents" {
			fmt.Fprintf(w, "  Left out %s: %s\n", f.display(), f.message)
		}
	}
}

// withOutcome are the files of chunk with one of outcomes.
func withOutcome(chunk []*uploadFile, outcomes ...string) []*uploadFile {
	var out []*uploadFile
	for _, f := range chunk {
		if slices.Contains(outcomes, f.outcome) {
			out = append(out, f)
		}
	}
	return out
}

// printRefusals says why each of files is not sent, a line for each
// reason however many files it is about. The server's sentences start
// with the file's name ("scan.pdf: files of this type cannot be
// uploaded ..."), so files refused for the same reason are listed before
// the rest of the sentence: over the allowance, sixty files make one line,
// not sixty.
func printRefusals(w io.Writer, files []*uploadFile) {
	var order []string
	byReason := map[string][]*uploadFile{}
	for _, f := range files {
		f.listed = true
		key := refusalReason(f)
		if _, ok := byReason[key]; !ok {
			order = append(order, key)
		}
		byReason[key] = append(byReason[key], f)
	}
	for _, key := range order {
		files := byReason[key]
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.label()
		}
		if code, ok := strings.CutPrefix(key, "\x00"); ok {
			fmt.Fprintf(w, "  %s: refused (%s)\n", someOf(names, 5), code)
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", someOf(names, 5), key)
	}
}

// refusalReason is why f is not sent: the server's sentence without the
// file's name at its start, or its code when it gave no sentence (marked
// with a NUL, which no sentence holds once through ui.SafeLine).
func refusalReason(f *uploadFile) string {
	message := ui.SafeLine(f.message)
	if message == "" {
		return "\x00" + ui.SafeLine(orDefault(f.code, "no reason given"))
	}
	if rest, ok := strings.CutPrefix(message, f.label()+": "); ok && rest != "" {
		return rest
	}
	return message
}

// someOf lists the first n of names, and says how many more there are.
func someOf(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(" and %d more", len(names)-n)
}

// workspaceName is the target workspace in messages.
func (r *uploadRun) workspaceName() string {
	return ui.SafeLine(orDefault(r.workspace.Name, orDefault(r.workspace.ID, r.ref)))
}

func (r *uploadRun) demoNote() string {
	return r.workspaceName() + " is a Demo workspace: a rebuild from sample data removes what you upload there."
}

// printResults prints what became of the files the plan did not account
// for already: the documents, as blocks or a table, the files that were
// refused or failed on their way, those the upload stopped before, and a
// line that sums up a batch.
func (r *uploadRun) printResults() {
	w := r.out
	var docs, problems []*uploadFile
	notSent := 0
	for _, f := range r.files {
		switch {
		case f.doc != nil:
			docs = append(docs, f)
		case f.listed || f.outcome == outcomeSkipped:
		case f.stopped:
			notSent++
		case f.outcome != "":
			problems = append(problems, f)
		}
	}
	if len(docs)+len(problems)+notSent == 0 {
		return
	}
	fmt.Fprintln(w)
	width := wrapWidth(w)
	blocks := len(docs) <= maxBlocks
	if blocks {
		for i, f := range docs {
			if i > 0 {
				fmt.Fprintln(w)
			}
			printDocument(w, f.label(), f.doc, f.outcome == outcomeDuplicate, width)
		}
		for i, f := range problems {
			if i > 0 || len(docs) > 0 {
				fmt.Fprintln(w)
			}
			printProblem(w, f, width)
		}
	} else {
		printDocumentTable(w, docs)
		for _, f := range problems {
			fmt.Fprintf(w, "  %s\n", problemLine(f))
		}
	}
	// A line about the whole batch stands apart from the blocks above it.
	if blocks && (notSent > 0 || len(r.files) > 1) {
		fmt.Fprintln(w)
	}
	if notSent > 0 && r.stop != nil {
		fmt.Fprintf(w, "%d %s not sent: %s.\n", notSent, plural(notSent, "file was", "files were"), r.stop.message)
	}
	if len(r.files) > 1 {
		fmt.Fprintln(w, r.summaryLine())
	}
	ids := r.documentIDs()
	if !blocks {
		if list := documentsPage(docs); list != "" {
			fmt.Fprintf(w, "Review them at %s\n", list)
		}
	}
	if waitingForReview(r.documents()) {
		fmt.Fprintln(w, nothingBooked)
	}
	if n := len(inProgress(r.documents())); r.flags.noWait && n > 0 {
		fmt.Fprintf(w, "Firmfact reads %s meanwhile; see what it read with `%s`.\n", plural(n, "it", "them"), r.statusCommand(ids))
	}
	r.printVarianceSteps()
}

// printProblem is the block of a file that was not stored on its way.
func printProblem(w io.Writer, f *uploadFile, width int) {
	head := "not stored"
	switch f.outcome {
	case outcomeBusy:
		head = "not stored, as firmfact was busy"
	case outcomeInProgress:
		head = "being uploaded by someone else"
	case outcomeFailed, outcomeNotSent:
		head = "not sent"
	}
	fmt.Fprintf(w, "%s: %s\n", f.label(), head)
	for _, line := range wrapText(problemText(f), width-2) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// problemText is why f was not stored, in the server's words when it gave
// them.
func problemText(f *uploadFile) string {
	if f.message != "" {
		return ui.SafeLine(f.message)
	}
	return "refused (" + ui.SafeLine(orDefault(f.code, "no reason given")) + ")"
}

// problemLine is f and why it was not stored, on one line. The server's
// sentences start with the file's name, which is then not said twice, as
// in the plan (printRefusals).
func problemLine(f *uploadFile) string {
	text := problemText(f)
	if strings.HasPrefix(text, f.label()) {
		return text
	}
	return f.label() + ": " + text
}

// documentsPage is the web page that lists a workspace's documents, where
// a batch's documents wait for review: a document's own link without its
// id, when it has the shape of one.
func documentsPage(files []*uploadFile) string {
	for _, f := range files {
		if base, ok := strings.CutSuffix(f.doc.URL, "/"+f.doc.ID); ok && strings.HasSuffix(base, "/documents") {
			return ui.SafeLine(base)
		}
	}
	return ""
}

// summaryLine sums up a batch: what was uploaded, and the states of the
// documents.
func (r *uploadRun) summaryLine() string {
	counts := map[string]int{}
	for _, f := range r.files {
		counts[f.outcome]++
	}
	var head []string
	if n := counts[outcomeCreated]; n > 0 {
		head = append(head, fmt.Sprintf("%d uploaded", n))
	}
	if n := counts[outcomeDuplicate]; n > 0 {
		head = append(head, fmt.Sprintf("%d already in firmfact", n))
	}
	if n := counts[outcomeRefused]; n > 0 {
		head = append(head, fmt.Sprintf("%d refused", n))
	}
	if n := counts[outcomeBusy] + counts[outcomeFailed] + counts[outcomeNotSent] + counts[outcomeInProgress]; n > 0 {
		head = append(head, fmt.Sprintf("%d not sent", n))
	}
	if n := counts[outcomeSkipped]; n > 0 {
		head = append(head, fmt.Sprintf("%d left out", n))
	}
	if counts[outcomeCreated]+counts[outcomeDuplicate] == 0 {
		head = append([]string{"Nothing uploaded"}, head...)
	}
	line := strings.Join(head, ", ")
	if states := stateCounts(r.documents()); states != "" {
		line += ": " + states
	}
	return line + "."
}

// The --json output of firmfact upload: the envelope every workspace
// command prints, with a result for each file in data.results, in the
// order of the command line. A result's document is the server's entry in
// document_result/1, as it sent it, unknown fields and all.
type uploadJSON struct {
	Data  uploadJSONData `json:"data"`
	Meta  uploadJSONMeta `json:"meta"`
	Notes []string       `json:"notes"`
}

type uploadJSONData struct {
	Workspace *upload.Workspace `json:"workspace"`
	Results   []uploadJSONFile  `json:"results"`
	// Summary counts the files by outcome, and the documents by state
	// under states.
	Summary map[string]any `json:"summary"`
	// Allowance is the monthly allowance as the first preflight found it,
	// before this upload.
	Allowance *upload.Allowance `json:"allowance,omitempty"`
}

type uploadJSONMeta struct {
	Schema string `json:"schema"`
	// VarianceExceeded and VarianceUnchecked are there with
	// --fail-on-variance only, empty lists when there are none: the
	// invoices over the threshold and the documents whose variance could
	// not be checked, by what the command line named (a file's path for
	// upload, a document's id for upload status).
	VarianceExceeded  *[]string `json:"variance_exceeded,omitempty"`
	VarianceUnchecked *[]string `json:"variance_unchecked,omitempty"`
}

type uploadJSONFile struct {
	// Path is the file as the command line or a folder named it; - for
	// standard input.
	Path string `json:"path"`
	// Filename is what firmfact calls it.
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	// SHA256 is left out for a file the CLI did not read: one of a type
	// firmfact does not read, or too large.
	SHA256   string          `json:"sha256,omitempty"`
	Outcome  string          `json:"outcome"`
	Code     string          `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
	Document json.RawMessage `json:"document,omitempty"`
}

func (r *uploadRun) json() uploadJSON {
	out := uploadJSON{Meta: uploadJSONMeta{Schema: orDefault(r.schema, upload.Schema)}, Notes: []string{}}
	if r.workspace.ID != "" {
		ws := r.workspace
		out.Data.Workspace = &ws
	}
	out.Data.Allowance = r.allowance
	out.Data.Results = make([]uploadJSONFile, 0, len(r.files))
	summary := map[string]any{}
	for _, f := range r.files {
		entry := uploadJSONFile{
			Path: f.src.Path, Filename: orDefault(f.name, f.file.Name), Size: f.file.Size, SHA256: f.file.SHA256,
			Outcome: orDefault(f.outcome, outcomeNotSent), Code: f.code, Message: f.message,
		}
		if f.doc != nil {
			entry.Document = documentJSON(f.doc)
		}
		out.Data.Results = append(out.Data.Results, entry)
		n, _ := summary[entry.Outcome].(int)
		summary[entry.Outcome] = n + 1
	}
	states := map[string]int{}
	for _, d := range r.documents() {
		states[d.State]++
	}
	summary["states"] = states
	out.Data.Summary = summary
	r.varianceGate().setMeta(&out.Meta)
	if r.demo {
		out.Notes = append(out.Notes, r.demoNote())
	}
	if n := len(r.found.Folders); n > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("Left out %d %s; add --recursive to upload the files in %s.", n, plural(n, "folder", "folders"), plural(n, "it", "them")))
	}
	if n := r.found.Hidden; n > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("Left out %d hidden %s.", n, plural(n, "file", "files")))
	}
	if n := r.found.Outside; n > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("Left out %d %s that %s out of the folders named.", n, plural(n, "link", "links"), plural(n, "leads", "lead")))
	}
	if r.stop != nil && r.stoppedFiles() > 0 {
		out.Notes = append(out.Notes, "Some files were not sent: "+r.stop.message+".")
	}
	if r.unread != nil {
		out.Notes = append(out.Notes, "The "+r.unreadMessage()+".")
	}
	if r.cut {
		out.Notes = append(out.Notes, "Stopped by an interrupt; run the same command again to send the rest: files already there are skipped.")
	}
	return out
}

// unreadMessage says that the documents could not be read back, and how to
// see them later, after "the".
func (r *uploadRun) unreadMessage() string {
	return fmt.Sprintf("files were sent, but reading back what firmfact made of them failed (%s); see it with `%s`",
		ui.SafeLine(r.unread.Error()), r.statusCommand(r.documentIDs()))
}

// documentJSON is doc as the server sent it, or as the CLI read it when
// the server's own copy is not kept.
func documentJSON(doc *upload.Document) json.RawMessage {
	if len(doc.Raw) > 0 && doc.State != stateMissing {
		return doc.Raw
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return raw
}

// stoppedFiles is how many files the upload stopped before.
func (r *uploadRun) stoppedFiles() int {
	n := 0
	for _, f := range r.files {
		if f.stopped {
			n++
		}
	}
	return n
}

// result is the error the upload ends with, whose exit status says how it
// went: see failure, and with --fail-on-variance, varianceGate.result.
func (r *uploadRun) result() error {
	return r.varianceGate().result(r.failure())
}

// varianceGate is --fail-on-variance's verdict on the upload's documents,
// each once, under the first file that named it; nil without the flag.
func (r *uploadRun) varianceGate() *varianceGate {
	if !r.flags.failOnVariance.on {
		return nil
	}
	var subjects []varianceSubject
	seen := map[string]bool{}
	for _, f := range r.files {
		if f.doc == nil || seen[f.doc.ID] {
			continue
		}
		seen[f.doc.ID] = true
		subjects = append(subjects, varianceSubject{name: f.label(), key: f.src.Path, doc: f.doc})
	}
	return gateVariance(r.flags.failOnVariance.threshold, subjects)
}

// failure is the error the upload ends with apart from --fail-on-variance:
// 1 when a file was refused or could not be read, 5 when one could not be
// sent now or was still being read once the upload stopped waiting, and
// the error's own status when the sign-in or the workspace stopped the
// upload. A refusal needs a person, so it wins over a wait that ran out.
// A document is still being read after the wait when the wait ran out, but
// also when it went back to being read before the full read that followed
// (a document of a group matched again, or a retry): either way, what
// firmfact made of it is not known yet.
func (r *uploadRun) failure() error {
	if r.stop != nil && r.stop.err != nil {
		return r.stop.err
	}
	var refused, later, stored int
	for _, f := range r.files {
		switch f.status {
		case ExitFailed:
			refused++
		case ExitUnavailable:
			later++
		}
		if f.outcome == outcomeCreated || f.outcome == outcomeDuplicate {
			stored++
		}
	}
	var unread, overQuota, reading int
	for _, d := range r.documents() {
		switch {
		case d.State == upload.StateFailed, d.State == stateMissing:
			unread++
		case d.State == upload.StateSkipped && d.Reason == "over_quota":
			overQuota++
		case upload.InProgress(d.State) && !r.flags.noWait:
			reading++
		}
	}
	var failures []string
	if refused > 0 {
		failures = append(failures, fmt.Sprintf("%d %s not stored", refused, plural(refused, "file was", "files were")))
	}
	if unread > 0 {
		failures = append(failures, fmt.Sprintf("%d %s not be read", unread, plural(unread, "document could", "documents could")))
	}
	if overQuota > 0 {
		failures = append(failures, fmt.Sprintf("%d %s skipped, as this month's allowance of documents was used up", overQuota, plural(overQuota, "document was", "documents were")))
	}
	if len(failures) == 0 && stored == 0 && later == 0 {
		failures = append(failures, "nothing was uploaded: firmfact does not read any of the files found")
	}
	if len(failures) > 0 {
		if r.unread != nil {
			failures = append(failures, "the "+r.unreadMessage())
		}
		return withExit(ExitFailed, errors.New(strings.Join(failures, "; ")))
	}
	var retry []string
	// The read back after the upload failed: its own exit status (no
	// longer signed in, the workspace gone), or 5 for one worth trying
	// again, as is any the CLI cannot place.
	status := ExitUnavailable
	if r.unread != nil {
		retry = append(retry, "the "+r.unreadMessage())
		if code := exitCode(r.unread); code != ExitFailed {
			status = code
		}
	}
	if later > 0 {
		retry = append(retry, fmt.Sprintf("%d %s not be sent now; run the command again later, and files already there are skipped", later, plural(later, "file could", "files could")))
	}
	if reading > 0 {
		var ids []string
		for _, d := range inProgress(r.documents()) {
			ids = append(ids, d.ID)
		}
		after := ""
		if r.timedOut {
			after = " after " + httpx.Span(r.flags.waitTimeout)
		}
		retry = append(retry, fmt.Sprintf("%d %s still being read%s; check with `%s`", reading, plural(reading, "document was", "documents were"), after, r.statusCommand(ids)))
	}
	if len(retry) > 0 {
		return withExit(status, errors.New(strings.Join(retry, "; ")))
	}
	return nil
}
