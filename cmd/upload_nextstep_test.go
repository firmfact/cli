package cmd

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// lsegInvoice is the LSEG invoice of the upload fixtures, 1,550.00 above
// its contract, with its links on host.
func lsegInvoice(t *testing.T, host string) *upload.Document {
	t.Helper()
	raw := readFixtureFile(t, filepath.Join(uploadTestdata, "documents", "lseg_invoice.json"))
	var doc upload.Document
	if err := json.Unmarshal([]byte(strings.ReplaceAll(raw, docsHost, host)), &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

// stepsRun is an upload to Acme, named on the command line as typed, on
// fixtureHost.
func stepsRun(workspace string) *uploadRun {
	return &uploadRun{
		app:       &App{Name: "firmfact", HostFlag: fixtureHost, Workspace: workspace},
		workspace: upload.Workspace{ID: acmeID, Name: "Acme"},
	}
}

func commandsOf(steps []nextStep) []string {
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.command)
	}
	return commands
}

// The steps after an invoice with a variance are the invoice's own: its
// review page, the contract item of the line that differs most, and its
// vendor's costs by month, in the workspace the upload named. Each runs
// as shown, with the server's tools.
func TestVarianceStepsRun(t *testing.T) {
	tools := withServerTools(t)
	doc := lsegInvoice(t, fixtureHost)
	steps := stepsRun("Acme").varianceSteps(doc)
	want := []string{
		"open " + fixtureHost + "/accounts/" + acmeID + "/documents/" + doc.ID,
		`contract-items list --query "Workspace Pro Licence" --workspace Acme`,
		"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly --workspace Acme",
	}
	if got := commandsOf(steps); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if why := steps[1].why; why != "the contract item line 1 is compared with" {
		t.Errorf("why = %q", why)
	}
	for _, s := range steps {
		assertRuns(t, tools, s.command, s.tool)
	}
	// The review link is one open takes, and opens as it is.
	if target, err := openTarget(fixtureHost, strings.TrimPrefix(steps[0].command, "open ")); err != nil || target != doc.URL {
		t.Errorf("open takes the link as %q (%v), want %q", target, err, doc.URL)
	}
}

