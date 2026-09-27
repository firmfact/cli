package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// uploadServer is a firmfact host with the upload endpoints, which it
// answers as the service does: a preflight says which files are new, which
// it has and which it refuses; an upload stores each file as a document
// that the reads then see go through its states to the entry a fixture
// holds. A test changes its answers with the fields below.
type uploadServer struct {
	t   *testing.T
	srv *httptest.Server

	// workspace is the one every request names, and me lists the
	// sign-in's workspaces (a Demo one, the default, and it).
	workspace string
	me        string
	// remaining is what is left of the monthly allowance.
	remaining int
	// states are the states the reads of a document see before its
	// final one, by file name; "reading" when not given.
	states map[string][]string
	// uploadHook answers the nth upload request (from 1) itself when it
	// returns true.
	uploadHook func(n int, w http.ResponseWriter, r *http.Request) bool
	// preflightHook does the same for preflights.
	preflightHook func(n int, w http.ResponseWriter, r *http.Request) bool
	// anyHook answers any request itself when it returns true.
	anyHook func(w http.ResponseWriter, r *http.Request) bool
	// schema is what the answers name in meta.schema.
	schema string
	// sets are the sets of reports the server recognises by name: from
	// the files of a preflight without group_as_related, each set's file
	// names, and its kind and guidance.
	sets func(names []string) []map[string]any
	// byName refuses a file for its name alone, when it returns a code,
	// as the service refuses a Data License delivery's.
	byName func(name string) (code, message string)

	mu         sync.Mutex
	docs       []*fakeDocument
	bySHA      map[string]*fakeDocument
	fixtures   map[string]string // file name to the document's entry once finished
	preflights int
	uploads    int
	// sent is the file names of each upload request, and related whether
	// it asked for a group.
	sent    [][]string
	related []bool
	// requests is every request, as "METHOD /path?query".
	requests []string
	// preflighted is the file names of each preflight.
	preflighted [][]string
}

// fakeDocument is a document the server holds.
type fakeDocument struct {
	id, filename, sha string
	own               bool
	states            []string
	reads             int
	final             map[string]any
}

// uploadTestdata holds the fixtures and golden files of the upload tests,
// by an absolute path: the tests change directory to where their files are.
var uploadTestdata = func() string {
	dir, err := filepath.Abs(filepath.Join("testdata", "upload"))
	if err != nil {
		panic(err)
	}
	return dir
}()

const (
	acmeID        = "1977ef5a-480b-4ea7-94ce-ce9fe4f308fd"
	acmeWorkspace = `{"id":"` + acmeID + `","name":"Acme","demo":false}`
	demoID        = "d2ff4919-02ba-4990-ae82-8793f225b71b"
	docsHost      = "https://app.firmfact.example"
)

