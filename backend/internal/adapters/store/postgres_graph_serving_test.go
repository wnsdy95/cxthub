//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestGraphServingRepairRejectsWrongPortAndBorrowedContexts(t *testing.T) {
	storage := &PostgresStore{}
	repo, read := graphProjectionRepairFixture()
	failure := *graphProjectionOutputCorruption(context.Background(), repo, read, domain.ErrIntegrity)
	if err := storage.RequestGraphServingRepair(context.Background(), failure); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("candidate receipt reached serving repair: %v", err)
	}
	failure.Serving, failure.ServingFence = true, 7
	if err := storage.RequestGraphProjectionRepair(context.Background(), failure); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("serving receipt reached candidate repair: %v", err)
	}
	for _, bound := range []context.Context{
		outbound.WithDocReadOnly(context.Background()),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage, readOnly: true}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: &PostgresStore{}}),
	} {
		if err := storage.RequestGraphServingRepair(bound, failure); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("serving repair borrowed caller transaction: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.RequestGraphServingRepair(canceled, failure); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled serving repair reached database: %v", err)
	}
	failure.ServingFence = 0
	if err := storage.RequestGraphServingRepair(context.Background(), failure); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unfenced serving repair reached database: %v", err)
	}
}
