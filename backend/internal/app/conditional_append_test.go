package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestConditionalAppendRequiresTransactionalStore(t *testing.T) {
	service, storage, ref := contextWriteService(t)
	before, err := storage.GetRef(systemTestContext(), ref.RepoID, ref.Kind, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	input := inbound.UpdateRefInput{RepoID: ref.RepoID, Ref: ref, Append: true, ExpectedTarget: ref.Target, ExpectedRevision: &domain.RepositoryRevision{}}
	if _, err := service.UpdateRef(systemTestContext(), input); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("nontransactional store accepted conditional append: %v", err)
	}
	if _, err := service.UpdateRef(context.Background(), input); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("authorization must precede condition checks: %v", err)
	}
	after, err := storage.GetRef(systemTestContext(), ref.RepoID, ref.Kind, ref.Name)
	if err != nil || after != before {
		t.Fatalf("conditional rejection changed ref: %+v %v", after, err)
	}
}
