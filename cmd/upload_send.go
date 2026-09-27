package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// What became of a file, as --json names it in outcome.
const (
	// outcomeCreated is a file this upload stored.
	outcomeCreated = "created"
	// outcomeDuplicate is a file whose bytes were already a document in
	// the workspace, which this upload left alone.
	outcomeDuplicate = "duplicate"
	// outcomeInProgress is a file someone was uploading to the workspace
	// at the same time.
	outcomeInProgress = "in_progress"
	// outcomeBusy is a file firmfact could not take then: it was too busy
	// to check uploads, or uploads were rate-limited.
	outcomeBusy = "busy"
	// outcomeRefused is a file firmfact refused; code and message say why.
	outcomeRefused = "refused"
	// outcomeSkipped is a file left out before anything was sent: found
	// in a folder or by a pattern and of a type firmfact does not read,
	// or with the same bytes as another file of the upload.
	outcomeSkipped = "skipped"
	// outcomeNotSent is a file the upload stopped before.
	outcomeNotSent = "not_sent"
	// outcomeFailed is a file whose sending failed: it changed on the
	// way, or no answer came.
	outcomeFailed = "failed"
)

// uploadFile is one file of an upload, and what became of it.
type uploadFile struct {
	src  upload.Source
	file upload.File
	// name is what firmfact calls the file: the name the preflight gave
	// back, which the server cleans up, else the name it was sent with.
	name string
	// outcome is empty while the file is still to be sent.
	outcome string
	// code and message are the server's reason, or the CLI's.
	code    string
	message string
	// status is what the file makes the exit status: 0, ExitFailed or
	// ExitUnavailable. A document's state can add to it (see
	// documentStatus).
	status int
	doc    *upload.Document
	// listed is set once the plan has shown why the file is not sent, so
	// the results do not say it again.
	listed bool
	// stopped is set on a file the upload stopped before, or with, for
	// the reason in uploadRun.stop, which the results say once for all of
	// them.
	stopped bool
}

func (f *uploadFile) set(outcome, code, message string, status int) {
	f.outcome, f.code, f.message, f.status = outcome, code, message, status
}

func (f *uploadFile) skip(code, message string) { f.set(outcomeSkipped, code, message, 0) }

// display is the file as the command line or a folder named it, for the
// CLI's own messages.
func (f *uploadFile) display() string {
	if f.src.Path == upload.Stdin {
		return "standard input"
	}
	return ui.SafeLine(f.src.Path)
}

// label is the file's name as firmfact has it, for the results.
func (f *uploadFile) label() string {
	return ui.SafeLine(orDefault(f.name, f.file.Name))
}

// uploadStop is why an upload stopped sending before every file went. The
// files it did not send get its code, message and status.
type uploadStop struct {
	code    string
	message string
	status  int
	// err, when set, is the error the command ends with: the sign-in
	// ended, or the workspace refused uploads, which no retry by the
	// CLI changes.
	err error
}

// halt stops the upload for stop's reason, unless it has stopped already.
func (r *uploadRun) halt(stop *uploadStop) {
	if r.stop == nil {
		r.stop = stop
	}
}

// errDeclined is an upload to Demo that the user said no to.
var errDeclined = errors.New("declined")

// options are the choices of this upload that every request shares.
func (r *uploadRun) options() upload.Options {
	return upload.Options{Related: r.flags.related, NewVersion: r.flags.newVersion}
}

