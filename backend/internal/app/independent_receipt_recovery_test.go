package app

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Exercises the real application resolver and real FS PutRepo origin enrichment.
// Initialization receipt persistence uses the candidate's existing recording
// adapter; the failing branch precedes the receipt-store call on every adapter.
func TestIndependentBeginReceiptRecoveryAfterOriginEnrichment(t *testing.T) {
	for _, phase := range []string{"unpublished", "modern_first"} {
		t.Run(phase, func(t *testing.T) {
			svc, st, ctx, id, request := initializationFixture(t)
			request.GitRemoteURL = ""
			receipt, err := svc.BeginRepositoryInitialization(ctx, "owner", id, request)
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := svc.BeginRepositoryInitialization(ctx, "owner", id, request)
			if err != nil || !reflect.DeepEqual(receipt, recovered) {
				t.Fatalf("unchanged recovery control: receipt=%+v err=%v", recovered, err)
			}
			registration := receipt.Repo
			registration.GitRemoteURL = "https://git.test/later-origin"
			current, err := svc.EnsureRepo(ctx, "owner", registration)
			if err != nil {
				t.Fatalf("ordinary registration: %v", err)
			}
			if current.GitRemoteURL != registration.GitRemoteURL {
				t.Fatal("fixture did not enrich origin")
			}
			if !reflect.DeepEqual(*st.receipt, receipt) {
				t.Fatal("ordinary registration changed immutable receipt")
			}
			if phase == "modern_first" {
				if err := svc.RecordHistory(ctx, domain.HistoryEvent{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RepoID: string(id), Branch: "feature", BranchID: "review-feature-birth", Kind: "birth", CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatalf("ordinary modern birth: %v", err)
				}
			}
			recovered, err = svc.BeginRepositoryInitialization(ctx, "owner", id, request)
			if err != nil || !reflect.DeepEqual(receipt, recovered) {
				t.Fatalf("exact original Begin must recover immutable receipt after ordinary registration; got ErrGitOriginMismatch=%v, err=%v", errors.Is(err, domain.ErrGitOriginMismatch), err)
			}
		})
	}
}

func TestIndependentBeginReceiptRecoveryWithStableOrigin(t *testing.T) {
	svc, _, ctx, id, request := initializationFixture(t)
	receipt, err := svc.BeginRepositoryInitialization(ctx, "owner", id, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnsureRepo(ctx, "owner", receipt.Repo); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.BeginRepositoryInitialization(ctx, "owner", id, request)
	if err != nil || !reflect.DeepEqual(receipt, recovered) {
		t.Fatalf("stable-origin control: %v", err)
	}
}
