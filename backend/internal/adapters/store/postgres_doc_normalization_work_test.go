//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docNormalizationPGStart(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) (*preparedLegacyCaptureWork, domain.DocRangeIndexJob, *docCaptureWorkPGTrace) {
	test.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, fixture.raw, "", "  "); err != nil {
		test.Fatal(err)
	}
	return docNormalizationPGStartSource(test, storage, ctx, fixture, pretty.Bytes())
}

func docNormalizationPGStartSource(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, stored []byte) (*preparedLegacyCaptureWork, domain.DocRangeIndexJob, *docCaptureWorkPGTrace) {
	test.Helper()
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
		test.Fatal(err)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	docCaptureWorkPGCopy(test, work, ctx, job)
	docCaptureWorkPGDecode(test, work, ctx, job)
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
		if work.normalization != nil {
			if work.original || work.canonical != nil || work.canonicalProof.Valid() || work.proofData != nil || work.work != nil {
				test.Fatal("normalization inherited authority before proof")
			}
			return work, job, trace
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			test.Fatal("normalized source did not enter its provisional path", quantum, progress, err)
		}
	}
	test.Fatal("normalization route was never selected")
	return nil, domain.DocRangeIndexJob{}, nil
}

func TestDocNormalizationWorkPGFencesEachPhase(test *testing.T) {
	for _, phase := range []string{"decode", "proof", "handoff"} {
		for _, change := range []string{"source version", "ownership", "lease expiry", "job fence", "snapshot identity", "source incarnation", "cancellation"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
				work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
				ready := false
				for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
					ready = phase == "decode" && work.normalization != nil && work.normalization.canonical == nil ||
						phase == "proof" && work.normalization != nil && work.normalization.canonical != nil ||
						phase == "handoff" && work.canonicalProof.Valid()
					if ready {
						break
					}
					if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
						test.Fatal("normalization passed a requested private phase", phase, quantum, progress, err)
					}
				}
				if !ready {
					test.Fatal("normalization never reached requested phase", phase)
				}
				selected, cancel := context.WithCancel(ctx)
				defer cancel()
				if change == "cancellation" {
					cancel()
				} else {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				}
				private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				before := fmt.Sprintf("%#v", work.normalization)
				queries := trace.queries.Load()
				progress, err := work.Step(selected, job)
				want := domain.ErrConflict
				if change == "ownership" || change == "snapshot identity" {
					want = domain.ErrNotFound
				} else if change == "cancellation" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || progress != 0 || work.work != nil || before != fmt.Sprintf("%#v", work.normalization) {
					test.Fatal("obsolete or canceled normalization advanced", phase, change, progress, err)
				}
				if change == "cancellation" && queries != trace.queries.Load() {
					test.Fatal("canceled normalization touched SQL")
				}
				if private != legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
					test.Fatal("fenced normalization changed persisted state")
				}
			})
		}
	}
}

func TestDocNormalizationWorkPGOwnsResourcesAndCleansScratch(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
	work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
	queries := trace.bodyReads.Load()
	paths := make([]string, 0, domain.LegacyNormalizationSlots)
	for _, spool := range work.normalization.storage.spools {
		paths = append(paths, spool.file.Name())
	}
	peakHeap, peakSpool := int64(0), int64(0)
	for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && !work.canonicalProof.Valid(); quantum++ {
		resources, err := work.RetainedResources()
		if err != nil || resources.HeapBytes < 0 || resources.SpoolBytes < 0 {
			test.Fatal("normalization omitted or corrupted resource accounting", resources, err)
		}
		var extents int64
		if work.decoding != nil {
			extents += int64(work.decoding.decoded.size)
		}
		if work.normalization != nil {
			if work.normalization.storage != nil {
				for _, spool := range work.normalization.storage.spools {
					extents += int64(spool.size)
				}
			}
			if work.normalization.data != nil {
				extents += int64(work.normalization.data.size)
			}
		}
		if resources.SpoolBytes != extents {
			test.Fatal("normalization did not account all actual spool extents", resources.SpoolBytes, extents)
		}
		peakHeap, peakSpool = max(peakHeap, resources.HeapBytes), max(peakSpool, resources.SpoolBytes)
		if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("provisional normalized source became publishable", err)
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			test.Fatal("normalization failed before separate handoff", quantum, progress, err)
		}
	}
	if !work.canonicalProof.Valid() || work.original || work.normalization != nil || trace.bodyReads.Load() != queries {
		test.Fatal("normalization bypassed source proof or reread SQL payload")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			test.Fatal("normalization scratch survived proof handoff", path, err)
		}
	}
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || work.decoding != nil || work.proofData != nil {
		test.Fatal("normalized source did not transfer its verified target exactly once", progress, err)
	}
	if err := work.Close(); err != nil {
		test.Fatal(err)
	}
	if err := work.Close(); err != nil {
		test.Fatal("normalization cleanup is not idempotent", err)
	}
	test.Logf("accounted retained-resource peaks, not RSS: heap=%d spool=%d", peakHeap, peakSpool)
}