// send asks the server about the files and sends the ones it would store,
// a preflight's worth (upload.MaxPreflightFiles) at a time: the allowance
// each preflight reports then counts the uploads before it.
func (r *uploadRun) send(ctx context.Context) error {
	var queue []*uploadFile
	for _, f := range r.files {
		if f.outcome == "" {
			queue = append(queue, f)
		}
	}
	if r.flags.related && len(queue) > upload.MaxPreflightFiles {
		return usageErrorf("--related sends the files as one group, which holds far fewer than these %d", len(queue))
	}
	total := len(queue)
	for i := 0; i < len(queue) && r.stop == nil; i += upload.MaxPreflightFiles {
		chunk := queue[i:min(i+upload.MaxPreflightFiles, len(queue))]
		p, err := r.uc.Preflight(ctx, r.ref, filesOf(chunk), r.options())
		switch {
		case err != nil && i == 0:
			return err
		case err != nil:
			r.halted(ctx, err)
			continue
		}
		r.applyPreflight(chunk, p, i == 0)
		groups := r.groups(chunk, p)
		for _, g := range groups {
			r.sendTotal += len(g.members)
		}
		if !r.app.JSONOutput {
			r.printPlan(chunk, groups, i, total)
		}
		if i == 0 {
			if err := r.confirm(ctx, groups); err != nil {
				return err
			}
		}
		for _, g := range groups {
			if r.stop != nil {
				break
			}
			if err := r.sendGroup(ctx, g); err != nil {
				return err
			}
		}
	}
	for _, f := range r.files {
		if f.outcome == "" && r.stop != nil {
			f.set(outcomeNotSent, r.stop.code, r.stop.message, r.stop.status)
			f.stopped = true
		}
	}
	return nil
}

// halted stops the upload after err, an error no retry by the CLI helps
// with now: a rate limit it has waited out, a server that is down, a
// sign-in that ended.
func (r *uploadRun) halted(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	code := exitCode(err)
	stop := &uploadStop{code: "unavailable", message: ui.SafeLine(err.Error()), status: ExitUnavailable}
	var apiErr *api.Error
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests:
		stop.code = "rate_limited"
		stop.message = "firmfact is rate-limiting uploads; run the command again later, and files already there are skipped"
	case code != ExitUnavailable:
		stop.code, stop.status, stop.err = errorCode(err), code, err
	}
	r.halt(stop)
}

// errorCode is the code of an answer from the server, in lower case as
// the outcomes are, or "error".
func errorCode(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Code != "" {
		return strings.ToLower(apiErr.Code)
	}
	return "error"
}

func filesOf(fs []*uploadFile) []upload.File {
	out := make([]upload.File, len(fs))
	for i, f := range fs {
		out[i] = f.file
	}
	return out
}

// applyPreflight takes the server's verdict on each file of chunk. A file
// of a type firmfact does not read is left out when a folder or a pattern
// found it, and refused when the command line named it.
func (r *uploadRun) applyPreflight(chunk []*uploadFile, p *upload.Preflight, first bool) {
	if first {
		if p.Workspace.ID != "" {
			r.workspace, r.ref = p.Workspace, p.Workspace.ID
		}
		r.demo = r.demo || p.Workspace.Demo
		allowance := p.Allowance
		r.allowance = &allowance
	}
	for _, pf := range p.Files {
		if pf.Index < 0 || pf.Index >= len(chunk) {
			continue
		}
		f := chunk[pf.Index]
		if pf.Name != "" {
			f.name = pf.Name
		}
		switch pf.Status {
		case upload.PreflightOK:
		case upload.PreflightDuplicate:
			f.set(outcomeDuplicate, "", "", 0)
			f.doc = pf.Document
		case upload.PreflightInProgress:
			f.set(outcomeInProgress, "in_progress", "", ExitUnavailable)
		case upload.PreflightRefused:
			if pf.Code == "unsupported_type" && !f.src.Named {
				f.skip(pf.Code, pf.Message)
				continue
			}
			f.set(outcomeRefused, pf.Code, pf.Message, ExitFailed)
		default:
			// A verdict this CLI does not know: not one to send on.
			f.set(outcomeRefused, pf.Status, pf.Message, ExitFailed)
		}
	}
}

// sendGroup is one request: a file, or files that go together.
type sendGroup struct {
	members []*uploadFile
	// related sends them with group_as_related (--related); a set the
	// server recognises by name goes without, as it forms the set itself.
	related bool
	// kind is the set's kind, for the plan: related, or the kind of
	// report set the server recognised.
	kind     string
	guidance *upload.Guidance
}

