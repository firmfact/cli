package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// storeFirstAndDrop stores the first file of an upload request, as the
// service does before a proxy on the way gives up on the request, and
// closes the connection without an answer.
func storeFirstAndDrop(s *uploadServer, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.t.Error(err)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	files, sums := r.MultipartForm.File["files[]"], r.MultipartForm.Value["sha256[]"]
	s.mu.Lock()
	s.newDocument(files[0].Filename, sums[0])
	s.mu.Unlock()
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

// answerInProgress answers a preflight with every file being uploaded
// right now, as while the request whose answer was lost is still stored.
func answerInProgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files []map[string]any `json:"files"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	for i, f := range req.Files {
		f["index"], f["status"] = i, "in_progress"
	}
	answerJSON(w, http.StatusOK, encode(map[string]any{"data": map[string]any{
		"workspace": json.RawMessage(acmeWorkspace), "files": req.Files, "sets": []any{},
		"allowance": map[string]any{"limit": 25, "used": 4, "pending": 0, "remaining": 21, "resets_on": "2026-10-01"},
	}}))
}

// A group whose answer was lost with some of its files stored is not sent
// again in part: the rest would be a group of their own, counted again.
func TestUploadDoesNotResendPartOfAGroup(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "invoice.pdf", "usage.xlsx")
	s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		storeFirstAndDrop(s, w, r)
		return true
	}
	stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "invoice.pdf", "usage.xlsx", "--related")
	if code, _ := Classify(err); code != ExitUnavailable {
		t.Errorf("exit %d: %v (stderr %q)", code, err, stderr)
	}
	if _, uploads := s.counts(); uploads != 1 {
		t.Errorf("%d upload requests; the rest of the group went again", uploads)
	}
	var got uploadJSON
	decodeOnly(t, "upload", stdout, &got)
	if r := got.Data.Results; len(r) != 2 || r[0].Outcome != outcomeCreated || r[1].Outcome != outcomeFailed || r[1].Code != "group_incomplete" {
		t.Errorf("results = %+v", r)
	}
}

// When the answer to an upload is lost while the server is still storing
// its file, the CLI asks again until the file is there, rather than giving
// up at once; one still being stored after that is worth a later run.
func TestUploadWaitsForAFileStillBeingStored(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf")

	t.Run("stored", func(t *testing.T) {
		s := newUploadServer(t)
		s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			dropAnswer(s, w, r, false)
			return true
		}
		s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			switch n {
			case 1:
				return false
			case 2, 3:
				answerInProgress(w, r)
				return true
			}
			// Stored at last.
			s.mu.Lock()
			if len(s.docs) == 0 {
				s.newDocument("LSEG-2026-09.pdf", sha256Hex(content("LSEG-2026-09.pdf")))
			}
			s.mu.Unlock()
			return false
		}
		stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
		if err != nil || !strings.Contains(stdout, "LSEG-2026-09.pdf: invoice, ready for review\n") {
			t.Fatalf("%v (stderr %q)\n%s", err, stderr, stdout)
		}
		if preflights, uploads := s.counts(); preflights != 4 || uploads != 1 {
			t.Errorf("%d preflights, %d uploads", preflights, uploads)
		}
	})
	t.Run("still being stored", func(t *testing.T) {
		prev := recheckWait
		// Long enough for a few rechecks however slow the machine.
		recheckWait = 250 * time.Millisecond
		t.Cleanup(func() { recheckWait = prev })
		s := newUploadServer(t)
		s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			dropAnswer(s, w, r, false)
			return true
		}
		s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 1 {
				return false
			}
			answerInProgress(w, r)
			return true
		}
		code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
		if code != ExitUnavailable || !strings.Contains(msg, "1 file could not be sent now") {
			t.Errorf("exit %d: %s", code, msg)
		}
		if preflights, uploads := s.counts(); preflights < 3 || uploads != 1 {
			t.Errorf("%d preflights, %d uploads", preflights, uploads)
		}
	})
}

// fullReads are the reads of documents in full the server had, as the
// number of ids each asked for.
func (s *uploadServer) fullReads() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, req := range s.requests {
		if !strings.HasPrefix(req, "GET ") || !strings.Contains(req, "ids=") || strings.Contains(req, "view=state") {
			continue
		}
		out = append(out, strings.Count(req, "%2C")+1)
	}
	return out
}

// Once files are stored, a read of what firmfact made of them that fails
// does not lose the report: the results show what the upload's answers
// said, the error says how to see the rest, and --json still prints the
// envelope. A read that fails in a way that may pass is tried again.
func TestUploadReportsWhenTheReadBackFails(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")
	fullRead := func(r *http.Request) bool {
		return r.Method == http.MethodGet && r.URL.Query().Has("ids") && r.URL.Query().Get("view") != "state"
	}

	t.Run("text", func(t *testing.T) {
		s := newUploadServer(t)
		s.anyHook = func(w http.ResponseWriter, r *http.Request) bool {
			if !fullRead(r) {
				return false
			}
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		var out, errOut strings.Builder
		err := NewRootCommand(Build{Version: "test"}, []string{"--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf"},
			IOStreams{In: strings.NewReader(""), Out: &out, Err: &errOut}).Execute()
		if code, _ := Classify(err); code != ExitUnavailable || !strings.Contains(err.Error(), "the files were sent, but reading back what firmfact made of them failed (") ||
			!strings.Contains(err.Error(), "see it with `firmfact upload status --workspace "+acmeID) {
			t.Errorf("exit %d: %v", code, err)
		}
		for _, want := range []string{"Uploading to Acme: 2 new\n", "LSEG-2026-09.pdf: ready for review\n", "BBG-88123.pdf: ready for review\n"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q in\n%s", want, out.String())
			}
		}
		if reads := s.fullReads(); len(reads) != fullReadAttempts {
			t.Errorf("full reads %v, want %d attempts", reads, fullReadAttempts)
		}
	})
	t.Run("json", func(t *testing.T) {
		s := newUploadServer(t)
		s.anyHook = func(w http.ResponseWriter, r *http.Request) bool {
			if !fullRead(r) {
				return false
			}
			answerJSON(w, http.StatusNotFound, `{"error":"Workspace not found","code":"NOT_FOUND"}`)
			return true
		}
		stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
		if code, _ := Classify(err); code != ExitNotFound {
			t.Errorf("exit %d: %v", code, err)
		}
		var got uploadJSON
		decodeOnly(t, "upload", stdout, &got)
		if r := got.Data.Results; len(r) != 2 || r[0].Outcome != outcomeCreated || r[1].Outcome != outcomeCreated {
			t.Errorf("results = %+v", r)
		}
		if len(got.Notes) != 1 || !strings.HasPrefix(got.Notes[0], "The files were sent, but reading back what firmfact made of them failed (Workspace not found)") {
			t.Errorf("notes = %q", got.Notes)
		}
	})
	t.Run("tried again", func(t *testing.T) {
		s := newUploadServer(t)
		failed := false
		s.anyHook = func(w http.ResponseWriter, r *http.Request) bool {
			if !fullRead(r) || failed {
				return false
			}
			failed = true
			w.WriteHeader(http.StatusGatewayTimeout)
			return true
		}
		stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
		if err != nil || !strings.Contains(stdout, "LSEG-2026-09.pdf: invoice, ready for review\n") {
			t.Errorf("%v\n%s", err, stdout)
		}
	})
}

