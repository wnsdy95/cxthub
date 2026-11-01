//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGGraphQualificationIdentityCompatibilityAppliesToEveryOperation(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result := graphQualificationResultPG(t, ctx, storage, key, true)
	if err := storage.RequireDocumentIdentity(ctx, key.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
	for _, identities := range []struct{ peer, binary []domain.DocumentIdentity }{
		{}, {peer: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}}, {binary: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}},
	} {
		bound := outbound.WithDocumentIdentityCompatibility(ctx, identities.peer, identities.binary)
		for _, operation := range []func() error{
			func() error { return storage.ConfigureGraphQualification(bound, key.RepoID, false) },
			func() error { _, err := storage.ClaimGraphQualification(bound, key.RepoID, time.Minute); return err },
			func() error { return storage.FinishGraphQualification(bound, lease, result) },
			func() error { return storage.FailGraphQualification(bound, lease, "not_current") },
			func() error { _, err := storage.ReadGraphQualification(bound, key); return err },
		} {
			if err := operation(); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatalf("qualification skipped current identity compatibility: %v", err)
			}
		}
	}
	if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) {
		t.Fatal("incompatible request changed qualification state")
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := storage.FinishGraphQualification(compatible, lease, result); err != nil {
		t.Fatal(err)
	}
	if read, err := storage.ReadGraphQualification(compatible, key); err != nil || read != result {
		t.Fatalf("compatible peer could not read qualification: %+v %v", read, err)
	}
}

func TestPGGraphQualificationRejectsNoncurrentAndUnavailableClaims(t *testing.T) {
	for _, state := range []string{"unconfigured", "disabled", "lagged", "terminal", "old_rules", "future_rules", "future_sequence", "missing_ready", "reset"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			switch state {
			case "unconfigured":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_qualification WHERE repo_id=$1`, string(key.RepoID))
			case "disabled":
				if err := storage.ConfigureGraphQualification(ctx, key.RepoID, false); err != nil {
					t.Fatal(err)
				}
			case "lagged":
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='unqualified lag' WHERE repo_id=$1`, string(key.RepoID))
			case "terminal":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET phase='needs_attention',reason='invalid_source' WHERE repo_id=$1`, string(key.RepoID))
			case "old_rules", "future_rules":
				rules := domain.GraphProjectionRules
				if state == "future_rules" {
					rules = domain.GraphViewProjectionRules + 1
				}
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=$2 WHERE repo_id=$1`, string(key.RepoID), rules)
			case "future_sequence":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_ready SET source_seq=source_seq+1 WHERE repo_id=$1`, string(key.RepoID))
			case "missing_ready":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_ready WHERE repo_id=$1`, string(key.RepoID))
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(key.RepoID))
			}
			before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
			if lease, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) || lease != (domain.GraphQualificationLease{}) {
				t.Fatalf("unavailable generation claimed: %+v %v", lease, err)
			}
			if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) {
				t.Fatal("unavailable generation consumed qualification budget")
			}
		})
	}
}

func TestPGGraphQualificationRejectsInvalidAndInexactResultsWithoutConsumingLease(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result := graphQualificationResultPG(t, ctx, storage, key, true)
	before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
	for _, mutate := range []func(*domain.GraphQualificationLease){
		func(next *domain.GraphQualificationLease) { next.Fence++ },
		func(next *domain.GraphQualificationLease) { next.LeaseUntil = next.LeaseUntil.Add(time.Second) },
		func(next *domain.GraphQualificationLease) { next.Dataset = "33333333-3333-4333-8333-333333333333" },
		func(next *domain.GraphQualificationLease) { next.Applied.Sequence-- },
	} {
		wrong := lease
		mutate(&wrong)
		wrongResult := result
		wrongResult.GraphQualificationKey = wrong.GraphQualificationKey
		if err := storage.FinishGraphQualification(ctx, wrong, wrongResult); !errors.Is(err, domain.ErrRefConflict) {
			t.Fatalf("inexact lease finished: %v", err)
		}
		if err := storage.FailGraphQualification(ctx, wrong, "read_error"); !errors.Is(err, domain.ErrRefConflict) {
			t.Fatalf("inexact lease failed: %v", err)
		}
	}
	invalid := result
	invalid.Actual = "not a bounded digest"
	if err := storage.FinishGraphQualification(ctx, lease, invalid); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid result accepted: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.FinishGraphQualification(canceled, lease, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled comparison completed: %v", err)
	}
	if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) {
		t.Fatal("invalid completion consumed lease or changed receipt")
	}
	if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
}

func TestPGGraphQualificationReadsBindExactKey(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true)); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.GraphQualificationKey){
		func(next *domain.GraphQualificationKey) { next.Dataset = "33333333-3333-4333-8333-333333333333" },
		func(next *domain.GraphQualificationKey) { next.Applied.Epoch = "33333333-3333-4333-8333-333333333333" },
		func(next *domain.GraphQualificationKey) { next.Applied.Sequence++ },
		func(next *domain.GraphQualificationKey) {
			next.RepoID = domain.HashContent([]byte("foreign qualification"))
			next.Applied.RepoID = next.RepoID
		},
	} {
		wrong := key
		mutate(&wrong)
		if result, err := storage.ReadGraphQualification(ctx, wrong); !errors.Is(err, domain.ErrNotFound) || result != (domain.GraphQualificationResult{}) {
			t.Fatalf("qualification leaked across key: %+v %v", result, err)
		}
	}
	for _, mutate := range []func(*domain.GraphQualificationKey){
		func(next *domain.GraphQualificationKey) { next.Rules = domain.GraphProjectionRules },
		func(next *domain.GraphQualificationKey) { next.ComparisonRules++ },
	} {
		wrong := key
		mutate(&wrong)
		if _, err := storage.ReadGraphQualification(ctx, wrong); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("qualification accepted incompatible key: %v", err)
		}
	}
}

