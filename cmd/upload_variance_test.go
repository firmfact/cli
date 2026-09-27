package cmd

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/upload"
)

// withInvoice gives s an invoice called name, with id, whose variance
// preview is variance (JSON, or null), in the currency the preview names.
func withInvoice(s *uploadServer, name, id, variance string) {
	s.fixtures[name] = `{"id":"` + id + `","state":"ready_for_review","own":true,"filename":"` + name + `",` +
		`"type":"invoice","read":{"type":"invoice","currency":"EUR","amounts":{"total":"1000.00"}},"variance":` + variance + `}`
}

// preview is a variance preview against 1,000.00 contracted, of amount,
// with the percent the server would round it to.
func preview(amount, percent string) string {
	return `{"preview":true,"status":"variance","currency":"EUR","amount":"` + amount + `","percent":"` + percent +
		`","invoiced":"1000.00","contract":"1000.00","summary":"EUR ` + amount + ` above the contract","lines":[]}`
}

const (
	underID = "11111111-1111-4111-8111-111111111111"
	atID    = "22222222-2222-4222-8222-222222222222"
	overID  = "33333333-3333-4333-8333-333333333333"
	otherID = "44444444-4444-4444-8444-444444444444"
)

// uploadGated uploads names to Acme with the flags given after them, and
// returns the exit status and the error's text.
func uploadGated(t *testing.T, s *uploadServer, args ...string) (int, string) {
	t.Helper()
	return exitStatusOf(t.Context(), "test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload"}, args...)...)
}

// An invoice under the threshold passes and one over it ends the upload
// with status 9; one exactly at it passes. A percentage is of the
// contracted amount and exact: 2.04% is over 2%, though the server rounds
// it to 2.0. An amount is in the invoice's currency. Without a value, any
// variance counts, and an invoice that matches its contract passes.
func TestFailOnVarianceThreshold(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	names := []string{"under.pdf", "at.pdf", "over.pdf", "LSEG-2026-09.pdf", "BBG-88123.pdf"}
	uploadDir(t, names...)
	for _, c := range []struct {
		name, file, threshold string
		code                  int
		want                  string
	}{
		{"under a percentage", "under.pdf", "2%", 0, ""},
		{"at a percentage", "at.pdf", "2%", 0, ""},
		{"over a percentage", "over.pdf", "2%", ExitVariance, "1 invoice is over the variance threshold of 2%: over.pdf, EUR 20.40 (2.04%) above the contract"},
		{"under an amount", "LSEG-2026-09.pdf", "2000", 0, ""},
		{"at an amount", "LSEG-2026-09.pdf", "1550", 0, ""},
		{"over an amount", "LSEG-2026-09.pdf", "1549.99", ExitVariance, "1 invoice is over the variance threshold of 1549.99: LSEG-2026-09.pdf, EUR 1,550.00 (14.2%) above the contract"},
		{"any variance", "LSEG-2026-09.pdf", "", ExitVariance, "1 invoice shows a variance: LSEG-2026-09.pdf, EUR 1,550.00 (14.2%) above the contract"},
		{"none at all", "BBG-88123.pdf", "", 0, ""},
		{"a percentage above the invoice's", "LSEG-2026-09.pdf", "15%", 0, ""},
		{"a percentage below the invoice's", "LSEG-2026-09.pdf", "14.22%", ExitVariance, "LSEG-2026-09.pdf, EUR 1,550.00 (14.2202%) above the contract"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newUploadServer(t)
			withInvoice(s, "under.pdf", underID, preview("19.99", "2.0"))
			withInvoice(s, "at.pdf", atID, preview("20.00", "2.0"))
			withInvoice(s, "over.pdf", overID, preview("20.40", "2.0"))
			flag := "--fail-on-variance"
			if c.threshold != "" {
				flag += "=" + c.threshold
			}
			code, msg := uploadGated(t, s, c.file, flag)
			if code != c.code || !strings.Contains(msg, c.want) {
				t.Errorf("exit %d, %q; want %d, %q", code, msg, c.code, c.want)
			}
		})
	}
}

