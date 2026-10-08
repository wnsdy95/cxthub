package store

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"path/filepath"
)

func (s *FSStore) RequireDocumentIdentity(ctx context.Context, repo domain.ContentHash, next domain.DocumentIdentity) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := s.refLock(repo, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
	r, err := s.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if err = domain.ValidateDocumentIdentityRequirement(r.RequiredDocIdentity, next); err != nil {
		return err
	}
	if r.RequiredDocIdentity == next {
		return nil
	}
	r.RequiredDocIdentity = next
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.repoDir(repo), "repo.json"), raw)
}
