package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
)

const docsPath = "/api/v1/cli/workspaces/Acme/documents"

// newClient is an upload client for srv, signed in with a token good for
// an hour, which it keeps in scratch space, never in the user's keyring.
func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	t.Setenv("FIRMFACT_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	t.Setenv("FIRMFACT_TOKEN_STORE", "file")
	t.Setenv("FIRMFACT_TOKEN", "")
	c := api.New(srv.URL)
	if err := c.SetToken(&config.Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return &Client{API: c}
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func answer(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// The preflight sends names, sizes and checksums, no bytes, and reads the
// server's verdict on each file, the sets, the allowance and the limits,
// keeping what it does not know in Raw.
func TestPreflight(t *testing.T) {
	invoice := writeFile(t, "LSEG-2026-07.pdf", "%PDF-1.7 an invoice")
	exe := writeFile(t, "notes.exe", "MZ")
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != docsPath+"/preflight" || r.Header.Get("Authorization") != "Bearer at-old" {
			t.Errorf("%s %s (%s)", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		want := fmt.Sprintf(`map[files:[map[name:LSEG-2026-07.pdf sha256:%s size:19] map[name:notes.exe sha256:%s size:2]] group_as_related:true]`, invoice.SHA256, exe.SHA256)
		if fmt.Sprint(got) != want {
			t.Errorf("request %v\nwant    %s", got, want)
		}
		answer(w, http.StatusOK, `{"success":true,"data":{
			"workspace":{"id":"1977ef5a-480b-4ea7-94ce-ce9fe4f308fd","name":"Acme","demo":false},
			"files":[
				{"index":0,"name":"LSEG-2026-07.pdf","size":19,"sha256":"`+invoice.SHA256+`","status":"duplicate",
				 "document":{"id":"423a2262-85dd-4cf1-9b51-60c7bbf2ff7d","state":"ready_for_review","url":"https://app.firmfact.example/d/423a","own":true}},
				{"index":1,"name":"notes.exe","size":2,"sha256":"`+exe.SHA256+`","status":"refused","code":"unsupported_type",
				 "message":"notes.exe: files of this type cannot be uploaded, so it was not stored.","hint":"new in a later server"}],
			"sets":[{"filenames":["LSEG-2026-07.pdf","notes.exe"],"kind":"related","bytes":21,"status":"ok"}],
			"allowance":{"limit":25,"used":3,"pending":1,"remaining":21,"resets_on":"2026-10-01"},
			"limits":{"max_files_per_request":10,"max_group_bytes":104857600,"max_file_bytes":52428799,"max_request_bytes":105906176,"extensions":[".pdf",".zip"]},
			"scanner":"clamav"}}`)
	})
	c := newClient(t, srv)

	p, err := c.Preflight(context.Background(), "Acme", []File{invoice, exe}, Options{Related: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Workspace.Name != "Acme" || len(p.Files) != 2 || len(p.Sets) != 1 {
		t.Fatalf("got %+v", p)
	}
	dup, refused := p.Files[0], p.Files[1]
	if dup.Status != PreflightDuplicate || dup.Document == nil || dup.Document.State != StateReadyForReview || dup.Document.ID != "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d" {
		t.Errorf("duplicate: %+v", dup)
	}
	if refused.Status != PreflightRefused || refused.Code != "unsupported_type" || !strings.Contains(string(refused.Raw), `"hint":"new in a later server"`) {
		t.Errorf("refused: %+v", refused)
	}
	if a := p.Allowance; a.Limit == nil || *a.Limit != 25 || a.Remaining == nil || *a.Remaining != 21 || a.ResetsOn != "2026-10-01" {
		t.Errorf("allowance: %+v", a)
	}
	if l := p.Limits; l.MaxFilesPerRequest != 10 || l.MaxFileBytes != 52428799 || l.MaxRequestBytes != 105906176 {
		t.Errorf("limits: %+v", l)
	}
	if !strings.Contains(string(p.Raw), `"scanner":"clamav"`) {
		t.Errorf("Raw lost what the CLI does not know: %s", p.Raw)
	}
}

// uploadEndpoint reads each upload as Rails does and checks it: every file
// part application/octet-stream, one checksum per file that matches its
// bytes. It records each form's own checksum, so a test can tell that a
// retry sent the same bytes.
type uploadEndpoint struct {
	mu    sync.Mutex
	forms []string
	names [][]string
}

func (u *uploadEndpoint) read(t *testing.T, r *http.Request) error {
	h := sha256.New()
	r.Body = io.NopCloser(io.TeeReader(r.Body, h))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return err
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	files, sums := r.MultipartForm.File["files[]"], r.MultipartForm.Value["sha256[]"]
	if len(files) == 0 || len(files) != len(sums) {
		return fmt.Errorf("%d files, %d checksums", len(files), len(sums))
	}
	var names []string
	for i, fh := range files {
		if ct := fh.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("%s: Content-Type %q", fh.Filename, ct)
		}
		f, err := fh.Open()
		if err != nil {
			return err
		}
		fsum := sha256.New()
		_, _ = io.Copy(fsum, f)
		f.Close()
		if got := hex.EncodeToString(fsum.Sum(nil)); got != sums[i] {
			return fmt.Errorf("%s: sha256 %s, declared %s", fh.Filename, got, sums[i])
		}
		names = append(names, fh.Filename)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.forms = append(u.forms, hex.EncodeToString(h.Sum(nil)))
	u.names = append(u.names, names)
	return nil
}

const createdAnswer = `{"success":true,"data":{
	"workspace":{"id":"1977ef5a-480b-4ea7-94ce-ce9fe4f308fd","name":"Acme","demo":false},
	"results":[{"index":0,"filename":"LSEG-2026-07.pdf","outcome":"created",
		"document":{"id":"423a2262-85dd-4cf1-9b51-60c7bbf2ff7d","state":"queued","url":"https://app.firmfact.example/d/423a",
			"own":true,"filename":"LSEG-2026-07.pdf","group_id":null,"created_at":"2026-09-27T16:59:48Z","queue_position":3}}],
	"summary":{"created":1}},
	"meta":{"schema":"document_result/1"}}`

// An upload streams the form to the server, which reads it as Rails does,
// and the answer's entries keep what the CLI does not know in Raw.
func TestUploadSendsTheForm(t *testing.T) {
	u := &uploadEndpoint{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != docsPath {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if err := u.read(t, r); err != nil {
			t.Error(err)
		}
		answer(w, http.StatusCreated, createdAnswer)
	})
	c := newClient(t, srv)
	f := writeFile(t, "LSEG-2026-07.pdf", "%PDF-1.7 an invoice")

	up, err := c.Upload(context.Background(), "Acme", []File{f}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if up.Schema != Schema || up.Summary["created"] != 1 || len(up.Results) != 1 || fmt.Sprint(u.names) != "[[LSEG-2026-07.pdf]]" {
		t.Fatalf("got %+v; the server saw %v", up, u.names)
	}
	res := up.Results[0]
	if res.Outcome != OutcomeCreated || res.Document == nil || res.Document.State != StateQueued || res.Document.GroupID != "" {
		t.Errorf("result %+v", res)
	}
	if !strings.Contains(string(res.Document.Raw), `"queue_position":3`) || !strings.Contains(string(res.Raw), `"queue_position":3`) {
		t.Errorf("Raw lost what the CLI does not know: %s", res.Raw)
	}
}

// A rate limit with Retry-After is waited out, and a token the server no
// longer takes is renewed; each time, the whole form is sent again, the
// same bytes from the first.
func TestUploadIsSentAgainAfterARateLimitAndARenewal(t *testing.T) {
	u := &uploadEndpoint{}
	var uploads, renewals atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(docsPath, func(w http.ResponseWriter, r *http.Request) {
		// As Puma does: the whole body first, then the application.
		if err := u.read(t, r); err != nil {
			t.Error(err)
		}
		switch uploads.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "1")
			answer(w, http.StatusTooManyRequests, `{"error":"Rate limit exceeded"}`)
		case 2:
			answer(w, http.StatusUnauthorized, `{"error":"Token expired"}`)
		default:
			if r.Header.Get("Authorization") != "Bearer at-new" {
				t.Errorf("sent again with %q", r.Header.Get("Authorization"))
			}
			answer(w, http.StatusCreated, createdAnswer)
		}
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		renewals.Add(1)
		answer(w, http.StatusOK, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`)
	})
	srv := serve(t, mux.ServeHTTP)
	c := newClient(t, srv)
	f := writeFile(t, "LSEG-2026-07.pdf", strings.Repeat("%PDF-1.7 an invoice\n", 100_000))

	up, err := c.Upload(context.Background(), "Acme", []File{f}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if up.Results[0].Outcome != OutcomeCreated || renewals.Load() != 1 {
		t.Errorf("outcome %q after %d renewals", up.Results[0].Outcome, renewals.Load())
	}
	if len(u.forms) != 3 || u.forms[0] != u.forms[1] || u.forms[1] != u.forms[2] {
		t.Errorf("forms %v, want the same three", u.forms)
	}
}

// A server that drops the connection part of the way through the form: no
// answer, and an error that says how far the upload got.
func TestDroppedUpload(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(io.Discard, r.Body, 128<<10)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	c := newClient(t, srv)
	f := writeFile(t, "big.pdf", strings.Repeat("%PDF-1.7 a long report\n", 400_000))

	_, err := c.Upload(context.Background(), "Acme", []File{f}, Options{})
	var streamErr *api.StreamError
	var netErr *httpx.Error
	if !errors.As(err, &streamErr) || !errors.As(err, &netErr) || netErr.Kind != httpx.Unreachable {
		t.Fatalf("want a broken *api.StreamError, got %#v", err)
	}
	if want := "the connection to " + strings.TrimPrefix(srv.URL, "http://") + " broke: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message %q, want it to start %q", err, want)
	}
}

// Refusals of a whole upload store nothing, and read as the server's
// sentence, with the details in fields of their own.
func TestRefusalsOfAWholeUpload(t *testing.T) {
	const typeMismatch = "LSEG-2026-07.docx: the contents do not match the .docx extension, so it was not stored. Check that the file has the right extension."
	const quota = "This upload needs 1 of this month's documents and 0 are left, so nothing was stored. The monthly limit resets on October 01, 2026."
	const busy = "The virus scanner is busy, so LSEG-2026-07.docx was not stored. Try again in a minute."
	cases := map[string]struct {
		status   int
		body     string
		attempts int32
		check    func(t *testing.T, e *RefusedError)
		message  string
	}{
		"file refused": {
			status: http.StatusUnprocessableEntity, attempts: 1, message: typeMismatch,
			body: `{"error":"` + typeMismatch + `","code":"FILE_REFUSED","details":{"results":[{"index":0,"filename":"LSEG-2026-07.docx","outcome":"type_mismatch","message":"` + typeMismatch + `"}]}}`,
			check: func(t *testing.T, e *RefusedError) {
				if len(e.Results) != 1 || e.Results[0].Outcome != "type_mismatch" || e.Results[0].Filename != "LSEG-2026-07.docx" {
					t.Errorf("results %+v", e.Results)
				}
			},
		},
		"over the allowance": {
			status: http.StatusForbidden, attempts: 1, message: quota,
			body: `{"error":"` + quota + `","code":"QUOTA_EXCEEDED","details":{"needed":1,"allowance":{"limit":25,"used":25,"pending":0,"remaining":0,"resets_on":"2026-10-01"}}}`,
			check: func(t *testing.T, e *RefusedError) {
				if a := e.Allowance; e.Needed != 1 || a == nil || a.Remaining == nil || *a.Remaining != 0 || a.ResetsOn != "2026-10-01" {
					t.Errorf("needed %d, allowance %+v", e.Needed, a)
				}
			},
		},
		"scanner busy": {
			// Waited out twice first, as any busy answer with Retry-After.
			status: http.StatusServiceUnavailable, attempts: 3, message: busy + " (request id: req-busy)",
			body: `{"error":"` + busy + `","code":"SCANNER_BUSY","details":{"results":[{"index":0,"filename":"LSEG-2026-07.docx","outcome":"scanner_busy"}]}}`,
			check: func(t *testing.T, e *RefusedError) {
				if len(e.Results) != 1 || e.Results[0].Outcome != "scanner_busy" {
					t.Errorf("results %+v", e.Results)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", "0")
				w.Header().Set("X-Request-Id", "req-busy")
				answer(w, tc.status, tc.body)
			})
			c := newClient(t, srv)
			f := writeFile(t, "LSEG-2026-07.docx", "PK not really a docx")

			_, err := c.Upload(context.Background(), "Acme", []File{f}, Options{})
			var refused *RefusedError
			var apiErr *api.Error
			if !errors.As(err, &refused) || !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Fatalf("want a *RefusedError of the %d, got %#v", tc.status, err)
			}
			if err.Error() != tc.message {
				t.Errorf("message %q\nwant    %q", err, tc.message)
			}
			tc.check(t, refused)
			if n := attempts.Load(); n != tc.attempts {
				t.Errorf("%d attempts, want %d", n, tc.attempts)
			}
		})
	}
}

// A host from before uploads answers every endpoint with a bare 404: the
// CLI says the host does not offer uploads, which exits 6 as a host without
// the CLI's endpoints. The API's own "not found" keeps its words.
func TestHostWithoutUploads(t *testing.T) {
	var notFound atomic.Value
	notFound.Store("")
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if body := notFound.Load().(string); body != "" {
			answer(w, http.StatusNotFound, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	c := newClient(t, srv)
	f := writeFile(t, "a.pdf", "%PDF-1.7")
	ctx := context.Background()
	calls := map[string]func() error{
		"preflight": func() error { _, err := c.Preflight(ctx, "Acme", []File{f}, Options{}); return err },
		"upload":    func() error { _, err := c.Upload(ctx, "Acme", []File{f}, Options{}); return err },
		"documents": func() error {
			_, err := c.Documents(ctx, "Acme", []string{"423a2262-85dd-4cf1-9b51-60c7bbf2ff7d"}, StateOnly)
			return err
		},
		"recent": func() error { _, err := c.Recent(ctx, "Acme", 0, Full); return err },
		"document": func() error {
			_, err := c.Document(ctx, "Acme", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", Full)
			return err
		},
	}
	for name, call := range calls {
		err := call()
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || !apiErr.Unsupported || err.Error() != srv.URL+" does not offer uploads yet" {
			t.Errorf("%s: got %#v", name, err)
		}
	}
	notFound.Store(`{"error":"Workspace not found","code":"NOT_FOUND"}`)
	err := calls["preflight"]()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Unsupported || err.Error() != "Workspace not found" {
		t.Errorf("the API's own 404: got %#v", err)
	}
}

// A proxy on the way that turns the upload away as too large, with a page
// of its own rather than the service's refusal, is named for what it is.
func TestProxyRefusesALargeUpload(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, "<html><h1>413 Request Entity Too Large</h1></html>")
	})
	c := newClient(t, srv)
	f := writeFile(t, "a.pdf", strings.Repeat("x", 3000))

	_, err := c.Upload(context.Background(), "Acme", []File{f}, Options{})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %#v", err)
	}
	if !strings.HasPrefix(err.Error(), "the upload (3.") || !strings.HasSuffix(err.Error(), " KB) is larger than "+srv.URL+" takes in one request") {
		t.Errorf("message %q", err)
	}
}

// Documents asks for the ids MaxIDs at a time, and gives back the results
// and the missing ids in the order asked.
func TestDocumentsAsksInTurns(t *testing.T) {
	ids := make([]string, 150)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	var asked []int
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != docsPath || r.URL.Query().Get("view") != "state" {
			t.Errorf("%s", r.URL)
		}
		batch := strings.Split(r.URL.Query().Get("ids"), ",")
		asked = append(asked, len(batch))
		var results []string
		var missing []string
		for _, id := range batch {
			// Every tenth id is not there.
			if strings.HasSuffix(id, "0") {
				missing = append(missing, `"`+id+`"`)
				continue
			}
			results = append(results, `{"id":"`+id+`","state":"reading","url":"https://app.firmfact.example/d","own":true}`)
		}
		answer(w, http.StatusOK, `{"success":true,"data":{"results":[`+strings.Join(results, ",")+`],"missing":[`+strings.Join(missing, ",")+`]},"meta":{"schema":"document_result/1"}}`)
	})
	c := newClient(t, srv)

	list, err := c.Documents(context.Background(), "Acme", ids, StateOnly)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(asked) != "[100 50]" || len(list.Results) != 135 || len(list.Missing) != 15 || list.Schema != Schema {
		t.Fatalf("asked %v; %d results, %d missing, schema %q", asked, len(list.Results), len(list.Missing), list.Schema)
	}
	if list.Results[0].ID != ids[1] || list.Results[134].ID != ids[149] || list.Missing[14] != ids[140] || !InProgress(list.Results[0].State) {
		t.Errorf("out of order: %s, %s, %s", list.Results[0].ID, list.Results[134].ID, list.Missing[14])
	}

	none, err := c.Documents(context.Background(), "Acme", nil, Full)
	if err != nil || len(none.Results) != 0 || none.Missing == nil || len(asked) != 2 {
		t.Errorf("no ids: %+v, %v, %d requests", none, err, len(asked))
	}
}

// A finished upload of the caller's own reads back in full: what was read,
// the contract match, the variance preview and what needs review. Numbers
// where decimal strings are expected are taken too, and fields the CLI
// does not know are kept in Raw.
func TestDocumentReadsTheWholeResult(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "document_result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "/documents") {
			answer(w, http.StatusOK, `{"success":true,"data":{"results":[]},"meta":{"schema":"document_result/1"}}`)
			return
		}
		answer(w, http.StatusOK, string(fixture))
	})
	c := newClient(t, srv)

	d, err := c.Document(context.Background(), "Acme", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", Full)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateReadyForReview || InProgress(d.State) || d.Type != "invoice" || d.Publishable == nil || !*d.Publishable || d.Booked == nil || *d.Booked || d.Error != nil {
		t.Errorf("document %+v", d)
	}
	r := d.Read
	if r == nil || r.Vendor.LinkedTo != "Refinitiv Limited" || r.Number != "INV-8841207" || r.Dates.Due != "2026-07-31" || r.Amounts.Total != "1180.00" || len(r.Checks) != 1 {
		t.Fatalf("read %+v", r)
	}
	if l := r.Lines[1]; l.Quantity != "1" || l.Item.Confidence != "0.85" || l.Period.Start != "2026-07-01" || l.Item.Name != "Real-Time Exchange Fees" {
		t.Errorf("line 2 %+v, item %+v", l, l.Item)
	}
	if m := d.ContractMatch; m.Status != "linked" || m.How != "automatic" || m.Score != "1" || m.RunnerUpScore != "" || m.Contract.Number != "LSEG-4471" {
		t.Errorf("contract match %+v", m)
	}
	if v := d.Variance; !v.Preview || v.Amount != "30.00" || v.Percent != "2.6" || v.Counts.Clean != 1 || len(v.Lines) != 1 || v.Lines[0].Driver != "unit_price" {
		t.Errorf("variance %+v", v)
	}
	if rv := d.Review; rv.Counts.Total != 6 || len(rv.Items) != 2 || rv.Items[0].Line != 1 || len(rv.Items[0].Actions) != 3 {
		t.Errorf("review %+v", rv)
	}
	var raw bytes.Buffer
	if err := json.Compact(&raw, d.Raw); err != nil || len(d.Notes) != 1 || !strings.Contains(raw.String(), `"approvals":{"required":2,"given":0}`) {
		t.Errorf("notes %v; Raw lost what the CLI does not know (%v)", d.Notes, err)
	}

	if _, err := c.Document(context.Background(), "Acme", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", StateOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recent(context.Background(), "Acme", 5, Full); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recent(context.Background(), "Acme", 0, StateOnly); err != nil {
		t.Fatal(err)
	}
	want := []string{
		docsPath + "/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
		docsPath + "/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d?view=state",
		docsPath + "?limit=5",
		docsPath + "?view=state",
	}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("asked\n%v\nwant\n%v", paths, want)
	}
}

// A workspace name goes in the path escaped.
func TestWorkspaceNameIsEscaped(t *testing.T) {
	var got string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		answer(w, http.StatusOK, `{"success":true,"data":{"results":[]},"meta":{}}`)
	})
	c := newClient(t, srv)
	if _, err := c.Recent(context.Background(), "Acme / Research?", 0, Full); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/cli/workspaces/Acme%20%2F%20Research%3F/documents"; got != want {
		t.Errorf("path %q, want %q", got, want)
	}
}

func TestInProgress(t *testing.T) {
	for state, want := range map[string]bool{
		StateQueued: true, StateReading: true, StateMatching: true, StateRetrying: true,
		StateReadyForReview: false, StatePublished: false, StateAttached: false, StateFailed: false, StateSkipped: false,
		"scanning": false, // unknown: shown as it is rather than waited for
	} {
		if InProgress(state) != want {
			t.Errorf("InProgress(%q) = %v", state, !want)
		}
	}
}

func TestDecimal(t *testing.T) {
	for raw, want := range map[string]Decimal{`"1180.00"`: "1180.00", `1180.5`: "1180.5", `null`: "", ` "0.85" `: "0.85"} {
		var d Decimal
		if err := json.Unmarshal([]byte(raw), &d); err != nil || d != want {
			t.Errorf("%s: %q, %v; want %q", raw, d, err, want)
		}
	}
	for _, raw := range []string{`{}`, `true`, `"unterminated`} {
		var d Decimal
		if err := d.UnmarshalJSON([]byte(raw)); err == nil {
			t.Errorf("%s: want an error", raw)
		}
	}
}