// Below the contract counts as much as above it.
func TestFailOnVarianceBelowTheContract(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "credit.pdf")
	s := newUploadServer(t)
	withInvoice(s, "credit.pdf", overID, preview("-60.00", "6.0"))
	code, msg := uploadGated(t, s, "credit.pdf", "--fail-on-variance=50")
	if code != ExitVariance || !strings.HasSuffix(msg, "credit.pdf, EUR 60.00 (6.0%) below the contract") {
		t.Errorf("exit %d: %s", code, msg)
	}
}

// In a batch, only the invoices over the threshold are named: a clean
// invoice, one under it, one without a contract match, a contract and an
// HR file pass. --json lists the files over it, by the paths the command
// line named, in meta.variance_exceeded; meta.variance_unchecked is there
// and empty. The results are printed as without the flag.
func TestFailOnVarianceMixedBatch(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	withInvoice(s, "under.pdf", underID, preview("19.99", "2.0"))
	names := []string{"BBG-88123.pdf", "LSEG-2026-09.pdf", "under.pdf", "FactSet-Q3.pdf", "BBG-Anywhere-2026.pdf", "HR-2026-09.xlsx"}
	uploadDir(t, names...)

	stdout, _, err := run("test", append([]string{"--host", s.URL(), "--workspace", "Acme", "--json", "upload", "--fail-on-variance=2%"}, names...)...)
	if code, _ := Classify(err); code != ExitVariance || err.Error() != "1 invoice is over the variance threshold of 2%: LSEG-2026-09.pdf, EUR 1,550.00 (14.2%) above the contract" {
		t.Errorf("exit %d: %v", code, err)
	}
	var got struct {
		Data struct {
			Results []struct{ Path, Outcome string }
		}
		Meta struct {
			Schema            string
			VarianceExceeded  []string `json:"variance_exceeded"`
			VarianceUnchecked []string `json:"variance_unchecked"`
		}
	}
	decodeOnly(t, "upload --json", stdout, &got)
	if len(got.Data.Results) != len(names) || got.Meta.Schema != "document_result/1" {
		t.Errorf("envelope = %+v", got)
	}
	if strings.Join(got.Meta.VarianceExceeded, " ") != "LSEG-2026-09.pdf" || got.Meta.VarianceUnchecked == nil || len(got.Meta.VarianceUnchecked) != 0 {
		t.Errorf("meta = %+v", got.Meta)
	}
	if !strings.Contains(stdout, `"variance_unchecked": []`) {
		t.Errorf("variance_unchecked is not an empty list:\n%s", stdout)
	}

	// Without the flag, meta says nothing of the variance, and the exit
	// status nothing either.
	stdout, _, err = run("test", append([]string{"--host", s.URL(), "--workspace", "Acme", "--json", "upload"}, names...)...)
	if err != nil || strings.Contains(stdout, "variance_exceeded") || strings.Contains(stdout, "variance_unchecked") {
		t.Errorf("without --fail-on-variance: %v\n%s", err, stdout)
	}
}

// More invoices over the threshold than a message names: the first three,
// in the order of the command line, and how many more; --json lists all.
func TestFailOnVarianceNamesAFew(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	names := []string{"a.pdf", "b.pdf", "c.pdf", "d.pdf", "e.pdf"}
	for i, name := range names {
		withInvoice(s, name, strings.Repeat(string(rune('a'+i)), 8)+"-0000-4000-8000-000000000000", preview("30.00", "3.0"))
	}
	uploadDir(t, names...)
	stdout, _, err := run("test", append([]string{"--host", s.URL(), "--workspace", "Acme", "--json", "upload", "--fail-on-variance=2%"}, names...)...)
	want := "5 invoices are over the variance threshold of 2%: a.pdf, EUR 30.00 (3.0%) above the contract; " +
		"b.pdf, EUR 30.00 (3.0%) above the contract; c.pdf, EUR 30.00 (3.0%) above the contract; and 2 more"
	if code, _ := Classify(err); code != ExitVariance || err.Error() != want {
		t.Errorf("exit %d: %v", code, err)
	}
	var got struct {
		Meta struct {
			VarianceExceeded []string `json:"variance_exceeded"`
		}
	}
	decodeOnly(t, "upload --json", stdout, &got)
	if strings.Join(got.Meta.VarianceExceeded, " ") != strings.Join(names, " ") {
		t.Errorf("meta = %+v", got.Meta)
	}
}

