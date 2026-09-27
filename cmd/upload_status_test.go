package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/upload"
)

// inUTC shows times in UTC for the rest of the test, whatever the
// machine's zone.
func inUTC(t *testing.T) {
	t.Helper()
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })
}

// upload status lists your recent uploads, newest first, with the ids
// that show one in full; with ids, it shows those in full, and says which
// ids are not documents in the workspace.
func TestUploadStatus(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	inUTC(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-Anywhere-2026.pdf")
	if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-Anywhere-2026.pdf", "--no-wait"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "status")
	if err != nil {
		t.Fatal(err)
	}
	want := `UPLOADED           FILE                   STATE               ID
27 Sep 2026 16:59  BBG-Anywhere-2026.pdf  waiting to be read  6c1f7a2d-4e3b-4d9c-8f80-2b3c4d5e6f70
27 Sep 2026 16:59  LSEG-2026-09.pdf       waiting to be read  423a2262-85dd-4cf1-9b51-60c7bbf2ff7d
Show one in full with ` + "`firmfact upload status <id>`" + `.
`
	if stdout != want {
		t.Errorf("recent =\n%s\nwant\n%s", stdout, want)
	}

	stdout, _, err = run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "status", "--wait",
		"423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", "6C1F7A2D-4E3B-4D9C-8F80-2B3C4D5E6F70")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Waiting for 2 documents to be read (at most 15m)...\n",
		"LSEG-2026-09.pdf: invoice, ready for review\n",
		"BBG-Anywhere-2026.pdf: contract, ready for review\n",
		"\n2 documents: 2 ready for review.\nNothing is booked until someone publishes it there.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}

	stdout, _, err = run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "status",
		"423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", "00000000-0000-4000-8000-000000000999")
	if code, _ := Classify(err); code != ExitNotFound || err.Error() != "no document 00000000-0000-4000-8000-000000000999 in this workspace" {
		t.Errorf("a missing id: exit %d: %v", code, err)
	}
	if !strings.HasPrefix(stdout, "LSEG-2026-09.pdf: invoice, ready for review\n") {
		t.Errorf("the found document was not shown:\n%s", stdout)
	}
}

// With --json, upload status prints the envelope: the documents as the
// server sent them, and the ids it did not find.
func TestUploadStatusJSON(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.has("LSEG-2026-09.pdf", content("LSEG-2026-09.pdf"))

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "status",
		"423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", "00000000-0000-4000-8000-000000000999")
	if code, _ := Classify(err); code != ExitNotFound {
		t.Errorf("exit %d: %v", code, err)
	}
	var got struct {
		Data struct {
			Results []map[string]any
			Missing []string
		}
		Meta  struct{ Schema string }
		Notes []string
	}
	decodeOnly(t, "status --json", stdout, &got)
	if len(got.Data.Results) != 1 || got.Data.Results[0]["approvals"] == nil || got.Data.Missing[0] != "00000000-0000-4000-8000-000000000999" ||
		got.Meta.Schema != "document_result/1" || got.Notes == nil {
		t.Errorf("status --json = %+v", got)
	}

	stdout, _, err = run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "status", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	decodeOnly(t, "status --json", stdout, &got)
	if len(got.Data.Results) != 1 || got.Data.Results[0]["state"] != "ready_for_review" || s.requests[len(s.requests)-1] != "GET /api/v1/cli/workspaces/Acme/documents?limit=5&view=state" {
		t.Errorf("recent --json = %+v (%s)", got, s.requests[len(s.requests)-1])
	}
}

// With --wait, upload status ends as an upload does: 1 when a document
// could not be read, 5 when the wait ran out.
func TestUploadStatusWait(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.states["LSEG-2026-09.pdf"] = strings.Split(strings.Repeat("reading ", 1000), " ")[:1000]
	uploadDir(t, "LSEG-2026-09.pdf", "scan-0034.pdf")
	if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "scan-0034.pdf", "--no-wait"); err != nil {
		t.Fatal(err)
	}
	code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "status", "--wait", "--wait-timeout", "20ms", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d")
	if code != ExitUnavailable || !strings.Contains(msg, "1 document was still being read after 20ms") {
		t.Errorf("exit %d: %s", code, msg)
	}
	code, msg = exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "status", "--wait", "7d2a8b3e-5f4c-4e0d-9a91-3c4d5e6f7081")
	if code != ExitFailed || msg != "1 document could not be read" {
		t.Errorf("exit %d: %s", code, msg)
	}
	// Without --wait, a status is a status.
	if code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "status", "7d2a8b3e-5f4c-4e0d-9a91-3c4d5e6f7081"); code != 0 {
		t.Errorf("exit %d: %s", code, msg)
	}
}

// upload status reads the workspace commands use, else the sign-in's
// default; mistakes on its command line are refused before anything is
// sent.
func TestUploadStatusCommandLine(t *testing.T) {
	isolate(t)
	s := newUploadServer(t)
	s.me = `[{"id":"` + acmeID + `","name":"Acme","default":true}]`
	stdout, _, err := run("test", "--host", s.URL(), "upload", "status")
	if err != nil || stdout != "No uploads of yours in this workspace yet.\n" {
		t.Errorf("%v: %q", err, stdout)
	}
	s.me = `[]`
	if code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "upload", "status"); code != ExitNotFound || !strings.Contains(msg, "no default workspace") {
		t.Errorf("exit %d: %s", code, msg)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"423a2262"}, "423a2262 is not a document id"},
		{[]string{"--wait"}, "--wait needs the ids"},
		{[]string{"--limit", "0"}, "--limit must be from 1 to 100"},
		{[]string{"--limit", "3", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d"}, "--limit is for the list of recent uploads"},
		{[]string{"--wait-timeout", "-1s", "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d"}, "--wait-timeout must be more than 0"},
	} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload", "status"}, c.args...)...)
		if code != ExitUsage || !strings.Contains(msg, c.want) {
			t.Errorf("%v: exit %d, %q", c.args, code, msg)
		}
	}
	if code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Nope", "upload", "status"); code != ExitNotFound {
		t.Errorf("an unknown workspace: exit %d, %q", code, msg)
	}
}

// A wait that ran out says how long it was as upload does: 15m, not 15m0s.
func TestUploadStatusWaitTimeoutReadsAsASpan(t *testing.T) {
	s := &uploadStatus{wait: true, waitTimeout: 15 * time.Minute}
	err := s.result([]*upload.Document{{ID: "x", State: upload.StateReading}}, nil, true)
	if code, _ := Classify(err); code != ExitUnavailable || err.Error() != "1 document was still being read after 15m; firmfact goes on reading, so check again later" {
		t.Errorf("exit %d: %v", code, err)
	}
}
