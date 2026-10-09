package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func catalogGenerationFixture(t *testing.T, byteAllowance bool) *nativeGenerationBridgeFixture {
	t.Helper()
	f := newNativeGenerationBridgeFixture(t)
	f.capability.Verified = false
	f.capability.InputAccountingPolicy = domain.CatalogEstimateReserveV1
	f.capability.Calibration = domain.AgentInputCalibration{}
	f.capability.WindowEstimateSource = "native_model_cache"
	f.capability.WindowEstimateObservedAt = "2026-10-09T00:00:00Z"
	f.capability.WindowEstimateHash = domain.HashContent([]byte("synthetic catalog"))
	f.capability.WindowEstimateClientVersion = "0.157.1"
	if byteAllowance {
		f.capability.Tokenizer = domain.UTF8ByteBoundCounter
	}
	return f
}

func TestNativeCatalogPreparationRecordsUsageWithoutCalibrationOrReplay(t *testing.T) {
	for _, byteAllowance := range []bool{false, true} {
		for _, outcome := range []string{"completed", "initial_compaction", "input_rejected", "unknown"} {
			t.Run(outcome+map[bool]string{true: "/byte", false: "/exact"}[byteAllowance], func(t *testing.T) {
				f := catalogGenerationFixture(t, byteAllowance)
				prepared := f.prepared(t, "PRIVATE_QUESTION")
				var records []delivcli.ProviderLaunchReceipt
				p, err := persistNativeGeneration(context.Background(), t.TempDir(), f.bridge.expected, f.returned, prepared, f.bridge.expectedNativeWindow, func(_ context.Context, r delivcli.ProviderLaunchReceipt) error {
					records = append(records, r)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				o := f.observation(outcome)
				o.ModelContextWindow = 100000 // differs from preparation
				o.UsageKnown = true
				o.UsageBeforeCompaction = outcome == "completed"
				o.TotalInputTokens = 90000
				if err = p.Observe(context.Background(), o); err != nil {
					t.Fatal("estimated mismatch killed session", err)
				}
				last := records[len(records)-1]
				if last.Capability != "estimated_for_preparation" || last.NativeInputObservation.WindowComparison != "changed" || last.NativeInputObservation.TotalInputTokens != 90000 || f.store.writes != 0 || f.preparations != 1 {
					t.Fatalf("bad estimate observation: %+v", last)
				}
				measurement := "exact"
				if byteAllowance {
					measurement = "utf8_byte_allowance"
				}
				if last.TokenMeasurement != measurement {
					t.Fatal("text counter mislabeled", last.TokenMeasurement)
				}
				if outcome == "completed" && last.NativeInputObservation.InputBudgetStatus != "over_target" {
					t.Fatal("smaller actual window ignored")
				}
				if outcome != "completed" && last.Acceptance != "unknown" {
					t.Fatal("failure represented as acceptance")
				}
				raw, _ := json.Marshal(last)
				if strings.Contains(string(raw), "PRIVATE_") {
					t.Fatal("private question escaped receipt")
				}
			})
		}
	}
}

func TestNativeCatalogObservationRejectsOtherThread(t *testing.T) {
	f := catalogGenerationFixture(t, false)
	p := f.prepared(t, "question")
	o := nativecodex.GenerationObservation{ThreadID: "other", TurnID: nativeGenerationTestTurn}
	if err := p.Observe(context.Background(), o); err == nil {
		t.Fatal("unrelated telemetry accepted")
	}
	if f.store.writes != 0 || f.preparations != 1 {
		t.Fatal("unrelated telemetry changed prepared request")
	}
}
