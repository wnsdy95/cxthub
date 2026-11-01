package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingProjectionStatusReadStore struct {
	*pendingProjectionReadStore
	statusReads int
	statusErr   error
}

func (storage *pendingProjectionStatusReadStore) ReadPendingProjectionStatus(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error) {
	storage.statusReads++
	result := storage.result
	result.Parts = nil
	return result, storage.statusErr
}

func TestRepositoryChangeStateDoesNotReadProjectionPayload(t *testing.T) {
	service, base, repo, _ := newPendingProjectionRead(t)
	storage := &pendingProjectionStatusReadStore{pendingProjectionReadStore: base}
	service.meta = storage
	ctx := systemTestContext()
	first, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || first.PendingProjection == nil || first.PendingProjection.State != "ready" {
		t.Fatal(first, err)
	}
	body, err := service.GetPendingState(ctx, repo)
	if err != nil || body.ReadGeneration.Generation != first.PendingProjection.Generation || body.ReadGeneration.SourceEpoch != first.PendingProjection.SourceEpoch {
		t.Fatal("notice and payload generations differ", first, body, err)
	}
	base.result.Ready = false
	base.result.Source.Sequence++
	second, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || first.Equal(second) || second.RepositoryRevision != first.RepositoryRevision || second.PendingProjection.State != "catching_up" {
		t.Fatal("source-only change hidden by unchanged counters", first, second, err)
	}
	base.result.NeedsAttention = true
	terminal, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || terminal.PendingProjection.State != "needs_attention" || terminal.Equal(second) {
		t.Fatal(terminal, err)
	}
	if storage.statusReads != 3 || base.reads != 1 || base.snapshotLists != 0 || base.evidenceReads != 0 {
		t.Fatal("notice read body or graph", storage.statusReads, base.reads, base.snapshotLists, base.evidenceReads)
	}
	storage.statusErr = domain.ErrForbidden
	if _, err := service.RepositoryChangeState(ctx, repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("notice suppressed status read error", err)
	}
	service.pendingProjectionRepos = nil
	legacy, err := service.RepositoryChangeState(ctx, repo)
	if err != nil || legacy.PendingProjection != nil || legacy.RepositoryRevision != first.RepositoryRevision || storage.statusReads != 4 {
		t.Fatal("disabled rollout read projection state", legacy, err)
	}
}
