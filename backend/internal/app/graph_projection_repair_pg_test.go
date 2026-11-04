//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func assertGraphBundleRepairPG(test *testing.T, storage *store.PostgresStore, connection *pgxpool.Pool, repo domain.ContentHash, dsn string) {
	test.Helper()
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	reader := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := reader.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	before, err := reader.GetRepositoryView(ctx, repo)
	if err != nil || before.ReadGeneration == nil {
		test.Fatal("missing repair baseline", err)
	}
	sourceBefore, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	candidateBefore, err := storage.ReadGraphProjection(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	changed, err := connection.Exec(ctx, `UPDATE graph_serving_parts SET image=jsonb_set(image,'{broken}','true')
		WHERE repo_id=$1 AND kind='header' AND entity_key='view'`, string(repo))
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("fixture did not corrupt exactly one ready header", err)
	}
	_, err = reader.GetRepositoryView(ctx, repo)
	var failure *domain.GraphProjectionCorruption
	if !errors.As(err, &failure) || !failure.Serving || failure.ServingFence <= 0 || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("corrupt read was not fenced", err)
	}
	status, err := reader.RepositoryChangeState(ctx, repo)
	if err != nil || status.GraphProjection == nil || status.GraphProjection.Complete || status.GraphProjection.State != "catching_up" {
		test.Fatal("repair was not durably queued after read transaction", status, err)
	}
	if _, err := reader.GetRepositoryView(ctx, repo); !errors.Is(err, domain.ErrGraphProjectionNotReady) {
		test.Fatal("read fell back or retried damaged generation", err)
	}
	candidateAfter, err := storage.ReadGraphProjection(ctx, repo)
	if err != nil || !reflect.DeepEqual(candidateAfter, candidateBefore) {
		test.Fatal("serving repair changed candidate", err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	worker := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		if err := worker.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal(err)
		}
		published, err := peer.ReadGraphProjectionStatus(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if published.Ready {
			qualifyGraphBundlePG(test, peer, connection, repo)
		}
		after, err := reader.GetRepositoryView(ctx, repo)
		if errors.Is(err, domain.ErrGraphProjectionNotReady) {
			continue
		}
		if err != nil || after.ReadGeneration == nil || !after.ReadGeneration.Current || after.ReadGeneration.Generation == before.ReadGeneration.Generation {
			test.Fatal("restart did not publish a new complete generation", err)
		}
		after.ReadGeneration, after.Graph.ReadGeneration = nil, nil
		before.ReadGeneration, before.Graph.ReadGeneration = nil, nil
		if !reflect.DeepEqual(after, before) {
			test.Fatal("repair changed graph/PR/history metadata")
		}
		sourceAfter, err := peer.ReadRepositoryGraphSourceGeneration(ctx, repo)
		if err != nil || sourceAfter != sourceBefore {
			test.Fatal("repair rewrote source", sourceBefore, sourceAfter, err)
		}
		return
	}
	test.Fatal("restarted worker did not complete repair")
}
