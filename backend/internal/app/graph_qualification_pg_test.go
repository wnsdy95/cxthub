//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func qualifyGraphBundlePG(test *testing.T, storage *store.PostgresStore, connection *pgxpool.Pool, repo domain.ContentHash) {
	test.Helper()
	ctx, cancel := context.WithTimeout(systemTestContext(), 45*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE graph_qualification SET next_attempt=clock_timestamp() WHERE repo_id=$1`, string(repo)); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessGraphQualifications(ctx, 1); err != nil {
		test.Fatal(err)
	}
	projection, err := storage.ReadGraphProjectionStatus(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	result, err := storage.ReadGraphQualification(ctx, projection.QualificationKey())
	if err != nil || !result.Matched() {
		test.Fatal("graph failed direct comparison", result, err)
	}
}

func TestPGGraphQualificationGatesServingAndSurvivesRestart(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		if err := worker.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal(err)
		}
		state, err := storage.ReadGraphProjectionStatus(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if state.Ready {
			break
		}
	}
	if err := worker.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if _, err := worker.GetRepositoryView(ctx, repo); !errors.Is(err, domain.ErrGraphProjectionNotReady) {
		test.Fatal("unqualified view served", err)
	}
	notice, err := worker.RepositoryChangeState(ctx, repo)
	if err != nil || notice.GraphProjection == nil || notice.GraphProjection.Complete || notice.GraphProjection.State != "catching_up" {
		test.Fatal("unqualified ready notice", notice, err)
	}
	source, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	qualifyGraphBundlePG(test, storage, connection, repo)
	peer, err := store.NewPostgresStore(ctx, fixture.basis.dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	if err := restarted.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	view, err := restarted.GetRepositoryView(ctx, repo)
	if err != nil || view.ReadGeneration == nil || !view.ReadGeneration.Current {
		test.Fatal("qualified view lost on restart", err)
	}
	after, err := peer.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil || after != source {
		test.Fatal("qualification changed source", err)
	}
	if err := peer.ConfigureGraphQualification(ctx, repo, false); err != nil {
		test.Fatal(err)
	}
	if _, err := restarted.GetRepositoryView(ctx, repo); !errors.Is(err, domain.ErrGraphProjectionNotReady) {
		test.Fatal("disabled gate kept serving", err)
	}
	if err := restarted.ConfigureGraphReads(nil); err != nil {
		test.Fatal(err)
	}
	if _, err := restarted.GetRepositoryView(ctx, repo); err != nil {
		test.Fatal("reader rollback did not restore direct read", err)
	}
}

func TestPGGraphQualificationPersistsMismatchWithoutRepairingSource(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	var projection domain.GraphProjectionRead
	for attempt := 0; attempt < 20; attempt++ {
		if err := worker.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal(err)
		}
		var err error
		projection, err = storage.ReadGraphProjection(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if projection.Ready {
			break
		}
	}
	view, err := domain.DecodeGraphViewProjection(ctx, repo, projection.Parts)
	if err != nil || len(view.Snapshots) == 0 {
		test.Fatal("missing fixture", err)
	}
	view.Snapshots[0].Message = "valid encoding but wrong business output"
	parts, err := domain.EncodeGraphViewProjection(ctx, repo, view)
	if err != nil {
		test.Fatal(err)
	}
	for _, part := range parts {
		changed, err := connection.Exec(ctx, `UPDATE graph_projection_parts SET image=$5::jsonb
			WHERE repo_id=$1 AND dataset=$2 AND kind=$3 AND entity_key=$4`, string(repo), projection.Dataset, part.Kind, part.Key, string(part.Value))
		if err != nil || changed.RowsAffected() != 1 {
			test.Fatal("failed to alter derived fixture", part.Key, err)
		}
	}
	if err := worker.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessGraphQualifications(ctx, 1); err != nil {
		test.Fatal(err)
	}
	result, err := storage.ReadGraphQualification(ctx, projection.QualificationKey())
	if err != nil || result.Validate() != nil || result.Matched() {
		test.Fatal("semantic mismatch not recorded", result, err)
	}
	if err := worker.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if _, err := worker.GetRepositoryView(ctx, repo); !errors.Is(err, domain.ErrGraphProjectionFailed) {
		test.Fatal("mismatched view served", err)
	}
	notice, err := worker.RepositoryChangeState(ctx, repo)
	if err != nil || notice.GraphProjection == nil || notice.GraphProjection.Complete || notice.GraphProjection.State != "needs_attention" {
		test.Fatal("mismatch not visible", notice, err)
	}
	after, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil || after != projection.Applied {
		test.Fatal("comparison repaired source to match", err)
	}
	var repairs int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM graph_projection_repairs WHERE repo_id=$1`, string(repo)).Scan(&repairs); err != nil || repairs != 0 {
		test.Fatal("semantic mismatch scheduled corruption repair", repairs, err)
	}
}

func TestPGGraphQualificationStartupAdvertisesWorkerIdentitySupport(test *testing.T) {
	service, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := storage.RequireDocumentIdentity(systemTestContext(), repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal("startup lacks binary worker identity", err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		if err := service.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal(err)
		}
		if err := service.ProcessGraphQualifications(ctx, 1); err != nil {
			test.Fatal(err)
		}
		workerContext := service.DocumentIdentityWorkerContext(systemTestContext())
		state, err := service.RepositoryChangeState(workerContext, repo)
		if err != nil {
			test.Fatal(err)
		}
		if err := service.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
			test.Fatal(err)
		}
		view, err := service.GetRepositoryView(workerContext, repo)
		if errors.Is(err, domain.ErrGraphProjectionNotReady) {
			continue
		}
		if err != nil || view.ReadGeneration == nil {
			test.Fatal("root-v1 shadow could not serve qualified output", state, err)
		}
		if _, err := service.GetRepositoryView(systemTestContext(), repo); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
			test.Fatal("worker compatibility escaped into unsupported reader", err)
		}
		return
	}
	test.Fatal("root-v1 qualification did not finish")
}