// newUploadServer starts a server with the documents in
// testdata/upload/documents, signed in for an hour.
func newUploadServer(t *testing.T) *uploadServer {
	t.Helper()
	s := &uploadServer{
		t:         t,
		workspace: acmeWorkspace,
		me: `[{"id":"` + demoID + `","name":"Demo","default":true,"demo":true},` +
			`{"id":"` + acmeID + `","name":"Acme"}]`,
		remaining: 21,
		schema:    "document_result/1",
		states:    map[string][]string{},
		bySHA:     map[string]*fakeDocument{},
		fixtures:  map[string]string{},
	}
	dir := filepath.Join(uploadTestdata, "documents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw := readFixtureFile(t, filepath.Join(dir, e.Name()))
		var head struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal([]byte(raw), &head); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		s.fixtures[head.Filename] = raw
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	storedToken(t, s.srv.URL, time.Hour)
	return s
}

func readFixtureFile(t testing.TB, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// URL is the host, for --host.
func (s *uploadServer) URL() string { return s.srv.URL }

// has puts a finished document of the caller's own in the workspace,
// holding content, with the fixture of name as its entry.
func (s *uploadServer) has(name, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.newDocument(name, sha256Hex(content))
	d.states = nil
}

// newDocument stores a document; mu is held.
func (s *uploadServer) newDocument(name, sha string) *fakeDocument {
	d := &fakeDocument{id: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(s.docs)+1), filename: name, sha: sha, own: true}
	d.final = map[string]any{"state": "ready_for_review"}
	if raw, ok := s.fixtures[name]; ok {
		d.final = nil
		if err := json.Unmarshal([]byte(raw), &d.final); err != nil {
			s.t.Fatal(err)
		}
		d.id = d.final["id"].(string)
	}
	d.states = slices.Clone(s.states[name])
	if d.states == nil {
		d.states = []string{"reading"}
	}
	d.states = append([]string{"queued"}, d.states...)
	s.docs = append(s.docs, d)
	s.bySHA[sha] = d
	return d
}

// state is the document's state now.
func (d *fakeDocument) state() string {
	if len(d.states) > 0 {
		return d.states[0]
	}
	return d.final["state"].(string)
}

// url is the document's review page.
func (d *fakeDocument) url() string {
	return docsHost + "/accounts/" + acmeID + "/documents/" + d.id
}

// stateEntry is the document as a poll sees it.
func (d *fakeDocument) stateEntry() map[string]any {
	e := map[string]any{"id": d.id, "state": d.state(), "url": d.url(), "own": d.own}
	if d.own {
		e["filename"], e["group_id"], e["created_at"] = d.filename, nil, "2026-09-27T16:59:48Z"
	}
	if reason, ok := d.final["reason"]; ok && len(d.states) == 0 {
		e["reason"] = reason
	}
	return e
}

// fullEntry is the document in full: its fixture once finished.
func (d *fakeDocument) fullEntry() map[string]any {
	if len(d.states) > 0 || !d.own {
		return d.stateEntry()
	}
	e := d.stateEntry()
	for k, v := range d.final {
		e[k] = v
	}
	return e
}

func (s *uploadServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	s.mu.Unlock()
	if s.anyHook != nil && s.anyHook(w, r) {
		return
	}
	if r.Header.Get("Authorization") != "Bearer at" {
		answerJSON(w, http.StatusUnauthorized, `{"error":"Invalid token","code":"UNAUTHORIZED"}`)
		return
	}
	if r.URL.Path == "/api/v1/cli/me" {
		answerJSON(w, http.StatusOK, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":`+s.me+`}}`)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/cli/workspaces/")
	ref, tail, _ := strings.Cut(rest, "/")
	if !ok || !strings.HasPrefix(tail, "documents") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var ws struct{ ID, Name string }
	_ = json.Unmarshal([]byte(s.workspace), &ws)
	if ref != ws.ID && !strings.EqualFold(ref, ws.Name) {
		answerJSON(w, http.StatusNotFound, `{"error":"Workspace not found","code":"NOT_FOUND"}`)
		return
	}
	switch {
	case r.Method == http.MethodPost && tail == "documents/preflight":
		s.mu.Lock()
		s.preflights++
		n := s.preflights
		s.mu.Unlock()
		if s.preflightHook != nil && s.preflightHook(n, w, r) {
			return
		}
		s.preflight(w, r)
	case r.Method == http.MethodPost && tail == "documents":
		s.mu.Lock()
		s.uploads++
		n := s.uploads
		s.mu.Unlock()
		if s.uploadHook != nil && s.uploadHook(n, w, r) {
			return
		}
		status, body := s.accept(r)
		answerJSON(w, status, body)
	case r.Method == http.MethodGet && tail == "documents":
		s.list(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(tail, "documents/"):
		s.show(w, r, strings.TrimPrefix(tail, "documents/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func answerJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// encode is v as JSON, for an answer.
func encode(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// supported are the extensions the service reads.
var supported = []string{".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".docx", ".xlsx", ".xls", ".csv", ".tsv", ".txt", ".eml", ".zip"}

func (s *uploadServer) preflight(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
		GroupAsRelated bool `json:"group_as_related"`
		NewVersion     bool `json:"new_version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Files) == 0 || len(req.Files) > 100 {
		answerJSON(w, http.StatusBadRequest, `{"error":"Name, size and SHA-256 of each file are required.","code":"BAD_REQUEST"}`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	left := s.remaining
	var files []map[string]any
	var names []string
	var bytes int64
	for _, f := range req.Files {
		names = append(names, filepath.Base(f.Name))
	}
	s.preflighted = append(s.preflighted, names)
	for i, f := range req.Files {
		entry := map[string]any{"index": i, "name": filepath.Base(f.Name), "size": f.Size, "sha256": f.SHA256, "status": "ok"}
		bytes += f.Size
		existing := s.bySHA[f.SHA256]
		code, message := "", ""
		if s.byName != nil {
			code, message = s.byName(f.Name)
		}
		switch {
		case code != "":
			entry["status"], entry["code"], entry["message"] = "refused", code, message
		case !slices.Contains(supported, strings.ToLower(filepath.Ext(f.Name))):
			entry["status"], entry["code"] = "refused", "unsupported_type"
			entry["message"] = f.Name + ": files of this type cannot be uploaded, so it was not stored. Supported: " + strings.Join(supported, ", ") + "."
		case f.Size == 0:
			entry["status"], entry["code"] = "refused", "empty_file"
			entry["message"] = f.Name + " is empty, so it was not stored."
		case existing != nil && !req.NewVersion:
			entry["status"] = "duplicate"
			entry["document"] = existing.stateEntry()
		case left <= 0 && (!req.GroupAsRelated || i == 0):
			entry["status"], entry["code"] = "refused", "quota_exceeded"
			entry["message"] = f.Name + ": this month's allowance of documents is used up, so it was not stored. The monthly limit resets on October 01, 2026."
		default:
			if !req.GroupAsRelated || i == 0 {
				left--
			}
		}
		files = append(files, entry)
	}
	sets := []map[string]any{}
	if req.GroupAsRelated && len(req.Files) > 1 {
		set := map[string]any{"filenames": names, "kind": "related", "bytes": bytes, "status": "ok"}
		if len(req.Files) > 10 {
			set["status"], set["code"] = "refused", "too_many_files"
			set["message"] = fmt.Sprintf("You selected %d files. Upload at most 10 at a time.", len(req.Files))
		}
		sets = append(sets, set)
	}
	if !req.GroupAsRelated && s.sets != nil {
		sets = append(sets, s.sets(names)...)
	}
	answerJSON(w, http.StatusOK, encode(map[string]any{"success": true, "data": map[string]any{
		"workspace": json.RawMessage(s.workspace),
		"files":     files,
		"sets":      sets,
		"allowance": map[string]any{"limit": 25, "used": 25 - s.remaining - 1, "pending": 1, "remaining": s.remaining, "resets_on": "2026-10-01"},
		"limits":    map[string]any{"max_files_per_request": 10, "max_group_bytes": 104857600, "max_file_bytes": 52428799, "max_request_bytes": 105906176, "extensions": supported},
	}}))
}

// accept reads an upload and stores its files, as the service does, and
// returns the answer.
func (s *uploadServer) accept(r *http.Request) (status int, body string) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return http.StatusBadRequest, `{"error":"No files were sent.","code":"BAD_REQUEST"}`
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	files, sums := r.MultipartForm.File["files[]"], r.MultipartForm.Value["sha256[]"]
	if len(files) != len(sums) {
		return http.StatusBadRequest, `{"error":"Each file needs its SHA-256.","code":"BAD_REQUEST"}`
	}
	related := r.FormValue("group_as_related") == "1"
	newVersion := r.FormValue("new_version") == "1"
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	var results []map[string]any
	summary := map[string]int{}
	status = http.StatusOK
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			s.t.Error(err)
			continue
		}
		content, _ := io.ReadAll(f)
		f.Close()
		if got := sha256Hex(string(content)); got != sums[i] {
			s.t.Errorf("%s: sha256 %s, declared %s", fh.Filename, got, sums[i])
		}
		names = append(names, fh.Filename)
		entry := map[string]any{"index": i, "filename": fh.Filename}
		if d := s.bySHA[sums[i]]; d != nil && !newVersion {
			entry["outcome"] = "duplicate"
			entry["message"] = fh.Filename + " is already in this workspace."
			entry["document"] = d.stateEntry()
		} else {
			d := s.newDocument(fh.Filename, sums[i])
			entry["outcome"] = "created"
			entry["document"] = d.stateEntry()
			status = http.StatusCreated
		}
		summary[entry["outcome"].(string)]++
		results = append(results, entry)
	}
	s.sent = append(s.sent, names)
	s.related = append(s.related, related)
	return status, encode(map[string]any{"success": true,
		"data": map[string]any{"workspace": json.RawMessage(s.workspace), "results": results, "summary": summary},
		"meta": map[string]any{"schema": s.schema}})
}

// list answers ?ids= (each read of a document in progress moves it on a
// state) and the recent uploads.
func (s *uploadServer) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stateOnly := r.URL.Query().Get("view") == "state"
	results := []map[string]any{}
	missing := []string{}
	if ids := r.URL.Query().Get("ids"); ids != "" {
		for _, id := range strings.Split(ids, ",") {
			d := s.byID(id)
			if d == nil {
				missing = append(missing, id)
				continue
			}
			if stateOnly && len(d.states) > 0 {
				d.reads++
				d.states = d.states[1:]
			}
			if stateOnly {
				results = append(results, d.stateEntry())
			} else {
				results = append(results, d.fullEntry())
			}
		}
		answerJSON(w, http.StatusOK, encode(map[string]any{"success": true,
			"data": map[string]any{"results": results, "missing": missing}, "meta": map[string]any{"schema": s.schema}}))
		return
	}
	for i := len(s.docs) - 1; i >= 0; i-- {
		if s.docs[i].own {
			results = append(results, s.docs[i].stateEntry())
		}
	}
	answerJSON(w, http.StatusOK, encode(map[string]any{"success": true,
		"data": map[string]any{"results": results}, "meta": map[string]any{"schema": s.schema}}))
}

func (s *uploadServer) show(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.byID(id)
	if d == nil {
		answerJSON(w, http.StatusNotFound, `{"error":"Document not found","code":"NOT_FOUND"}`)
		return
	}
	answerJSON(w, http.StatusOK, encode(map[string]any{"success": true, "data": d.fullEntry(), "meta": map[string]any{"schema": s.schema}}))
}

func (s *uploadServer) byID(id string) *fakeDocument {
	for _, d := range s.docs {
		if d.id == id {
			return d
		}
	}
	return nil
}

// counts are how many preflights and upload requests the server had.
func (s *uploadServer) counts() (preflights, uploads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preflights, s.uploads
}

// sentFiles is the file names of each upload request, joined.
func (s *uploadServer) sentFiles() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, names := range s.sent {
		out = append(out, strings.Join(names, "+"))
	}
	return out
}