// groups are the requests chunk makes, in the order of their first files:
// a set of files that go together (all of them with --related, else each
// set the server recognises by name) in one request, and every other file
// in one of its own. Only files the server would store are sent. A set
// over the limits for one request is refused as a whole, and so is a
// --related group with a file the server refused: a group stored without
// one of its files would be read, and counted, as if it were whole.
func (r *uploadRun) groups(chunk []*uploadFile, p *upload.Preflight) []sendGroup {
	// The server knows a file by its position; a set names its files, and
	// every file with a name a set names belongs to it.
	byName := map[string][]int{}
	for i, f := range chunk {
		byName[f.name] = append(byName[f.name], i)
	}
	grouped := map[int]bool{}
	var groups []sendGroup
	for _, set := range p.Sets {
		var members []int
		seen := map[string]bool{}
		for _, name := range set.Filenames {
			if !seen[name] {
				seen[name] = true
				members = append(members, byName[name]...)
			}
		}
		var send, refused []*uploadFile
		for _, i := range members {
			grouped[i] = true
			switch f := chunk[i]; f.outcome {
			case "":
				send = append(send, f)
			case outcomeRefused:
				refused = append(refused, f)
			}
		}
		switch {
		case len(send) == 0:
			continue
		case set.Status == upload.PreflightRefused:
			for _, f := range send {
				f.set(outcomeRefused, orDefault(set.Code, "set_refused"), set.Message, ExitFailed)
			}
			continue
		case r.flags.related && len(refused) > 0:
			for _, f := range send {
				f.set(outcomeNotSent, "group_incomplete",
					fmt.Sprintf("not sent, as %s was refused and --related sends the files together or not at all", refused[0].label()), ExitFailed)
			}
			continue
		}
		groups = append(groups, sendGroup{members: send, related: r.flags.related, kind: set.Kind, guidance: set.Guidance})
	}
	for i, f := range chunk {
		if f.outcome == "" && !grouped[i] {
			groups = append(groups, sendGroup{members: []*uploadFile{f}, related: r.flags.related})
		}
	}
	position := map[*uploadFile]int{}
	for i, f := range chunk {
		position[f] = i
	}
	sortGroups(groups, position)
	return groups
}

// sortGroups orders groups by the position of their first file.
func sortGroups(groups []sendGroup, position map[*uploadFile]int) {
	for i := 1; i < len(groups); i++ {
		for j := i; j > 0 && position[groups[j].members[0]] < position[groups[j-1].members[0]]; j-- {
			groups[j], groups[j-1] = groups[j-1], groups[j]
		}
	}
}

// confirm asks before an upload to a Demo workspace that the command line
// did not name: a rebuild wipes what is uploaded there. target has refused
// it already where there is no one to ask. An upload that sends nothing
// asks nothing.
func (r *uploadRun) confirm(ctx context.Context, groups []sendGroup) error {
	if r.named || !r.demo || r.flags.yes || len(groups) == 0 {
		return nil
	}
	name := ui.SafeLine(orDefault(r.workspace.Name, r.workspace.ID))
	if !r.app.attended() {
		return usageErrorf("without --workspace, this upload would go to %s, a Demo workspace; name the workspace with --workspace, or add --yes", name)
	}
	ok, err := r.app.Confirm(ctx, fmt.Sprintf("%s is a Demo workspace, and a rebuild removes what you upload there. Upload to %s all the same?", name, name))
	if err != nil {
		return err
	}
	if !ok {
		return errDeclined
	}
	return nil
}

// sendGroup sends g, and takes what became of each of its files from the
// answer. It returns an error only to end the command at once (Ctrl-C);
// a failure that stops the upload is recorded with halt.
func (r *uploadRun) sendGroup(ctx context.Context, g sendGroup) error {
	r.showSending(g)
	err := r.attempt(ctx, g, false)
	r.sendDone += len(g.members)
	return err
}

// attempt sends g once; retried is set for the second attempt at the same
// files, which is the last.
func (r *uploadRun) attempt(ctx context.Context, g sendGroup, retried bool) error {
	r.sent = true
	res, err := r.uc.Upload(ctx, r.ref, filesOf(g.members), upload.Options{Related: g.related, NewVersion: r.flags.newVersion})
	if err != nil {
		return r.sendFailed(ctx, g, err, retried)
	}
	applyOutcomes(g.members, res.Results)
	return nil
}

// showSending puts what is being sent on the live line.
func (r *uploadRun) showSending(g sendGroup) {
	what := g.members[0].label()
	var size int64
	for _, f := range g.members {
		size += f.file.Size
	}
	if n := len(g.members); n > 1 {
		what += fmt.Sprintf(" and %d more", n-1)
	}
	r.live.show(fmt.Sprintf("Sending %d of %d: %s (%s)", r.sendDone+1, r.sendTotal, what, ui.Bytes(size)))
}

