package cmd

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/ui"
	"github.com/firmfact/cli/internal/upload"
)

// --fail-on-variance, on upload and upload status, is for pipelines. Once
// firmfact has read the documents, the command ends with ExitVariance when
// an invoice's variance preview is further from its contract than the
// threshold allows, and --json lists those invoices in
// meta.variance_exceeded.
//
// A pipeline must never read a pass for a variance nobody checked. A
// document still being read, or one that could not be read, ends the
// command with its own status already. A document whose variance the CLI
// cannot see ends it with ExitFailed, listed in meta.variance_unchecked:
// someone else's upload, whose results only they see; an invoice whose
// contract the sign-in may not view, whose preview firmfact could not work
// out, or for which it sent none; and an invoice with a contract none of
// whose lines is matched to it yet, or whose review page is not built, as
// nothing was compared. A document that is not an invoice, and an invoice
// with no contract to compare it with, passes: there is no variance to
// hold it to.

// varianceFlag is --fail-on-variance[=threshold].
type varianceFlag struct {
	on        bool
	threshold upload.Threshold
}

func (f *varianceFlag) String() string {
	if !f.on {
		return ""
	}
	return f.threshold.String()
}

func (f *varianceFlag) Set(s string) error {
	t, err := upload.ParseThreshold(s)
	if err != nil {
		return err
	}
	f.on, f.threshold = true, t
	return nil
}

func (f *varianceFlag) Type() string { return "threshold" }

// addVarianceFlag gives cmd --fail-on-variance. The value is optional, so
// it follows an = (--fail-on-variance=2%): after a space, it would be an
// argument of its own.
func addVarianceFlag(cmd *cobra.Command, f *varianceFlag) {
	cmd.Flags().Var(f, "fail-on-variance", fmt.Sprintf("exit with status %d when an invoice is further from its contract than `threshold`: "+
		"=2%% of the contracted amount, =50 in its currency, or none for any variance", ExitVariance))
	cmd.Flags().Lookup("fail-on-variance").NoOptDefVal = upload.AnyVariance
	_ = cmd.RegisterFlagCompletionFunc("fail-on-variance", func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return []cobra.Completion{"1%", "2%", "5%", upload.AnyVariance}, cobra.ShellCompDirectiveNoFileComp
	})
}

// varianceHelp is the paragraph of upload's and upload status's help about
// --fail-on-variance, ending with more, wrapped as the rest of the help.
func varianceHelp(more string) string {
	text := fmt.Sprintf("--fail-on-variance is for pipelines: once firmfact has read the documents, "+
		"the command ends with exit status %[1]d when an invoice is further from its contract than the threshold allows, above or below it. "+
		"The threshold is a percentage of the contracted amount (--fail-on-variance=2%%) or an amount in the invoice's currency (--fail-on-variance=50), "+
		"and an invoice exactly at it passes; without one, any variance of a cent or more counts. "+
		"Documents that are not invoices, and invoices with no contract to compare them with, never exceed it. "+
		"A document whose variance cannot be checked, such as someone else's upload, an invoice whose contract you may not view, "+
		"or one whose lines are not matched to its contract yet, "+
		"makes the exit status 1, as a document that could not be read does, and a wait that ran out makes it 5: "+
		"so %[1]d means that everything else went well. %[2]s", ExitVariance, more)
	return strings.Join(wrapText(text, 74), "\n")
}

// thresholdWord matches an argument that was most likely meant as the
// value of --fail-on-variance, typed after a space: 2%, 50, 2.5 %. Its
// characters need no quotes in any shell the CLI runs in.
var thresholdWord = regexp.MustCompile(`^[0-9][0-9.,]* ?%?$`)

// thresholdHint is what to add to the error about arg, a word that names
// no file or document, when it reads as a threshold for --fail-on-variance
// typed after a space rather than an =; "" when it does not.
func (f *varianceFlag) thresholdHint(arg string) string {
	if !f.on || f.threshold.Limit != nil || !thresholdWord.MatchString(arg) {
		return ""
	}
	return "; to give --fail-on-variance a threshold, join the two with =, as in --fail-on-variance=" + strings.ReplaceAll(arg, " ", "")
}

