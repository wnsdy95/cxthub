//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGGraphServingIdentityAndTransactionBoundaries(t *testing.T) {
	storage, ctx, initial := graphServingPG(t)
	repo := initial.Source.RepoID
	failure := graphServingCorruptPG(t, ctx, storage, repo)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	before := graphServingSnapshotPG(t, ctx, storage, repo)
	for _, operation := range []func() error{
		func() error { _, err := storage.ReadQualifiedGraphProjection(ctx, repo); return err },
		func() error { _, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo); return err },
		func() error { return storage.RequestGraphServingRepair(ctx, failure) },
	} {
		if err := operation(); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
			t.Fatalf("serving operation skipped current identity: %v", err)
		}
	}
	if !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) {
		t.Fatal("incompatible identity modified serving state")
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := storage.WithinReadSnapshot(compatible, func(bound context.Context) error {
		if err := storage.RequestGraphServingRepair(bound, failure); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("repair borrowed read-only transaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(compatible, repo, func(bound context.Context) error {
		if err := storage.RequestGraphServingRepair(bound, failure); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("repair borrowed write transaction: %v", err)
		}
		if _, err := storage.ReadQualifiedGraphProjection(bound, repo); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("serving read borrowed write transaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequestGraphServingRepair(compatible, failure); err != nil {
		t.Fatal(err)
	}
	graphQualificationDuePG(t, compatible, storage, repo)
	if _, err := storage.ClaimGraphQualification(compatible, repo, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("identity mutation qualified an outdated candidate: %v", err)
	}
	job := graphProjectionCapturePG(t, compatible, storage, graphProjectionClaimPG(t, compatible, storage, repo), 0)
	graphProjectionPublishPG(t, compatible, storage, job)
	current, err := storage.ReadGraphProjectionStatus(compatible, repo)
	if err != nil || !current.Ready {
		t.Fatalf("compatible candidate not current: %+v %v", current, err)
	}
	graphServingPromotePG(t, compatible, storage, current.QualificationKey())
}

func TestPGGraphServingReadRejectsLogicalSizeAndExtraRowsBeforePayload(t *testing.T) {
	for _, mode := range []string{"header", "body", "rows", "extra", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			want := domain.ErrRepositoryViewSourceLimit
			switch mode {
			case "header", "body":
				kind := "header"
				if mode == "body" {
					kind = initial.Parts[0].Kind
					for _, part := range initial.Parts {
						if part.Kind != "header" {
							kind = part.Kind
							break
						}
					}
				}
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_parts SET image=jsonb_build_object('large',repeat('x',$3)) WHERE repo_id=$1 AND kind=$2`, string(repo), kind, domain.MaxPendingProjectionOutputBytes)
			case "rows", "extra":
				count := domain.MaxPendingProjectionOutputRows + 1
				if mode == "extra" {
					count, want = 1, domain.ErrIntegrity
				}
				catalogExecPG(t, ctx, storage.pool, `INSERT INTO graph_serving_parts(repo_id,dataset,source_seq,kind,entity_key,image)
					SELECT $1,$2,$3,'extra',entry::text,'{}' FROM generate_series(1,$4::integer) entry`, string(repo), initial.Dataset, initial.Applied.Sequence, count)
			case "manifest":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_heads SET expected_hash=$2,actual_hash=$2 WHERE repo_id=$1`, string(repo), string(domain.HashContent([]byte("wrong manifest digest"))))
				want = domain.ErrIntegrity
			}
			var read domain.GraphProjectionRead
			err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				if mode == "header" || mode == "body" || mode == "rows" {
					transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
					transaction.Tx = graphProjectionRepairFaultTx{Tx: transaction.Tx, needle: "SELECT image FROM graph_serving_parts", failure: errors.New("unbounded payload was fetched")}
					bound = context.WithValue(bound, repositoryTxKey{}, &transaction)
				}
				var err error
				read, err = storage.ReadQualifiedGraphProjection(bound, repo)
				return err
			})
			var failure *domain.GraphProjectionCorruption
			if !errors.Is(err, want) || !errors.As(err, &failure) || failure.Validate() != nil || !failure.Serving || failure.ServingFence != initial.ServingFence ||
				!read.Complete || read.ServingFence != initial.ServingFence || len(read.Parts) != 0 {
				t.Fatalf("invalid output lost bounded serving receipt: %+v %v", read, err)
			}
			if status, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo); err != nil || !status.Complete || status.ServingFence != initial.ServingFence {
				t.Fatalf("metadata status unexpectedly decoded body: %+v %v", status, err)
			}
		})
	}
}

