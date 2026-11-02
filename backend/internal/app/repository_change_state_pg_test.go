//go:build postgres

package app

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingNoticeMutationStore struct {
	*store.PostgresStore
	afterRevision func()
}

func (storage *pendingNoticeMutationStore) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	revision, err := storage.PostgresStore.RepositoryRevision(ctx, repo)
	if mutate := storage.afterRevision; err == nil && mutate != nil {
		storage.afterRevision = nil
		mutate()
	}
	return revision, err
}

func TestPGRepositoryChangeStateTracksSourceWithoutCounterChange(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	if err := service.ConfigurePendingProjections(ctx, []domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storage.ConfigurePendingProjection(systemTestContext(), repo, false); err != nil {
			t.Error(err)
		}
	})
	catchUp := func() domain.RepositoryChangeState {
		t.Helper()
		for range 10 {
			if err := service.ProcessPendingProjections(ctx, 16); err != nil {
				t.Fatal(err)
			}
			state, err := service.RepositoryChangeState(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if state.PendingProjection.State == "ready" {
				body, err := service.GetPendingState(ctx, repo)
				if err != nil || body.ReadGeneration.Generation != state.PendingProjection.Generation || body.ReadGeneration.SourceEpoch != state.PendingProjection.SourceEpoch {
					t.Fatal("notice does not name the body generation", state, body, err)
				}
				return state
			}
		}
		t.Fatal("worker did not catch up")
		return domain.RepositoryChangeState{}
	}
	first := catchUp()
	spy := &pendingNoticeMutationStore{PostgresStore: storage}
	service.meta = spy
	spy.afterRevision = func() {
		branch := "notice-source-only"
		if err := storage.UpdateRepoConfig(ctx, repo, &branch, nil); err != nil {
			t.Fatal(err)
		}
	}
	concurrent, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || !concurrent.Equal(first) {
		t.Fatal("notice mixed counters and source from different snapshots", first, concurrent, err)
	}
	second, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || first.Equal(second) || first.RepositoryRevision != second.RepositoryRevision || second.PendingProjection.State != "catching_up" || second.PendingProjection.SourceEpoch == first.PendingProjection.SourceEpoch {
		t.Fatal("source-only update disappeared behind counters", first.RepositoryRevision, first.PendingProjection, second.RepositoryRevision, second.PendingProjection, err)
	}
	third := catchUp()
	if second.Equal(third) || second.PendingProjection.SourceEpoch != third.PendingProjection.SourceEpoch || first.PendingProjection.Generation == third.PendingProjection.Generation {
		t.Fatal("worker-only readiness did not produce notice", first, second, third)
	}
	body, err := service.GetPendingState(ctx, repo)
	if err != nil || body.DefaultBranch != "notice-source-only" {
		t.Fatal("ready body omitted source-only mutation", body, err)
	}
	service.pendingProjectionRepos = nil
	legacy, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || legacy.PendingProjection != nil || legacy.RepositoryRevision != first.RepositoryRevision {
		t.Fatal("reader rollback changed original counters", legacy, err)
	}
}