// What --fail-on-variance makes of a document.
type varianceVerdict int

const (
	// variancePasses: within the threshold, no variance, or nothing to
	// compare, or a document the exit status accounts for otherwise.
	variancePasses varianceVerdict = iota
	varianceOver
	varianceUnchecked
)

// varianceCheck is --fail-on-variance's verdict on a document: with the
// measure of one over the threshold, or why one could not be checked.
type varianceCheck struct {
	verdict varianceVerdict
	measure upload.Measure
	why     string
}

// checkVariance holds d's variance preview to t.
func checkVariance(t upload.Threshold, d *upload.Document) varianceCheck {
	unchecked := func(why string) varianceCheck { return varianceCheck{verdict: varianceUnchecked, why: why} }
	switch {
	case upload.InProgress(d.State), d.State == upload.StateFailed, d.State == stateMissing,
		d.State == upload.StateSkipped && d.Reason == "over_quota":
		// A document still being read, or one that could not be read:
		// the exit status says so already (see uploadRun.failure).
		return varianceCheck{}
	case !d.Own:
		return unchecked("uploaded by someone else, whose results only they see")
	}
	v := d.Variance
	if v == nil {
		if documentType(d) == "invoice" {
			return unchecked("firmfact sent no variance preview for it")
		}
		return varianceCheck{}
	}
	switch v.Status {
	case "none":
		return varianceCheck{}
	case "variance":
		m, ok := v.Measure()
		switch {
		case !ok:
			return unchecked("its variance is not an amount: " + ui.SafeLine(string(v.Amount)))
		case t.Exceeded(m):
			return varianceCheck{verdict: varianceOver, measure: m}
		}
		return varianceCheck{}
	case "not_available":
		switch v.Reason {
		case "new_contract", "no_lines":
			// A contract still to be made, or no line, to compare with.
			return varianceCheck{}
		case "no_match":
			// No line is matched to a contract item. That passes only
			// for an invoice with no contract: one linked or suggested
			// to a contract was not compared with it, and a review page
			// not built yet has no matches, whatever the invoice has.
			switch {
			case d.Review != nil && d.Review.AnalysisPending:
				return unchecked("its review page is not ready, so firmfact has not compared it with a contract")
			case d.ContractMatch != nil && d.ContractMatch.Status != "none":
				return unchecked("no line is matched to a contract item yet")
			}
			return varianceCheck{}
		}
		return unchecked(orDefault(ui.SafeLine(strings.TrimSpace(v.Message)), "no variance preview: "+words(v.Reason)))
	}
	return unchecked("a variance preview this CLI does not know: " + words(v.Status))
}

// documentType is d's type, or the type read from it.
func documentType(d *upload.Document) string {
	if d.Type == "" && d.Read != nil {
		return d.Read.Type
	}
	return d.Type
}

// varianceSubject is a document --fail-on-variance checks: its name in
// messages, and its key in the JSON, which is what the command line named
// (a file's path for upload, a document's id for upload status).
type varianceSubject struct {
	name, key string
	doc       *upload.Document
}

// varianceFinding is a subject over the threshold, or not checked.
type varianceFinding struct {
	varianceSubject
	check varianceCheck
}

// varianceGate is --fail-on-variance's verdict on a command's documents.
type varianceGate struct {
	threshold       upload.Threshold
	over, unchecked []varianceFinding
}

// gateVariance checks each of subjects against t, in their order.
func gateVariance(t upload.Threshold, subjects []varianceSubject) *varianceGate {
	g := &varianceGate{threshold: t}
	for _, s := range subjects {
		switch c := checkVariance(t, s.doc); c.verdict {
		case varianceOver:
			g.over = append(g.over, varianceFinding{s, c})
		case varianceUnchecked:
			g.unchecked = append(g.unchecked, varianceFinding{s, c})
		}
	}
	return g
}

// maxVarianceNames is how many documents a message about the variance
// names; the JSON lists every one.
const maxVarianceNames = 3