// A pipeline never reads a pass for a variance nobody checked. An invoice
// whose contract the sign-in may not view, whose preview firmfact could
// not work out or did not send, or whose preview this CLI cannot read, and
// a document someone else uploaded, fail the upload with status 1 and are
// listed in meta.variance_unchecked. A document that could not be read or
// was still being read ends it as without the flag, and an invoice over
// the threshold is named all the same.
func TestFailOnVarianceUnchecked(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "hidden.pdf", "odd.pdf", "LSEG-2026-09.pdf", "scan-0034.pdf", "theirs.pdf", "bare.pdf", "notanamount.pdf")
	cases := []struct {
		name  string
		setup func(*uploadServer)
		args  []string
		code  int
		want  string
	}{
		{"a contract the sign-in may not view", func(s *uploadServer) {
			withInvoice(s, "hidden.pdf", underID, `{"preview":true,"status":"not_available","reason":"hidden_contract","message":"You cannot view the contract this invoice is matched to."}`)
		}, []string{"hidden.pdf"}, ExitFailed,
			"the variance of 1 document could not be checked: hidden.pdf (You cannot view the contract this invoice is matched to.)"},
		{"a preview firmfact could not work out", func(s *uploadServer) {
			withInvoice(s, "odd.pdf", underID, `{"preview":true,"status":"not_available","reason":"not_comparable"}`)
		}, []string{"odd.pdf"}, ExitFailed, "odd.pdf (no variance preview: not comparable)"},
		{"no preview at all", func(s *uploadServer) {
			withInvoice(s, "bare.pdf", underID, `null`)
		}, []string{"bare.pdf"}, ExitFailed, "bare.pdf (firmfact sent no variance preview for it)"},
		{"a preview of a kind this CLI does not know", func(s *uploadServer) {
			withInvoice(s, "odd.pdf", underID, `{"preview":true,"status":"estimated"}`)
		}, []string{"odd.pdf"}, ExitFailed, "odd.pdf (a variance preview this CLI does not know: estimated)"},
		{"an amount that is not one", func(s *uploadServer) {
			withInvoice(s, "notanamount.pdf", underID, `{"preview":true,"status":"variance","amount":"lots"}`)
		}, []string{"notanamount.pdf"}, ExitFailed, "notanamount.pdf (its variance is not an amount: lots)"},
		{"someone else's upload", func(s *uploadServer) {
			s.has("theirs.pdf", content("theirs.pdf"))
			s.docs[0].own = false
		}, []string{"theirs.pdf"}, ExitFailed,
			"the variance of 1 document could not be checked: theirs.pdf (uploaded by someone else, whose results only they see)"},
		{"a document that could not be read, beside one over", nil, []string{"scan-0034.pdf", "LSEG-2026-09.pdf"}, ExitFailed,
			"1 document could not be read; 1 invoice is over the variance threshold of 2%: LSEG-2026-09.pdf"},
		{"a document still being read, beside one over", func(s *uploadServer) {
			s.states["scan-0034.pdf"] = strings.Split(strings.Repeat("reading ", 1000), " ")[:1000]
		}, []string{"scan-0034.pdf", "LSEG-2026-09.pdf", "--wait-timeout", "50ms"}, ExitUnavailable,
			"1 document was still being read after 50ms; check with `firmfact upload status --workspace " + acmeID + " 7d2a8b3e-5f4c-4e0d-9a91-3c4d5e6f7081`; 1 invoice is over"},
		{"one unchecked beside one still being read", func(s *uploadServer) {
			s.states["scan-0034.pdf"] = strings.Split(strings.Repeat("reading ", 1000), " ")[:1000]
			withInvoice(s, "bare.pdf", underID, `null`)
		}, []string{"scan-0034.pdf", "bare.pdf", "--wait-timeout", "50ms"}, ExitFailed,
			"still being read after 50ms; check with"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newUploadServer(t)
			if c.setup != nil {
				c.setup(s)
			}
			code, msg := uploadGated(t, s, append([]string{"--fail-on-variance=2%"}, c.args...)...)
			if code != c.code || !strings.Contains(msg, c.want) {
				t.Errorf("exit %d, %q; want %d, %q", code, msg, c.code, c.want)
			}
		})
	}

	t.Run("listed in the JSON", func(t *testing.T) {
		s := newUploadServer(t)
		withInvoice(s, "bare.pdf", underID, `null`)
		stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "bare.pdf", "LSEG-2026-09.pdf", "--fail-on-variance")
		if code, _ := Classify(err); code != ExitFailed {
			t.Errorf("exit %d: %v", code, err)
		}
		var got struct {
			Meta struct {
				VarianceExceeded  []string `json:"variance_exceeded"`
				VarianceUnchecked []string `json:"variance_unchecked"`
			}
		}
		decodeOnly(t, "upload --json", stdout, &got)
		if strings.Join(got.Meta.VarianceUnchecked, " ") != "bare.pdf" || strings.Join(got.Meta.VarianceExceeded, " ") != "LSEG-2026-09.pdf" {
			t.Errorf("meta = %+v", got.Meta)
		}
	})
}

