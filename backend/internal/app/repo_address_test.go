package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Moving a repository's address rewrites the database it is stored in, which
// only the PostgreSQL store does; any other store says so before anything is
// read.
func TestMovingARepositoryAddressNeedsThePostgreSQLStore(t *testing.T) {
	filesystem := store.NewFSStore(t.TempDir())
	_, err := NewService(filesystem, filesystem, nil).MoveRepoAddress(context.Background(), "http://localhost:5173/acme/app", "https://cxthub.example/acme/app", false)
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "needs the PostgreSQL store") {
		t.Fatalf("the file store: %v", err)
	}
}
