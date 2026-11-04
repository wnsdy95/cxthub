package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestContributionMemoryNormalizationPreservesPRWireIdentity(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	fixture.publish(t, fixture.digest(t, "", "recorded project memory"), true)
	fixture.reconcile(t)
	ctx := systemTestContext()
	history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := fixture.worker.core.verifiedPRContributions(ctx, fixture.repo.ID, history)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := fixture.storage.ListPRContributionMemories(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(pins)
	if err != nil {
		t.Fatal(err)
	}
	records, normalized, err := normalizePRContributionMemory(ctx, fixture.repo.ID, verified, pins)
	if err != nil || len(records) != 1 || len(normalized) != 1 {
		t.Fatalf("normalization: records=%+v pins=%+v err=%v", records, normalized, err)
	}
	after, err := json.Marshal(normalized)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("pin wire identity changed: before=%s after=%s err=%v", before, after, err)
	}
	if records[0].Record.ID != verified[0].Record.ID || records[0].Record.IntegrationSHA != verified[0].Record.PR.MergeSHA || records[0].Valid != verified[0].Valid {
		t.Fatalf("PR evidence identity changed: %+v", records[0])
	}
	corrupt := append([]domain.PRContributionMemory(nil), pins...)
	corrupt[0].MemoryHash = hh("tampered memory")
	if _, _, err := normalizePRContributionMemory(ctx, fixture.repo.ID, verified, corrupt); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("normalization bypassed immutable pin validation: %v", err)
	}
	invalid := append([]verifiedPRContribution(nil), verified...)
	invalid[0].Record.OriginalHead = effectiveOID(99)
	if _, _, err := normalizePRContributionMemory(ctx, fixture.repo.ID, invalid, pins); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("normalization bypassed contribution identity validation: %v", err)
	}
	unchanged, err := json.Marshal(pins)
	if err != nil || !bytes.Equal(before, unchanged) {
		t.Fatal("normalization mutated persisted pin input")
	}
}

func TestContributionMemorySharedEngineHandlesGenericPins(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmtEmptyMemory(empty), func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			hash := domain.ContentHash("")
			if !empty {
				hash = fixture.digest(t, "", "shared pinned memory")
			}
			publication := fixture.publish(t, hash, true)
			fixture.reconcile(t)
			ctx := systemTestContext()
			history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := fixture.worker.core.verifiedPRContributions(ctx, fixture.repo.ID, history)
			if err != nil || len(verified) != 1 {
				t.Fatalf("fixture contributions: %+v %v", verified, err)
			}
			pins, err := fixture.storage.ListPRContributionMemories(ctx, fixture.repo.ID)
			if err != nil || len(pins) != 1 {
				t.Fatalf("fixture pins: %+v %v", pins, err)
			}
			snapshots, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := fixture.worker.core.newCodeEvidence(ctx, fixture.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			inclusion, err := fixture.worker.core.branchContext(ctx, fixture.main, strings.Repeat("e", 40), snapshots, history, evidence)
			if err != nil {
				t.Fatal(err)
			}
			pr := verified[0].Record
			generic := (domain.GitContribution{RepoID: pr.RepoID, GitOrigin: pr.GitOrigin, ProviderRepositoryID: pr.ProviderRepositoryID,
				DestinationBranchID: pr.DestinationBranchID, SourceBranchID: pr.SourceBranchID, Capture: pr.Capture,
				OriginalHead: pr.OriginalHead, IntegrationSHA: pr.PR.MergeSHA}).WithID()
			pin := (domain.GitContributionMemory{RepoID: generic.RepoID, ContributionID: generic.ID, PublicationID: publication.ID,
				MemoryHash: pins[0].MemoryHash, MemorySource: pins[0].MemorySource, MemoryPinned: true}).WithID()
			inputs := []verifiedGitContribution{{Record: generic, Valid: true, Publications: verified[0].Publications}}
			records, memories, err := normalizeGitContributionMemory(ctx, fixture.repo.ID, inputs, []domain.GitContributionMemory{pin})
			if err != nil {
				t.Fatal(err)
			}
			prPlan, err := fixture.worker.core.prepareVerifiedPRContributionMemory(ctx, fixture.repo.ID, inclusion, snapshots, verified, pins)
			if err != nil {
				t.Fatal(err)
			}
			inclusion.Merges[0].EventID, inclusion.Merges[0].PRNumber = generic.ID, 0
			genericPlan, err := fixture.worker.core.prepareVerifiedContributionMemory(ctx, fixture.repo.ID, inclusion, snapshots, records, memories)
			if err != nil || len(genericPlan.Versions) != 1 || len(prPlan.Versions) != 1 {
				t.Fatalf("generic memory projection: %+v %v", genericPlan, err)
			}
			got, want := genericPlan.Versions[0], prPlan.Versions[0]
			if got.State != want.State || got.Hash != want.Hash || !reflect.DeepEqual(got.Digest(), want.Digest()) || got.Pins[0].ContributionID != generic.ID {
				t.Fatalf("generic projection changed memory semantics: got=%+v want=%+v", got, want)
			}
			prRecords, prPins, err := normalizePRContributionMemory(ctx, fixture.repo.ID, verified, pins)
			if err != nil {
				t.Fatal(err)
			}
			mixedRecords := append(prRecords, records...)
			mixedPins := append(prPins, memories...)
			mixedPlan, err := fixture.worker.core.prepareVerifiedContributionMemory(ctx, fixture.repo.ID, inclusion, snapshots, mixedRecords, mixedPins)
			if err != nil || len(mixedPlan.Versions) != 1 {
				t.Fatalf("mixed PR/Git evidence duplicated a memory version: %+v %v", mixedPlan, err)
			}
			mixed := mixedPlan.Versions[0]
			if mixed.State != want.State || mixed.Hash != want.Hash || !reflect.DeepEqual(mixed.Digest(), want.Digest()) || len(mixed.Pins) != 2 {
				t.Fatalf("mixed contribution lost independent pins: %+v", mixed)
			}
			mixedRecords[0], mixedRecords[1] = mixedRecords[1], mixedRecords[0]
			mixedPins[0], mixedPins[1] = mixedPins[1], mixedPins[0]
			reorderedPlan, err := fixture.worker.core.prepareVerifiedContributionMemory(ctx, fixture.repo.ID, inclusion, snapshots, mixedRecords, mixedPins)
			if err != nil || !reflect.DeepEqual(reorderedPlan, mixedPlan) {
				t.Fatalf("arrival order changed mixed memory projection: %+v %v", reorderedPlan, err)
			}
			pin.MemoryPinned = false
			pin = pin.WithID()
			if _, _, err := normalizeGitContributionMemory(ctx, fixture.repo.ID, inputs, []domain.GitContributionMemory{pin}); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("unpinned generic memory accepted: %v", err)
			}
		})
	}
}

func fmtEmptyMemory(empty bool) string {
	if empty {
		return "explicit empty"
	}
	return "original pinned version"
}