// The check needs what firmfact read, so --no-wait with it is refused
// before anything is read or sent; so is a threshold that cannot be read
// for certain. A threshold typed after a space reads as a file to upload,
// which the error says how to fix.
func TestFailOnVarianceCommandLine(t *testing.T) {
	isolate(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf", "2024/invoice.pdf")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"LSEG-2026-09.pdf", "--fail-on-variance", "--no-wait"}, "--fail-on-variance needs what firmfact read from the invoices, which --no-wait does not wait for"},
		{[]string{"LSEG-2026-09.pdf", "--fail-on-variance=2%", "--no-wait"}, "--fail-on-variance needs what firmfact read"},
		{[]string{"LSEG-2026-09.pdf", "--fail-on-variance=2,5%"}, "write the threshold with a decimal point"},
		{[]string{"LSEG-2026-09.pdf", "--fail-on-variance="}, "give a percentage of the contracted amount"},
		{[]string{"LSEG-2026-09.pdf", "--fail-on-variance=-1"}, "-1 is not a threshold"},
		{[]string{"LSEG-2026-09.pdf", "--fail-on-variance", "2%"}, "no such file: 2%; to give --fail-on-variance a threshold, join the two with =, as in --fail-on-variance=2%"},
		{[]string{"--fail-on-variance", "2.5 %", "LSEG-2026-09.pdf"}, "no such file: 2.5 %; to give --fail-on-variance a threshold, join the two with =, as in --fail-on-variance=2.5%"},
		{[]string{"--fail-on-variance", "50", "LSEG-2026-09.pdf"}, "as in --fail-on-variance=50"},
	} {
		code, msg := uploadGated(t, s, c.args...)
		if code != ExitUsage || !strings.Contains(msg, c.want) {
			t.Errorf("%v: exit %d, %q", c.args, code, msg)
		}
	}
	// With a threshold given, or without the flag, a word that names no
	// file is only that.
	for _, args := range [][]string{{"--fail-on-variance=2%", "50"}, {"50"}} {
		if code, msg := uploadGated(t, s, args...); code != ExitUsage || msg != "no such file: 50" {
			t.Errorf("%v: exit %d, %q", args, code, msg)
		}
	}
	if preflights, uploads := s.counts(); preflights+uploads != 0 {
		t.Errorf("%d preflights and %d uploads for command lines that cannot run", preflights, uploads)
	}
	// A folder with a name like a threshold is a folder.
	fastUploadPolls(t)
	if code, msg := uploadGated(t, s, "--fail-on-variance", "2024", "-r"); code != 0 {
		t.Errorf("a folder called 2024: exit %d, %q", code, msg)
	}
}

