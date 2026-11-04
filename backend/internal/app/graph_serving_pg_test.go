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

func publishGraphCandidatePG(test *testing.T, worker *Service, storage *store.PostgresStore, repo domain.ContentHash) domain.GraphProjectionRead {
	test.Helper()
	ctx, cancel := context.WithTimeout(systemTestContext(), 45*time.Second)
	defer cancel()
	for attempt := 0; attempt < 20; attempt++ {
		if err := worker.ProcessGraphProjections(ctx, 1); err != nil {
			test.Fatal(err)
		}
		projection, err := storage.ReadGraphProjection(ctx, repo)
		if err != nil {
			test.Fatal(err)
		}
		if projection.Ready {
			return projection
		}
	}
	test.Fatal("graph candidate did not reach current source")
	return domain.GraphProjectionRead{}
}

func alterGraphCandidatePG(test *testing.T, connection *pgxpool.Pool, projection domain.GraphProjectionRead) {
	test.Helper()
	ctx := systemTestContext()
	view, err := domain.DecodeGraphViewProjection(ctx, projection.Source.RepoID, projection.Parts)
	if err != nil || len(view.Snapshots) == 0 {
		test.Fatal("missing candidate fixture", err)
	}
	view.Snapshots[0].Message = "candidate with valid hashes but incorrect semantics"
	parts, err := domain.EncodeGraphViewProjection(ctx, projection.Source.RepoID, view)
	if err != nil {
		test.Fatal(err)
	}
	for _, part := range parts {
		changed, err := connection.Exec(ctx, `UPDATE graph_projection_parts SET image=$5::jsonb
			WHERE repo_id=$1 AND dataset=$2 AND kind=$3 AND entity_key=$4`, string(projection.Source.RepoID), projection.Dataset, part.Kind, part.Key, string(part.Value))
		if err != nil || changed.RowsAffected() != 1 {
			test.Fatal("failed to alter candidate fixture", part.Key, err)
		}
	}
}

func TestPGGraphServingRetainsQualifiedBundleAcrossPublicationAndMismatch(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	initial := publishGraphCandidatePG(test, worker, storage, repo)
	want, err := domain.DecodeGraphViewProjection(ctx, repo, initial.Parts)
	if err != nil {
		test.Fatal(err)
	}
	qualifyGraphBundlePG(test, storage, connection, repo)
	first, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || first.ServingFence <= 0 || !first.Ready {
		test.Fatal("missing first serving head", first, err)
	}
	if err := worker.SetSessionArchived(ctx, repo, fixture.admission.Capture, true); err != nil {
		test.Fatal(err)
	}
	newer := publishGraphCandidatePG(test, worker, storage, repo)
	if newer.Applied == initial.Applied {
		test.Fatal("fixture did not publish newer candidate")
	}
	assertGraphBundleServingPG(test, storage, repo, want, false)
	alterGraphCandidatePG(test, connection, newer)
	if err := worker.ConfigureGraphQualifications(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `UPDATE graph_qualification SET next_attempt=clock_timestamp() WHERE repo_id=$1`, string(repo)); err != nil {
		test.Fatal(err)
	}
	if err := worker.ProcessGraphQualifications(ctx, 1); err != nil {
		test.Fatal(err)
	}
	comparison, err := storage.ReadGraphQualification(ctx, newer.QualificationKey())
	if err != nil || comparison.Matched() {
		test.Fatal("candidate mismatch not persisted", comparison, err)
	}
	assertGraphBundleServingPG(test, storage, repo, want, false)
	retained, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || retained.ServingFence != first.ServingFence || retained.Qualification == nil || !retained.Qualification.Matched() ||
		retained.Qualification.GraphQualificationKey != initial.QualificationKey() {
		test.Fatal("newer mismatch replaced old proof", retained, err)
	}
	if err := worker.SetSessionArchived(ctx, repo, fixture.admission.Capture, false); err != nil {
		test.Fatal(err)
	}
	latest := publishGraphCandidatePG(test, worker, storage, repo)
	changed, err := connection.Exec(ctx, `UPDATE graph_projection_parts SET image=jsonb_set(image,'{broken}','true')
		WHERE repo_id=$1 AND dataset=$2 AND kind='header' AND entity_key='view'`, string(repo), latest.Dataset)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("failed to corrupt candidate header", err)
	}
	if _, err := connection.Exec(ctx, `UPDATE graph_qualification SET next_attempt=clock_timestamp() WHERE repo_id=$1`, string(repo)); err != nil {
		test.Fatal(err)
	}
	var failure *domain.GraphProjectionCorruption
	if err := worker.ProcessGraphQualifications(ctx, 1); !errors.As(err, &failure) || failure.Serving || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("shadow did not detect/repair candidate corruption", err)
	}
	assertGraphBundleServingPG(test, storage, repo, want, false)
	latest = publishGraphCandidatePG(test, worker, storage, repo)
	wantLatest, err := domain.DecodeGraphViewProjection(ctx, repo, latest.Parts)
	if err != nil {
		test.Fatal(err)
	}
	qualifyGraphBundlePG(test, storage, connection, repo)
	assertGraphBundleServingPG(test, storage, repo, wantLatest, true)
	current, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || current.ServingFence <= first.ServingFence || current.Applied != latest.Applied {
		test.Fatal("matching newer candidate did not promote", current, err)
	}
}

func TestPGGraphServingSnapshotRemainsCoherentAcrossPromotion(test *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo}); err != nil {
		test.Fatal(err)
	}
	publishGraphCandidatePG(test, worker, storage, repo)
	qualifyGraphBundlePG(test, storage, connection, repo)
	if err := worker.SetSessionArchived(ctx, repo, fixture.admission.Capture, true); err != nil {
		test.Fatal(err)
	}
	publishGraphCandidatePG(test, worker, storage, repo)
	var previousFence int64
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		before, err := storage.ReadQualifiedGraphProjection(bound, repo)
		if err != nil {
			return err
		}
		previousFence = before.ServingFence
		qualifyGraphBundlePG(test, storage, connection, repo)
		after, err := storage.ReadQualifiedGraphProjection(bound, repo)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return errors.New("read snapshot mixed serving generations across promotion")
		}
		notice, err := storage.ReadQualifiedGraphProjectionStatus(bound, repo)
		if err != nil {
			return err
		}
		after.Parts = nil
		if !reflect.DeepEqual(notice, after) {
			return errors.New("status read escaped serving snapshot")
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	after, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || after.ServingFence <= previousFence || !after.Ready {
		test.Fatal("new read did not observe completed promotion", after, err)
	}
}
