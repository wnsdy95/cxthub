//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func normalizationStageRestartPGOutput(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, staged []byte, completedBytes int) (int, bool) {
	test.Helper()
	if work.normalization == nil || work.normalization.preparation == nil || work.normalization.prepared ||
		work.normalization.canonical != nil || work.canonicalProof.Valid() || work.work != nil || work.decoding == nil {
		test.Fatal("Stage restart cut passed live input processing or acquired canonical authority")
	}
	if work.checkpoint.busy() {
		return 0, false
	}
	consumed, err := work.normalization.preparation.InputConsumed()
	if err != nil || consumed {
		test.Fatal("Stage restart must not substitute an input-consumed checkpoint", consumed, err)
	}
	if _, err := work.normalization.preparation.ParserInputPosition(ctx); err == nil {
		return 0, false
	} else if !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("could not distinguish actual event-output Stage from decoder progress", err)
	}
	spool, err := work.normalization.storage.slot(ctx, 0)
	if err != nil {
		test.Fatal(err)
	}
	if spool.size <= completedBytes+domain.LegacyCanonicalEncodingBytes {
		return 0, false
	}
	if spool.size >= len(staged) {
		test.Fatal("Stage cut missed the partially emitted large Output event", spool.size, len(staged))
	}
	body, err := spool.Read(ctx, 0, spool.size)
	if err != nil || !bytes.Equal(body, staged[:spool.size]) {
		test.Fatal("actual Stage spool lost the completed Input or the current Output prefix", err)
	}
	return spool.size - completedBytes, true
}

