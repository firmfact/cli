package cmd

import (
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// After an upload, the next steps follow up the first invoice whose
// preview shows a variance: its review page, the contract item of the line
// that differs most, and the vendor's costs month by month. A batch gets
// the steps for one invoice, and a count of the others. With
// --fail-on-variance, that is the first invoice over its threshold, when
// there is one: the steps then follow up what failed the command, not an
// invoice within the threshold before it. Like every next step, they are
// for a person at a terminal (see printNextStep), and each needs the tool
// it runs to be on offer.
//
// The steps are made of what the server sent, so each value is escaped,
// and must stand as one word on any shell's command line (see shellWord).
// A step whose value cannot is left out, rather than shown with a word
// that would not run as typed.

// printVarianceSteps prints the next steps for the first invoice of the
// upload whose preview shows a variance, and how many more show one.
func (r *uploadRun) printVarianceSteps() {
	if !r.app.attended() {
		return
	}
	v := r.firstVariance()
	if v.doc == nil {
		return
	}
	steps := r.varianceSteps(v.doc)
	if len(steps) == 0 {
		return
	}
	head := plural(len(steps), "Next step", "Next steps") + " for " + v.name + ", " + v.words
	m := r.app.Mode()
	fmt.Fprintf(r.out, "\n%s\n", m.Dim(head+":"))
	for _, s := range steps {
		fmt.Fprintf(r.out, "  %s  %s\n", m.Orange(r.app.Name+" "+s.command), m.Dim("("+s.why+")"))
	}
	switch {
	case v.others > 0 && v.over:
		fmt.Fprintf(r.out, "%d more %s over the threshold too.\n", v.others, plural(v.others, "invoice is", "invoices are"))
	case v.others > 0:
		fmt.Fprintf(r.out, "%d more %s a variance too.\n", v.others, plural(v.others, "invoice shows", "invoices show"))
	}
}

// varianceWords say how far an invoice is off its contract, in the CLI's
// words: "EUR 1,550.00 (14.2%) above the contract". The server's summary
// is in the member's language, and one kind of it is a sentence of its
// own, where the heading wants a few words.
func varianceWords(v *upload.Variance) string {
	amount, ok := v.Amount.Rat()
	if !ok {
		return "which differs from its contract"
	}
	direction := " above the contract"
	switch amount.Sign() {
	case 0:
		return "whose lines differ from its contract but add up to it"
	case -1:
		direction = " below the contract"
	}
	text := moneyText(v.Currency, upload.Decimal(amount.Abs(amount).FloatString(2)))
	if percent, ok := v.Percent.Rat(); ok {
		text += " (" + percent.Abs(percent).FloatString(1) + "%)"
	}
	return text + direction
}

// followedVariance is the invoice the next steps follow up.
type followedVariance struct {
	doc  *upload.Document
	name string
	// words say how far it is from its contract, for the heading.
	words string
	// over is set when it is over --fail-on-variance's threshold; others
	// is then how many more invoices are, and otherwise how many more
	// show a variance.
	over   bool
	others int
}

// firstVariance is the first document of the upload, in the order of the
// command line, whose preview shows a variance, with the name of its file,
// and how many other documents show one. With --fail-on-variance, it is
// the first over the threshold, when there is one, said as the error says
// it (see varianceGate.measureWords), and others counts those over it.
func (r *uploadRun) firstVariance() followedVariance {
	var shown, over []followedVariance
	gate := r.flags.failOnVariance
	seen := map[string]bool{}
	for _, f := range r.files {
		d := f.doc
		if d == nil || seen[d.ID] || d.Variance == nil || d.Variance.Status != "variance" {
			continue
		}
		seen[d.ID] = true
		shown = append(shown, followedVariance{doc: d, name: f.label(), words: varianceWords(d.Variance)})
		if !gate.on {
			continue
		}
		if c := checkVariance(gate.threshold, d); c.verdict == varianceOver {
			g := varianceGate{threshold: gate.threshold}
			over = append(over, followedVariance{doc: d, name: f.label(), words: g.measureWords(d.Variance.Currency, c.measure), over: true})
		}
	}
	if len(over) > 0 {
		shown = over
	}
	if len(shown) == 0 {
		return followedVariance{}
	}
	first := shown[0]
	first.others = len(shown) - 1
	return first
}

// varianceSteps are the next steps for doc, an invoice whose preview shows
// a variance, that the host offers (see varianceFollowUps).
func (r *uploadRun) varianceSteps(doc *upload.Document) []nextStep {
	host, err := r.app.Host()
	if err != nil {
		return nil
	}
	steps := r.varianceFollowUps(host, doc)
	return slices.DeleteFunc(steps, func(s nextStep) bool { return !r.app.offered(s) })
}

// varianceFollowUps are the follow-ups of doc on host, whichever tools the
// host offers: open its review page, list the contract item of the line
// that differs most, and the vendor's costs month by month.
func (r *uploadRun) varianceFollowUps(host string, doc *upload.Document) []nextStep {
	var steps []nextStep
	if link := reviewLink(host, doc); link != "" {
		steps = append(steps, stepReviewPage.with(link))
	}
	workspace, ok := r.workspaceWords()
	if !ok {
		return steps
	}
	if v := doc.Variance; v != nil {
		if l := largestVarianceLine(v.Lines); l != nil {
			if item := serverWord(l.Item.Name); item != "" {
				s := stepContractItem.with(append([]string{"--query", item}, workspace...)...)
				s.why = fmt.Sprintf("the contract item line %d is compared with", l.Line)
				steps = append(steps, s)
			}
		}
	}
	if vendor := linkedVendor(doc.Read); vendor != "" {
		steps = append(steps, stepVendorTrend.with(append([]string{"--entity-name", vendor, "--monthly"}, workspace...)...))
	}
	return steps
}

// reviewLink is doc's review page as the word open takes: its link, when
// that is a page of host; empty when it is not, as open would refuse it.
func reviewLink(host string, doc *upload.Document) string {
	link := strings.TrimSpace(doc.URL)
	if link == "" && doc.Review != nil {
		link = strings.TrimSpace(doc.Review.URL)
	}
	if link == "" {
		return ""
	}
	if _, err := openTarget(host, link); err != nil {
		return ""
	}
	return argWord(ui.SafeLine(link), "")
}

// workspaceWords name the upload's workspace on the next commands: the
// --workspace of the command line, as it was typed, or else the
// workspace's id. FIRMFACT_WORKSPACE applies to the next commands as it
// did to this one, and so does the default, so they need no words. ok is
// false when the workspace cannot be named, and the steps would run in
// another.
func (r *uploadRun) workspaceWords() (words []string, ok bool) {
	if r.app.Workspace == "" {
		return nil, true
	}
	for _, ref := range []string{r.app.Workspace, r.workspace.ID} {
		if word := serverWord(ref); word != "" {
			return []string{"--workspace", word}, true
		}
	}
	return nil, false
}

// largestVarianceLine is the line with a contract item whose difference is
// the largest, the first of those that tie; nil when no line has an item
// the caller may see. A line that is not in the contract has none.
func largestVarianceLine(lines []upload.VarianceLine) *upload.VarianceLine {
	var line *upload.VarianceLine
	var largest *big.Rat
	for i := range lines {
		l := &lines[i]
		if l.Item == nil || strings.TrimSpace(l.Item.Name) == "" {
			continue
		}
		amount, ok := l.Amount.Rat()
		if !ok {
			amount = new(big.Rat)
		}
		amount.Abs(amount)
		if line == nil || amount.Cmp(largest) > 0 {
			line, largest = l, amount
		}
	}
	return line
}

// linkedVendor is the vendor rd was read from, as --entity-name takes it:
// the workspace's vendor it is linked to. A vendor that is not linked yet
// may be new, or be another one, so it gets no step.
func linkedVendor(rd *upload.Read) string {
	if rd == nil || rd.Vendor == nil || rd.Vendor.Status != "linked" {
		return ""
	}
	return serverWord(orDefault(rd.Vendor.LinkedTo, rd.Vendor.Name))
}

// serverWord is s, a name or id the server sent or the command line gave,
// escaped, as a flag's value on a command line: in double quotes when it
// needs them, and empty when it is empty or would need more. It is empty
// too when s starts with -: the flag before it would take it as its value
// all the same, but a person reads "--query --host=..." as two flags.
func serverWord(s string) string {
	s = ui.SafeLine(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	return argWord(s, "")
}
