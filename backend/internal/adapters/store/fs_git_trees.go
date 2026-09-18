package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitTreeStore = (*FSStore)(nil)

func (s *FSStore) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	var c domain.GitCommitTree
	err := readJSON(s.gitEvidencePath("git-commit-trees", repo, origin+":"+commit), &c)
	if errors.Is(err, os.ErrNotExist) {
		err = domain.ErrNotFound
	}
	if err == nil {
		err = c.Validate()
		if c.Commit != commit {
			err = domain.ErrIntegrity
		}
	}
	return c, err
}
func (s *FSStore) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, oid string) (domain.GitTreeNode, error) {
	var n domain.GitTreeNode
	err := readJSON(s.gitEvidencePath("git-tree-nodes", repo, origin+":"+oid), &n)
	if errors.Is(err, os.ErrNotExist) {
		err = domain.ErrNotFound
	}
	if err == nil {
		err = n.Validate()
		if n.OID != oid {
			err = domain.ErrIntegrity
		}
	}
	return n, err
}

// Caller holds the scan publication lock. Nodes precede the commit binding;
// a crash can leave reusable objects, never a complete-looking partial tree.
func (s *FSStore) writeGitTreeFS(repo domain.ContentHash, origin string, evidence domain.GitTreeEvidence) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	for _, n := range evidence.Nodes {
		old, err := s.GetGitTreeNode(context.Background(), repo, origin, n.OID)
		if err == nil {
			if !reflect.DeepEqual(n, old) {
				return domain.ErrIntegrity
			}
			continue
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		b, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if err = writeAtomic(s.gitEvidencePath("git-tree-nodes", repo, origin+":"+n.OID), b); err != nil {
			return err
		}
	}
	c := evidence.Commit
	old, err := s.GetGitCommitTree(context.Background(), repo, origin, c.Commit)
	if err == nil {
		if !reflect.DeepEqual(c, old) {
			return domain.ErrIntegrity
		}
		return nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(s.gitEvidencePath("git-commit-trees", repo, origin+":"+c.Commit), b)
}