func TestNormalizationCheckpointPGStageOutputColdRestartBeforeInputConsumed(test *testing.T) {
	for _, mode := range []string{"manual", "automatic"} {
		test.Run(mode, func(test *testing.T) {
			dsn := os.Getenv("CXT_TEST_DSN")
			if dsn == "" {
				test.Fatal("Stage restart requires an explicitly supplied existing owned local PostgreSQL server")
			}
			config, err := pgx.ParseConfig(dsn)
			if err != nil {
				test.Fatal("invalid local PostgreSQL test configuration")
			}
			if !processAdmissionLocalHost(config.Host) {
				test.Fatal("Stage restart refuses a nonlocal PostgreSQL server")
			}
			for _, fallback := range config.Fallbacks {
				if !processAdmissionLocalHost(fallback.Host) {
					test.Fatal("Stage restart refuses a nonlocal PostgreSQL fallback")
				}
			}
			storage, ctx := rootProjectionIsolatedPG(test)
			if _, owned := docDisplayOwnedFixturesPG.Load(storage.pool.Config().ConnConfig.Database); !owned {
				test.Fatal("Stage restart requires its own isolated local database")
			}
			directory := rangeSpoolTempDir(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			document := domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
					SessionOriginID: string(repo), Fidelity: domain.FidelityFull, Cwd: strings.Repeat("parser-progress🙂", 150000)},
				Events: []domain.CIREvent{
					{Kind: domain.EventToolCall, Seq: 1, CallID: "stage-restart-call", ToolName: "stage-restart",
						Input: map[string]any{"payload": strings.Repeat("stage-input🙂", 100000) + "한🙂\"\\\n<>&",
							"tail": []any{true, nil, "Input must remain staged"}}},
					{Kind: domain.EventToolResult, Seq: 2, CallID: "stage-restart-call",
						Output: map[string]any{"payload": strings.Repeat("stage-output🙂", 100000) + "한🙂\"\\\n<>&",
							"tail": []any{false, nil, "Output must resume after its saved offset"}}},
				},
			}
			for ordinal := 2; ordinal < 2301; ordinal++ {
				document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 2303 - ordinal, Role: domain.RoleAssistant,
					Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("event-%d", ordinal)}}})
			}
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
			legacyByteRecoveryPGPreservationFixture(test, storage, ctx, fixture)
			stored, err := json.MarshalIndent(document, "", " ")
			if err != nil {
				test.Fatal(err)
			}
			var canonical struct {
				Events []json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(fixture.raw, &canonical); err != nil || len(canonical.Events) != 2301 {
				test.Fatal("Stage restart fixture lost its complete event corpus", err)
			}
			staged := append(bytes.Clone(canonical.Events[0]), canonical.Events[1]...)
			completedBytes := len(canonical.Events[0])
			work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
			defer work.Close()
			preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
			normalizationParserRestartPGAwaitRoot(test, work, ctx, job, 0)
			normalizationParserRestartPGAssertObjects(test, storage, ctx, job, work, stored)
			input := work.checkpoint.root.Parser.Input
			trace := normalizationParserRepeatPGWatch(test, work, ctx, input, stored)
			emitted := 0
			normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				var ready bool
				emitted, ready = normalizationStageRestartPGOutput(test, work, ctx, staged, completedBytes)
				return ready
			})
			prior := work.checkpoint.state
			if work.checkpoint.root.Parser == nil || work.checkpoint.root.Parser.Input != input || !work.checkpoint.root.State.InputPending() {
				test.Fatal("Stage cut discarded the acknowledged live parser or its original decoded input")
			}
			parser := *work.checkpoint.root.Parser
			priorOutput := work.checkpoint.root.State.Files[0].Size
			position, phase := normalizationParserRepeatPGPosition(test, storage, ctx, job, prior, parser.Ref)
			if mode == "manual" {
				if err := work.checkpoint.begin(ctx, work.normalization.preparation); err != nil {
					test.Fatalf("actual Stage BeginCheckpoint must preserve the pending Output encoding after %d emitted bytes without consuming input; got %v", emitted, err)
				}
			} else {
				normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
					if checkpoint.capture != nil {
						control, err := checkpoint.capture.Control(ctx)
						if err != nil {
							test.Fatal(err)
						}
						if control.Staging() && control.Count == 1 && control.Active.Sequence == 2 && control.Written > emitted {
							return true
						}
					}
					spool, err := work.normalization.storage.slot(ctx, 0)
					if err != nil || spool.size >= len(staged) {
						test.Fatal("actual Output passed its partial event without an automatic Stage checkpoint", err)
					}
					return false
				})
				if work.checkpoint.state != prior {
					test.Fatal("automatic cut skipped an acknowledged root")
				}
			}
			control, err := work.checkpoint.capture.Control(ctx)
			if err != nil || control.Parsing() || control.Ready() || control.Count != 1 || control.Written < emitted ||
				control.Written >= len(canonical.Events[1]) || len(control.HashState) == 0 || control.Active.Sequence != 2 ||
				control.Active.Ordinal != 1 || control.Active.Offset != uint64(completedBytes) ||
				control.Files[0].Size != completedBytes+control.Written || control.Files[1].Size == 0 {
				test.Fatal("Stage capture did not bind the completed Input and the exact partial Output/hash offset", control, err)
			}
			if mode == "automatic" && control.Files[0].Size-priorOutput < domain.ConversationManifestChunkBytes {
				test.Fatal("automatic capture did not reach its existing output-byte threshold", control.Files[0].Size-priorOutput)
			}
			if _, err := work.normalization.preparation.Step(ctx); !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
				test.Fatal("pending Stage checkpoint did not freeze the real event encoder", err)
			}
			normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				return !checkpoint.busy() && checkpoint.state.hash != prior.hash
			})
			state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
			if err != nil || !found || state != work.checkpoint.state || state.sequence != prior.sequence+1 ||
				state.stage.plan != "" || state.stage.complete {
				test.Fatal("partial event Stage was not independently acknowledged as the latest durable root", state, found, err)
			}
			body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, state.hash, state.bytes)
			if err != nil {
				test.Fatal(err)
			}
			root, err := decodeLegacyNormalizationCheckpoint(body, state)
			if err != nil || !reflect.DeepEqual(root, work.checkpoint.root) || !reflect.DeepEqual(root.State, control) ||
				root.Parser == nil || root.Parser.Input != input || root.Parser.Ref == parser.Ref {
				test.Fatal("durable Stage root lost its exact pending-event offset, parser state or original input prefix", err)
			}
			nextPosition, nextPhase := normalizationParserRepeatPGPosition(test, storage, ctx, job, state, root.Parser.Ref)
			if nextPosition >= len(stored) || nextPosition < position || nextPosition == position && nextPhase == phase && control.Files[0].Size <= priorOutput {
				test.Fatal("Stage root did not advance input or output while retaining unconsumed source suffix", position, nextPosition, phase, nextPhase)
			}
			consumed, err := work.normalization.preparation.InputConsumed()
			if err != nil || consumed || work.decoding == nil || work.normalization.prepared || work.normalization.canonical != nil ||
				work.canonicalProof.Valid() || work.work != nil {
				test.Fatal("Stage ACK was replaced by an input-consumed root or prematurely released the live decoder", consumed, err)
			}
			if trace.objects.Load() == 0 || trace.bodyReads.Load() != 0 || trace.inputUploads.Load() != 0 || trace.inputReads.Load() != 0 {
				test.Fatal("Stage capture reread original bytes or recopied the previously acknowledged decoded input", trace.objects.Load(),
					trace.bodyReads.Load(), trace.inputUploads.Load(), trace.inputReads.Load())
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("partial event Stage acquired publication authority before fresh canonical verification", err)
			}
			if err := ctx.Err(); err != nil {
				test.Fatal("Stage producer exceeded its unchanged setup deadline", err)
			}
			restart, cancel := context.WithTimeout(test.Context(), 30*time.Second)
			defer cancel()
			normalizationCheckpointPGColdReclaim(test, storage, restart, fixture, job, work)
			if work.checkpoint != nil || work.decoding != nil || work.normalization != nil || work.source != nil {
				test.Fatal("cold Stage continuation retained the producer's process-local state")
			}
			if counts := generationCountsPG(test, storage, restart, fixture.hash); counts[0] != 1 || counts[1] == 0 ||
				legacyByteRecoveryPGPreservedImage(test, storage, restart, fixture) != preserved {
				test.Fatal("cold Stage continuation duplicated canonical publication or changed original context", counts)
			}
			if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
				test.Fatal("Stage capture or fresh-process continuation leaked local spools", entries, err)
			}
		})
	}
}

