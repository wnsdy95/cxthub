package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contributionQueryCountingStore struct {
	*store.FSStore
	histories int
	prs       int
	gits      int
	gitError  error
}

func (storage *contributionQueryCountingStore) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	storage.histories++
	return storage.FSStore.ListHistoryEvents(ctx, repo)
}

func (storage *contributionQueryCountingStore) ListPRContributions(ctx context.Context, repo domain.ContentHash) ([]domain.PRContribution, error) {
	storage.prs++
	return storage.FSStore.ListPRContributions(ctx, repo)
}

func (storage *contributionQueryCountingStore) ListGitContributions(ctx context.Context, repo domain.ContentHash) ([]domain.GitContribution, error) {
	storage.gits++
	if storage.gitError != nil {
		return nil, storage.gitError
	}
	return storage.FSStore.ListGitContributions(ctx, repo)
}

func TestContributionQueryCachesEmptyLedgerOnlyForSameInput(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	core := fixture.worker.core
	counted := &contributionQueryCountingStore{FSStore: fixture.storage}
	core.meta = counted
	history, err := core.ListHistory(ctx, fixture.repo.ID)
	if err != nil || len(history) == 0 {
		t.Fatalf("history fixture: %v", err)
	}
	evidence, err := core.newCodeEvidence(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 32 {
		rows, err := core.contextContributionsForEvidence(ctx, fixture.repo.ID, history, evidence)
		if err != nil || len(rows) != 0 {
			t.Fatalf("empty query = %+v %v", rows, err)
		}
	}
	if counted.prs != 1 || counted.gits != 1 {
		t.Fatalf("empty ledgers reread: PR=%d Git=%d", counted.prs, counted.gits)
	}
	replaced := slices.Clone(history)
	replaced[0].BranchID = "replacement-identity"
	if _, err := core.contextContributionsForEvidence(ctx, fixture.repo.ID, replaced, evidence); err != nil {
		t.Fatal(err)
	}
	if counted.prs != 2 || counted.gits != 2 {
		t.Fatal("equal-length different history reused cached verification")
	}
	if _, err := core.contextContributionsForEvidence(ctx, hh("foreign-query-repo"), history, evidence); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("cross-repository cache use = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := core.contextContributionsForEvidence(canceled, fixture.repo.ID, replaced, evidence); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached query ignored cancellation: %v", err)
	}
	evidence, _ = core.newCodeEvidence(ctx, fixture.repo.ID)
	counted.gitError = errors.New("temporary ledger read failure")
	if _, err := core.gitContributionsForEvidence(ctx, fixture.repo.ID, history, evidence); !errors.Is(err, counted.gitError) {
		t.Fatal("expected storage error", err)
	}
	counted.gitError = nil
	if _, err := core.gitContributionsForEvidence(ctx, fixture.repo.ID, history, evidence); err != nil || counted.gits != 4 {
		t.Fatalf("failed read cached: reads=%d error=%v", counted.gits, err)
	}
}

func TestBranchMemoryQueryReusesPinnedHistoryAndVerification(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	selected := fixture.scanMemoryCode(t)
	digest := fixture.digest(t, "", "exact original pinned claim")
	fixture.publish(t, digest, true)
	fixture.reconcile(t)
	counted := &contributionQueryCountingStore{FSStore: fixture.storage}
	fixture.worker.core.meta = counted
	projection, err := fixture.worker.core.QueryBranchMemory(systemTestContext(), fixture.repo.ID, "main", fixture.main.Target, selected)
	if err != nil || !projection.Found || projection.Inclusion == nil {
		t.Fatalf("query failed: %+v %v", projection, err)
	}
	if counted.histories != 1 || counted.prs != 1 || counted.gits != 1 {
		t.Fatalf("query repeated pinned inputs: history=%d PR=%d Git=%d", counted.histories, counted.prs, counted.gits)
	}
	items, err := effectiveMemoryItems(projection.Digest, "claims")
	if err != nil || len(items) != 1 || items[0].Text != "exact original pinned claim" {
		t.Fatalf("pin lost: %+v %v", items, err)
	}
}