// The exit status is 9 and its name variance_exceeded, which --json's
// error says.
func TestVarianceExitStatus(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf")
	_, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "LSEG-2026-09.pdf", "--fail-on-variance=10%")
	var buf bytes.Buffer
	if code := ReportError(&buf, err, true); code != 9 {
		t.Errorf("exit %d", code)
	}
	want := `{"error":{"message":"1 invoice is over the variance threshold of 10%: LSEG-2026-09.pdf, EUR 1,550.00 (14.2%) above the contract","code":9,"status":"variance_exceeded"}}` + "\n"
	if buf.String() != want {
		t.Errorf("error = %s", buf.String())
	}
}

// upload status checks the documents it names as upload does, once each
// is read: it waits as --wait does, and lists them by id in the JSON. It
// needs ids, and says so of a threshold typed after a space.
func TestUploadStatusFailOnVariance(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf", "--no-wait"); err != nil {
		t.Fatal(err)
	}
	const lseg, bbg = "423a2262-85dd-4cf1-9b51-60c7bbf2ff7d", "5b0e6f1c-3d2a-4c8b-9e7f-1a2b3c4d5e6f"
	status := func(args ...string) (string, error) {
		stdout, _, err := run("test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload", "status"}, args...)...)
		return stdout, err
	}

	stdout, err := status(bbg, "--fail-on-variance")
	if err != nil || !strings.Contains(stdout, "Waiting for 1 document to be read") {
		t.Errorf("a clean invoice: %v\n%s", err, stdout)
	}
	stdout, err = status(lseg, bbg, "--json", "--fail-on-variance=2%")
	if code, _ := Classify(err); code != ExitVariance || err.Error() != "1 invoice is over the variance threshold of 2%: LSEG-2026-09.pdf, EUR 1,550.00 (14.2%) above the contract" {
		t.Errorf("exit %d: %v", code, err)
	}
	var got struct {
		Meta struct {
			VarianceExceeded  []string `json:"variance_exceeded"`
			VarianceUnchecked []string `json:"variance_unchecked"`
		}
	}
	decodeOnly(t, "status --json", stdout, &got)
	if strings.Join(got.Meta.VarianceExceeded, " ") != lseg || got.Meta.VarianceUnchecked == nil {
		t.Errorf("meta = %+v", got.Meta)
	}
	if _, err := status(lseg, "--fail-on-variance=1550"); err != nil {
		t.Errorf("at the threshold: %v", err)
	}
	// An id that is not a document here is 4, as without the flag, and
	// the invoice over the threshold is named all the same.
	_, err = status(lseg, "00000000-0000-4000-8000-000000000999", "--fail-on-variance")
	if code, _ := Classify(err); code != ExitNotFound || !strings.Contains(err.Error(), "; 1 invoice shows a variance: LSEG-2026-09.pdf") {
		t.Errorf("exit %d: %v", code, err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--fail-on-variance"}, "--fail-on-variance needs the ids of the documents to check"},
		{[]string{lseg, "--fail-on-variance", "2%"}, "2% is not a document id; to give --fail-on-variance a threshold, join the two with =, as in --fail-on-variance=2%"},
		{[]string{lseg, "2%"}, "2% is not a document id; ids look like"},
	} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload", "status"}, c.args...)...)
		if code != ExitUsage || !strings.Contains(msg, c.want) {
			t.Errorf("%v: exit %d, %q", c.args, code, msg)
		}
	}
}

