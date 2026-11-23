package documentquery

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type sourceReadScope struct {
	Scope
	test *testing.T
}

func (scope sourceReadScope) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened another admitted read")
	return domain.ErrIntegrity
}

func (scope sourceReadScope) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened another snapshot")
	return domain.ErrIntegrity
}
