//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func prHistoryPGStores(t *testing.T) (*PostgresStore, *PostgresStore) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	databaseName := domain.NewID("pr_history_")
	database := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	var pools []*pgxpool.Pool
	t.Cleanup(func() {
		for _, pool := range pools {
			pool.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+database); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	config.ConnConfig.Database = databaseName
	config.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	for range 2 {
		pool, err := pgxpool.NewWithConfig(ctx, config.Copy())
		if err != nil {
			t.Fatal(err)
		}
		pools = append(pools, pool)
	}
	storage, replica := &PostgresStore{pool: pools[0]}, &PostgresStore{pool: pools[1]}
	if _, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	if count, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil || count != 0 {
		t.Fatal("migration replay", count, err)
	}
	return storage, replica
}

func TestPGPRHistory(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	t.Run("CAS", func(t *testing.T) { checkPRHistoryCAS(t, storage, replica) })
	t.Run("participations", func(t *testing.T) { checkPRParticipations(t, storage, replica) })
	t.Run("observations", func(t *testing.T) { checkPRObservationPaging(t, storage) })
	t.Run("document-identity", func(t *testing.T) { checkPRHistoryDocumentIdentity(t, storage) })
	t.Run("job-scheduling", func(t *testing.T) { checkPRHistoryJobScheduling(t, storage) })
	t.Run("job-fairness", func(t *testing.T) { checkPRHistoryJobFairness(t, storage) })
}

func TestPGPRHistoryTransactionRollback(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	for _, existing := range []bool{false, true} {
		name := "insert"
		if existing {
			name = "update"
		}
		t.Run(name, func(t *testing.T) {
			ctx, checkpoint, participation := prHistoryFixture(t, storage)
			if existing {
				if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
					t.Fatal(err)
				}
			}
			before, err := storage.RepositoryRevision(ctx, checkpoint.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			observation := prHistoryObservation(checkpoint, "rolled-back-observation")
			failure := errors.New("injected after checkpoint, evidence, observation, and revision")
			publish := func(transaction context.Context) error {
				if err := storage.RecordGitRefObservation(transaction, observation, checkpoint.NextAttempt); err != nil {
					return err
				}
				if err := storage.PutPRParticipation(transaction, participation); err != nil {
					return err
				}
				next := checkpoint
				if existing {
					next.Version++
				}
				next.Cursor = "next-provider-page"
				next.EvidenceCursor = observation.ID
				next.MatchCursor = observation.ID + ":" + participation.PublicationID
				if err := storage.SavePRHistoryCheckpoint(transaction, next, next.Version-1); err != nil {
					return err
				}
				got, err := storage.GetPRHistoryCheckpoint(transaction, next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number)
				if err != nil || !reflect.DeepEqual(got, next) {
					t.Fatalf("transaction cannot read own checkpoint: %+v %v", got, err)
				}
				rows, err := storage.ListPRParticipations(transaction, next.RepoID, next.GitOrigin, next.PR.Number)
				if err != nil || len(rows) != 1 || rows[0] != participation {
					t.Fatalf("transaction cannot read own participation: %+v %v", rows, err)
				}
				observations, err := storage.ListPRRefObservations(transaction, next.RepoID, next.GitOrigin, next.ProviderRepositoryID, next.PR.Number, "", 1)
				if err != nil || len(observations) != 1 || observations[0].ID != observation.ID {
					t.Fatalf("transaction cannot read own observation: %+v %v", observations, err)
				}
				return storage.AdvanceRepositoryRevision(transaction, checkpoint.RepoID, false)
			}
			assertInvisible := func() {
				t.Helper()
				got, err := replica.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				if existing {
					if err != nil || !reflect.DeepEqual(got, checkpoint) {
						t.Fatal("uncommitted checkpoint update leaked", got, err)
					}
				} else if !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted checkpoint insert leaked", got, err)
				}
				rows, err := replica.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
				if err != nil || len(rows) != 0 {
					t.Fatal("uncommitted participation leaked", rows, err)
				}
				observations, err := replica.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "", 1)
				if err != nil || len(observations) != 0 {
					t.Fatal("uncommitted observation leaked", observations, err)
				}
				after, err := replica.RepositoryRevision(ctx, checkpoint.RepoID)
				if err != nil || after != before {
					t.Fatal("uncommitted revision leaked", after, err)
				}
			}
			err = storage.WithinRepository(ctx, checkpoint.RepoID, func(transaction context.Context) error {
				if err := publish(transaction); err != nil {
					return err
				}
				assertInvisible()
				return failure
			})
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			assertInvisible()
			var scans int
			if err := replica.db(ctx).QueryRow(ctx, `SELECT count(*) FROM git_scan_jobs WHERE repo_id=$1`, checkpoint.RepoID).Scan(&scans); err != nil || scans != 0 {
				t.Fatal("dependent observation scan leaked", scans, err)
			}
			if err := storage.WithinRepository(ctx, checkpoint.RepoID, publish); err != nil {
				t.Fatal("retry after rollback", err)
			}
			got, err := replica.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
			if err != nil || got.Cursor != "next-provider-page" || got.EvidenceCursor != observation.ID {
				t.Fatal("committed checkpoint missing", got, err)
			}
			rows, err := replica.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
			if err != nil || len(rows) != 1 || rows[0] != participation {
				t.Fatal("committed participation missing", rows, err)
			}
		})
	}
}

