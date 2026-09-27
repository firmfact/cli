package cmd

import (
	"fmt"
	"strings"
)

// A next step is the one command most worth running after this one, with
// why. Shown on interactive terminals only (never in scripts or --json),
// and only when the server offers the tool it points at.
type nextStep struct {
	command string // what to type, without "firmfact "
	tool    string // MCP tool it runs; empty for a command that needs none
	why     string
}

// People type a hint exactly as shown, so it carries every flag its tool
// requires: `analyze cost-trends` on its own fails on --entity-type.
// TestNextStepsRun checks each one against the server's tool list in
// testdata/tools.json.
var (
	stepAnalyzeSpend = nextStep{"analyze cost-trends --entity-type vendor", "analyze_cost_trends", "see the spend by vendor and how it moves"}
	stepAllocations  = nextStep{"analyze allocations --analysis-type by_cost", "analyze_allocations", "see which allocations cost the most"}
	stepAsk          = nextStep{`ask "Which contracts renew in the next 90 days?"`, chatTool, "ask in plain English"}
	stepVendors      = nextStep{"vendors list", "list_vendors", "start with who you pay"}
	stepWorkspaces   = nextStep{"workspaces status", "", "see how far the Demo workspace is"}

	// The follow-ups of an invoice whose preview shows a variance, which
	// uploadRun.varianceSteps completes with the invoice's review link, a
	// contract item's name and the vendor's.
	stepReviewPage   = nextStep{"open", "", "go through the variance on its review page"}
	stepContractItem = nextStep{"contract-items list", "list_contract_items", "the contract item a line is compared with"}
	stepVendorTrend  = nextStep{"analyze cost-trends --entity-type vendor", "analyze_cost_trends", "the vendor's costs, month by month"}

	// allNextSteps is every hint above, for that test.
	allNextSteps = []nextStep{stepAnalyzeSpend, stepAllocations, stepAsk, stepVendors, stepWorkspaces,
		stepReviewPage, stepContractItem, stepVendorTrend}
)

// with is step with words added to its command: the link, name or flags
// that make it about one thing.
func (s nextStep) with(words ...string) nextStep {
	s.command = strings.Join(append([]string{s.command}, words...), " ")
	return s
}

// stepAfterTool picks the follow-up for a workspace command: lookups lead to
// analysis, analysis leads to a question.
func stepAfterTool(tool string) (nextStep, bool) {
	switch {
	case tool == "list_allocations":
		return stepAllocations, true
	case strings.HasPrefix(tool, "list_") && tool != "list_workspaces":
		return stepAnalyzeSpend, true
	case strings.HasPrefix(tool, "analyze_"):
		return stepAsk, true
	}
	return nextStep{}, false
}

// printNextStep shows step to a person at a terminal, in plain text where
// colour is off (NO_COLOR); scripts, pipes and --json get none.
func (a *App) printNextStep(step nextStep) {
	if !a.attended() {
		return
	}
	if !a.offered(step) {
		return
	}
	m := a.Mode()
	fmt.Fprintf(a.Out, "\n%s %s  %s\n", m.Dim("Next step:"), m.Orange(a.Name+" "+step.command), m.Dim("("+step.why+")"))
}

// offered reports whether the host offers the tool step runs, as far as its
// cached tool list says; a step that needs no tool always is.
func (a *App) offered(step nextStep) bool {
	if step.tool == "" {
		return true
	}
	host, err := a.Host()
	return err == nil && toolCached(host, step.tool)
}

func toolCached(host, tool string) bool {
	tc := loadToolCache(host)
	if tc == nil {
		return false
	}
	for _, t := range tc.Tools {
		if t.Name == tool {
			return true
		}
	}
	return false
}
