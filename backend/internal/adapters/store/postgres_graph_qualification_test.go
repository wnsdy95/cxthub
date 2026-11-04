//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func graphQualificationFixture() (domain.GraphQualificationLease, domain.GraphQualificationResult) {
	repo := domain.HashContent([]byte("graph qualification"))
	key := domain.GraphQualificationKey{RepoID: repo, Dataset: "11111111-1111-4111-8111-111111111111",
		Applied: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope,
			RepoID: repo, Epoch: "22222222-2222-4222-8222-222222222222", Sequence: 7}, Rules: domain.GraphViewProjectionRules, ComparisonRules: domain.GraphQualificationRules}
	return domain.GraphQualificationLease{GraphQualificationKey: key, Fence: 1, LeaseUntil: time.Now().Add(time.Minute)},
		domain.GraphQualificationResult{GraphQualificationKey: key, Expected: domain.HashContent([]byte("expected")), Actual: domain.HashContent([]byte("actual"))}
}

func TestGraphQualificationRejectsBorrowedAndReadOnlyWrites(t *testing.T) {
	storage := &PostgresStore{}
	lease, result := graphQualificationFixture()
	for _, ctx := range []context.Context{
		outbound.WithDocReadOnly(context.Background()),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage, readOnly: true}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: &PostgresStore{}}),
	} {
		for _, operation := range []func() error{
			func() error { return storage.ConfigureGraphQualification(ctx, lease.RepoID, true) },
			func() error { _, err := storage.ClaimGraphQualification(ctx, lease.RepoID, time.Minute); return err },
			func() error { return storage.FinishGraphQualification(ctx, lease, result) },
			func() error { return storage.FailGraphQualification(ctx, lease, "read_error") },
		} {
			if err := operation(); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("qualification escaped caller context: %v", err)
			}
		}
	}
}

func TestGraphQualificationValidatesInputsBeforeStorage(t *testing.T) {
	storage := &PostgresStore{}
	lease, result := graphQualificationFixture()
	ctx := context.Background()
	for _, duration := range []time.Duration{0, time.Second - 1, 5*time.Minute + 1} {
		if _, err := storage.ClaimGraphQualification(ctx, lease.RepoID, duration); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid duration reached storage: %v", err)
		}
	}
	for _, reason := range []string{"", "matched", "mismatch", "unexpected details"} {
		if err := storage.FailGraphQualification(ctx, lease, reason); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("unbounded failure reason reached storage: %v", err)
		}
	}
	for _, mutate := range []func(*domain.GraphQualificationResult){
		func(next *domain.GraphQualificationResult) { next.Expected = "invalid" },
		func(next *domain.GraphQualificationResult) { next.Actual = "" },
		func(next *domain.GraphQualificationResult) { next.Dataset = "33333333-3333-4333-8333-333333333333" },
		func(next *domain.GraphQualificationResult) { next.Applied.Sequence++ },
		func(next *domain.GraphQualificationResult) { next.ComparisonRules++ },
	} {
		next := result
		mutate(&next)
		if err := storage.FinishGraphQualification(ctx, lease, next); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid result reached storage: %v", err)
		}
	}
	if err := storage.FinishGraphQualification(ctx, domain.GraphQualificationLease{}, result); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	if err := storage.ConfigureGraphQualification(ctx, "bad repo", true); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal(err)
	}
	if _, err := storage.ReadGraphQualification(ctx, domain.GraphQualificationKey{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error { return storage.ConfigureGraphQualification(ctx, lease.RepoID, true) },
		func() error { _, err := storage.ClaimGraphQualification(ctx, lease.RepoID, time.Minute); return err },
		func() error { return storage.FinishGraphQualification(ctx, lease, result) },
		func() error { return storage.FailGraphQualification(ctx, lease, "not_current") },
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request reached storage: %v", err)
		}
	}
}
