package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type deleteFailStore struct {
	*store.FSStore
	deleteSnapshotCalls int
	deleteDocCalls      int
}

func (s *deleteFailStore) DeleteSnapshot(context.Context, domain.ContentHash, domain.ContentHash) error {
	s.deleteSnapshotCalls++
	return errors.New("injected snapshot delete failure")
}

func (s *deleteFailStore) DeleteDoc(ctx context.Context, repoID, hash domain.ContentHash) error {
	s.deleteDocCalls++
	return s.FSStore.DeleteDoc(ctx, repoID, hash)
}

func newFsckSvc(t *testing.T) (*Service, *store.FSStore) {
	t.Helper()
	st := store.NewFSStore(t.TempDir())
	return NewService(st, st, auth.NewTeamTokenAuth(), st), st
}

func hh(c string) domain.ContentHash {
	sum := sha256.Sum256([]byte(c))
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}