func TestPGPRHistoryForeignTransaction(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, checkpoint, participation := prHistoryFixture(t, storage)
	err := storage.WithinRepository(ctx, checkpoint.RepoID, func(transaction context.Context) error {
		operations := []func() error{
			func() error { return replica.SavePRHistoryCheckpoint(transaction, checkpoint, 0) },
			func() error { return replica.PutPRParticipation(transaction, participation) },
			func() error {
				_, err := replica.ListPRHistoryJobs(transaction, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.NextAttempt, 1)
				return err
			},
			func() error {
				_, err := replica.GetPRHistoryCheckpoint(transaction, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				return err
			},
			func() error {
				_, err := replica.ListPRParticipations(transaction, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
				return err
			},
			func() error {
				_, err := replica.ListPRRefObservations(transaction, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "", 1)
				return err
			},
		}
		for index, operation := range operations {
			if err := operation(); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("operation %d escaped owning transaction: %v", index, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPGPRHistoryCorruption(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, kind := range []string{"checkpoint-scope", "checkpoint-version", "checkpoint-payload", "participation-key", "participation-scope", "observation-key", "observation-scope", "observation-payload"} {
		t.Run(kind, func(t *testing.T) {
			ctx, checkpoint, participation := prHistoryFixture(t, storage)
			if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutPRParticipation(ctx, participation); err != nil {
				t.Fatal(err)
			}
			observation := prHistoryObservation(checkpoint, "original")
			if err := storage.RecordGitRefObservation(ctx, observation, checkpoint.NextAttempt); err != nil {
				t.Fatal(err)
			}
			var mutationErr error
			switch kind {
			case "checkpoint-scope":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_history_checkpoints SET payload=jsonb_set(payload,'{provider_repository_id}','99') WHERE repo_id=$1`, checkpoint.RepoID)
			case "checkpoint-version":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_history_checkpoints SET version=99 WHERE repo_id=$1`, checkpoint.RepoID)
			case "checkpoint-payload":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_history_checkpoints SET payload='{}' WHERE repo_id=$1`, checkpoint.RepoID)
			case "participation-key":
				other := participation
				other.PullRequestID++
				raw, err := json.Marshal(other.WithID())
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_participations SET payload=$2 WHERE repo_id=$1`, checkpoint.RepoID, raw)
			case "participation-scope":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_participations SET provider_repository_id=99 WHERE repo_id=$1`, checkpoint.RepoID)
			case "observation-key":
				raw, err := json.Marshal(prHistoryObservation(checkpoint, "different"))
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_ref_observations SET payload=$2 WHERE repo_id=$1`, checkpoint.RepoID, raw)
			case "observation-scope":
				other := checkpoint
				other.RepoID = domain.HashContent([]byte("foreign observation"))
				raw, err := json.Marshal(prHistoryObservation(other, "original"))
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_ref_observations SET payload=$2 WHERE repo_id=$1`, checkpoint.RepoID, raw)
			case "observation-payload":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_ref_observations SET payload=jsonb_set(payload,'{after}','"invalid"') WHERE repo_id=$1`, checkpoint.RepoID)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			var readErr error
			switch kind {
			case "checkpoint-scope", "checkpoint-version", "checkpoint-payload":
				_, readErr = storage.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				next := checkpoint
				next.Version++
				if err := storage.SavePRHistoryCheckpoint(ctx, next, checkpoint.Version); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt checkpoint overwritten", err)
				}
			case "participation-key", "participation-scope":
				_, readErr = storage.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
				if err := storage.PutPRParticipation(ctx, participation); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("mismatched duplicate accepted", err)
				}
			default:
				_, readErr = storage.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "", 1)
			}
			if !errors.Is(readErr, domain.ErrIntegrity) {
				t.Fatal("corrupt payload or row identity trusted", readErr)
			}
		})
	}
}

func TestPGPRHistoryPolicyPin(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, checkpoint, participation := prHistoryFixture(t, storage)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := storage.db(bounded).Begin(bounded)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(tx)
	bound := context.WithValue(bounded, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: storage, repo: checkpoint.RepoID})
	if err := storage.SavePRHistoryCheckpoint(bound, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRParticipation(bound, participation); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- replica.RequireDocumentIdentity(bounded, checkpoint.RepoID, domain.DocumentIdentityRootV1)
	}()
	select {
	case err := <-done:
		t.Fatal("identity opt-in crossed retained publication policy", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := tx.Rollback(bounded); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("stale worker ignored identity opt-in", err)
	}
}

func TestPGPRHistoryJobCorruption(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, kind := range []string{"job-key", "job-scope", "checkpoint-scope", "checkpoint-version"} {
		t.Run(kind, func(t *testing.T) {
			ctx, checkpoint, _ := prHistoryFixture(t, storage)
			job := domain.PRPromotionJob{ID: domain.PRPromotionID(checkpoint.RepoID, checkpoint.PR.Number), RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, PR: checkpoint.PR, State: "completed", CreatedAt: checkpoint.NextAttempt}
			if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
				t.Fatal(err)
			}
			var mutationErr error
			switch kind {
			case "job-key", "job-scope":
				if kind == "job-key" {
					job.PR.Number++
				} else {
					job.RepoID = domain.HashContent([]byte("foreign job"))
				}
				job.ID = domain.PRPromotionID(job.RepoID, job.PR.Number)
				raw, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$2 WHERE repo_id=$1`, checkpoint.RepoID, raw)
			case "checkpoint-scope":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_history_checkpoints SET payload=jsonb_set(payload,'{provider_repository_id}','99') WHERE repo_id=$1`, checkpoint.RepoID)
			case "checkpoint-version":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_history_checkpoints SET version=99 WHERE repo_id=$1`, checkpoint.RepoID)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			jobs, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.NextAttempt, 1)
			if !errors.Is(err, domain.ErrIntegrity) || len(jobs) != 0 {
				t.Fatal("corrupt work selection leaked", jobs, err)
			}
		})
	}
}
