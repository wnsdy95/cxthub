package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type metadataOnlyProjectionStore struct {
	outbound.MetadataStore
}

func (storage *metadataOnlyProjectionStore) InReadOnlyTransaction(context.Context) bool {
	return true
}

func TestRepositoryCommandsRequireExplicitAuthority(t *testing.T) {
	svc, _ := newFsckSvc(t)
	_, err := svc.Commit(context.Background(), inbound.CommitInput{RepoID: hh("unbound")})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("missing actor accepted: %v", err)
	}
}