// A step is left out when what it needs is not there, or would not run as
// shown: a word no shell reads the same in double quotes, a review page on
// another host, a vendor that is not linked, a variance on lines outside
// the contract only.
func TestVarianceStepsLeftOut(t *testing.T) {
	withServerTools(t)
	cases := []struct {
		name      string
		workspace string
		change    func(d *upload.Document)
		want      []string
	}{
		{name: "workspace from the environment or the default", want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Workspace Pro Licence"`,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
		{name: "workspace typed as no shell reads it, so its id", workspace: `Acme "Q3"`, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Workspace Pro Licence" --workspace ` + acmeID,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly --workspace " + acmeID,
		}},
		{name: "review page on another host", change: func(d *upload.Document) {
			d.URL = "https://elsewhere.example/documents/1"
			d.Review.URL = d.URL
		}, want: []string{
			`contract-items list --query "Workspace Pro Licence"`,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
		{name: "review link only in the review", change: func(d *upload.Document) {
			d.URL = ""
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Workspace Pro Licence"`,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
		{name: "no link at all", change: func(d *upload.Document) {
			d.URL, d.Review = "", nil
		}, want: []string{
			`contract-items list --query "Workspace Pro Licence"`,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
		{name: "vendor suggested, not linked", change: func(d *upload.Document) {
			d.Read.Vendor.Status = "suggested"
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Workspace Pro Licence"`,
		}},
		{name: "vendor linked to itself", change: func(d *upload.Document) {
			d.Read.Vendor.LinkedTo = ""
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Workspace Pro Licence"`,
			`analyze cost-trends --entity-type vendor --entity-name "Refinitiv Limited" --monthly`,
		}},
		{name: "names no shell reads the same", change: func(d *upload.Document) {
			d.Variance.Lines[0].Item.Name = "Pro $HOME"
			d.Read.Vendor.LinkedTo = "LSEG `id`"
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
		}},
		{name: "names that read as flags", change: func(d *upload.Document) {
			d.Variance.Lines[0].Item.Name = "--host=https://evil.example"
			d.Read.Vendor.LinkedTo = " -rf"
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
		}},
		{name: "a review path with a query, and names in quotes", change: func(d *upload.Document) {
			d.URL = "/accounts/x/documents/1?tab=lines#top"
			d.Variance.Lines[0].Item.Name = " O'Brien & Co. "
			d.Read.Vendor.LinkedTo = "#1 (Ltd)"
		}, want: []string{
			`open "/accounts/x/documents/1?tab=lines#top"`,
			`contract-items list --query "O'Brien & Co."`,
			`analyze cost-trends --entity-type vendor --entity-name "#1 (Ltd)" --monthly`,
		}},
		{name: "only lines outside the contract", change: func(d *upload.Document) {
			d.Variance.Lines = d.Variance.Lines[1:]
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
		{name: "the line that differs most, below the contract too", change: func(d *upload.Document) {
			d.Variance.Lines = append(d.Variance.Lines,
				upload.VarianceLine{Line: 2, Amount: "-1200.01", Item: &upload.ItemRef{Name: "Real-Time Exchange Fees"}},
				upload.VarianceLine{Line: 4, Amount: "-1200.01", Item: &upload.ItemRef{Name: "Later tie"}})
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Real-Time Exchange Fees"`,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
		{name: "an amount that is no decimal counts as none", change: func(d *upload.Document) {
			d.Variance.Lines[0].Amount = "n/a"
			d.Variance.Lines = append(d.Variance.Lines, upload.VarianceLine{Line: 2, Amount: "0.01", Item: &upload.ItemRef{Name: "Real-Time Exchange Fees"}})
		}, want: []string{
			"open " + fixtureHost + "/accounts/" + acmeID + "/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d",
			`contract-items list --query "Real-Time Exchange Fees"`,
			"analyze cost-trends --entity-type vendor --entity-name LSEG --monthly",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := lsegInvoice(t, fixtureHost)
			if c.change != nil {
				c.change(doc)
			}
			if got := commandsOf(stepsRun(c.workspace).varianceSteps(doc)); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// A workspace that can be named neither as typed nor by its id leaves out
// the steps that would run in it, so none runs in another workspace.
func TestVarianceStepsWithAWorkspaceThatCannotBeNamed(t *testing.T) {
	withServerTools(t)
	r := stepsRun("Acme $Q3")
	r.workspace.ID = ""
	doc := lsegInvoice(t, fixtureHost)
	want := "open " + fixtureHost + "/accounts/" + acmeID + "/documents/" + doc.ID
	if got := commandsOf(r.varianceSteps(doc)); len(got) != 1 || got[0] != want {
		t.Errorf("steps = %q, want the review page alone", got)
	}
}

// The steps that run a tool are shown only when the host offers it; open
// needs none.
func TestVarianceStepsNeedTheirTools(t *testing.T) {
	isolate(t)
	doc := lsegInvoice(t, fixtureHost)
	got := commandsOf(stepsRun("Acme").varianceSteps(doc))
	if len(got) != 1 || !strings.HasPrefix(got[0], "open ") {
		t.Errorf("steps without a tool list = %q, want the review page alone", got)
	}
	r := stepsRun("Acme")
	r.app.HostFlag = "not a host"
	if got := r.varianceSteps(doc); got != nil {
		t.Errorf("steps on an invalid host = %q", commandsOf(got))
	}
}

// The first invoice with a variance, in the order of the command line, is
// the one the steps are for; the others are counted, each document once,
// and invoices without a variance are not.
func TestFirstVariance(t *testing.T) {
	lseg := lsegInvoice(t, fixtureHost)
	later := lsegInvoice(t, fixtureHost)
	later.ID = "later"
	clean := &upload.Document{ID: "clean", Variance: &upload.Variance{Status: "none"}}
	unknown := &upload.Document{ID: "unknown", Variance: &upload.Variance{Status: "not_available"}}
	r := &uploadRun{files: []*uploadFile{
		{name: "clean.pdf", doc: clean},
		{name: "refused.pdf"},
		{name: "unknown.pdf", doc: unknown},
		{name: "LSEG-2026-09.pdf", doc: lseg},
		{name: "copy.pdf", doc: lseg},
		{name: "LSEG-2026-10.pdf", doc: later},
	}}
	doc, name, others := r.firstVariance()
	if doc != lseg || name != "LSEG-2026-09.pdf" || others != 1 {
		t.Errorf("firstVariance = %v, %q, %d; want the LSEG invoice and 1 other", doc != nil, name, others)
	}
	r.files = r.files[:3]
	if doc, _, _ := r.firstVariance(); doc != nil {
		t.Errorf("firstVariance found %s among invoices without a variance", doc.ID)
	}
}

// Off a terminal, as with --json, an upload shows no next steps: a script
// reads the results, not advice.
func TestUploadShowsNoStepsToAScript(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	onServerHost(s)
	if err := saveToolCache(s.URL(), serverTools(t)); err != nil {
		t.Fatal(err)
	}
	uploadDir(t, "LSEG-2026-09.pdf")
	stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
	if err != nil {
		t.Fatalf("upload: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "EUR 1,550.00 (14.2%) above the contract") || strings.Contains(stdout, "Next step") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

// onServerHost puts the review links of s's documents on s itself, as the
// service puts them on the host that was asked.
func onServerHost(s *uploadServer) {
	for name, raw := range s.fixtures {
		s.fixtures[name] = strings.ReplaceAll(raw, docsHost, s.URL())
	}
}

// The heading says how far the invoice is off its contract in the CLI's
// words, whatever the language of the server's summary, and in few of
// them when the lines differ but add up to the contract.
func TestVarianceWords(t *testing.T) {
	cases := []struct {
		v    upload.Variance
		want string
	}{
		{upload.Variance{Currency: "EUR", Amount: "1550.00", Percent: "14.2", Summary: "EUR 1.550,00 (14,2%) boven het contract"}, "EUR 1,550.00 (14.2%) above the contract"},
		{upload.Variance{Currency: "USD", Amount: "-30.5", Percent: "-2.6"}, "USD 30.50 (2.6%) below the contract"},
		{upload.Variance{Currency: "USD", Amount: "30"}, "USD 30.00 above the contract"},
		{upload.Variance{Amount: "0.00", Summary: "The lines differ from the contract, but add up to the same total."}, "whose lines differ from its contract but add up to it"},
		{upload.Variance{Currency: "EUR", Summary: "EUR 1,550.00 above the contract"}, "which differs from its contract"},
	}
	for _, c := range cases {
		if got := varianceWords(&c.v); got != c.want {
			t.Errorf("varianceWords(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// assertFollowUps fails unless each follow-up of doc, a document from the
// server, prints as it is on a terminal and reads, to a shell, as its own
// words and the values it took from doc, each one word: a name or a link
// cannot add a flag, a word or a command. Its heading prints as it is too.
func assertFollowUps(t *testing.T, doc *upload.Document) {
	t.Helper()
	if doc.Variance != nil {
		heading := varianceWords(doc.Variance)
		assertNoTerminalControls(t, "next steps' heading", heading)
		if safe := ui.SafeLine(heading); safe != heading {
			t.Errorf("a heading with characters a terminal acts on: %q", heading)
		}
	}
	for _, s := range stepsRun("Acme").varianceFollowUps(fixtureHost, doc) {
		line := s.command + "  (" + s.why + ")"
		assertNoTerminalControls(t, "next step", line)
		if safe := ui.SafeLine(line); safe != line {
			t.Errorf("a next step with characters a terminal acts on: %q", line)
		}
		words := commandWords(t, s.command)
		var want []string
		switch s.tool {
		case "":
			if len(words) != 2 || words[0] != "open" {
				t.Errorf("%q reads as %q, want open and a link", s.command, words)
				continue
			}
			if target, err := openTarget(fixtureHost, words[1]); err != nil || !strings.HasPrefix(target, fixtureHost+"/") {
				t.Errorf("open would refuse %q, or open it elsewhere: %q (%v)", words[1], target, err)
			}
			continue
		case stepContractItem.tool:
			name := largestVarianceLine(doc.Variance.Lines).Item.Name
			want = []string{"contract-items", "list", "--query", ui.SafeLine(strings.TrimSpace(name))}
		case stepVendorTrend.tool:
			name := orDefault(doc.Read.Vendor.LinkedTo, doc.Read.Vendor.Name)
			want = []string{"analyze", "cost-trends", "--entity-type", "vendor", "--entity-name", ui.SafeLine(strings.TrimSpace(name)), "--monthly"}
		default:
			t.Errorf("a next step for tool %q: %q", s.tool, s.command)
			continue
		}
		want = append(want, "--workspace", "Acme")
		if !slices.Equal(words, want) {
			t.Errorf("%q reads as %q, want %q", s.command, words, want)
		}
	}
}
