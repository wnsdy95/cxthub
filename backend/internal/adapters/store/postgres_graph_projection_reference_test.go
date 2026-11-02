//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestGraphProjectionReferenceValidatesBeforeStorage(test *testing.T) {
	storage := &PostgresStore{}
	qualification, result := graphQualificationFixture()
	lease := domain.GraphProjectionLease{RepoID: qualification.RepoID, Dataset: qualification.Dataset, Epoch: qualification.Applied.Epoch,
		Fence: qualification.Fence, Phase: "capture", AppliedSequence: -1, InputSequence: -1, LeaseUntil: qualification.LeaseUntil}
	page := domain.RepositoryViewSourcePage{Version: qualification.Applied.Version, Scope: qualification.Applied.Scope,
		RepoID: lease.RepoID, Epoch: lease.Epoch, Through: qualification.Applied.Sequence, Mode: "baseline", Checkpoint: &qualification.Applied}
	reference := domain.GraphProjectionReference{GraphQualificationKey: qualification.GraphQualificationKey, Expected: result.Expected}
	for _, mutate := range []func(*domain.GraphProjectionReference){
		func(next *domain.GraphProjectionReference) { next.Expected = "invalid" },
		func(next *domain.GraphProjectionReference) { next.Dataset = "33333333-3333-4333-8333-333333333333" },
		func(next *domain.GraphProjectionReference) { next.Applied.Sequence++ },
		func(next *domain.GraphProjectionReference) { next.Applied.Scope = domain.RepositoryViewSourceScope },
		func(next *domain.GraphProjectionReference) { next.Rules++ },
		func(next *domain.GraphProjectionReference) { next.ComparisonRules++ },
	} {
		next := reference
		mutate(&next)
		if err := storage.SaveGraphProjectionPageWithReference(context.Background(), lease, page, next); !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("invalid reference reached storage: %v", err)
		}
	}
	for _, next := range []domain.GraphProjectionLease{
		{RepoID: lease.RepoID, Dataset: lease.Dataset, Epoch: lease.Epoch, Cursor: "continuation"},
		{RepoID: lease.RepoID, Dataset: lease.Dataset, Epoch: lease.Epoch, Phase: "project"},
	} {
		if err := storage.SaveGraphProjectionPageWithReference(context.Background(), next, page, reference); !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("non-first-page reference reached storage: %v", err)
		}
	}
	for _, ctx := range []context.Context{
		outbound.WithDocReadOnly(context.Background()),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage, readOnly: true}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: &PostgresStore{}}),
	} {
		if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); !errors.Is(err, domain.ErrConflict) {
			test.Fatalf("reference save escaped caller transaction: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
}
