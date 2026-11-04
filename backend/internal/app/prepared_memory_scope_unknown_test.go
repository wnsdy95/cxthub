package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPreparedMemoryScopesRejectUnknownBorrowedTransactionAuthority(test *testing.T) {
	for _, prepare := range []bool{false, true} {
		test.Run(map[bool]string{false: "read", true: "preparation"}[prepare], func(test *testing.T) {
			scope := &preparedMemoryQueryScope{transactional: true}
			checks := 0
			ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				checks++
				return bound, domain.ErrIntegrity
			})
			repo := domain.HashContent([]byte(test.Name()))
			var err error
			if prepare {
				_, err = scope.authorizePreparation(ctx, repo)
			} else {
				err = scope.WithMemoryReadResources(ctx, repo, func(context.Context) error { test.Fatal("unknown transaction entered read consumer"); return nil })
			}
			if !errors.Is(err, domain.ErrCatalogUnsupported) || checks != 0 {
				test.Fatal("unknown borrowed transaction executed general authentication", err, checks)
			}
		})
	}
}
