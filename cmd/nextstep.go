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

	// allNextSteps is every hint above, for that test.
	allNextSteps = []nextStep{stepAnalyzeSpend, stepAllocations, stepAsk, stepVendors, stepWorkspaces}
)

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
	if step.tool != "" {
		host, err := a.Host()
		if err != nil || !toolCached(host, step.tool) {
			return
		}
	}
	m := a.Mode()
	fmt.Fprintf(a.Out, "\n%s %s  %s\n", m.Dim("Next step:"), m.Orange(a.Name+" "+step.command), m.Dim("("+step.why+")"))
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
