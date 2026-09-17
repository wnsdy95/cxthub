package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *FSStore) CompareAndSwapSecrets(ctx context.Context, repo domain.ContentHash, expected, next []byte) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	lock := s.refLock(repo, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
	old, err := s.GetSecretsEnvelope(ctx, repo)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if !bytes.Equal(old, expected) {
		return domain.ErrRefConflict
	}
	return writeAtomic(filepath.Join(s.repoDir(repo), "secrets.enc.json"), next)
}
