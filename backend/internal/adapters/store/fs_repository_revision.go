package store

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"path/filepath"
	"sync"
)

var revisionMu sync.Mutex

func (s *FSStore) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.RepositoryRevision{}, err
	}
	var v domain.RepositoryRevision
	b, err := os.ReadFile(filepath.Join(s.repoDir(repo), "view-revision.json"))
	if os.IsNotExist(err) {
		return v, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	return v, err
}
func (s *FSStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	revisionMu.Lock()
	defer revisionMu.Unlock()
	v, err := s.RepositoryRevision(ctx, repo)
	if err != nil {
		return err
	}
	if pending {
		v.Pending++
	} else {
		v.Graph++
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.repoDir(repo), "view-revision.json"), b)
}
