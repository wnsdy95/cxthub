//go:build postgres

package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestNormalizationCheckpointPGOutputDoesNotAdvanceDecodeCadence(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	rangeSpoolTempDir(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventToolCall, Seq: 1, CallID: "cadence", ToolName: "cadence",
			Input: map[string]any{"payload": strings.Repeat("output-cadence", 100000)}}},
	}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	stored, err := json.MarshalIndent(document, "", " ")
	if err != nil {
		test.Fatal(err)
	}
	work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
	defer work.Close()
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		if checkpoint.busy() {
			return false
		}
		consumed, err := work.normalization.inputConsumed()
		if err != nil || consumed {
			test.Fatal("cadence fixture passed its live event-output boundary", consumed, err)
		}
		_, err = work.normalization.preparation.ParserInputPosition(ctx)
		return errors.Is(err, domain.ErrDocRangeIndexPending)
	})
	before, state := work.parserSteps, work.checkpoint.state
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
		test.Fatal("actual event output failed", progress, err)
	}
	if _, err := work.normalization.preparation.ParserInputPosition(ctx); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("cadence fixture left its event-output phase", err)
	}
	if work.parserSteps != before || work.checkpoint.state != state {
		test.Fatal("event output inflated Decode checkpoint cadence", before, work.parserSteps)
	}
}

func TestNormalizationCheckpointPGParserProgressIsDurableBeforeInputConsumed(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	rangeSpoolTempDir(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull, Cwd: strings.Repeat("parser-progress🙂", 150000)},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "resume after the large header"}}}},
	}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	stored, err := json.MarshalIndent(document, "", " ")
	if err != nil {
		test.Fatal(err)
	}
	work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
	defer work.Close()
	observed := -1
	var peakHeap int64
	for step := 0; step < normalizationCheckpointPGStepLimit; step++ {
		busy := work.checkpoint != nil && work.checkpoint.busy()
		consumed := false
		if !busy {
			consumed, err = work.normalization.inputConsumed()
			if err != nil {
				test.Fatal(err)
			}
		}
		resources, err := work.RetainedResources()
		if err != nil {
			test.Fatal(err)
		}
		peakHeap = max(peakHeap, resources.HeapBytes)
		if !busy && !consumed && resources.HeapBytes > 512<<10 && observed < 0 {
			observed = step
		}
		if observed >= 0 && !busy {
			if consumed {
				late, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				test.Fatalf("no checkpoint while input remained live: active_at=%d consumed_at=%d late_root=%t retained_heap_peak=%d", observed, step, found && late.hash != "", peakHeap)
			}
			state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
			if err != nil {
				test.Fatal(err)
			}
			if found && state.hash != "" {
				if state.stage.plan != "" || state.stage.complete || state.sequence == 0 {
					test.Fatal("parser checkpoint was replaced with completed canonical staging", state)
				}
				return
			}
		}
		progress, err := work.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending || work.normalization == nil || work.normalization.canonical != nil {
			test.Fatal("parser checkpoint candidate bypassed its required processing boundary", step, progress, err)
		}
	}
	test.Fatal("active parser never obtained a durable checkpoint", observed, peakHeap)
}
