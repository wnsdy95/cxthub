package main

import (
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Compare telemetry with the original preparation and the observed window.
// A cache estimate can disagree without invalidating a productive session.
// Never reinterpret missing telemetry as zero input or authorize replay here.
func nativeCodexInputObservation(b domain.AgentContextBudget, expectedWindow int, o nativecodex.GenerationObservation) *delivcli.ProviderInputObservation {
	r := &delivcli.ProviderInputObservation{
		WindowComparison: "unknown", InputBudgetStatus: "unknown",
		InitialCompaction: o.Outcome == "initial_compaction", ModelRerouted: o.ModelRerouted,
		Ineligible: o.Ineligible, ExecutionKnown: o.ExecutionKnown, ExecutionStarted: o.ExecutionStarted,
	}
	if o.Ineligible || o.ModelRerouted {
		return r
	}
	if o.ModelContextWindow > 0 {
		r.ContextWindow = o.ModelContextWindow
		r.WindowComparison = "matched"
		if o.ModelContextWindow != expectedWindow {
			r.WindowComparison = "changed"
		}
	}
	if o.UsageKnown && o.TotalInputTokens >= 0 {
		r.UsageKnown, r.TotalInputTokens = true, o.TotalInputTokens
		r.UsageBeforeCompaction = o.UsageBeforeCompaction
		if o.UsageBeforeCompaction && r.ContextWindow > 0 {
			window := r.ContextWindow
			limit := min(b.InitialInputLimit, window/5*4+window%5*4/5)
			r.InputBudgetStatus = "within_target"
			if o.TotalInputTokens > limit {
				r.InputBudgetStatus = "over_target"
			}
		}
	}
	return r
}
