//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func normalizationParserRestartPGStart(test *testing.T, storage *PostgresStore, ctx context.Context, nested bool) (legacyByteStagePGFixture, *preparedLegacyCaptureWork, domain.DocRangeIndexJob, []byte) {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "resume after the large header"}}}},
	}
	if nested {
		document.Events = []domain.CIREvent{
			{Kind: domain.EventMessage, Seq: 9, Role: domain.RoleAssistant,
				Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("older-event🙂", 1300)}}},
			{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "already staged before the replacement"}}},
			{Kind: domain.EventCompaction, Seq: 7, ReplacementComplete: true, Replacement: []domain.CIREvent{
				{Kind: domain.EventMessage, Seq: 8, Role: domain.RoleAssistant,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "completed replacement sibling"}}},
				{Kind: domain.EventCompaction, Seq: 3, ReplacementComplete: true, Replacement: []domain.CIREvent{
					{Kind: domain.EventMessage, Seq: 5, Role: domain.RoleAssistant,
						Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("replacement-progress🙂", 45000)}}},
				}},
			}},
			{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "source continues after the replacement"}}},
		}
	} else {
		document.Envelope.Cwd = strings.Repeat("parser-progress🙂", 150000)
	}
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	legacyByteRecoveryPGPreservationFixture(test, storage, ctx, fixture)
	stored, err := json.MarshalIndent(document, "", " ")
	if err != nil {
		test.Fatal(err)
	}
	work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
	return fixture, work, job, stored
}

func normalizationParserRestartPGAwaitRoot(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob, wantCount int) {
	test.Helper()
	for step := 0; step < normalizationCheckpointPGStepLimit; step++ {
		if work.normalization == nil || work.normalization.canonical != nil || work.canonicalProof.Valid() || work.work != nil {
			test.Fatal("parser recovery cut advanced into canonical verification or publication", step)
		}
		if !work.checkpoint.busy() {
			consumed, err := work.normalization.inputConsumed()
			if err != nil || consumed {
				test.Fatal("parser checkpoint missed its live-input boundary", step, consumed, err)
			}
			if work.checkpoint != nil && work.checkpoint.state.hash != "" {
				root := work.checkpoint.root
				if root.Parser == nil || root.State.Version != 2 || !root.State.Parsing() || root.State.Count != wantCount ||
					work.decoding == nil || work.normalization.prepared {
					test.Fatal("early ACK discarded the original decoder or saved the wrong processing state", root.State, step)
				}
				return
			}
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("real worker failed before acknowledging its live parser checkpoint", step, progress, err)
		}
	}
	test.Fatal("real worker never acknowledged a live parser checkpoint")
}

func normalizationParserRestartPGAssertObjects(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, work *preparedLegacyCaptureWork, stored []byte) {
	test.Helper()
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || state != work.checkpoint.state || state.hash == "" || state.sequence != 1 ||
		state.stage.plan != "" || state.stage.complete {
		test.Fatal("parser checkpoint is not the first independently committed processing root", state, found, err)
	}
	body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, state.hash, state.bytes)
	if err != nil {
		test.Fatal(err)
	}
	root, err := decodeLegacyNormalizationCheckpoint(body, state)
	if err != nil || !reflect.DeepEqual(root, work.checkpoint.root) || root.Parser == nil || !root.State.Parsing() {
		test.Fatal("durable root differs from the acknowledged parser checkpoint", err)
	}
	parser := root.Parser
	for _, reference := range []legacyNormalizationBufferRef{parser.Ref, parser.Pins} {
		body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, reference.Hash, reference.Bytes)
		if err != nil || len(body) != reference.Bytes || domain.HashContent(body) != reference.Hash {
			test.Fatal("parser control or retention pins are missing from immutable staging", reference, err)
		}
	}
	input := parser.Input
	if input.Stage != state.stage.incarnation || input.Generation != 1 || input.ChunkBytes != domain.LegacyNormalizationPartBytes ||
		input.Tree.DataBytes != len(stored) || input.Tree.Leaves != (len(stored)+input.ChunkBytes-1)/input.ChunkBytes {
		test.Fatal("parser input did not retain the complete decoded source as bounded immutable parts", input, len(stored))
	}
	for offset := 0; offset < len(stored); offset += input.ChunkBytes {
		length := min(input.ChunkBytes, len(stored)-offset)
		part, err := storage.readLegacyNormalizationPrefix(ctx, job, state, input, offset, length)
		if err != nil || !bytes.Equal(part, stored[offset:offset+length]) {
			test.Fatal("captured parser input changed or lost a decoded source part", offset, err)
		}
	}
	var controlPinned, pinsPinned, inputPinned bool
	err = storage.pool.QueryRow(ctx, `WITH RECURSIVE reachable(hash) AS (
 SELECT $3::text UNION
 SELECT edge.child_hash FROM doc_legacy_checkpoint_edges_v1 edge JOIN reachable parent ON edge.parent_hash=parent.hash
 WHERE edge.doc_hash=$1 AND edge.stage_id=$2::uuid
 ) SELECT EXISTS(SELECT 1 FROM reachable WHERE hash=$4),
 EXISTS(SELECT 1 FROM reachable WHERE hash=$5),EXISTS(SELECT 1 FROM reachable WHERE hash=$6)`,
		job.DocHash, state.stage.incarnation, state.hash, parser.Ref.Hash, parser.Pins.Hash, input.Tree.Hash).
		Scan(&controlPinned, &pinsPinned, &inputPinned)
	if err != nil || !controlPinned || !pinsPinned || !inputPinned {
		test.Fatal("durable parser root does not retain its control, pins and decoded-input graph", controlPinned, pinsPinned, inputPinned, err)
	}
	var depth int
	err = storage.pool.QueryRow(ctx, `WITH RECURSIVE pins(hash,depth) AS (
 SELECT $3::text,0 UNION
 SELECT edge.child_hash,parent.depth+1 FROM doc_legacy_checkpoint_edges_v1 edge JOIN pins parent ON edge.parent_hash=parent.hash
 WHERE edge.doc_hash=$1 AND edge.stage_id=$2::uuid
 ) SELECT max(depth) FROM pins`, job.DocHash, state.stage.incarnation, parser.Pins.Hash).Scan(&depth)
	if err != nil || depth > 9 {
		test.Fatal("parser retention index grew with the number of objects instead of bounded tree height", depth, err)
	}
}