func TestDocNormalizationWorkPGIgnoredGiantValueAndMidFrameFence(test *testing.T) {
	for _, scenario := range []struct {
		event   bool
		changed bool
	}{{}, {changed: true}, {event: true}, {event: true, changed: true}} {
		test.Run(fmt.Sprintf("event=%t/changed=%t", scenario.event, scenario.changed), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			stored := append([]byte(`{"ignored":"`), bytes.Repeat([]byte("x"), 3*domain.ConversationManifestChunkBytes+73)...)
			stored = append(stored, []byte(`",`)...)
			stored = append(stored, fixture.raw[1:]...)
			if scenario.event {
				var parts struct {
					Envelope json.RawMessage   `json:"envelope"`
					Events   []json.RawMessage `json:"events"`
				}
				if err := json.Unmarshal(fixture.raw, &parts); err != nil || len(parts.Events) == 0 {
					test.Fatal("ignored event fixture lost its original events", err)
				}
				events := make([][]byte, len(parts.Events))
				for index, event := range parts.Events {
					events[index] = event
				}
				events[0] = append([]byte(`{"ignored_native":"`), bytes.Repeat([]byte("x"), 3*domain.ConversationManifestChunkBytes+73)...)
				events[0] = append(events[0], []byte(`",`)...)
				events[0] = append(events[0], parts.Events[0][1:]...)
				stored = domain.AssembleCanonicalDoc(parts.Envelope, [][]byte{bytes.Join(events, []byte(","))})
			}
			work, job, trace := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
			queries := trace.bodyReads.Load()
			for quantum := 0; quantum < 3; quantum++ {
				if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.canonicalProof.Valid() {
					test.Fatal("ignored value did not remain provisional across framing steps", quantum, progress, err)
				}
			}
			if scenario.changed {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
				progress, err := work.Step(ctx, job)
				if !errors.Is(err, domain.ErrConflict) || progress != 0 || work.work != nil || work.canonicalProof.Valid() {
					test.Fatal("mid-frame source change bypassed authority confirmation", progress, err)
				}
				return
			}
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && !work.canonicalProof.Valid(); quantum++ {
				resources, err := work.RetainedResources()
				if err != nil || resources.HeapBytes <= 0 || resources.HeapBytes >= 1<<20 {
					test.Fatal("ignored value was retained as a whole or lost accounting", resources, err)
				}
				if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
					test.Fatal("framed or normalized input became publishable without original proof", err)
				}
				if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
					test.Fatal("ignored input did not earn the original source proof", quantum, progress, err)
				}
			}
			if !work.canonicalProof.Valid() || work.original || trace.bodyReads.Load() != queries {
				test.Fatal("ignored input changed routing or reread SQL bodies")
			}
			if progress, err := docCaptureWorkPGDrainPreparation(work, ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work == nil {
				test.Fatal("ignored input did not hand off the verified target", progress, err)
			}
			progress := outbound.DocRangeWorkPending
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
				var err error
				progress, err = work.Step(ctx, job)
				if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
					test.Fatal("normalized target failed delegated preparation", quantum, progress, err)
				}
			}
			if progress != outbound.DocRangeWorkPublishable {
				test.Fatal("ignored input did not finish delegated preparation")
			}
			if err := work.Publish(ctx, job); err != nil {
				test.Fatal(err)
			}
			proof, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
			if err != nil || !bytes.Equal(proof.Bytes(), fixture.raw) {
				test.Fatal("ignored input changed the actual published canonical document", err)
			}
		})
	}
}

