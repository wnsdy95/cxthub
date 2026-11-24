package store

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"path/filepath"
)

func (s *FSStore) contextProtocol(repo domain.ContentHash) (int, error) {
	raw, err := os.ReadFile(filepath.Join(s.repoDir(repo), "context-protocol"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if string(raw) != "1\n" {
		return 0, domain.ErrIntegrity
	}
	return 1, nil
}
