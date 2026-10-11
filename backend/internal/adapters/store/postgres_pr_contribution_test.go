//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGPRContribution(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	t.Run("ledger", func(t *testing.T) { checkPRContributionLedger(t, storage, replica) })
	t.Run("concurrency", func(t *testing.T) { checkPRContributionConcurrency(t, storage, replica) })
	t.Run("policy", func(t *testing.T) { checkPRContributionPolicyAndCancellation(t, storage) })
	t.Run("observation-lookup", func(t *testing.T) { checkPRRefObservationLookup(t, storage) })
}

func TestPGPRContributionTransactionRollback(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	observation := prContributionObservation(contribution)
	failure := errors.New("abort contribution, memory, and witness")
	assertInvisible := func() {
		t.Helper()
		if rows, err := replica.ListPRContributions(ctx, contribution.RepoID); err != nil || len(rows) != 0 {
			t.Fatal("uncommitted contribution leaked", rows, err)
		}
		if rows, err := replica.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || len(rows) != 0 {
			t.Fatal("uncommitted memory leaked", rows, err)
		}
		if _, err := replica.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("uncommitted witness leaked", err)
		}
	}
	publish := func(transaction context.Context) error {
		if err := storage.PutPRContribution(transaction, contribution); err != nil {
			return err
		}
		if err := storage.PutPRContributionMemory(transaction, memory); err != nil {
			return err
		}
		if err := storage.RecordGitRefObservation(transaction, observation, time.Now().UTC()); err != nil {
			return err
		}
		if rows, err := storage.ListPRContributions(transaction, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContribution{contribution}) {
			t.Fatal("transaction cannot read contribution", rows, err)
		}
		if rows, err := storage.ListPRContributionMemories(transaction, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContributionMemory{memory}) {
			t.Fatal("transaction cannot read memory", rows, err)
		}
		if got, err := storage.GetPRRefObservation(transaction, contribution.RepoID, contribution.GitOrigin, observation.ID); err != nil || !reflect.DeepEqual(got, observation) {
			t.Fatal("transaction cannot read witness", got, err)
		}
		assertInvisible()
		return nil
	}
	err := storage.WithinRepository(ctx, contribution.RepoID, func(transaction context.Context) error {
		if err := publish(transaction); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	assertInvisible()
	if err := storage.WithinRepository(ctx, contribution.RepoID, publish); err != nil {
		t.Fatal("retry after rollback", err)
	}
	if rows, err := replica.ListPRContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContribution{contribution}) {
		t.Fatal("committed contribution missing", rows, err)
	}
	if rows, err := replica.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContributionMemory{memory}) {
		t.Fatal("committed memory missing", rows, err)
	}
	if got, err := replica.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID); err != nil || !reflect.DeepEqual(got, observation) {
		t.Fatal("committed witness missing", got, err)
	}
}

func TestPGPRContributionSavepointRecovery(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	if _, err := storage.db(ctx).Exec(ctx, `CREATE FUNCTION reject_ledger_memory() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected ledger memory failure'; END $$;
 CREATE TRIGGER reject_ledger_memory BEFORE INSERT ON pr_contribution_memories FOR EACH ROW EXECUTE FUNCTION reject_ledger_memory()`); err != nil {
		t.Fatal(err)
	}
	err := storage.WithinRepository(ctx, contribution.RepoID, func(transaction context.Context) error {
		if err := storage.PutPRContribution(transaction, contribution); err != nil {
			return err
		}
		var databaseError *pgconn.PgError
		if err := storage.PutPRContributionMemory(transaction, memory); !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatal("failure injection did not reach memory insert", err)
		}
		if rows, err := storage.ListPRContributions(transaction, contribution.RepoID); err != nil || len(rows) != 1 {
			t.Fatal("nested rollback aborted parent transaction", rows, err)
		}
		if _, err := storage.db(transaction).Exec(transaction, `DROP TRIGGER reject_ledger_memory ON pr_contribution_memories`); err != nil {
			return err
		}
		return storage.PutPRContributionMemory(transaction, memory)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := replica.ListPRContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.PRContributionMemory{memory}) {
		t.Fatal("parent transaction did not recover", rows, err)
	}
}

