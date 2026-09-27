package upload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/ui"
)

// The server's limits on the shape of a request. What one upload may hold
// (files, bytes) comes back in each preflight (Limits), as the server sees
// it then.
const (
	// MaxPreflightFiles is how many files one preflight may name.
	MaxPreflightFiles = 100
	// MaxIDs is how many documents one read may ask for; Documents asks
	// for more in turns.
	MaxIDs = 100
)

// Options are the choices an upload and its preflight share.
type Options struct {
	// Related sends the files as one group of related documents, which
	// the monthly allowance counts once (firmfact upload --related).
	Related bool
	// NewVersion stores files that are already in the workspace again, as
	// new versions, instead of reporting them as duplicates.
	NewVersion bool
}

// View is how much of a document a read asks for.
type View string

const (
	// Full is everything the server has to say: for the caller's own
	// upload, once finished, what was read and what needs review.
	Full View = ""
	// StateOnly is each document's state and link, for polling.
	StateOnly View = "state"
)

// Client reads and writes the upload endpoints of one host through API.
type Client struct {
	API *api.Client
}

// Workspace is the workspace an answer is about.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Demo bool   `json:"demo"`
}

// The statuses of a file in a preflight.
const (
	// PreflightOK is a file the upload would store.
	PreflightOK = "ok"
	// PreflightDuplicate is a file whose bytes are already a document in
	// the workspace; Document says which, and in what state.
	PreflightDuplicate = "duplicate"
	// PreflightInProgress is a file being uploaded to the workspace right
	// now, by this user or another.
	PreflightInProgress = "in_progress"
	// PreflightRefused is a file the upload would refuse, with Code and
	// Message saying why.
	PreflightRefused = "refused"
)

// Preflight is the server's view of an upload before any bytes are sent.
// Nothing is stored by asking.
type Preflight struct {
	Workspace Workspace       `json:"workspace"`
	Files     []PreflightFile `json:"files"`
	// Sets are the groups the files form: all of them with Related, else
	// each set of related files the server recognises by name. Each is
	// one upload request.
	Sets      []Set     `json:"sets"`
	Allowance Allowance `json:"allowance"`
	Limits    Limits    `json:"limits"`

	Raw json.RawMessage `json:"-"`
}

func (p *Preflight) UnmarshalJSON(b []byte) error {
	type fields Preflight
	return keepRaw(b, (*fields)(p), &p.Raw)
}

