//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type admissionLookupGuardPG struct {
	*gitContributionApplyPGStore
	witnessRows int
	ownerReads  int
}

func (storage *admissionLookupGuardPG) ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error) {
	return nil, errors.New("full history read is forbidden in indexed preparation")
}

func (storage *admissionLookupGuardPG) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return nil, errors.New("history tag enumeration is forbidden in indexed preparation")
}

func (storage *admissionLookupGuardPG) ListGitPublicationWitnesses(ctx context.Context, repo domain.ContentHash, query domain.GitPublicationWitnessQuery) ([]domain.HistoryEvent, error) {
	events, err := storage.PostgresStore.ListGitPublicationWitnesses(ctx, repo, query)
	storage.witnessRows += len(events)
	return events, err
}

func (storage *admissionLookupGuardPG) FindGitForeignHeadOwner(ctx context.Context, repo domain.ContentHash, head, branch string) (domain.HistoryEvent, error) {
	storage.ownerReads++
	return storage.PostgresStore.FindGitForeignHeadOwner(ctx, repo, head, branch)
}

func TestPGGitAdmissionLookupFrozenEvidenceSurvivesLowerIDs(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "pinned")
	fixture.observeIntegration(t, "main", true)
	ctx := systemTestContext()
	publication := fixture.publication
	if err := fixture.storage.WithinRepository(ctx, fixture.basis.repo.ID, func(write context.Context) error {
		for index := range 4352 {
			event := domain.HistoryEvent{ID: fmt.Sprintf("%032x", index+1), RepoID: publication.RepoID, Kind: "position", BranchID: publication.BranchID,
				Branch: publication.Branch, LocalBranch: publication.LocalBranch, WorktreeID: publication.WorktreeID, Source: publication.Target,
				Target: publication.Target, GitAfter: publication.GitAfter, CreatedAt: time.Unix(100, 0).UTC()}
			if index >= 256 {
				event.GitAfter, event.BranchID, event.Branch = strings.Repeat("e", 40), "unrelated", "unrelated"
			}
			if err := fixture.storage.ApplyHistoryEvent(write, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	full, err := fixture.scans.core.readGitAdmissionSnapshot(ctx, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := full.index.ObservationIDs(publication.ID)
	if len(ordinary) <= domain.MaxGitAdmissionWitnesses || slices.Contains(ordinary[:domain.MaxGitAdmissionWitnesses], fixture.admission.OrdinaryObservationIDs[0]) {
		t.Fatal("fixture does not put original witness outside new selection window")
	}
	guard := &admissionLookupGuardPG{gitContributionApplyPGStore: fixture.storage}
	fixture.scans.core.meta = guard
	started := time.Now()
	prepared, authorization := fixture.prepare(t)
	elapsed := time.Since(started)
	if !prepared.snapshot.targeted || prepared.snapshot.generation == nil || guard.witnessRows != len(fixture.admission.OrdinaryObservationIDs)+len(fixture.admission.CarryObservationIDs) || guard.ownerReads != 1 {
		t.Fatal("historical preparation did not read exact bounded evidence", guard.witnessRows, guard.ownerReads)
	}
	if len(prepared.snapshot.history) > 5 || prepared.publication != publication {
		t.Fatal("unrelated history leaked or original publication changed", len(prepared.snapshot.history))
	}
	fullPublication, err := full.admissionPublication(fixture.admission, fixture.head)
	if err != nil || fullPublication != prepared.publication {
		t.Fatal("indexed preparation differs from authoritative full history", err)
	}
	fullBytes, _ := json.Marshal(gitAdmissionEvidenceEvents(full.history))
	selectedBytes, _ := json.Marshal(gitAdmissionEvidenceEvents(prepared.snapshot.history))
	t.Logf("full=%d events/%d bytes selected=%d events/%d bytes prepare=%s", len(full.history), len(fullBytes), len(prepared.snapshot.history), len(selectedBytes), elapsed)
	before := fixture.state(t, ctx)
	if _, err := fixture.scans.applyGitContribution(ctx, prepared, authorization); err != nil {
		t.Fatal(err)
	}
	assertGitContributionAppliedOnce(t, before, fixture.state(t, ctx), prepared)
}

func TestPGGitAdmissionLookupReconciliationUsesBoundedEvidence(t *testing.T) {
	scans, fixture, storage, _, _ := admissionBatchReconcileFixture(t, 1)
	guard := &admissionLookupGuardPG{gitContributionApplyPGStore: &gitContributionApplyPGStore{admissionBatchPG: storage}}
	fixture.worker.core.meta = guard
	if err := scans.Reconcile(systemTestContext()); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(systemTestContext(), fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
	if err != nil || len(admissions) != 1 || guard.witnessRows != 2 || guard.ownerReads != 1 {
		t.Fatal("bounded reconciliation failed", admissions, guard.witnessRows, guard.ownerReads, err)
	}
}

func TestPGGitAdmissionLookupHydrationPreservesPreReadSnapshot(t *testing.T) {
	fixture := newAdmissionPGFixture(t)
	ctx := systemTestContext()
	carryHead := strings.Repeat("e", 40)
	publication := fixture.frozen.snapshot.history[fixture.prepared.PublicationID]
	carry := publication
	carry.ID, carry.Kind, carry.GitAfter, carry.MemoryHash, carry.MemoryPinned = strings.Repeat("7", 32), "position", carryHead, "", false
	if err := fixture.storage.ApplyHistoryEvent(ctx, carry); err != nil {
		t.Fatal(err)
	}
	frozen, err := fixture.worker.core.readGitAdmissionEvidence(ctx, fixture.repo.ID, []gitAdmissionLookup{{publicationID: publication.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frozen.index.CarryObservationIDs(publication.ID, carryHead); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("fixture included carry before provider read", err)
	}
	observations := append([]domain.GitRefObservation{}, fixture.observations...)
	observations[0].After = carryHead
	observations[0] = observations[0].WithID()
	receipt, err := domain.NewGitHeadReceipt(fixture.claim, observations, false)
	if err != nil {
		t.Fatal(err)
	}
	before := gitAdmissionEvidenceEvents(frozen.history)
	if err := fixture.storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		hydrated, err := fixture.worker.core.hydrateGitAdmissionEvidence(read, frozen, []domain.HistoryEvent{publication}, *receipt)
		if err != nil {
			return err
		}
		ids, err := hydrated.index.CarryObservationIDs(publication.ID, carryHead)
		if err != nil || !slices.Equal(ids, []string{carry.ID}) || *hydrated.generation != *frozen.generation {
			t.Fatal("same-generation carry lookup failed", ids, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(before) != len(frozen.history) {
		t.Fatal("hydration mutated frozen history")
	}
	for _, event := range before {
		if !reflect.DeepEqual(event, frozen.history[event.ID]) {
			t.Fatal("hydration changed original evidence")
		}
	}
	if _, err := frozen.index.CarryObservationIDs(publication.ID, carryHead); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("hydration mutated frozen index", err)
	}
}
