//go:build postgres

package store

import (
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
)

func TestP6PGRequirementPersistenceAndGuard(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityLegacy); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("downgrade", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE repos SET required_doc_identity='' WHERE id=$1`, string(repo)); err == nil {
		t.Fatal("SQL downgrade bypassed migration trigger")
	}
	if err := s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentity("future")); err == nil {
		t.Fatal("unknown accepted")
	}
	if err := s.UpdateRepoAbout(ctx, repo, "other", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRepo(ctx, repo)
	if err != nil || r.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal("requirement lost", r, err)
	}
	if _, err := s.PutRepo(ctx, domain.Repo{ID: domain.HashContent([]byte(t.Name())), RequiredDocIdentity: domain.DocumentIdentityRootV1}); err == nil {
		t.Fatal("registration opted in")
	}
}
