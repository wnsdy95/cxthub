package main

import (
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestNativeCodexObservationSeparatesEstimatedWindowAndActualInput(t *testing.T) {
	b := domain.AgentContextBudget{InitialInputLimit: 760000}
	for _, tc := range []struct {
		name               string
		o                  nativecodex.GenerationObservation
		comparison, status string
		known, compact     bool
	}{
		{"match", nativecodex.GenerationObservation{ModelContextWindow: 950000, UsageKnown: true, UsageBeforeCompaction: true, TotalInputTokens: 700000}, "matched", "within_target", true, false},
		{"smaller window over target", nativecodex.GenerationObservation{ModelContextWindow: 200000, UsageKnown: true, UsageBeforeCompaction: true, TotalInputTokens: 170000}, "changed", "over_target", true, false},
		{"smaller window fits", nativecodex.GenerationObservation{ModelContextWindow: 200000, UsageKnown: true, UsageBeforeCompaction: true, TotalInputTokens: 150000}, "changed", "within_target", true, false},
		{"larger window never raises original target", nativecodex.GenerationObservation{ModelContextWindow: 1000000, UsageKnown: true, UsageBeforeCompaction: true, TotalInputTokens: 770000}, "changed", "over_target", true, false},
		{"missing usage", nativecodex.GenerationObservation{ModelContextWindow: 950000}, "matched", "unknown", false, false},
		{"missing window", nativecodex.GenerationObservation{UsageKnown: true, UsageBeforeCompaction: true, TotalInputTokens: 170000}, "unknown", "unknown", true, false},
		{"compacted usage", nativecodex.GenerationObservation{Outcome: "initial_compaction", ModelContextWindow: 950000, UsageKnown: true, TotalInputTokens: 150000}, "matched", "unknown", true, true},
		{"rerouted", nativecodex.GenerationObservation{ModelRerouted: true, ModelContextWindow: 950000, UsageKnown: true, UsageBeforeCompaction: true, TotalInputTokens: 150000}, "unknown", "unknown", false, false},
		{"ineligible", nativecodex.GenerationObservation{Ineligible: true, ModelContextWindow: 950000, UsageKnown: true, TotalInputTokens: 150000}, "unknown", "unknown", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := nativeCodexInputObservation(b, 950000, tc.o)
			if r.WindowComparison != tc.comparison || r.InputBudgetStatus != tc.status || r.UsageKnown != tc.known || r.InitialCompaction != tc.compact {
				t.Fatalf("unexpected observation: %+v", r)
			}
			if !r.UsageKnown && r.TotalInputTokens != 0 {
				t.Fatal("unknown usage carries a count")
			}
		})
	}
}
