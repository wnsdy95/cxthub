package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type headReceiptTestStore interface {
	outbound.MetadataStore
	outbound.GitHeadScanStore
	outbound.GitScanStore
	outbound.GitHeadReceiptStore
	RequireDocumentIdentity(context.Context, domain.ContentHash, domain.DocumentIdentity) error
}

func headReceiptFixture(t *testing.T, storage headReceiptTestStore) (context.Context, domain.Repo, time.Time) {
	t.Helper()
	ctx := context.Background()
	repo := domain.Repo{ID: domain.HashContent([]byte(t.Name())), GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	return ctx, repo, time.Now().UTC()
}

func headReceiptObservation(repo domain.Repo, head string) domain.GitRefObservation {
	return domain.GitRefObservation{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "reconciliation", Ref: "refs/heads/main", After: strings.Repeat(head, 40), Provider: &domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91}}.WithID()
}

func checkHeadReceiptOccurrences(t *testing.T, storage, reader headReceiptTestStore) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	var receipts []domain.GitHeadReceipt
	for index, head := range []string{"a", "b", "a"} {
		at := now.Add(time.Duration(index) * 6 * time.Minute)
		claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, at, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		observation := headReceiptObservation(repo, head)
		if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{observation}, false, at); err != nil {
			t.Fatal(err)
		}
		if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{observation}, false, at); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("receipt existence bypassed completed-claim CAS", err)
		}
		receipt, err := reader.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version)
		if err != nil || receipt.Validate() != nil || receipt.Version != claim.Version || receipt.Page != claim.Page || receipt.Heads[0].ObservationID != observation.ID {
			t.Fatal("incorrect occurrence", receipt, err)
		}
		receipts = append(receipts, receipt)
	}
	if receipts[0].ID == receipts[2].ID || receipts[0].Heads[0].ObservationID != receipts[2].Heads[0].ObservationID || receipts[0].Heads[0].ObservationID == receipts[1].Heads[0].ObservationID {
		t.Fatal("A to B to A lost occurrence or content identity")
	}
	var listed []domain.GitHeadReceipt
	for after := int64(0); ; {
		page, err := reader.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if page[0].Version <= after {
			t.Fatal("cursor did not advance")
		}
		listed = append(listed, page...)
		after = page[0].Version
	}
	if !reflect.DeepEqual(listed, receipts) {
		t.Fatal("version cursor lost occurrences", listed)
	}
	jobs, err := reader.ListGitScans(ctx, repo.ID, "", 10)
	if err != nil || len(jobs) != 2 {
		t.Fatal("immutable endpoint jobs duplicated", jobs, err)
	}
	if _, err := reader.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL+"-other", 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("origin scope leak", err)
	}
	if rows, err := reader.ListGitHeadReceipts(ctx, domain.HashContent([]byte("foreign")), repo.GitRemoteURL, 0, 10); err != nil || len(rows) != 0 {
		t.Fatal("repository scope leak", rows, err)
	}
}

func checkHeadReceiptFences(t *testing.T, storage headReceiptTestStore) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	first, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	oldObservation := headReceiptObservation(repo, "a")
	if err := storage.FinishGitHeadScan(ctx, first, []domain.GitRefObservation{oldObservation}, false, first.LeaseUntil); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired worker published", err)
	}
	second, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	newObservation := headReceiptObservation(repo, "b")
	if err := storage.FinishGitHeadScan(ctx, second, []domain.GitRefObservation{newObservation}, true, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGitHeadScan(ctx, first, []domain.GitRefObservation{oldObservation}, false, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("out-of-order stale worker published", err)
	}
	if _, err := storage.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, first.Version); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("stale receipt exists", err)
	}
	rows, err := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Version != second.Version || !rows[0].More {
		t.Fatal("stale completion changed receipt history", rows, err)
	}
	jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
	if err != nil || len(jobs) != 1 || jobs[0].Commit != newObservation.After {
		t.Fatal("stale worker leaked observations/jobs", jobs, err)
	}
}

func checkHeadReceiptValidationAndCompatibility(t *testing.T, storage headReceiptTestStore) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	qualified := headReceiptObservation(repo, "a")
	legacy := qualified
	legacy.Provider, legacy.Ref = nil, "refs/heads/legacy"
	legacy = legacy.WithID()
	for _, page := range [][]domain.GitRefObservation{{qualified, legacy}, {qualified, qualified}} {
		if err := storage.FinishGitHeadScan(ctx, claim, page, false, now); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("invalid page accepted", err)
		}
	}
	jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
	if err != nil || len(jobs) != 0 {
		t.Fatal("validation wrote partial page", jobs, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error {
			return storage.FinishGitHeadScan(canceled, claim, []domain.GitRefObservation{qualified}, false, now)
		},
		func() error { _, err := storage.GetGitHeadReceipt(canceled, repo.ID, repo.GitRemoteURL, 1); return err },
		func() error {
			_, err := storage.ListGitHeadReceipts(canceled, repo.ID, repo.GitRemoteURL, 0, 10)
			return err
		},
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", err)
		}
	}
	if err := storage.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{legacy}, true, now); err != nil {
		t.Fatal(err)
	}
	next, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil || next.Page != 2 {
		t.Fatal(next, err)
	}
	if err := storage.FinishGitHeadScan(ctx, next, nil, false, now); err != nil {
		t.Fatal(err)
	}
	rows, err := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10)
	if err != nil || len(rows) != 0 {
		t.Fatal("legacy/empty page asserted authority", rows, err)
	}
	for _, query := range []struct {
		after int64
		limit int
	}{{-1, 1}, {0, 0}, {0, 102}} {
		if _, err := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, query.after, query.limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid pagination accepted", query, err)
		}
	}
}

func checkHeadReceiptConcurrencyAndPolicy(t *testing.T, storage, peer headReceiptTestStore) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsOut := make(chan error, 2)
	for index, writer := range []headReceiptTestStore{storage, peer} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			observation := headReceiptObservation(repo, fmt.Sprint(index+1))
			errorsOut <- writer.FinishGitHeadScan(ctx, claim, []domain.GitRefObservation{observation}, true, now)
		}()
	}
	wait.Wait()
	close(errorsOut)
	accepted := 0
	for err := range errorsOut {
		if err == nil {
			accepted++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatal("parallel finish count", accepted)
	}
	next, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo.ID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGitHeadScan(ctx, next, []domain.GitRefObservation{headReceiptObservation(repo, "c")}, false, now); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("finish bypassed policy change", err)
	}
	if _, err := storage.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, next.Version); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("policy failure published receipt", err)
	}
}

func TestFSGitHeadReceipts(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	peer := NewFSStore(storage.dataDir)
	t.Run("occurrences", func(t *testing.T) { checkHeadReceiptOccurrences(t, storage, peer) })
	t.Run("fences", func(t *testing.T) { checkHeadReceiptFences(t, storage) })
	t.Run("compatibility", func(t *testing.T) { checkHeadReceiptValidationAndCompatibility(t, storage) })
	t.Run("concurrency-policy", func(t *testing.T) { checkHeadReceiptConcurrencyAndPolicy(t, storage, peer) })
}
