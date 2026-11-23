package store

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"path/filepath"
)

func encodeRef(ref domain.Ref) []byte {
	if ref.Kind == domain.RefBranch && ref.BranchID != "" {
		raw, _ := json.Marshal(ref)
		return append(raw, '\n')
	}
	return []byte(string(ref.Target) + "\n")
}

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

func (s *FSStore) applyProtocolHistory(ctx context.Context, e domain.HistoryEvent) error {
	repo := domain.ContentHash(e.RepoID)
	protocol, err := s.contextProtocol(repo)
	if err != nil || protocol == 0 {
		return err
	}
	switch e.Kind {
	case "birth":
		if e.Target != "" {
			return writeAtomic(s.refFile(repo, domain.RefBranch, e.Branch), encodeRef(domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Target}))
		}
	case "rename":
		if e.Source != "" {
			if err := writeAtomic(s.refFile(repo, domain.RefBranch, e.Branch), encodeRef(domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Source})); err != nil {
				return err
			}
		}
		if err := removeFileDurable(s.refFile(repo, domain.RefBranch, e.PreviousBranch)); err != nil {
			return err
		}
		head, err := s.getRefRaw(ctx, repo, domain.RefHead, domain.HeadRefName)
		if err == nil && head.Symbolic == e.PreviousBranch {
			return writeAtomic(s.refFile(repo, domain.RefHead, domain.HeadRefName), []byte("ref: refs/heads/"+e.Branch+"\n"))
		}
		if err != nil && err != domain.ErrNotFound {
			return err
		}
	case "archive":
		if e.Source != "" {
			if err := s.detachHeadFromBranchRaw(ctx, repo, e.Branch, e.Source); err != nil {
				return err
			}
		}
		return removeFileDurable(s.refFile(repo, domain.RefBranch, e.Branch))
	}
	return nil
}