// applyOutcomes takes what became of each of members from an upload's
// answer, which lists them by their position in the request.
func applyOutcomes(members []*uploadFile, results []upload.Outcome) {
	for _, o := range results {
		if o.Index < 0 || o.Index >= len(members) {
			continue
		}
		f := members[o.Index]
		if o.Filename != "" {
			f.name = o.Filename
		}
		switch o.Outcome {
		case upload.OutcomeCreated:
			f.set(outcomeCreated, "", "", 0)
			f.doc = o.Document
		case upload.OutcomeDuplicate:
			f.set(outcomeDuplicate, "", o.Message, 0)
			f.doc = o.Document
		case upload.OutcomeInProgress:
			f.set(outcomeInProgress, o.Outcome, o.Message, ExitUnavailable)
		case "scanner_busy", "scanner_unavailable":
			f.set(outcomeBusy, o.Outcome, o.Message, ExitUnavailable)
		default:
			f.set(outcomeRefused, o.Outcome, o.Message, ExitFailed)
		}
	}
	for _, f := range members {
		if f.outcome == "" {
			f.set(outcomeFailed, "no_outcome", "Firmfact's answer did not say what became of this file", ExitFailed)
		}
	}
}

// sendFailed handles an upload request that did not store its files as
// asked: the server refused it (for these files, or for the whole upload),
// a file changed on its way, or no answer came. retried is set on the
// second attempt at the same files, which is the last.
func (r *uploadRun) sendFailed(ctx context.Context, g sendGroup, err error, retried bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var (
		changed    *upload.ChangedError
		refused    *upload.RefusedError
		unreadable *fs.PathError
		apiErr     *api.Error
		stream     *api.StreamError
	)
	switch {
	case errors.As(err, &changed):
		var which *uploadFile
		for _, f := range g.members {
			if f.file.Path == changed.Path {
				which = f
			}
		}
		for _, f := range g.members {
			if f == which {
				f.set(outcomeFailed, "changed", f.display()+" changed while it was being uploaded, so nothing was stored; upload it again once it is complete", ExitFailed)
			} else {
				f.set(outcomeNotSent, "group_incomplete", "not stored, as a file sent with it changed on the way", ExitFailed)
			}
		}
		return nil
	case errors.As(err, &refused):
		r.refusedWhole(g, refused)
		return nil
	case errors.As(err, &unreadable):
		// A file gone or locked since it was hashed: that file's trouble,
		// not the connection's.
		for _, f := range g.members {
			if f.file.Path == unreadable.Path {
				f.set(outcomeFailed, "unreadable", "could not read "+f.display()+": "+ui.SafeLine(unreadable.Err.Error()), ExitFailed)
			} else {
				f.set(outcomeNotSent, "group_incomplete", "not stored, as a file sent with it could not be read", ExitFailed)
			}
		}
		return nil
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			for _, f := range g.members {
				f.set(outcomeBusy, errorCodeOr(apiErr, "unavailable"), ui.SafeLine(apiErr.Error()), ExitUnavailable)
			}
			r.halted(ctx, err)
			return nil
		case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
			// A proxy's answer, or the server's trouble after the files
			// arrived: they may have been stored.
			return r.recheck(ctx, g, err, retried)
		case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			for _, f := range g.members {
				f.set(outcomeRefused, errorCodeOr(apiErr, "too_large"), ui.SafeLine(apiErr.Error()), ExitFailed)
			}
			return nil
		}
		// The workspace or the sign-in, not these files: the rest would
		// fare the same.
		for _, f := range g.members {
			f.set(outcomeNotSent, errorCode(err), ui.SafeLine(apiErr.Error()), exitCode(err))
			f.stopped = true
		}
		r.halt(&uploadStop{code: errorCode(err), message: ui.SafeLine(apiErr.Error()), status: exitCode(err), err: err})
		return nil
	case errors.As(err, &stream):
		return r.recheck(ctx, g, err, retried)
	}
	if code := exitCode(err); code == ExitSignedOut || code == ExitUnsupported {
		for _, f := range g.members {
			f.set(outcomeNotSent, "not_signed_in", ui.SafeLine(err.Error()), code)
			f.stopped = true
		}
		r.halt(&uploadStop{code: "not_signed_in", message: ui.SafeLine(err.Error()), status: code, err: err})
		return nil
	}
	// An answer the CLI could not read: the server may have stored the
	// files all the same.
	return r.recheck(ctx, g, err, retried)
}

