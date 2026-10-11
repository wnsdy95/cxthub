//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historicalContributionReadGuard struct {
	*gitContributionApplyPGStore
}

func (storage *historicalContributionReadGuard) ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error) {
	return nil, errors.New("historical query must reuse caller history")
}

func (storage *historicalContributionReadGuard) ListGitAdmissionRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return nil, errors.New("historical proof must not read current admission refs")
}

func (storage *historicalContributionReadGuard) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return nil, errors.New("historical proof must not read current refs")
}

func (storage *historicalContributionReadGuard) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	return domain.RepositoryRevision{}, errors.New("historical proof must not read admission revisions")
}

func TestPGGitContributionQueryUsesOnlyHistoricalProofInputs(t *testing.T) {
	fixture := newGitContributionQueryPGFixture(t)
	core, repo := fixture.basis.worker.core, fixture.basis.repo.ID
	core.meta = &historicalContributionReadGuard{gitContributionApplyPGStore: fixture.storage}
	_, err := repositoryReadForRepo(systemTestContext(), core, repo, func(ctx context.Context) (bool, error) {
		history, err := fixture.storage.ListHistoryEvents(ctx, repo)
		if err != nil {
			return false, err
		}
		evidence, err := core.newCodeEvidence(ctx, repo)
		if err != nil {
			return false, err
		}
		rows, err := core.gitContributionsForEvidence(ctx, repo, history, evidence)
		if err != nil {
			return false, err
		}
		if len(rows) != 1 || !rows[0].valid || rows[0].record != fixture.proof.Contribution || rows[0].publications[fixture.publication.ID] != fixture.publication {
			t.Fatalf("historical proof changed: %+v", rows)
		}
		snapshot, err := core.readGitAdmissionHistory(ctx, repo, history)
		if err != nil {
			return false, err
		}
		if snapshot.generation != nil || len(snapshot.refs) != 0 || len(snapshot.owners) != 0 || snapshot.revision != (domain.RepositoryRevision{}) {
			t.Fatal("read-only historical inputs acquired write authority")
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