// The documents are read back in full a few at a time, so that each read
// stays well inside the time a proxy on the way gives it.
func TestUploadReadsBackInBatches(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.remaining = 100
	var names []string
	for i := range fullReadBatch + 5 {
		names = append(names, "inbox/"+itoa(100+i)+".pdf")
	}
	uploadDir(t, names...)
	if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "inbox", "-r"); err != nil {
		t.Fatal(err)
	}
	if reads := s.fullReads(); !slices.Equal(reads, []int{fullReadBatch, 5}) {
		t.Errorf("full reads of %v documents", reads)
	}
}

// With --json, an interrupt after files went still prints the envelope:
// what was stored, and what was not sent.
func TestUploadJSONWhenInterrupted(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")
	s := newUploadServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			return false
		}
		// The server sees the connection close only once it has read the
		// body.
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done()
		return true
	}
	stdout, _, err := runContext(ctx, "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if code, _ := Classify(err); code != ExitInterrupted {
		t.Errorf("exit %d: %v", code, err)
	}
	var got uploadJSON
	decodeOnly(t, "upload", stdout, &got)
	if r := got.Data.Results; len(r) != 2 || r[0].Outcome != outcomeCreated || r[1].Outcome != outcomeNotSent {
		t.Errorf("results = %+v", r)
	}
	if !slices.Contains(got.Notes, "Stopped by an interrupt; run the same command again to send the rest: files already there are skipped.") {
		t.Errorf("notes = %q", got.Notes)
	}
}

// --json names the schema the server named for the entries it passes on.
func TestUploadJSONNamesTheServersSchema(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf")
	s := newUploadServer(t)
	s.schema = "document_result/2"
	var got struct {
		Meta struct {
			Schema string `json:"schema"`
		} `json:"meta"`
	}
	for _, args := range [][]string{
		{"upload", "LSEG-2026-09.pdf"},
		{"upload", "status", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d"},
		{"upload", "status"},
	} {
		stdout, _, err := run("test", append([]string{"--host", s.URL(), "--workspace", "Acme", "--json"}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		decodeOnly(t, strings.Join(args, " "), stdout, &got)
		if got.Meta.Schema != "document_result/2" {
			t.Errorf("%v: meta.schema = %q", args, got.Meta.Schema)
		}
	}
}