// overMessage says which invoices are over the threshold, and by how much;
// "" for none.
func (g *varianceGate) overMessage() string {
	if len(g.over) == 0 {
		return ""
	}
	n := len(g.over)
	head := fmt.Sprintf("%d %s over the variance threshold of %s", n, plural(n, "invoice is", "invoices are"), g.threshold)
	if g.threshold.Any() {
		head = fmt.Sprintf("%d %s a variance", n, plural(n, "invoice shows", "invoices show"))
	}
	var items []string
	for _, f := range g.over {
		items = append(items, f.name+", "+g.measureWords(f.doc.Variance.Currency, f.check.measure))
	}
	return head + ": " + listSome(items)
}

// uncheckedMessage says which documents' variance could not be checked,
// and why; "" for none.
func (g *varianceGate) uncheckedMessage() string {
	if len(g.unchecked) == 0 {
		return ""
	}
	n := len(g.unchecked)
	var items []string
	for _, f := range g.unchecked {
		items = append(items, f.name+" ("+f.check.why+")")
	}
	return fmt.Sprintf("the variance of %d %s could not be checked: %s", n, plural(n, "document", "documents"), listSome(items))
}

// listSome joins the first maxVarianceNames of items, and says how many
// more there are.
func listSome(items []string) string {
	shown := items[:min(len(items), maxVarianceNames)]
	text := strings.Join(shown, "; ")
	if more := len(items) - len(shown); more > 0 {
		text += fmt.Sprintf("; and %d more", more)
	}
	return text
}

// measureWords is how far an invoice is from its contract: "EUR 1,550.00
// (14.2%) above the contract". Against a percentage, the percentage has as
// many decimals as it takes to be over the threshold: 2.04%, not 2.0%.
func (g *varianceGate) measureWords(currency string, m upload.Measure) string {
	direction := " above the contract"
	if m.Amount.Sign() < 0 {
		direction = " below the contract"
	}
	text := moneyText(currency, upload.Decimal(new(big.Rat).Abs(m.Amount).FloatString(2)))
	if m.Percent != nil {
		var limit *big.Rat
		if g.threshold.Percent {
			limit = g.threshold.Limit
		}
		text += " (" + percentOver(m.Percent, limit) + "%)"
	}
	return text + direction
}

// percentOver is p with one decimal, or with as many more as it takes to
// read as over limit (nil for none), up to six.
func percentOver(p, limit *big.Rat) string {
	text := p.FloatString(1)
	for digits := 2; limit != nil && digits <= 6; digits++ {
		if shown, ok := new(big.Rat).SetString(text); ok && shown.Cmp(limit) > 0 {
			break
		}
		text = p.FloatString(digits)
	}
	return text
}

// keys are the keys of findings, for the JSON: never null, so that a
// script can read an empty list as none.
func keys(findings []varianceFinding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.key)
	}
	return out
}

// setMeta puts the gate's lists in meta, for --json.
func (g *varianceGate) setMeta(meta *uploadJSONMeta) {
	if g == nil {
		return
	}
	over, unchecked := keys(g.over), keys(g.unchecked)
	meta.VarianceExceeded, meta.VarianceUnchecked = &over, &unchecked
}

// result is the error the gate adds to a command's own: failure, the
// command's error for anything else that went wrong, or nil. A document
// not checked is a failure (ExitFailed); an invoice over the threshold is
// ExitVariance only when nothing else went wrong, and otherwise part of
// the message of the status that did.
func (g *varianceGate) result(failure error) error {
	if g == nil {
		return failure
	}
	var parts []string
	status := ExitVariance
	if failure != nil {
		status = exitCode(failure)
	}
	if msg := g.uncheckedMessage(); msg != "" {
		parts = append(parts, msg)
		// A variance nobody checked needs a person, as a document that
		// could not be read does, and running again later would not
		// check it: 1 wins over 5, as it does for an upload's files.
		if failure == nil || status == ExitUnavailable {
			status = ExitFailed
		}
	}
	if msg := g.overMessage(); msg != "" {
		parts = append(parts, msg)
	}
	switch {
	case len(parts) == 0:
		return failure
	case failure == nil:
		return withExit(status, errors.New(strings.Join(parts, "; ")))
	}
	return withExit(status, fmt.Errorf("%w; %s", failure, strings.Join(parts, "; ")))
}