func TestDocNormalizationWorkPGStreamingEventOutputIsFenced(test *testing.T) {
	for _, change := range []string{"unchanged", "source version", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 5*(64<<10))
			work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
			queries := trace.bodyReads.Load()
			partial := false
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
				if work.normalization != nil && work.normalization.storage != nil && work.normalization.canonical == nil {
					body := work.normalization.storage.spools[0]
					partial = body.size >= 64<<10 && body.size < len(fixture.stream)/2
					if partial {
						break
					}
				}
				if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.canonicalProof.Valid() {
					test.Fatal("fixture passed its unfinished streamed event", quantum, progress, err)
				}
			}
			if !partial {
				test.Fatal("event output did not yield while its body was incomplete")
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("unfinished streamed event became publishable", err)
			}
			if change != "unchanged" {
				selected, cancel := context.WithCancel(ctx)
				defer cancel()
				want := error(domain.ErrConflict)
				if change == "cancellation" {
					cancel()
					want = context.Canceled
				} else {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				}
				before := work.normalization.storage.spools[0].size
				progress, err := work.Step(selected, job)
				if !errors.Is(err, want) || progress != 0 || work.work != nil || work.canonicalProof.Valid() || work.normalization.storage.spools[0].size != before {
					test.Fatal("obsolete streamed event advanced or earned authority", progress, err)
				}
				return
			}
			progress, err := docCaptureWorkPGDrainPreparation(work, ctx, job)
			if err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || trace.bodyReads.Load() != queries {
				test.Fatal("streamed event failed its verified handoff or reread SQL", progress, err)
			}
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
				progress, err = work.Step(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
			}
			if progress != outbound.DocRangeWorkPublishable {
				test.Fatal("streamed event never became publishable")
			}
			if err := work.Publish(ctx, job); err != nil {
				test.Fatal(err)
			}
			proof, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
			if err != nil || !bytes.Equal(proof.Bytes(), fixture.raw) {
				test.Fatal("streamed event changed the actual published canonical source", err)
			}
		})
	}
}

func TestDocNormalizationWorkPGLargeKeySourceRetainsFences(test *testing.T) {
	for _, change := range []string{"unchanged", "source version", "ownership", "cancellation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			prefix := strings.Repeat("prefix", (3*(64<<10))/6)
			input := map[string]any{}
			for _, suffix := range []string{"z", "a", "m"} {
				input[prefix+suffix] = []any{map[string]any{"identity": suffix}, map[string]any{"tail": suffix + "-tail"}}
			}
			document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
				SessionOriginID: string(repo), Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{
				{Kind: domain.EventToolCall, Seq: 0, CallID: "synthetic", ToolName: "synthetic", Input: input},
			}}
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
			work, job, trace := docNormalizationPGStart(test, storage, ctx, fixture)
			queries := trace.bodyReads.Load()
			for quantum := 0; quantum < 12; quantum++ {
				progress, err := work.Step(ctx, job)
				if err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.normalization == nil || work.normalization.canonical != nil {
					test.Fatal("large-key preparation escaped its provisional phase", quantum, progress, err)
				}
			}
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("unfinished large-key preparation became publishable", err)
			}
			if change != "unchanged" {
				selected, cancel := context.WithCancel(ctx)
				defer cancel()
				want := error(domain.ErrConflict)
				if change == "ownership" {
					want = domain.ErrNotFound
				}
				if change == "cancellation" {
					cancel()
					want = context.Canceled
				} else {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
				}
				if progress, err := work.Step(selected, job); !errors.Is(err, want) || progress != 0 || work.work != nil || work.canonicalProof.Valid() {
					test.Fatal("obsolete large-key input advanced or earned authority", progress, err)
				}
				return
			}
			progress, err := docCaptureWorkPGDrainPreparation(work, ctx, job)
			if err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || trace.bodyReads.Load() != queries {
				test.Fatal("large-key source failed its original verified handoff or reread SQL", progress, err)
			}
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && progress != outbound.DocRangeWorkPublishable; quantum++ {
				progress, err = work.Step(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
			}
			if progress != outbound.DocRangeWorkPublishable {
				test.Fatal("large-key source never became publishable")
			}
			if err := work.Publish(ctx, job); err != nil {
				test.Fatal(err)
			}
			proof, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
			if err != nil || !bytes.Equal(proof.Bytes(), fixture.raw) {
				test.Fatal("large-key ordering changed the actual published source", err)
			}
		})
	}
}

