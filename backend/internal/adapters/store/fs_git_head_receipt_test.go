package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSGitHeadReceiptReopenAndCorruption(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	observation := headReceiptObservation(repo, "a")
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{observation}, false, now); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFSStore(storage.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := reopened.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version)
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(storage.dataDir, "git-observations"))
	if err != nil || len(files) != 1 {
		t.Fatal("immutable observation count", len(files), err)
	}
	corrupt := receipt
	corrupt.RepoID = domain.HashContent([]byte("foreign"))
	corrupt.Heads = append([]domain.GitHeadReceiptHead(nil), receipt.Heads...)
	foreignObservation := observation
	foreignObservation.RepoID = corrupt.RepoID
	corrupt.Heads[0].ObservationID = foreignObservation.WithID().ID
	corrupt = corrupt.WithID()
	raw, _ := json.Marshal(corrupt)
	if err := writeAtomic(storage.gitHeadReceiptPath(repo.ID, repo.GitRemoteURL, claim.Version), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("cross-scope receipt read", err)
	}
	if _, err := reopened.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("cross-scope receipt listed", err)
	}
}

func TestFSGitHeadReceiptCollisionAndPreflightFailure(t *testing.T) {
	storage := NewFSStore(t.TempDir())
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
	if err := storage.putGitHeadReceiptFS(ctx, *receipt); err != nil {
		t.Fatal(err)
	}
	changed := headReceiptObservation(repo, "b")
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{changed}, false, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("immutable occurrence overwritten", err)
	}
	if checkpoint, err := storage.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); err != nil || !reflect.DeepEqual(checkpoint, claim) {
		t.Fatal("collision advanced checkpoint", checkpoint, err)
	}
	if jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10); err != nil || len(jobs) != 0 {
		t.Fatal("collision leaked endpoint work", jobs, err)
	}
	if err := os.Remove(storage.gitHeadReceiptPath(repo.ID, repo.GitRemoteURL, claim.Version)); err != nil {
		t.Fatal(err)
	}
	blocked := storage.gitHeadReceiptPath(repo.ID, repo.GitRemoteURL, claim.Version)
	if err := os.Mkdir(blocked, 0755); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{observation}, false, now); err == nil {
		t.Fatal("receipt path failure ignored")
	}
	if checkpoint, err := storage.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); err != nil || !reflect.DeepEqual(checkpoint, claim) {
		t.Fatal("preflight receipt failure changed checkpoint", checkpoint, err)
	}
}
