//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func metadataGenerationPGFixture(t *testing.T, storage *PostgresStore) (context.Context, domain.ContentHash) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	return ctx, repo
}

func metadataGenerationPGRead(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.RepositoryMetadataGeneration {
	t.Helper()
	generation, err := storage.ReadRepositoryMetadataGeneration(ctx, repo)
	if err != nil || generation.Validate() != nil || generation.RepoID != repo {
		t.Fatal("invalid generation", generation, err)
	}
	return generation
}

func TestPGRepositoryMetadataGenerationSnapshotsAndNoMutation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo := metadataGenerationPGFixture(t, storage)
	before := catalogReadStatePG(t, ctx, storage, repo)
	rows := catalogRowsPG(t, ctx, storage, repo, -1)
	revision, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	generation := metadataGenerationPGRead(t, ctx, storage, repo)
	if generation.Epoch != before.epoch || generation.Sequence != before.head {
		t.Fatal("generation did not match authoritative catalog", generation, before)
	}
	for range 2 {
		if got := metadataGenerationPGRead(t, ctx, peer, repo); got != generation {
			t.Fatal("outside read mutated generation", got, generation)
		}
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		if got := metadataGenerationPGRead(t, write, storage, repo); got != generation {
			t.Fatal("write read changed generation", got, generation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, peer, repo); got != before {
		t.Fatal("read changed catalog state", before, got)
	}
	if got := catalogRowsPG(t, ctx, peer, repo, -1); !reflect.DeepEqual(rows, got) {
		t.Fatal("read published catalog images", rows, got)
	}
	if got, err := peer.RepositoryRevision(ctx, repo); err != nil || got != revision {
		t.Fatal("read advanced public revision", got, revision, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if got := metadataGenerationPGRead(t, read, storage, repo); got != generation {
			t.Fatal("snapshot began at wrong generation", got, generation)
		}
		if err := peer.ApplyHistoryEvent(ctx, catalogHistoryPG(repo)); err != nil {
			return err
		}
		if got := metadataGenerationPGRead(t, ctx, peer, repo); got.Sequence <= generation.Sequence {
			t.Fatal("concurrent commit did not advance generation", got, generation)
		}
		if got := metadataGenerationPGRead(t, read, storage, repo); got != generation {
			t.Fatal("reader escaped owned snapshot", got, generation)
		}
		if history, err := storage.ListHistoryEvents(read, repo); err != nil || len(history) != 0 {
			t.Fatal("generation and metadata used different snapshots", history, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGRepositoryMetadataGenerationTracksStorageAndRawChanges(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo := metadataGenerationPGFixture(t, storage)
	revision, err := storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	previous := metadataGenerationPGRead(t, ctx, storage, repo)
	event := catalogHistoryPG(repo)
	for _, mutation := range []func() error{
		func() error { return storage.ApplyHistoryEvent(ctx, event) },
		func() error {
			_, err := peer.pool.Exec(ctx, `UPDATE context_history SET event=jsonb_set(event,'{branch}','"raw-name"'::jsonb) WHERE repo_id=$1 AND id=$2`, repo, event.ID)
			return err
		},
		func() error {
			_, err := peer.pool.Exec(ctx, `DELETE FROM context_history WHERE repo_id=$1 AND id=$2`, repo, event.ID)
			return err
		},
	} {
		if err := mutation(); err != nil {
			t.Fatal(err)
		}
		current := metadataGenerationPGRead(t, ctx, storage, repo)
		if current.Epoch != previous.Epoch || current.Sequence <= previous.Sequence {
			t.Fatal("accepted metadata mutation missed", previous, current)
		}
		if got, err := storage.RepositoryRevision(ctx, repo); err != nil || got != revision {
			t.Fatal("fixture relied on app revision", got, revision, err)
		}
		previous = current
	}
	catalogExecPG(t, ctx, peer.pool, `SELECT cxt_reset_catalog($1)`, repo)
	reset := metadataGenerationPGRead(t, ctx, storage, repo)
	if reset.Epoch == previous.Epoch || reset.Sequence != 0 || reset == previous {
		t.Fatal("catalog reset identity lost", previous, reset)
	}
	if got, err := storage.RepositoryRevision(ctx, repo); err != nil || got != revision {
		t.Fatal("catalog epoch reset changed public revision", got, revision, err)
	}
}

func TestPGRepositoryMetadataGenerationScopeAndPolicy(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo := metadataGenerationPGFixture(t, storage)
	other := domain.HashContent([]byte(t.Name() + " other"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	reject := func(bound context.Context, reader *PostgresStore, id domain.ContentHash, want error) {
		t.Helper()
		got, err := reader.ReadRepositoryMetadataGeneration(bound, id)
		if !errors.Is(err, want) || got != (domain.RepositoryMetadataGeneration{}) {
			t.Fatal("invalid generation scope accepted", got, err, want)
		}
	}
	reject(ctx, storage, "invalid", domain.ErrValidation)
	reject(ctx, storage, domain.HashContent([]byte("missing repository")), domain.ErrNotFound)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		reject(read, peer, repo, domain.ErrConflict)
		metadataGenerationPGRead(t, read, storage, other)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		escaped = write
		reject(write, peer, repo, domain.ErrConflict)
		reject(write, storage, other, domain.ErrConflict)
		metadataGenerationPGRead(t, write, storage, repo)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.ReadRepositoryMetadataGeneration(escaped, repo); err == nil || got != (domain.RepositoryMetadataGeneration{}) {
		t.Fatal("closed transaction escaped to pool", got, err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	reject(ctx, storage, repo, domain.ErrDocumentIdentityUpgradeRequired)
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		reject(write, storage, repo, domain.ErrDocumentIdentityUpgradeRequired)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	peerOnly := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, nil)
	reject(peerOnly, storage, repo, domain.ErrDocumentIdentityUpgradeRequired)
	capable := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	generation := metadataGenerationPGRead(t, capable, storage, repo)
	if err := storage.WithinRepository(capable, repo, func(write context.Context) error {
		if got := metadataGenerationPGRead(t, write, storage, repo); got != generation {
			t.Fatal("capable owner read changed", got, generation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM repository_catalog_state WHERE repo_id=$1`, other)
	reject(ctx, storage, other, domain.ErrNotFound)
}

func TestPGRepositoryMetadataGenerationRollbackAndCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		name := "rollback"
		if canceled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, repo := metadataGenerationPGFixture(t, storage)
			before := metadataGenerationPGRead(t, ctx, storage, repo)
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			abort := errors.New("abort metadata publication")
			err := storage.WithinRepository(work, repo, func(write context.Context) error {
				metadataGenerationPGRead(t, write, storage, repo)
				if err := storage.ApplyHistoryEvent(write, catalogHistoryPG(repo)); err != nil {
					return err
				}
				inside := metadataGenerationPGRead(t, write, storage, repo)
				if inside.Epoch != before.Epoch || inside.Sequence <= before.Sequence {
					t.Fatal("generation missed caller's uncommitted mutation", before, inside)
				}
				if got := metadataGenerationPGRead(t, ctx, peer, repo); got != before {
					t.Fatal("uncommitted generation published", before, got)
				}
				if canceled {
					cancel()
					if got, err := storage.ReadRepositoryMetadataGeneration(write, repo); !errors.Is(err, context.Canceled) || got != (domain.RepositoryMetadataGeneration{}) {
						t.Fatal("canceled transaction returned generation", got, err)
					}
					return work.Err()
				}
				return abort
			})
			want := abort
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("outer abort lost", err)
			}
			if got := metadataGenerationPGRead(t, ctx, peer, repo); got != before {
				t.Fatal("generation survived rollback", before, got)
			}
			if history, err := peer.ListHistoryEvents(ctx, repo); err != nil || len(history) != 0 {
				t.Fatal("history survived rollback", history, err)
			}
			if err := peer.WithinRepository(ctx, repo, func(write context.Context) error {
				metadataGenerationPGRead(t, write, peer, repo)
				return peer.ApplyHistoryEvent(write, catalogHistoryPG(repo))
			}); err != nil {
				t.Fatal("rollback did not release generation lock", err)
			}
			if got := metadataGenerationPGRead(t, ctx, storage, repo); got.Sequence <= before.Sequence {
				t.Fatal("retry did not commit generation", before, got)
			}
		})
	}
}

func TestPGRepositoryMetadataGenerationWriteFence(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo := metadataGenerationPGFixture(t, storage)
	event := catalogHistoryPG(repo)
	if err := storage.ApplyHistoryEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	before := metadataGenerationPGRead(t, ctx, storage, repo)
	mutation := `UPDATE context_history SET event=jsonb_set(event,'{branch}','"changed-after-fence"'::jsonb) WHERE repo_id=$1 AND id=$2`
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		if got := metadataGenerationPGRead(t, write, storage, repo); got != before {
			t.Fatal("fence read changed", got, before)
		}
		for _, query := range []string{
			`SELECT repo_id FROM repository_catalog_state WHERE repo_id=$1 AND $2::text IS NOT NULL FOR SHARE NOWAIT`,
			mutation,
		} {
			attempt, err := peer.pool.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := attempt.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
				rollbackPG(attempt)
				return err
			}
			_, err = attempt.Exec(ctx, query, repo, event.ID)
			rollbackPG(attempt)
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "55P03" {
				t.Fatal("metadata mutation crossed generation fence", query, err)
			}
		}
		if got := metadataGenerationPGRead(t, write, storage, repo); got != before {
			t.Fatal("blocked mutation changed generation", before, got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, peer.pool, mutation, repo, event.ID)
	if got := metadataGenerationPGRead(t, ctx, storage, repo); got.Sequence <= before.Sequence {
		t.Fatal("outer commit did not release fence", before, got)
	}
}

func TestPGRepositoryMetadataGenerationWaitingCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo := metadataGenerationPGFixture(t, storage)
	before := metadataGenerationPGRead(t, ctx, storage, repo)
	holder, err := peer.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(holder)
	catalogExecPG(t, ctx, holder, `SELECT repo_id FROM repository_catalog_state WHERE repo_id=$1 FOR UPDATE`, repo)
	bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err = storage.WithinRepository(bounded, repo, func(write context.Context) error {
		got, err := storage.ReadRepositoryMetadataGeneration(write, repo)
		if got != (domain.RepositoryMetadataGeneration{}) {
			t.Fatal("canceled fence exposed generation", got)
		}
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("waiting fence ignored deadline", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		if got := metadataGenerationPGRead(t, write, storage, repo); got != before {
			t.Fatal("canceled fence changed generation", got, before)
		}
		return nil
	}); err != nil {
		t.Fatal("canceled fence retained lock", err)
	}
}