func TestDocNormalizationWorkPGScratchDamageKeepsSourcePriority(test *testing.T) {
	for _, changed := range []bool{false, true} {
		test.Run(fmt.Sprintf("changed=%t", changed), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			work, job, _ := docNormalizationPGStart(test, storage, ctx, fixture)
			spool := work.normalization.storage.spools[0]
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && spool.size == 0; quantum++ {
				if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil || work.canonicalProof.Valid() {
					test.Fatal("normalization did not seal selected bodies before corruption", quantum, progress, err)
				}
			}
			if spool.size == 0 {
				test.Fatal("normalization did not write selected event bodies")
			}
			if _, err := spool.file.WriteAt([]byte("!"), 0); err != nil {
				test.Fatal(err)
			}
			if changed {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
			}
			var progress outbound.DocRangeWorkProgress
			var err error
			for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && err == nil; quantum++ {
				progress, err = work.Step(ctx, job)
				if work.work != nil || work.canonicalProof.Valid() || progress != outbound.DocRangeWorkPending && err == nil {
					test.Fatal("corrupt selected bodies acquired authority before detection", quantum, progress, err)
				}
			}
			want := domain.ErrPreparedDocRangeObject
			if changed {
				want = domain.ErrConflict
			}
			if !errors.Is(err, want) || progress == outbound.DocRangeWorkPublishable || work.work != nil || work.canonicalProof.Valid() {
				test.Fatal("normalization scratch corruption escaped original source priority", progress, err, want)
			}
		})
	}
}

func TestDocNormalizationWorkOriginalOracleCandidates(test *testing.T) {
	selected := 0
	for _, fixture := range legacyMonolithicVerifierCases(test) {
		if fixture.hash == domain.HashContent(fixture.raw) && !fixture.canceled {
			continue
		}
		selected++
		test.Run(fixture.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fixture.canceled {
				cancel()
			}
			source := domain.LegacyByteSource{Bytes: len(fixture.raw), Read: func(ctx context.Context, offset, length int) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length < 0 || offset > len(fixture.raw) || length > len(fixture.raw)-offset {
					return nil, domain.ErrIntegrity
				}
				return fixture.raw[offset : offset+length], nil
			}}
			for offset := 0; offset < len(fixture.raw); offset += domain.ConversationManifestChunkBytes {
				part := fixture.raw[offset:min(offset+domain.ConversationManifestChunkBytes, len(fixture.raw))]
				source.Parts = append(source.Parts, domain.ConversationManifestChunk{Hash: domain.HashContent(part), Bytes: int64(len(part))})
			}
			var verifier domain.CanonicalDocVerifier
			work, err := newLegacyNormalizationWork(ctx, fixture.hash, source, &verifier)
			if work != nil {
				defer func() {
					if closeErr := work.Close(); closeErr != nil {
						test.Error(closeErr)
					}
				}()
			}
			complete := false
			for quantum := 0; err == nil && !complete && quantum < docCaptureWorkPGPreparationStepLimit; quantum++ {
				complete, err = work.Step(ctx)
			}
			if !errors.Is(err, fixture.wantErr) || (err == nil) != fixture.expected.Valid() || !complete && err == nil {
				test.Fatal("normalized producer changed original acceptance or error", complete, err, fixture.wantErr)
			}
			if err != nil {
				return
			}
			proof, err := work.canonical.Result()
			if err != nil || !proof.Valid() {
				test.Fatal("normalized producer did not earn original authority", err)
			}
			plan, err := proof.Plan()
			if err != nil {
				test.Fatal(err)
			}
			want, err := domain.NewLegacyByteBackfill(ctx, fixture.expected)
			if err != nil {
				test.Fatal(err)
			}
			gotSpec, _ := plan.Spec()
			wantSpec, _ := want.Spec()
			if gotSpec != wantSpec {
				test.Fatal("normalized producer changed original plan", gotSpec, wantSpec)
			}
			for ordinal := range gotSpec.Blocks {
				got, err := plan.Block(ctx, ordinal)
				expected, wantErr := want.Block(ctx, ordinal)
				if err != nil || wantErr != nil || got.Descriptor() != expected.Descriptor() || !bytes.Equal(got.Bytes(), expected.Bytes()) {
					test.Fatal("normalized producer changed original event bytes", ordinal, err, wantErr)
				}
			}
		})
	}
	if selected < 10 {
		test.Fatal("original normalization candidate corpus was not exercised", selected)
	}
}
