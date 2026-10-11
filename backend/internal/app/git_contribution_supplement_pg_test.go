//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type supplementPGStore struct {
	*gitContributionApplyPGStore
	clock         time.Time
	rejectFinish  bool
	missingMemory domain.ContentHash
	corruptMemory domain.ContentHash
}

func (storage *supplementPGStore) ClaimGitContributionSupplement(ctx context.Context, repo domain.ContentHash, _ time.Time, lease time.Duration) (domain.GitContributionSupplement, error) {
	storage.clock = storage.clock.Add(10 * time.Minute)
	return storage.PostgresStore.ClaimGitContributionSupplement(ctx, repo, storage.clock, lease)
}

func (storage *supplementPGStore) FinishGitContributionSupplement(ctx context.Context, job domain.GitContributionSupplement) error {
	if err := storage.PostgresStore.FinishGitContributionSupplement(ctx, job); err != nil {
		return err
	}
	if storage.rejectFinish {
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *supplementPGStore) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if hash == storage.missingMemory {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	if hash == storage.corruptMemory {
		return domain.MemoryDigest{Summary: "corrupt supplement"}, nil
	}
	return storage.PostgresStore.GetMemory(ctx, repo, hash)
}

func newSupplementPGFixture(t *testing.T) (*gitContributionQueryPGFixture, *supplementPGStore) {
	t.Helper()
	fixture := newGitContributionQueryPGFixture(t)
	storage := &supplementPGStore{gitContributionApplyPGStore: fixture.storage, clock: time.Now().UTC()}
	fixture.scans.core.meta = storage
	fixture.scans.core.blobs = storage
	return fixture, storage
}

func publishGitSupplement(t *testing.T, fixture *gitContributionQueryPGFixture) domain.HistoryEvent {
	t.Helper()
	publication := fixture.publication
	publication.ID = strings.Repeat("f", 32)
	digest := domain.MemoryDigest{SnapshotID: publication.Target, PreviousMemoryHash: publication.MemoryHash, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: publication.Target, Claims: []domain.MemoryClaim{{Kind: "code", Text: "revised published memory", Code: &domain.MemoryCodeScope{Commit: publication.GitAfter, Paths: []string{"file"}}}}}}}
	hash, err := fixture.storage.PutMemory(systemTestContext(), fixture.basis.repo.ID, digest)
	if err != nil {
		t.Fatal(err)
	}
	publication.MemoryHash = hash
	if err := fixture.scans.core.RecordHistory(systemTestContext(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.scans.discoverGitAssociations(systemTestContext(), fixture.basis.repo); err != nil {
		t.Fatal(err)
	}
	return publication
}

func assertGitSupplementEffect(t *testing.T, fixture *gitContributionQueryPGFixture, before gitContributionApplyPGState, publication domain.HistoryEvent) {
	t.Helper()
	after := fixture.state(t, systemTestContext())
	if !reflect.DeepEqual(after.refs, before.refs) || !reflect.DeepEqual(after.parents, before.parents) || !reflect.DeepEqual(after.proofs, before.proofs) || !reflect.DeepEqual(after.contributions, before.contributions) {
		t.Fatal("memory supplement changed integration or context position")
	}
	wantRevision := before.revision
	wantRevision.Evidence++
	if after.revision != wantRevision || len(after.memories) != len(before.memories)+1 {
		t.Fatal("pin and one evidence revision not atomic", after.revision, len(after.memories))
	}
	want := (domain.GitContributionMemory{RepoID: fixture.basis.repo.ID, ContributionID: fixture.proof.Contribution.ID, PublicationID: publication.ID, MemoryHash: publication.MemoryHash, MemorySource: publication.MemorySource, MemoryPinned: true}).WithID()
	found := false
	for _, pin := range after.memories {
		found = found || pin == want
	}
	if !found {
		t.Fatal("exact new publication pin absent")
	}
	memory, err := fixture.scans.core.QueryBranchMemory(systemTestContext(), fixture.basis.repo.ID, "main", fixture.main.Target, "")
	if err != nil {
		t.Fatal(err)
	}
	items, err := effectiveMemoryItems(memory.Digest, "claims")
	if err != nil || len(items) != 1 || items[0].Text != "revised published memory" {
		t.Fatalf("new version did not supersede original without mutable fallback: %+v %v", items, err)
	}
}

func TestPGGitContributionSupplementAutomaticOriginalVersionAndReplay(t *testing.T) {
	fixture, _ := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	before := fixture.state(t, systemTestContext())
	if err := fixture.scans.ProcessContributions(systemTestContext(), 8); err != nil {
		t.Fatal(err)
	}
	assertGitSupplementEffect(t, fixture, before, publication)
	after := fixture.state(t, systemTestContext())
	if _, err := fixture.scans.discoverGitAssociations(systemTestContext(), fixture.basis.repo); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scans.ProcessContributions(systemTestContext(), 8); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, fixture.state(t, systemTestContext())) {
		t.Fatal("replay duplicated memory effect")
	}
}

func TestPGGitContributionSupplementWaitsForMemory(t *testing.T) {
	fixture, storage := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	before := fixture.state(t, systemTestContext())
	storage.missingMemory = publication.MemoryHash
	view := fixture.query(t)
	fixture.assertMerge(t, view.Inclusion, "included", true)
	memory, err := fixture.scans.core.QueryBranchMemory(systemTestContext(), fixture.basis.repo.ID, "main", fixture.main.Target, "")
	if !errors.Is(err, domain.ErrConflict) || memory.Digest.Summary != "" || memory.Digest.HasMemoryClaims() {
		t.Fatalf("pending supplement was replaced by stale or mutable memory: %+v %v", memory, err)
	}
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixture.state(t, systemTestContext())) {
		t.Fatal("missing memory changed semantic state")
	}
	storage.missingMemory = ""
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	assertGitSupplementEffect(t, fixture, before, publication)
}

func TestPGGitContributionSupplementRejectsCorruptMemoryWithoutEffect(t *testing.T) {
	fixture, storage := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	before := fixture.state(t, systemTestContext())
	storage.corruptMemory = publication.MemoryHash
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("corrupt memory was accepted: %v", err)
	}
	if !reflect.DeepEqual(before, fixture.state(t, systemTestContext())) {
		t.Fatal("corrupt memory changed semantic state")
	}
	if _, err := storage.ClaimGitContributionSupplement(systemTestContext(), fixture.basis.repo.ID, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("attention work remained automatically claimable: %v", err)
	}
}

func TestPGGitContributionSupplementCompletionFailureRollsBackPin(t *testing.T) {
	fixture, storage := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	before := fixture.state(t, systemTestContext())
	storage.rejectFinish = true
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("expected injected failure", err)
	}
	if !reflect.DeepEqual(before, fixture.state(t, systemTestContext())) {
		t.Fatal("completion failure left pin or revision")
	}
	storage.rejectFinish = false
	if err := fixture.scans.ProcessContributions(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	assertGitSupplementEffect(t, fixture, before, publication)
}