func TestNormalizationCheckpointPGAutomaticOutputGenerationReset(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	rangeSpoolTempDir(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleAssistant,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "after the duplicate events field"}}}},
	}
	envelope, err := json.Marshal(document.Envelope)
	if err != nil {
		test.Fatal(err)
	}
	prior, err := json.Marshal([]domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
		Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("discarded-output", 100000)}}}})
	if err != nil {
		test.Fatal(err)
	}
	latest, err := json.Marshal(document.Events)
	if err != nil {
		test.Fatal(err)
	}
	stored := []byte(`{"envelope":` + string(envelope) + `,"events":` + string(prior) + `,"events":` + string(latest) + `}`)
	var decoded domain.CIRDocument
	if err := json.Unmarshal(stored, &decoded); err != nil || !reflect.DeepEqual(decoded, document) {
		test.Fatal("duplicate field fixture changed the independent typed result", err)
	}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	legacyByteRecoveryPGPreservationFixture(test, storage, ctx, fixture)
	work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
	defer work.Close()
	preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		return !checkpoint.busy() && checkpoint.root.State.Staging() && checkpoint.root.State.Files[0].Size >= domain.ConversationManifestChunkBytes
	})
	generation := work.checkpoint.root.State.Files[0].Generation
	reset := false
	progress := outbound.DocRangeWorkPending
	for step := 0; step < normalizationCheckpointPGStepLimit && progress == outbound.DocRangeWorkPending; step++ {
		progress, err = work.Step(ctx, job)
		if err != nil {
			test.Fatal("valid duplicate events failed after acknowledged output", step, err)
		}
		if work.checkpoint != nil && work.checkpoint.root.State.Files[0].Generation > generation {
			reset = true
		}
	}
	if !reset || progress != outbound.DocRangeWorkPublishable {
		test.Fatal("duplicate output did not publish through a fresh file generation", reset, progress)
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal(err)
	}
	legacyByteRecoveryPGAssertPublished(test, storage, ctx, fixture)
	if generationCountsPG(test, storage, ctx, fixture.hash)[0] != 1 || legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != preserved {
		test.Fatal("generation reset changed original records or duplicated publication")
	}
}