// errorCodeOr is the code of apiErr in lower case, or def without one.
func errorCodeOr(apiErr *api.Error, def string) string {
	if apiErr.Code == "" {
		return def
	}
	return strings.ToLower(apiErr.Code)
}

// refusedWhole takes a refusal of a whole request, which stored nothing:
// the files it names with their own outcome, and the others with the
// server's sentence. The monthly allowance and a server too busy to check
// uploads would refuse the rest of the upload too, so they stop it.
func (r *uploadRun) refusedWhole(g sendGroup, refused *upload.RefusedError) {
	message := ui.SafeLine(refused.Error())
	code := errorCodeOr(refused.Err, "refused")
	busy := refused.Err.Status == http.StatusServiceUnavailable
	for _, o := range refused.Results {
		if o.Index < 0 || o.Index >= len(g.members) {
			continue
		}
		f := g.members[o.Index]
		if o.Filename != "" {
			f.name = o.Filename
		}
		if busy {
			f.set(outcomeBusy, o.Outcome, o.Message, ExitUnavailable)
		} else {
			f.set(outcomeRefused, o.Outcome, o.Message, ExitFailed)
		}
	}
	for _, f := range g.members {
		switch {
		case f.outcome != "":
		case busy:
			f.set(outcomeBusy, code, message, ExitUnavailable)
		default:
			f.set(outcomeRefused, code, message, ExitFailed)
		}
	}
	switch {
	case refused.Err.Code == "QUOTA_EXCEEDED":
		r.halt(&uploadStop{code: "quota_exceeded", message: "this month's allowance of documents is used up", status: ExitFailed})
	case busy:
		r.halt(&uploadStop{code: code, message: "firmfact is too busy to check uploads now; run the command again later, and files already there are skipped", status: ExitUnavailable})
	}
}

// recheck finds out what became of g's files when no answer came, or none
// the CLI could read, by asking the preflight again: a file that is now in
// the workspace was stored before the answer was lost, and one that is not
// is sent again, once. A second failure stops the upload.
func (r *uploadRun) recheck(ctx context.Context, g sendGroup, cause error, retried bool) error {
	lost := ui.SafeLine(cause.Error())
	unknown := func() {
		for _, f := range g.members {
			f.set(outcomeFailed, "no_answer", lost+"; whether firmfact stored it is not known, which running the command again shows", ExitUnavailable)
		}
		r.halt(&uploadStop{code: "no_answer", message: "an upload before them got no answer (" + lost + ")", status: ExitUnavailable})
	}
	// A new version is stored whether or not an earlier one is there, so
	// the preflight cannot tell whether this one was.
	if retried || r.flags.newVersion {
		unknown()
		return nil
	}
	p, err := r.uc.Preflight(ctx, r.ref, filesOf(g.members), r.options())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		unknown()
		return nil
	}
	verdicts := map[int]upload.PreflightFile{}
	for _, pf := range p.Files {
		verdicts[pf.Index] = pf
	}
	var again []*uploadFile
	for i, f := range g.members {
		switch pf := verdicts[i]; pf.Status {
		case upload.PreflightDuplicate:
			// Stored before the answer was lost.
			f.set(outcomeCreated, "", "", 0)
			f.doc = pf.Document
		case upload.PreflightInProgress:
			f.set(outcomeBusy, "in_progress", "Firmfact may still be storing it after its answer was lost; run the command again in a minute to see", ExitUnavailable)
		case upload.PreflightRefused:
			f.set(outcomeRefused, pf.Code, pf.Message, ExitFailed)
		default:
			again = append(again, f)
		}
	}
	if len(again) == 0 {
		return nil
	}
	r.uc.API.Debugf("sending %d %s again after: %v", len(again), plural(len(again), "file", "files"), cause)
	return r.attempt(ctx, sendGroup{members: again, related: g.related, kind: g.kind}, true)
}