type graphQualificationCorruptReadTx struct {
	pgx.Tx
	corrupt func([]any)
}

func (transaction graphQualificationCorruptReadTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	row := transaction.Tx.QueryRow(ctx, query, args...)
	if strings.Contains(query, "FROM graph_qualification qualification") {
		return graphQualificationCorruptReadRow{Row: row, corrupt: transaction.corrupt}
	}
	return row
}

type graphQualificationCorruptReadRow struct {
	pgx.Row
	corrupt func([]any)
}

func (row graphQualificationCorruptReadRow) Scan(destinations ...any) error {
	if err := row.Row.Scan(destinations...); err != nil {
		return err
	}
	row.corrupt(destinations)
	return nil
}

func TestPGGraphQualificationReadValidatesStoredReceipt(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true)); err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []func([]any){
		func(destinations []any) { *destinations[0].(*string) = "33333333-3333-4333-8333-333333333333" },
		func(destinations []any) { *destinations[5].(*domain.ContentHash) = "invalid digest" },
		func(destinations []any) { *destinations[7].(*string) = "mismatch" },
		func(destinations []any) { *destinations[8].(*time.Time) = time.Time{} },
	} {
		err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
			transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
			transaction.Tx = graphQualificationCorruptReadTx{Tx: transaction.Tx, corrupt: corrupt}
			read, err := storage.ReadGraphQualification(context.WithValue(bound, repositoryTxKey{}, &transaction), key)
			if read != (domain.GraphQualificationResult{}) {
				t.Fatal("corrupt qualification returned a receipt")
			}
			return err
		})
		if !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("corrupt qualification accepted: %v", err)
		}
	}
	for _, query := range []string{
		`UPDATE graph_qualification SET reason='unbounded details' WHERE repo_id=$1`,
		`UPDATE graph_qualification SET actual_hash=repeat('x',1000) WHERE repo_id=$1`,
		`UPDATE graph_qualification SET result_state='mismatch' WHERE repo_id=$1`,
		`UPDATE graph_qualification SET expected_hash=NULL WHERE repo_id=$1`,
	} {
		var constraint *pgconn.PgError
		if _, err := storage.pool.Exec(ctx, query, string(key.RepoID)); !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatalf("qualification storage lacks bounded shape constraint: %v", err)
		}
	}
}

func graphQualificationRejectWritesPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, delay bool) {
	t.Helper()
	body := `RAISE EXCEPTION 'qualification test rollback' USING ERRCODE='23514';`
	if delay {
		body = `PERFORM pg_sleep(10);`
	}
	catalogExecPG(t, ctx, storage.pool, `CREATE OR REPLACE FUNCTION cxt_test_graph_qualification_reject() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.repo_id=TG_ARGV[0] THEN `+body+` END IF; RETURN NEW; END $$`)
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`CREATE TRIGGER cxt_test_graph_qualification_reject AFTER INSERT OR UPDATE ON graph_qualification
		FOR EACH ROW EXECUTE FUNCTION cxt_test_graph_qualification_reject('%s')`, repo))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DROP TRIGGER IF EXISTS cxt_test_graph_qualification_reject ON graph_qualification;
			DROP FUNCTION IF EXISTS cxt_test_graph_qualification_reject()`); err != nil {
			t.Error(err)
		}
	})
}

func TestPGGraphQualificationWriteRollbackAndCancellation(t *testing.T) {
	for _, operation := range []string{"configure", "claim", "finish", "fail", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			var lease domain.GraphQualificationLease
			if operation == "finish" || operation == "fail" || operation == "cancel" {
				lease = graphQualificationClaimPG(t, ctx, storage, key)
			}
			before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
			graphQualificationRejectWritesPG(t, ctx, storage, key.RepoID, operation == "cancel")
			var err error
			switch operation {
			case "configure":
				err = storage.ConfigureGraphQualification(ctx, key.RepoID, false)
			case "claim":
				var claimed domain.GraphQualificationLease
				claimed, err = storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute)
				if claimed != (domain.GraphQualificationLease{}) {
					t.Fatal("failed claim returned an uncommitted lease")
				}
			case "finish":
				err = storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true))
			case "fail":
				err = storage.FailGraphQualification(ctx, lease, "resource_limit")
			case "cancel":
				bound, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				err = storage.FinishGraphQualification(bound, lease, graphQualificationResultPG(t, ctx, storage, key, true))
			}
			var databaseError *pgconn.PgError
			if operation == "cancel" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("canceled comparison result: %v", err)
				}
			} else if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
				t.Fatalf("write did not return original transaction error: %v", err)
			}
			if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) {
				t.Fatal("failed transaction changed qualification lease or receipt")
			}
		})
	}
}