func TestNormalizationParserBindingMatchesSharedDocumentScope(test *testing.T) {
	work := &legacyNormalizationCheckpointWork{job: domain.DocRangeIndexJob{RepoID: domain.HashContent([]byte("repo-a")), DocHash: domain.HashContent([]byte("shared document"))},
		state: legacyByteCheckpoint{stage: legacyByteStage{sourceID: "source", sourceVersion: 3, incarnation: "stage"}}}
	binding := work.parserBinding()
	work.job.RepoID = domain.HashContent([]byte("repo-b"))
	if work.parserBinding() != binding {
		test.Fatal("repository-local authorization was mixed into the globally shared document checkpoint identity")
	}
	work.state.stage.sourceVersion++
	if work.parserBinding() == binding {
		test.Fatal("parser checkpoint identity did not follow current source generation")
	}
}

func TestNormalizationCheckpointPGParserColdRestartBeforeInputConsumed(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		nested bool
		count  int
	}{
		{name: "large-header"},
		{name: "nested-replacement-after-staged-events", nested: true, count: 2},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage, ctx := rootProjectionIsolatedPG(test)
			directory := rangeSpoolTempDir(test)
			fixture, work, job, stored := normalizationParserRestartPGStart(test, storage, ctx, scenario.nested)
			defer work.Close()
			preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
			normalizationParserRestartPGAwaitRoot(test, work, ctx, job, scenario.count)
			normalizationParserRestartPGAssertObjects(test, storage, ctx, job, work, stored)
			if scenario.nested {
				control := work.checkpoint.root.State
				if control.Files[0].Size == 0 || control.Files[1].Size == 0 ||
					control.Files[0].BufferBytes+control.Files[0].PageBytes+control.Files[1].BufferBytes+control.Files[1].PageBytes == 0 {
					test.Fatal("replacement cut lost already-staged event bytes, records or partial spool state", control.Files)
				}
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("live parser checkpoint became publishable before canonical proof", err)
			}
			normalizationCheckpointPGColdReclaim(test, storage, ctx, fixture, job, work)
			if work.decoding != nil || work.normalization != nil || work.source != nil {
				test.Fatal("closed producer retained a process-local decoder, normalizer or source")
			}
			if counts := generationCountsPG(test, storage, ctx, fixture.hash); counts[0] != 1 || counts[1] == 0 {
				test.Fatal("cold parser continuation did not publish exactly one complete generation", counts)
			}
			if legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != preserved {
				test.Fatal("parser recovery changed original conversation, memory, refs or publication identity")
			}
			if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
				test.Fatal("producer or fresh-process parser consumer leaked local spools", entries, err)
			}
		})
	}
}

func TestNormalizationCheckpointPGParserFirstRootSaveFences(test *testing.T) {
	for _, change := range []string{"source version", "ownership", "lease expiry"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := rootProjectionIsolatedPG(test)
			rangeSpoolTempDir(test)
			fixture, work, job, _ := normalizationParserRestartPGStart(test, storage, ctx, false)
			defer work.Close()
			normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
				return checkpoint.capture != nil && checkpoint.next.Parser != nil && checkpoint.next.State.Version == 2 &&
					checkpoint.next.State.Parsing() && checkpoint.pending.hash != ""
			})
			prior := work.checkpoint.state
			if prior.hash != "" || prior.sequence != 0 || !work.checkpoint.next.valid(prior) {
				test.Fatal("fence fixture missed the first parser root's uncommitted save boundary", prior)
			}
			legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
			private := legacyNormalizationGCPGImage(test, storage, ctx, job)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			progress, err := work.Step(ctx, job)
			want := domain.ErrConflict
			if change == "ownership" {
				want = domain.ErrNotFound
			}
			if progress != 0 || !errors.Is(err, want) || work.checkpoint.state != prior || work.canonicalProof.Valid() || work.work != nil {
				test.Fatal("obsolete parser checkpoint retained save or publication authority", change, progress, err)
			}
			if legacyNormalizationGCPGImage(test, storage, ctx, job) != private ||
				legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public ||
				generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
				test.Fatal("fenced first parser-root save changed durable checkpoint or public state", change)
			}
		})
	}
}