func TestPGGraphServingReadNeverLabelsSourceSQLOrCancellationAsCorruption(t *testing.T) {
	for _, mode := range []string{"source", "sql", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			graphServingCorruptPG(t, ctx, storage, repo)
			want := error(domain.ErrIntegrity)
			switch mode {
			case "source":
				catalogExecPG(t, ctx, storage.pool, `UPDATE repository_graph_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(repo))
			case "sql":
				want = &pgconn.PgError{Code: "57014", Message: "statement timeout"}
			case "cancel":
				want = context.Canceled
			}
			var read domain.GraphProjectionRead
			err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				if mode == "sql" {
					transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
					transaction.Tx = graphProjectionRepairFaultTx{Tx: transaction.Tx, needle: "FROM graph_serving_parts", failure: want}
					bound = context.WithValue(bound, repositoryTxKey{}, &transaction)
				}
				if mode == "cancel" {
					var cancel context.CancelFunc
					bound, cancel = context.WithCancel(bound)
					cancel()
				}
				var err error
				read, err = storage.ReadQualifiedGraphProjection(bound, repo)
				return err
			})
			var failure *domain.GraphProjectionCorruption
			if !errors.Is(err, want) || errors.As(err, &failure) || !reflect.DeepEqual(read, domain.GraphProjectionRead{}) {
				t.Fatalf("non-output error produced repair receipt: %+v %v", read, err)
			}
		})
	}
}

func graphServingTriggerPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, table, body string) {
	t.Helper()
	catalogExecPG(t, ctx, storage.pool, `CREATE OR REPLACE FUNCTION cxt_test_graph_serving_hook() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.repo_id=TG_ARGV[0] THEN `+body+` END IF; RETURN NEW; END $$`)
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`CREATE TRIGGER cxt_test_graph_serving_hook AFTER INSERT OR UPDATE ON %s
		FOR EACH ROW EXECUTE FUNCTION cxt_test_graph_serving_hook('%s')`, table, repo))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DROP TRIGGER IF EXISTS cxt_test_graph_serving_hook ON `+table+`; DROP FUNCTION IF EXISTS cxt_test_graph_serving_hook()`); err != nil {
			t.Error(err)
		}
	})
}

func TestPGGraphServingPromotionCopiesValidatedBytesNotNewCandidateRead(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result := graphQualificationResultPG(t, ctx, storage, key, true)
	candidate, err := storage.ReadGraphProjection(ctx, key.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	graphServingTriggerPG(t, ctx, storage, key.RepoID, "graph_serving_heads",
		`UPDATE graph_projection_parts SET image='{}' WHERE repo_id=NEW.repo_id AND kind='header';`)
	if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, storage, key.RepoID)
	if !reflect.DeepEqual(read.Parts, candidate.Parts) || read.Qualification.Actual != result.Actual {
		t.Fatal("promotion copied changed bytes after validating candidate")
	}
	if _, err := storage.ReadGraphProjection(ctx, key.RepoID); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("fixture did not change candidate after validation: %v", err)
	}
}

func TestPGGraphServingPromotionAndRepairRollback(t *testing.T) {
	for _, mode := range []string{"promotion", "repair", "cancel", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			var failure domain.GraphProjectionCorruption
			var lease domain.GraphQualificationLease
			var result domain.GraphQualificationResult
			if mode == "repair" {
				failure = graphServingCorruptPG(t, ctx, storage, repo)
			} else {
				next := graphQualificationAdvancePG(t, ctx, storage, repo)
				graphQualificationDuePG(t, ctx, storage, repo)
				duration := time.Minute
				if mode == "expiry" {
					duration = time.Second
				}
				var err error
				lease, err = storage.ClaimGraphQualification(ctx, repo, duration)
				if err != nil {
					t.Fatal(err)
				}
				result = graphQualificationResultPG(t, ctx, storage, next, true)
			}
			table, body := "graph_serving_parts", `RAISE EXCEPTION 'serving rollback' USING ERRCODE='23514';`
			if mode == "repair" {
				table = "graph_qualification"
			} else if mode == "cancel" || mode == "expiry" {
				table, body = "graph_serving_heads", `PERFORM pg_sleep(2);`
			}
			graphServingTriggerPG(t, ctx, storage, repo, table, body)
			before := graphServingSnapshotPG(t, ctx, storage, repo)
			receipt := graphQualificationSnapshotPG(t, ctx, storage, repo)
			var err error
			if mode == "repair" {
				err = storage.RequestGraphServingRepair(ctx, failure)
			} else {
				bound := ctx
				if mode == "cancel" {
					var cancel context.CancelFunc
					bound, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
					defer cancel()
				}
				err = storage.FinishGraphQualification(bound, lease, result)
			}
			var databaseError *pgconn.PgError
			switch mode {
			case "cancel":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancellation result: %v", err)
				}
			case "expiry":
				if !errors.Is(err, domain.ErrRefConflict) {
					t.Fatalf("lease expired during promotion was committed: %v", err)
				}
			default:
				if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
					t.Fatalf("SQL error was misclassified: %v", err)
				}
			}
			if !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) || !bytes.Equal(receipt, graphQualificationSnapshotPG(t, ctx, storage, repo)) {
				t.Fatal("failed transaction changed serving head, parts, ledger, or qualification")
			}
		})
	}
}
