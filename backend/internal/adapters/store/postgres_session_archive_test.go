//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func sessionArchivePG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, repo
}

func TestPGSessionArchivePersistence(t *testing.T) {
	st, ctx, _ := sessionArchivePG(t)
	checkSessionArchivePersistence(t, ctx, st)
}

func TestPGSessionArchiveValidation(t *testing.T) {
	st, ctx, _ := sessionArchivePG(t)
	checkSessionArchiveValidation(t, ctx, st)
}

func TestPGSessionArchiveTransactionAndRevisionRollback(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "transactional archive")
	before, err := st.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort archive transaction")
	archive := func(bound context.Context) error {
		if err := st.PutSessionArchive(bound, record); err != nil {
			return err
		}
		if records, err := st.ListSessionArchives(bound, repo); err != nil || len(records) != 1 {
			t.Fatalf("archive not visible inside transaction: %+v %v", records, err)
		}
		if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 0 {
			t.Fatalf("uncommitted archive escaped transaction: %+v %v", records, err)
		}
		if err := st.AdvanceRepositoryRevision(bound, repo, false); err != nil {
			return err
		}
		return nil
	}
	err = st.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := archive(bound); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("archive rollback: %v", err)
	}
	if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 0 {
		t.Fatalf("archive survived rollback: %+v %v", records, err)
	}
	if after, err := st.RepositoryRevision(ctx, repo); err != nil || after != before {
		t.Fatalf("archive revision survived rollback: %+v %v", after, err)
	}
	if err := st.WithinRepository(ctx, repo, archive); err != nil {
		t.Fatal(err)
	}
	committed, err := st.RepositoryRevision(ctx, repo)
	if err != nil || committed.Graph != before.Graph+1 || committed.Pending != before.Pending || committed.Evidence != before.Evidence {
		t.Fatalf("application revision did not commit exactly once: before=%+v after=%+v err=%v", before, committed, err)
	}
	restore := func(bound context.Context) error {
		if err := st.DeleteSessionArchive(bound, repo, record.Key); err != nil {
			return err
		}
		if records, err := st.ListSessionArchives(bound, repo); err != nil || len(records) != 0 {
			t.Fatalf("restore not visible inside transaction: %+v %v", records, err)
		}
		if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 1 {
			t.Fatalf("uncommitted restore escaped transaction: %+v %v", records, err)
		}
		return st.AdvanceRepositoryRevision(bound, repo, false)
	}
	err = st.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := restore(bound); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("restore rollback: %v", err)
	}
	records, err := st.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 1 {
		t.Fatalf("restore survived rollback: %+v %v", records, err)
	}
	requireSessionArchive(t, records[0], record)
	if after, err := st.RepositoryRevision(ctx, repo); err != nil || after != committed {
		t.Fatalf("restore revision survived rollback: %+v %v", after, err)
	}
	if err := st.WithinRepository(ctx, repo, restore); err != nil {
		t.Fatal(err)
	}
	if records, err := st.ListSessionArchives(ctx, repo); err != nil || len(records) != 0 {
		t.Fatalf("restore did not commit: %+v %v", records, err)
	}
	if after, err := st.RepositoryRevision(ctx, repo); err != nil || after.Graph != committed.Graph+1 {
		t.Fatalf("restore revision did not commit exactly once: %+v %v", after, err)
	}
}

func TestPGSessionArchiveForeignTransactionRejected(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "foreign transaction")
	peer := &PostgresStore{pool: st.pool}
	if err := st.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := peer.PutSessionArchive(bound, record); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign insert escaped transaction: %v", err)
		}
		if records, err := peer.ListSessionArchives(bound, repo); !errors.Is(err, domain.ErrConflict) || records != nil {
			t.Fatalf("foreign read escaped transaction: %+v %v", records, err)
		}
		if err := peer.DeleteSessionArchive(bound, repo, record.Key); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign restore escaped transaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGSessionArchiveForeignKeysAndMalformedReads(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	record := sessionArchiveFixture(t, ctx, st, repo, domain.ProviderCodex, "session", "archive foreign keys")
	missingRepo := record
	missingRepo.RepoID = domain.HashContent([]byte(string(repo) + "missing"))
	missingSnapshot := record
	missingSnapshot.SnapshotID = domain.HashContent([]byte("missing snapshot"))
	for _, invalid := range []domain.SessionArchive{missingRepo, missingSnapshot} {
		var pgerr *pgconn.PgError
		if err := st.PutSessionArchive(ctx, invalid); !errors.As(err, &pgerr) || pgerr.Code != "23503" {
			t.Fatalf("missing foreign key accepted: %v", err)
		}
	}
	otherRepo := domain.HashContent([]byte(string(repo) + "other"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: otherRepo}); err != nil {
		t.Fatal(err)
	}
	foreignSnapshot := record
	foreignSnapshot.RepoID = otherRepo
	var pgerr *pgconn.PgError
	if err := st.PutSessionArchive(ctx, foreignSnapshot); !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatalf("foreign repository snapshot accepted: %v", err)
	}
	if err := st.PutSessionArchive(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE session_archives SET session_id='mismatched session' WHERE repo_id=$1 AND key=$2`, repo, record.Key); err != nil {
		t.Fatal(err)
	}
	if records, err := st.ListSessionArchives(ctx, repo); !errors.Is(err, domain.ErrIntegrity) || records != nil {
		t.Fatalf("malformed stored archive accepted: %+v %v", records, err)
	}
	if records, err := st.ListSessionArchives(ctx, otherRepo); err != nil || len(records) != 0 {
		t.Fatalf("foreign malformed archive affected other repo: %+v %v", records, err)
	}
}
