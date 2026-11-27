package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestRepositoryCommandsRequireExplicitAuthority(t *testing.T) {
	svc, _ := newFsckSvc(t)
	_, err := svc.Commit(context.Background(), inbound.CommitInput{RepoID: hh("unbound")})
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("missing actor accepted: %v", err)
	}
}
