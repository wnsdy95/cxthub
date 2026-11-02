//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphQualificationInputSizeTx struct {
	pgx.Tx
	queries, rows, transferred int
	valid                      bool
}

func (transaction *graphQualificationInputSizeTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	transaction.queries++
	if query != graphQualificationInputSizesSQL {
		transaction.valid = false
	}
	rows, err := transaction.Tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	fields := rows.FieldDescriptions()
	if len(fields) != 1 || fields[0].DataTypeOID != pgtype.Int4OID {
		transaction.valid = false
	}
	return &graphQualificationInputSizeRows{Rows: rows, transaction: transaction}, nil
}

type graphQualificationInputSizeRows struct {
	pgx.Rows
	transaction *graphQualificationInputSizeTx
}

func (rows *graphQualificationInputSizeRows) Next() bool {
	if !rows.Rows.Next() {
		return false
	}
	rows.transaction.rows++
	for _, raw := range rows.Rows.RawValues() {
		rows.transaction.transferred += len(raw)
	}
	return true
}

func graphQualificationInputCheckPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) (*graphQualificationInputSizeTx, error) {
	t.Helper()
	var observed *graphQualificationInputSizeTx
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
		observed = &graphQualificationInputSizeTx{Tx: transaction.Tx, valid: true}
		transaction.Tx = observed
		return storage.CheckGraphQualificationInputs(context.WithValue(bound, repositoryTxKey{}, &transaction), repo)
	})
	if observed == nil || !observed.valid || observed.queries > 1 || observed.transferred > 4*(domain.MaxPendingProjectionInputRows+1) {
		t.Fatalf("qualification sizing transferred payloads or exceeded descriptor cap: %+v", observed)
	}
	return observed, err
}

func graphQualificationUnjournaledInputPG(t *testing.T, ctx context.Context, storage *PostgresStore, query string, args ...any) {
	t.Helper()
	transaction := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, transaction, `SET LOCAL session_replication_role=replica`)
	catalogExecPG(t, ctx, transaction, query, args...)
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPGGraphQualificationInputsSmallSnapshotAndForeignIsolation(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	before := catalogReadStatePG(t, ctx, storage, key.RepoID)
	observed, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
	if err != nil || observed.queries != 1 || observed.rows == 0 {
		t.Fatalf("small input rejected: %+v %v", observed, err)
	}
	foreign := domain.HashContent([]byte(t.Name() + key.Dataset))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
		t.Fatal(err)
	}
	graphQualificationUnjournaledInputPG(t, ctx, storage, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		VALUES($1,'qualification','foreign',jsonb_build_object('oversized',repeat('x',$2)))`, string(foreign), domain.MaxRepositoryViewSourceImageBytes)
	after, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
	if err != nil || observed.rows != after.rows {
		t.Fatalf("foreign rows entered budget: %+v %v", after, err)
	}
	if _, err := graphQualificationInputCheckPG(t, ctx, storage, foreign); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("foreign oversized fixture was not oversized: %v", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if err := storage.CheckGraphQualificationInputs(bound, key.RepoID); err != nil {
			return err
		}
		graphQualificationUnjournaledInputPG(t, ctx, storage, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
			VALUES($1,'qualification','later',jsonb_build_object('oversized',repeat('x',$2)))`, string(key.RepoID), domain.MaxRepositoryViewSourceImageBytes)
		return storage.CheckGraphQualificationInputs(bound, key.RepoID)
	}); err != nil {
		t.Fatalf("input gate escaped the pinned read snapshot: %v", err)
	}
	if _, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("fresh snapshot missed actual input mutation: %v", err)
	}
	if after := catalogReadStatePG(t, ctx, storage, key.RepoID); before != after {
		t.Fatal("fixture unexpectedly journaled the actual input mutation")
	}
}

func TestPGGraphQualificationInputsBoundLogicalRowsWithoutJournalCoverage(t *testing.T) {
	for _, table := range []string{"metadata", "git_tree", "git_proof"} {
		t.Run(table, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			before := catalogReadStatePG(t, ctx, storage, key.RepoID)
			var query string
			switch table {
			case "metadata":
				query = `INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,'oversized',jsonb_build_object('padding',repeat('x',$2)))`
			case "git_tree":
				query = `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload) VALUES($1,'qualification','oversized',jsonb_build_object('padding',repeat('x',$2)))`
			case "git_proof":
				query = `INSERT INTO git_contribution_proofs(repo_id,id,admission_id,context_head_id,contribution_id,payload)
					VALUES($1,repeat('a',64),repeat('b',64),repeat('c',64),repeat('d',64),jsonb_build_object('padding',repeat('x',$2)))`
			}
			graphQualificationUnjournaledInputPG(t, ctx, storage, query, string(key.RepoID), domain.MaxRepositoryViewSourceImageBytes)
			observed, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
			if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || observed.rows > 20 || observed.transferred > 80 {
				t.Fatalf("logical oversized input reached full reads: %+v %v", observed, err)
			}
			if after := catalogReadStatePG(t, ctx, storage, key.RepoID); before != after {
				t.Fatal("budget rejection depended on source journal coverage")
			}
		})
	}
}

func TestPGGraphQualificationInputsBoundCountAndTotalBytes(t *testing.T) {
	for _, limit := range []string{"rows", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			if limit == "rows" {
				graphQualificationUnjournaledInputPG(t, ctx, storage, `INSERT INTO pending_contexts(repo_id,session_id,data)
					SELECT $1,'limit-'||ordinal::text,'{}'::jsonb FROM generate_series(1,$2::integer) AS ordinal`, string(key.RepoID), domain.MaxPendingProjectionInputRows+1)
			} else {
				graphQualificationUnjournaledInputPG(t, ctx, storage, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
					SELECT $1,'qualification',ordinal::text,jsonb_build_object('padding',repeat('x',524288)) FROM generate_series(1,65) AS ordinal`, string(key.RepoID))
			}
			observed, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
			if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || observed.rows > domain.MaxPendingProjectionInputRows+1 {
				t.Fatalf("actual input budget not enforced: %+v %v", observed, err)
			}
			if limit == "rows" && observed.rows != domain.MaxPendingProjectionInputRows+1 {
				t.Fatalf("fixture hit bytes before count cap: %+v", observed)
			}
			if limit == "bytes" && observed.rows > 70 {
				t.Fatalf("total-byte limit did not stop descriptor consumption: %+v", observed)
			}
		})
	}
}

func TestPGGraphQualificationInputsCheckIdentityBeforeSizing(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	if err := storage.RequireDocumentIdentity(ctx, key.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	observed, err := graphQualificationInputCheckPG(t, ctx, storage, key.RepoID)
	if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || observed.queries != 0 {
		t.Fatalf("input sizing preceded identity compatibility: %+v %v", observed, err)
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if _, err := graphQualificationInputCheckPG(t, compatible, storage, key.RepoID); err != nil {
		t.Fatal(err)
	}
}
