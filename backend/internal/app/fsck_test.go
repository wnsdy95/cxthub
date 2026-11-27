package app

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func newFsckSvc(t *testing.T) (*Service, *store.FSStore) {
	t.Helper()
	st := store.NewFSStore(t.TempDir())
	return NewService(st, st, st), st
}

func hh(c string) domain.ContentHash {
	sum := sha256.Sum256([]byte(c))
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}
