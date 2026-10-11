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

func TestPGGitContribution(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	t.Run("ledger", func(t *testing.T) { checkGitContributionLedger(t, storage, replica) })
	t.Run("concurrency", func(t *testing.T) { checkGitContributionConcurrency(t, storage, replica) })
	t.Run("policy", func(t *testing.T) { checkGitContributionPolicyAndCancellation(t, storage) })
}

func TestPGGitContributionTransactionRollback(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	before, err := storage.RepositoryRevision(ctx, contribution.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort contribution, memory, and revision")
	assertInvisible := func() {
		t.Helper()
		if rows, err := replica.ListGitContributions(ctx, contribution.RepoID); err != nil || len(rows) != 0 {
			t.Fatal("uncommitted contribution leaked", rows, err)
		}
		if rows, err := replica.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || len(rows) != 0 {
			t.Fatal("uncommitted memory leaked", rows, err)
		}
		if revision, err := replica.RepositoryRevision(ctx, contribution.RepoID); err != nil || revision != before {
			t.Fatal("uncommitted revision leaked", revision, err)
		}
	}
	publish := func(transaction context.Context) error {
		if err := storage.PutGitContribution(transaction, contribution); err != nil {
			return err
		}
		if err := storage.PutGitContributionMemory(transaction, memory); err != nil {
			return err
		}
		if err := storage.AdvanceEvidenceRevision(transaction, contribution.RepoID); err != nil {
			return err
		}
		if rows, err := storage.ListGitContributions(transaction, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContribution{contribution}) {
			t.Fatal("transaction cannot read contribution", rows, err)
		}
		if rows, err := storage.ListGitContributionMemories(transaction, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionMemory{memory}) {
			t.Fatal("transaction cannot read memory", rows, err)
		}
		want := before
		want.Evidence++
		if revision, err := storage.RepositoryRevision(transaction, contribution.RepoID); err != nil || revision != want {
			t.Fatal("transaction cannot read evidence revision", revision, err)
		}
		assertInvisible()
		return nil
	}
	err = storage.WithinRepository(ctx, contribution.RepoID, func(transaction context.Context) error {
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
	if rows, err := replica.ListGitContributions(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContribution{contribution}) {
		t.Fatal("committed contribution missing", rows, err)
	}
	if rows, err := replica.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionMemory{memory}) {
		t.Fatal("committed memory missing", rows, err)
	}
	want := before
	want.Evidence++
	if revision, err := replica.RepositoryRevision(ctx, contribution.RepoID); err != nil || revision != want {
		t.Fatal("committed revision missing or changed graph/pending", revision, err)
	}
}

func TestPGGitContributionSavepointRecovery(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	if _, err := storage.db(ctx).Exec(ctx, `CREATE FUNCTION reject_git_ledger_memory() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected ledger memory failure'; END $$;
 CREATE TRIGGER reject_git_ledger_memory BEFORE INSERT ON git_contribution_memories FOR EACH ROW EXECUTE FUNCTION reject_git_ledger_memory()`); err != nil {
		t.Fatal(err)
	}
	err := storage.WithinRepository(ctx, contribution.RepoID, func(transaction context.Context) error {
		if err := storage.PutGitContribution(transaction, contribution); err != nil {
			return err
		}
		var databaseError *pgconn.PgError
		if err := storage.PutGitContributionMemory(transaction, memory); !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatal("failure injection did not reach memory insert", err)
		}
		if rows, err := storage.ListGitContributions(transaction, contribution.RepoID); err != nil || len(rows) != 1 {
			t.Fatal("nested rollback aborted parent transaction", rows, err)
		}
		if _, err := storage.db(transaction).Exec(transaction, `DROP TRIGGER reject_git_ledger_memory ON git_contribution_memories`); err != nil {
			return err
		}
		return storage.PutGitContributionMemory(transaction, memory)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := replica.ListGitContributionMemories(ctx, contribution.RepoID); err != nil || !reflect.DeepEqual(rows, []domain.GitContributionMemory{memory}) {
		t.Fatal("parent transaction did not recover", rows, err)
	}
}

func TestPGGitContributionForeignTransaction(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	err := storage.WithinRepository(ctx, contribution.RepoID, func(transaction context.Context) error {
		operations := []func() error{
			func() error { return replica.PutGitContribution(transaction, contribution) },
			func() error { return replica.PutGitContributionMemory(transaction, memory) },
			func() error { _, err := replica.ListGitContributions(transaction, contribution.RepoID); return err },
			func() error {
				_, err := replica.ListGitContributionMemories(transaction, contribution.RepoID)
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

func TestPGGitContributionPolicyPin(t *testing.T) {
	storage, replica := prHistoryPGStores(t)
	for _, operation := range []string{"contribution", "memory"} {
		t.Run(operation, func(t *testing.T) {
			ctx, contribution, memory := gitContributionStoreFixture(t, storage)
			if err := storage.PutGitContribution(ctx, contribution); err != nil {
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
				err = storage.PutGitContribution(bound, contribution)
			} else {
				err = storage.PutGitContributionMemory(bound, memory)
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
			if err := storage.PutGitContribution(ctx, contribution); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal("stale worker crossed document identity fence", err)
			}
		})
	}
}

func TestPGGitContributionMemoryConstraints(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, contribution, memory := gitContributionStoreFixture(t, storage)
	if err := storage.PutGitContribution(ctx, contribution); err != nil {
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
			_, err = storage.db(ctx).Exec(ctx, `INSERT INTO git_contribution_memories(repo_id,id,contribution_id,publication_id,memory_hash,memory_source,memory_pinned,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, next.RepoID, next.ID, next.ContributionID, next.PublicationID, next.MemoryHash, next.MemorySource, next.MemoryPinned, raw)
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != code {
				t.Fatal("database invariant not enforced", err)
			}
		})
	}
}

func TestPGGitContributionCorruption(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, kind := range []string{"contribution-key", "contribution-scope", "contribution-payload", "memory-key", "memory-scope", "memory-parent", "memory-publication", "memory-hash", "memory-source", "memory-payload"} {
		t.Run(kind, func(t *testing.T) {
			ctx, contribution, memory := gitContributionStoreFixture(t, storage)
			if err := storage.PutGitContribution(ctx, contribution); err != nil {
				t.Fatal(err)
			}
			if err := storage.PutGitContributionMemory(ctx, memory); err != nil {
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
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_contributions SET payload=$2 WHERE repo_id=$1`, contribution.RepoID, raw)
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
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_contribution_memories SET payload=$2 WHERE repo_id=$1`, memory.RepoID, raw)
			case "memory-publication":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_contribution_memories SET publication_id=$2 WHERE repo_id=$1`, memory.RepoID, strings.Repeat("f", 32))
			case "memory-hash":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_contribution_memories SET memory_hash=$2 WHERE repo_id=$1`, memory.RepoID, domain.HashContent([]byte("other")))
			case "memory-source":
				_, mutationErr = storage.db(ctx).Exec(ctx, `UPDATE git_contribution_memories SET memory_source=$2 WHERE repo_id=$1`, memory.RepoID, domain.HashContent([]byte("other")))
			}
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			var readErr error
			if strings.HasPrefix(kind, "contribution") {
				_, readErr = storage.ListGitContributions(ctx, contribution.RepoID)
				if err := storage.PutGitContribution(ctx, contribution); err == nil {
					t.Fatal("corrupt row overwritten")
				}
			} else {
				_, readErr = storage.ListGitContributionMemories(ctx, contribution.RepoID)
				if err := storage.PutGitContributionMemory(ctx, memory); err == nil {
					t.Fatal("corrupt memory overwritten")
				}
			}
			if !errors.Is(readErr, domain.ErrIntegrity) {
				t.Fatal("corruption not detected", readErr)
			}
		})
	}
}
