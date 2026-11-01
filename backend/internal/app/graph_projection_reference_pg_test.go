//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphReferenceCapturePG struct {
	*store.PostgresStore
	afterPage func(context.Context) error
}

func (storage *graphReferenceCapturePG) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	page, err := storage.PostgresStore.RepositoryGraphSourceChanges(ctx, repo, request)
	if err == nil && storage.afterPage != nil {
		hook := storage.afterPage
		storage.afterPage = nil
		err = hook(ctx)
	}
	return page, err
}

func TestPGGraphProjectionReferencePinsFirstPageAndSurvivesRestart(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	wrapped := &graphReferenceCapturePG{PostgresStore: storage}
	worker := NewService(wrapped, wrapped, auth.NewTeamTokenAuth(), gitengine.NewEngine(wrapped), wrapped)
	before, err := repositoryRead(ctx, worker, func(bound context.Context) (domain.ContentHash, error) {
		return worker.graphQualificationReference(bound, repo)
	})
	if err != nil {
		test.Fatal(err)
	}
	checkpoint, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	wrapped.afterPage = func(bound context.Context) error {
		if !storage.InReadOnlyTransaction(bound) {
			test.Fatal("source page did not share the reference snapshot")
		}
		_, err := connection.Exec(ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(fixture.main.Target), "committed between page and reference")
		return err
	}
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessGraphProjections(ctx, 1); err != nil || wrapped.afterPage != nil {
		test.Fatal("first-page capture did not run", err)
	}
	after, err := repositoryRead(ctx, worker, func(bound context.Context) (domain.ContentHash, error) {
		return worker.graphQualificationReference(bound, repo)
	})
	if err != nil || after == before {
		test.Fatal("fixture did not distinguish independent source generations", err)
	}
	peer, err := store.NewPostgresStore(ctx, fixture.basis.dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	if err := restarted.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	for range 20 {
		candidate, err := peer.ReadGraphProjection(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if candidate.Complete {
			if candidate.Ready || candidate.Applied != checkpoint {
				test.Fatal("capture followed later source instead of frozen first page", candidate.Applied)
			}
			if err := restarted.ProcessGraphQualifications(ctx, 1); err != nil {
				test.Fatal(err)
			}
			served, err := peer.ReadQualifiedGraphProjection(ctx, repo)
			if err != nil || !served.Complete || served.Ready || served.Qualification == nil || !served.Qualification.Matched() || served.Qualification.Expected != before {
				test.Fatal("restart lost frozen independent reference", served, err)
			}
			return
		}
		if err := restarted.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal(err)
		}
	}
	test.Fatal("frozen candidate did not complete")
}

func TestPGGraphServingRepairUsesReferenceWithoutQuietSource(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	publishGraphCandidatePG(test, worker, storage, repo)
	if err := worker.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessGraphQualifications(ctx, 1); err != nil {
		test.Fatal(err)
	}
	initial, err := storage.ReadQualifiedGraphProjection(ctx, repo)
	if err != nil || !initial.Complete {
		test.Fatal("no initial serving generation", err)
	}
	if _, err := connection.Exec(ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(fixture.main.Target), "write after serving before repair"); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE graph_serving_parts SET image='{}' WHERE repo_id=$1 AND kind='header'`, string(repo)); err != nil {
		test.Fatal(err)
	}
	if err := worker.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if _, err := worker.GetRepositoryView(ctx, repo); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("serving corruption was not rejected and queued for repair", err)
	}
	peer, err := store.NewPostgresStore(ctx, fixture.basis.dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	if err := restarted.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if err := restarted.ProcessGraphQualifications(ctx, 1); err != nil {
		test.Fatal(err)
	}
	repaired, err := peer.ReadQualifiedGraphProjection(ctx, repo)
	if err != nil || !repaired.Complete || repaired.Ready || repaired.Applied != initial.Applied || repaired.ServingFence <= initial.ServingFence || repaired.Qualification == nil || !repaired.Qualification.Matched() {
		test.Fatal("old candidate could not repair serving while source remained ahead", repaired, err)
	}
	if err := restarted.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	view, err := restarted.GetRepositoryView(ctx, repo)
	if err != nil || view.ReadGeneration == nil || view.ReadGeneration.Current || !view.ReadGeneration.Complete {
		test.Fatal("repaired graph not honestly served as a complete prior generation", view.ReadGeneration, err)
	}
}