func TestPGPRContributionForeignTransaction(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	err := storage.WithinRepository(ctx, contribution.RepoID, func(transaction context.Context) error {
		operations := []func() error{
			func() error { return replica.PutPRContribution(transaction, contribution) },
			func() error { return replica.PutPRContributionMemory(transaction, memory) },
			func() error { _, err := replica.ListPRContributions(transaction, contribution.RepoID); return err },
			func() error {
				_, err := replica.ListPRContributionMemories(transaction, contribution.RepoID)
				return err
			},
			func() error {
				_, err := replica.GetPRRefObservation(transaction, contribution.RepoID, contribution.GitOrigin, strings.Repeat("e", 64))
				return err
			},
		}
		for index, operation := range operations {
			if err := operation(); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("operation escaped owning transaction", index, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPGPRContributionPolicyPin(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	for _, operation := range []string{"contribution", "memory"} {
		t.Run(operation, func(t *testing.T) {
			ctx, contribution, memory := prContributionStoreFixture(t, storage)
			if err := storage.PutPRContribution(ctx, contribution); err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			tx, err := storage.db(bounded).Begin(bounded)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(tx)
			bound := context.WithValue(bounded, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: storage, repo: contribution.RepoID})
			if operation == "contribution" {
				err = storage.PutPRContribution(bound, contribution)
			} else {
				err = storage.PutPRContributionMemory(bound, memory)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- replica.RequireDocumentIdentity(bounded, contribution.RepoID, domain.DocumentIdentityRootV1)
			}()
			select {
			case err := <-done:
				t.Fatal("identity opt-in crossed retained ledger fence", err)
			case <-time.After(40 * time.Millisecond):
			}
			if err := tx.Rollback(bounded); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := storage.PutPRContribution(ctx, contribution); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal("stale worker crossed document identity fence", err)
			}
		})
	}
}

func TestPGPRContributionMemoryConstraints(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, contribution, memory := prContributionStoreFixture(t, storage)
	if err := storage.PutPRContribution(ctx, contribution); err != nil {
		t.Fatal(err)
	}
	foreignRepo := domain.HashContent([]byte(t.Name() + "/foreign"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreignRepo}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"foreign-parent", "missing-parent", "unpinned", "missing-source", "publication", "memory-hash"} {
		t.Run(kind, func(t *testing.T) {
			next := memory
			code := "23514"
			switch kind {
			case "foreign-parent":
				next.RepoID = foreignRepo
				code = "23503"
			case "missing-parent":
				next.ContributionID = strings.Repeat("e", 64)
				code = "23503"
			case "unpinned":
				next.MemoryPinned = false
			case "missing-source":
				next.MemorySource = ""
			case "publication":
				next.PublicationID = strings.Repeat("A", 32)
			case "memory-hash":
				next.MemoryHash = "bad"
			}
			next = next.WithID()
			raw, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			_, err = storage.db(ctx).Exec(ctx, `INSERT INTO pr_contribution_memories(repo_id,id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, next.RepoID, next.ID, next.ContributionID, next.PublicationID, next.MemoryHash, next.MemorySource, next.MemoryPinned, raw)
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != code {
				t.Fatal("database invariant not enforced", err)
			}
		})
	}
}

func TestPGPRContributionCorruption(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, kind := range []string{"contribution-key", "contribution-scope", "contribution-payload", "memory-key", "memory-scope", "memory-parent", "memory-publication", "memory-hash", "memory-source", "memory-payload", "observation-key", "observation-scope", "observation-origin", "observation-payload"} {
		t.Run(kind, func(t *testing.T) {
			ctx, contribution, memory := prContributionStoreFixture(t, storage)
			if err := storage.PutPRContribution(ctx, contribution); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutPRContributionMemory(ctx, memory); err != nil {
				t.Fatal(err)
			}
			observation := prContributionObservation(contribution)
			if err := storage.RecordGitRefObservation(ctx, observation, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			var mutationErr error
			switch kind {
			case "contribution-key", "contribution-scope", "contribution-payload", "memory-parent":
				other := contribution
				other.Capture = domain.HashContent([]byte("other"))
				if kind == "contribution-scope" {
					other.RepoID = domain.HashContent([]byte("foreign"))
				}
				value := any(other.WithID())
				if kind == "contribution-payload" {
					value = struct{}{}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_contributions SET payload=$2 WHERE repo_id=$1`, contribution.RepoID, raw)
			case "memory-key", "memory-scope", "memory-payload":
				other := memory
				other.MemoryHash = domain.HashContent([]byte("other"))
				if kind == "memory-scope" {
					other.RepoID = domain.HashContent([]byte("foreign"))
				}
				value := any(other.WithID())
				if kind == "memory-payload" {
					value = struct{}{}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_contribution_memories SET payload=$2 WHERE repo_id=$1`, contribution.RepoID, raw)
			case "memory-publication":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_contribution_memories SET publication_id=$2 WHERE repo_id=$1`, contribution.RepoID, strings.Repeat("f", 32))
			case "memory-hash":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_contribution_memories SET memory_hash=$2 WHERE repo_id=$1`, contribution.RepoID, domain.HashContent([]byte("other")))
			case "memory-source":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE pr_contribution_memories SET memory_source=$2 WHERE repo_id=$1`, contribution.RepoID, domain.HashContent([]byte("other")))
			default:
				other := observation
				switch kind {
				case "observation-key":
					other.Delivery = "other"
				case "observation-scope":
					other.RepoID = domain.HashContent([]byte("foreign"))
				case "observation-origin":
					other.GitOrigin += "-foreign"
				}
				value := any(other.WithID())
				if kind == "observation-payload" {
					value = struct{}{}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_ref_observations SET payload=$2 WHERE repo_id=$1`, contribution.RepoID, raw)
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			var readErr error
			switch {
			case strings.HasPrefix(kind, "contribution"):
				_, readErr = storage.ListPRContributions(ctx, contribution.RepoID)
				if err := storage.PutPRContribution(ctx, contribution); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt contribution overwritten", err)
				}
			case strings.HasPrefix(kind, "memory"):
				_, readErr = storage.ListPRContributionMemories(ctx, contribution.RepoID)
				if err := storage.PutPRContributionMemory(ctx, memory); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt memory overwritten", err)
				}
			default:
				_, readErr = storage.GetPRRefObservation(ctx, contribution.RepoID, contribution.GitOrigin, observation.ID)
			}
			if !errors.Is(readErr, domain.ErrIntegrity) {
				t.Fatal("corrupt row trusted", readErr)
			}
		})
	}
}