// PreflightFile is one file of a preflight, by its position (Index) in
// the request: two files may share a name.
type PreflightFile struct {
	Index    int       `json:"index"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Status   string    `json:"status"`
	Code     string    `json:"code,omitempty"`
	Message  string    `json:"message,omitempty"`
	Document *Document `json:"document,omitempty"`

	Raw json.RawMessage `json:"-"`
}

func (f *PreflightFile) UnmarshalJSON(b []byte) error {
	type fields PreflightFile
	return keepRaw(b, (*fields)(f), &f.Raw)
}

// Set is a group of files that one upload request would send together.
type Set struct {
	Filenames []string `json:"filenames"`
	// Kind is related for a group the user asked for, else the kind of
	// report set the server recognised.
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
	// Status is ok, or refused with Code and Message when the group is over
	// the limits for one.
	Status   string    `json:"status"`
	Code     string    `json:"code,omitempty"`
	Message  string    `json:"message,omitempty"`
	Guidance *Guidance `json:"guidance,omitempty"`
}

// Guidance is the upload screen's advice on a recognised report set, such
// as a part that is missing from it.
type Guidance struct {
	Status     string   `json:"status,omitempty"`
	Title      string   `json:"title,omitempty"`
	Message    string   `json:"message,omitempty"`
	Suggestion string   `json:"suggestion,omitempty"`
	Missing    []string `json:"missing,omitempty"`
}

// Allowance is the workspace's monthly allowance of documents. Limit and
// Remaining are nil when the plan sets no figure.
type Allowance struct {
	Limit     *int   `json:"limit"`
	Used      int    `json:"used"`
	Pending   int    `json:"pending"`
	Remaining *int   `json:"remaining"`
	ResetsOn  string `json:"resets_on"`
}

// Limits are what one upload request may hold, as the server has them.
type Limits struct {
	MaxFilesPerRequest int      `json:"max_files_per_request"`
	MaxGroupBytes      int64    `json:"max_group_bytes"`
	MaxFileBytes       int64    `json:"max_file_bytes"`
	MaxRequestBytes    int64    `json:"max_request_bytes"`
	Extensions         []string `json:"extensions"`
}

// Preflight asks the server what an upload of files to workspace would do,
// without sending their bytes.
func (c *Client) Preflight(ctx context.Context, workspace string, files []File, opts Options) (*Preflight, error) {
	type entry struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	req := struct {
		Files          []entry `json:"files"`
		GroupAsRelated bool    `json:"group_as_related,omitempty"`
		NewVersion     bool    `json:"new_version,omitempty"`
	}{Files: make([]entry, len(files)), GroupAsRelated: opts.Related, NewVersion: opts.NewVersion}
	for i, f := range files {
		req.Files[i] = entry{Name: f.Name, Size: f.Size, SHA256: f.SHA256}
	}
	var out envelope[Preflight]
	if err := c.API.JSON(ctx, http.MethodPost, documentsPath(workspace)+"/preflight", req, &out, true); err != nil {
		return nil, c.explain(err, 0)
	}
	return &out.Data, nil
}

// The outcomes of a file in an upload that the CLI acts on. The others say
// why the file was not stored (not_stored, infected, scanner_busy, ...),
// with a Message.
const (
	OutcomeCreated    = "created"
	OutcomeDuplicate  = "duplicate"
	OutcomeInProgress = "in_progress"
)

// Upload is the server's answer to an upload request: one outcome per
// file, in the order sent.
type Upload struct {
	Workspace Workspace `json:"workspace"`
	Results   []Outcome `json:"results"`
	// Summary counts the files by outcome.
	Summary map[string]int `json:"summary"`
	// Schema is the version of the entries (see Schema).
	Schema string `json:"-"`

	Raw json.RawMessage `json:"-"`
}

func (u *Upload) UnmarshalJSON(b []byte) error {
	type fields Upload
	return keepRaw(b, (*fields)(u), &u.Raw)
}

// Outcome is what became of one file of an upload. Document is the stored
// document, or for a duplicate the one already in the workspace.
type Outcome struct {
	Index    int       `json:"index"`
	Filename string    `json:"filename"`
	Outcome  string    `json:"outcome"`
	Message  string    `json:"message,omitempty"`
	Document *Document `json:"document,omitempty"`

	Raw json.RawMessage `json:"-"`
}

func (o *Outcome) UnmarshalJSON(b []byte) error {
	type fields Outcome
	return keepRaw(b, (*fields)(o), &o.Raw)
}

// Upload sends files to workspace in one request: one file, or one group
// (opts.Related, or a set the server recognises). The files are streamed
// from disk and checked against Hash's findings on the way (see
// ChangedError).
//
// A refusal of the whole request, which stores nothing, is a *RefusedError.
// A request that got no answer is an *api.StreamError, which says whether
// all of it was sent: when it was, the server may have stored it, which a
// preflight shows.
func (c *Client) Upload(ctx context.Context, workspace string, files []File, opts Options) (*Upload, error) {
	b, err := newBody(files, opts)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Content-Type": b.ContentType()}
	resp, err := c.API.SendBody(ctx, http.MethodPost, documentsPath(workspace), b, headers, true)
	if err != nil {
		return nil, c.explain(err, b.Size())
	}
	defer resp.Body.Close()
	var out envelope[Upload]
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, api.WithRequestID(fmt.Errorf("unexpected answer from the server: %w", err), resp)
	}
	out.Data.Schema = out.Meta.Schema
	return &out.Data, nil
}

// DocumentList is documents read back, in the order asked for, and the
// ids that are not documents the caller may see in the workspace.
type DocumentList struct {
	Results []Document `json:"results"`
	Missing []string   `json:"missing"`
	// Schema is the version of the entries (see Schema).
	Schema string `json:"-"`
}

// Documents reads back the documents with ids in workspace, MaxIDs at a
// time.
func (c *Client) Documents(ctx context.Context, workspace string, ids []string, view View) (*DocumentList, error) {
	list := &DocumentList{Results: []Document{}, Missing: []string{}}
	for len(ids) > 0 {
		batch := ids[:min(len(ids), MaxIDs)]
		ids = ids[len(batch):]
		q := url.Values{"ids": {strings.Join(batch, ",")}}
		page, err := c.list(ctx, workspace, q, view)
		if err != nil {
			return nil, err
		}
		list.Results = append(list.Results, page.Results...)
		list.Missing = append(list.Missing, page.Missing...)
		list.Schema = page.Schema
	}
	return list, nil
}

// Recent reads back the caller's most recent uploads to workspace, newest
// first: at most limit of them, or the server's default number (20) for
// zero.
func (c *Client) Recent(ctx context.Context, workspace string, limit int, view View) (*DocumentList, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.list(ctx, workspace, q, view)
}

func (c *Client) list(ctx context.Context, workspace string, q url.Values, view View) (*DocumentList, error) {
	if view != Full {
		q.Set("view", string(view))
	}
	path := documentsPath(workspace)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out envelope[DocumentList]
	if err := c.API.JSON(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return nil, c.explain(err, 0)
	}
	if out.Data.Results == nil {
		out.Data.Results = []Document{}
	}
	if out.Data.Missing == nil {
		out.Data.Missing = []string{}
	}
	out.Data.Schema = out.Meta.Schema
	return &out.Data, nil
}

// Document reads back one document of workspace.
func (c *Client) Document(ctx context.Context, workspace, id string, view View) (*Document, error) {
	path := documentsPath(workspace) + "/" + url.PathEscape(id)
	if view != Full {
		path += "?view=" + url.QueryEscape(string(view))
	}
	var out envelope[Document]
	if err := c.API.JSON(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return nil, c.explain(err, 0)
	}
	return &out.Data, nil
}

// envelope is the API's answer around its data.
type envelope[T any] struct {
	Data T `json:"data"`
	Meta struct {
		Schema string `json:"schema"`
	} `json:"meta"`
}

func documentsPath(workspace string) string {
	return "/api/v1/cli/workspaces/" + url.PathEscape(workspace) + "/documents"
}

// RefusedError is an upload the server turned away as a whole, so nothing
// of it was stored: a file it refused (FILE_REFUSED, with every file's
// outcome in Results), the monthly allowance (QUOTA_EXCEEDED, with Needed
// and Allowance), or a virus scanner too busy for any of the files
// (SCANNER_BUSY, which the client has already waited out twice). It
// unwraps to the *api.Error, whose status and code say which.
type RefusedError struct {
	Err       *api.Error
	Results   []Outcome
	Needed    int
	Allowance *Allowance
}

// Error is the server's sentence, without the details that Results,
// Needed and Allowance hold.
func (e *RefusedError) Error() string {
	plain := *e.Err
	plain.Details = nil
	return plain.Error()
}

func (e *RefusedError) Unwrap() error { return e.Err }

// explain makes the server's refusals read well: an answer's details
// become a *RefusedError rather than Go's rendering of nested maps at the
// end of the sentence, a host without the endpoints says that it does not
// take uploads, and a proxy's bare 413 says how large the upload was.
// size is the request body's, for an upload.
func (c *Client) explain(err error, size int64) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.Unsupported:
		// A bare 404: a firmfact from before uploads, or a host that is
		// not firmfact.
		e := *apiErr
		e.Message = c.API.Host + " does not offer uploads yet"
		return &e
	case apiErr.Status == http.StatusRequestEntityTooLarge && apiErr.Code == "":
		// Not the service's own refusal, which has a code: a proxy on
		// the way with a lower limit.
		e := *apiErr
		e.Message = fmt.Sprintf("the upload (%s) is larger than %s takes in one request", ui.Bytes(size), c.API.Host)
		return &e
	case len(apiErr.Details) > 0:
		return refused(apiErr)
	}
	return err
}

// refused reads the details of e.
func refused(e *api.Error) *RefusedError {
	r := &RefusedError{Err: e}
	raw, err := json.Marshal(e.Details)
	if err != nil {
		return r
	}
	var details struct {
		Results   []Outcome  `json:"results"`
		Needed    int        `json:"needed"`
		Allowance *Allowance `json:"allowance"`
	}
	if json.Unmarshal(raw, &details) == nil {
		r.Results, r.Needed, r.Allowance = details.Results, details.Needed, details.Allowance
	}
	return r
}