// Against a percentage, a percentage has as many decimals as it takes to
// read as over it, up to six; otherwise one.
func TestPercentOver(t *testing.T) {
	rat := func(s string) *big.Rat {
		r, _ := new(big.Rat).SetString(s)
		return r
	}
	for _, c := range []struct {
		p, limit, want string
	}{
		{"14.2201834", "2", "14.2"},
		{"2.04", "2", "2.04"},
		{"2.0004", "2", "2.0004"},
		{"2.0000001", "2", "2.000000"},
		{"2.04", "", "2.0"},
	} {
		var limit *big.Rat
		if c.limit != "" {
			limit = rat(c.limit)
		}
		if got := percentOver(rat(c.p), limit); got != c.want {
			t.Errorf("percentOver(%s, %s) = %s, want %s", c.p, c.limit, got, c.want)
		}
	}
}

// A document the check has nothing to hold to passes: not an invoice, no
// contract to compare with, or one the exit status accounts for already.
func TestCheckVariancePasses(t *testing.T) {
	anyVariance := upload.Threshold{}
	for _, d := range []*upload.Document{
		{Own: true, State: upload.StateReadyForReview, Type: "contract"},
		{Own: true, State: upload.StateReadyForReview, Type: "invoice", Variance: &upload.Variance{Status: "not_available", Reason: "no_match"}},
		{Own: true, State: upload.StateReadyForReview, Type: "invoice", Variance: &upload.Variance{Status: "not_available", Reason: "new_contract"}},
		{Own: true, State: upload.StateReadyForReview, Type: "invoice", Variance: &upload.Variance{Status: "not_available", Reason: "no_lines"}},
		{Own: true, State: upload.StateReadyForReview, Type: "invoice", Variance: &upload.Variance{Status: "none"}},
		{Own: true, State: upload.StateFailed, Type: "invoice"},
		{Own: true, State: upload.StateReading},
		{State: upload.StateReading},
		{Own: true, State: stateMissing},
		{Own: true, State: upload.StateSkipped, Reason: "over_quota"},
	} {
		if c := checkVariance(anyVariance, d); c.verdict != variancePasses {
			t.Errorf("%+v: %+v", d, c)
		}
	}
	// A type the server left out, but read.
	d := &upload.Document{Own: true, State: upload.StateReadyForReview, Read: &upload.Read{Type: "invoice"}}
	if c := checkVariance(anyVariance, d); c.verdict != varianceUnchecked {
		t.Errorf("an invoice by what was read: %+v", c)
	}
	// Without the flag there is no gate, and nothing to add to an error.
	var g *varianceGate
	if g.result(nil) != nil {
		t.Error("a nil gate made an error")
	}
	var meta uploadJSONMeta
	g.setMeta(&meta)
	if meta.VarianceExceeded != nil {
		t.Error("a nil gate set meta")
	}
}

// --fail-on-variance= completes to a few thresholds, and the flag reads
// back as it was given, for --help's defaults and cobra's own use.
func TestFailOnVarianceFlag(t *testing.T) {
	isolate(t)
	candidates, directive := complete(t, "test", "upload", "--fail-on-variance=")
	if strings.Join(candidates, " ") != "1% 2% 5% any" || directive != ":4" {
		t.Errorf("--fail-on-variance=<Tab> = %v %s", candidates, directive)
	}
	var f varianceFlag
	if f.String() != "" || f.Type() != "threshold" {
		t.Errorf("unset: %q, %q", f.String(), f.Type())
	}
	for _, s := range []string{"2.5%", "any"} {
		if err := f.Set(s); err != nil || f.String() != s {
			t.Errorf("Set(%q): %v, %q", s, err, f.String())
		}
	}
}
