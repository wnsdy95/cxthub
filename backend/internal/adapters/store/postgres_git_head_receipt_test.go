//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGitHeadReceipts(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	t.Run("occurrences", func(t *testing.T) {
		checkHeadReceiptOccurrences(t, storage, peer)
		repo := domain.HashContent([]byte(t.Name()))
		var count int
		if err := storage.pool.QueryRow(context.Background(), `SELECT count(*) FROM git_ref_observations WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 2 {
			t.Fatal("A to B to A duplicated immutable observations", count, err)
		}
	})
	t.Run("fences", func(t *testing.T) { checkHeadReceiptFences(t, storage) })
	t.Run("compatibility", func(t *testing.T) { checkHeadReceiptValidationAndCompatibility(t, storage) })
	t.Run("concurrency-policy", func(t *testing.T) { checkHeadReceiptConcurrencyAndPolicy(t, storage, peer) })
}

func TestPGGitHeadReceiptOuterRollbackAndCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		name := "rollback"
		if canceled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, repo, now := headReceiptFixture(t, storage)
			claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			before, err := storage.RepositoryRevision(ctx, repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			observation := headReceiptObservation(repo, "a")
			expected, err := domain.NewGitHeadReceipt(claim, []domain.GitRefObservation{observation}, false)
			if err != nil {
				t.Fatal(err)
			}
			abort := errors.New("abort occurrence")
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			err = storage.WithinRepository(work, repo.ID, func(transaction context.Context) error {
				if err := storage.FinishGitHeadScan(transaction, claim, []domain.GitRefObservation{observation}, false, now); err != nil {
					return err
				}
				if err := storage.AdvanceEvidenceRevision(transaction, repo.ID); err != nil {
					return err
				}
				got, err := storage.GetGitHeadReceipt(transaction, repo.ID, repo.GitRemoteURL, claim.Version)
				if err != nil || !reflect.DeepEqual(got, *expected) {
					t.Fatal("transaction cannot read receipt", got, err)
				}
				rows, err := storage.ListGitHeadReceipts(transaction, repo.ID, repo.GitRemoteURL, 0, 10)
				if err != nil || !reflect.DeepEqual(rows, []domain.GitHeadReceipt{*expected}) {
					t.Fatal("transaction list escaped handle", rows, err)
				}
				if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted receipt visible", err)
				}
				if canceled {
					cancel()
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
			for _, table := range []string{"git_ref_observations", "git_scan_jobs", "git_head_receipts"} {
				var count int
				if err := peer.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo.ID).Scan(&count); err != nil || count != 0 {
					t.Fatal("publication escaped outer rollback", table, count, err)
				}
			}
			after, err := peer.RepositoryRevision(ctx, repo.ID)
			if err != nil || after != before {
				t.Fatal("revision escaped rollback", before, after, err)
			}
			checkpoint, err := peer.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL)
			if err != nil || checkpoint.State != "running" || checkpoint.Version != claim.Version || !checkpoint.LeaseUntil.Equal(claim.LeaseUntil) {
				t.Fatal("checkpoint escaped rollback", checkpoint, err)
			}
			if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{observation}, false, now); err != nil {
				t.Fatal("rolled-back live claim could not retry", err)
			}
			if got, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); err != nil || !reflect.DeepEqual(got, *expected) {
				t.Fatal("retry lost receipt", got, err)
			}
		})
	}
}

func TestPGGitHeadReceiptNestedCheckpointFailure(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `CREATE FUNCTION reject_head_receipt_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'checkpoint failure'; END $$;
CREATE TRIGGER reject_head_receipt_checkpoint BEFORE UPDATE ON git_head_scans FOR EACH ROW EXECUTE FUNCTION reject_head_receipt_checkpoint()`); err != nil {
		t.Fatal(err)
	}
	observation := headReceiptObservation(repo, "a")
	err = storage.WithinRepository(ctx, repo.ID, func(transaction context.Context) error {
		if err := storage.FinishGitHeadScan(transaction, claim, []domain.GitRefObservation{observation}, false, now); err == nil {
			t.Fatal("injected checkpoint failure not returned")
		}
		for _, table := range []string{"git_ref_observations", "git_scan_jobs", "git_head_receipts"} {
			var count int
			if err := storage.db(transaction).QueryRow(transaction, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("savepoint did not roll back page", table, count, err)
			}
		}
		if _, err := storage.db(transaction).Exec(transaction, `DROP TRIGGER reject_head_receipt_checkpoint ON git_head_scans`); err != nil {
			return err
		}
		if err := storage.FinishGitHeadScan(transaction, claim, []domain.GitRefObservation{observation}, false, now); err != nil {
			return err
		}
		return storage.AdvanceEvidenceRevision(transaction, repo.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); err != nil {
		t.Fatal("recovered savepoint receipt missing", err)
	}
}

func TestPGGitHeadReceiptCollisionAndCorruption(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	observation := headReceiptObservation(repo, "a")
	receipt, err := domain.NewGitHeadReceipt(claim, []domain.GitRefObservation{observation}, false)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	for range 2 {
		if err := putGitHeadReceiptPG(ctx, transaction, *receipt); err != nil {
			t.Fatal("identical payload rejected", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	changed := headReceiptObservation(repo, "b")
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{changed}, false, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("immutable occurrence overwritten", err)
	}
	if checkpoint, err := peer.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); err != nil || !reflect.DeepEqual(checkpoint, claim) {
		t.Fatal("collision advanced checkpoint", checkpoint, err)
	}
	for _, table := range []string{"git_ref_observations", "git_scan_jobs"} {
		var count int
		if err := peer.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("collision leaked endpoint work", table, count, err)
		}
	}
	if got, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); err != nil || !reflect.DeepEqual(got, *receipt) {
		t.Fatal("collision altered original receipt", got, err)
	}
	corrupt := *receipt
	corrupt.RepoID = domain.HashContent([]byte("foreign"))
	corrupt.Heads = append([]domain.GitHeadReceiptHead(nil), receipt.Heads...)
	foreignObservation := observation
	foreignObservation.RepoID = corrupt.RepoID
	corrupt.Heads[0].ObservationID = foreignObservation.WithID().ID
	corrupt = corrupt.WithID()
	raw, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE git_head_receipts SET id=$1,payload=$2 WHERE repo_id=$3 AND origin=$4 AND version=$5`, corrupt.ID, raw, repo.ID, repo.GitRemoteURL, claim.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("cross-scope receipt read", err)
	}
	if _, err := peer.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("cross-scope receipt listed", err)
	}
}
