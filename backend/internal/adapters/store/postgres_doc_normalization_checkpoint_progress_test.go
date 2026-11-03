//go:build postgres

package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestNormalizationCheckpointPGPreparationSavesOnlyNewRecoveryProgress(test *testing.T) {
	storage, ctx := chunkReusePGContext(test, test.Context())
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	children := make([]domain.CIREvent, 4097)
	for ordinal := range children {
		children[ordinal] = domain.CIREvent{Kind: domain.EventToolCall, Seq: (len(children) - 1 - ordinal) / 2,
			CallID: fmt.Sprintf("child-%d", ordinal), ToolName: "checkpoint-progress",
			Input: map[string]any{"z": "last", "a": "first", "m": "middle"}}
	}
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
		SessionOriginID: string(repo), Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{
		{Kind: domain.EventCompaction, Seq: 7, Replacement: children, ReplacementComplete: true},
	}}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	stored, err := json.MarshalIndent(document, "", "  ")
	if err != nil || bytes.Equal(stored, fixture.raw) {
		test.Fatal("fixture lost its noncanonical source", err)
	}
	work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
	defer work.Close()
	started, renewed := time.Now(), time.Now()
	var prior domain.LegacyNormalizationControl
	var priorPosition int
	var sequence int64
	var inspected uint64
	var encoding uintptr
	steps, saves, captureSteps := 0, 0, 0
	for work.work == nil {
		if time.Since(renewed) >= 20*time.Second {
			if err := storage.RenewDocRangeJob(ctx, job, time.Now().UTC(), 2*time.Minute); err != nil {
				test.Fatal(err)
			}
			renewed = time.Now()
		}
		if work.checkpoint.busy() {
			captureSteps++
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("normalization failed before verified handoff", steps, progress, err)
		}
		steps++
		if work.normalization != nil && work.normalization.preparation != nil && !work.normalization.prepared {
			native := docCanonicalPagesPGNativeSnapshot(test, work, true)
			if native.encoding != 0 {
				if encoding != 0 && (native.encoding != encoding || native.inspectionWork < inspected) {
					test.Fatal("checkpoint rewound live preparation", encoding, native.encoding, inspected, native.inspectionWork)
				}
				encoding, inspected = native.encoding, native.inspectionWork
			}
		}
		if work.checkpoint != nil && work.checkpoint.state.sequence > sequence {
			checkpoint := work.checkpoint
			control := checkpoint.root.State
			saves++
			test.Logf("checkpoint=%d count=%d written=%d input=%d output=%d inspected=%d steps=%d capture_steps=%d elapsed=%s",
				checkpoint.state.sequence, control.Count, control.Written, checkpoint.parserPosition, control.Files[0].Size,
				inspected, steps, captureSteps, time.Since(started))
			if control.Staging() && control.Written == 0 && prior.Staging() && prior.Written == 0 &&
				checkpoint.parserPosition == priorPosition && reflect.DeepEqual(control, prior) {
				test.Fatal("saved the same restart point again despite live preparation progressing", sequence, checkpoint.state.sequence)
			}
			prior, priorPosition, sequence = control, checkpoint.parserPosition, checkpoint.state.sequence
		}
	}
	if saves == 0 || inspected == 0 {
		test.Fatal("did not exercise source checkpointing during real preparation", saves, inspected)
	}
	for {
		progress, err := work.Step(ctx, job)
		if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
			test.Fatal("verified preparation did not finish", progress, err)
		}
		if progress == outbound.DocRangeWorkPublishable {
			break
		}
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal(err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	legacyByteRecoveryPGAssertPublished(test, storage, ctx, fixture)
	test.Logf("completed checkpoints=%d steps=%d capture_steps=%d elapsed=%s", saves, steps, captureSteps, time.Since(started))
}
