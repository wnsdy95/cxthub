package storage

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"path/filepath"
)

// Keep the identity attached to the pointer. A later history fetch must never
// relabel an older local pointer merely because its branch name was reused.
func encodeRef(ref domain.Ref) []byte {
	if ref.Kind == domain.RefBranch && ref.BranchID != "" {
		raw, _ := json.Marshal(ref)
		return append(raw, '\n')
	}
	return []byte(string(ref.Target) + "\n")
}

// AdoptLegacyBranchIdentity binds a pre-identity replica to the first proven
// cloud branch. Reused names are deliberately excluded. It changes no selected
// code, snapshot or memory; each old observation is retained before relabeling.
func (s *FileStore) AdoptLegacyBranchIdentity(ctx context.Context, repo string, remote domain.Ref) error {
	if remote.Kind != domain.RefBranch || remote.BranchID == "" {
		return nil
	}
	return s.withRefMutationLock(ctx, func() error {
		events, err := s.listHistoryEvents(repo)
		if err != nil {
			return err
		}
		state, err := domain.ProjectContextBranches(events)
		if err != nil {
			return err
		}
		if state.Released[remote.Name] != "" || state.Active[remote.Name].ID != remote.BranchID {
			return nil
		}
		legacy := domain.LegacyContextBranchID(repo, remote.Name)
		current, err := s.getRefRaw(ctx, repo, domain.RefBranch, remote.Name)
		if err == domain.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if current.BranchID != "" && current.BranchID != legacy && current.BranchID != remote.BranchID {
			return nil
		}
		current.BranchID = remote.BranchID
		if err := s.putRefRaw(current); err != nil {
			return err
		}
		dir := filepath.Join(s.storeDir(), "worktrees")
		entries, err := readCxtDir(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			owner := *s
			owner.worktreeID = entry.Name()
			p, err := owner.readPosition()
			if err == domain.ErrNotFound {
				continue
			}
			if err != nil {
				return err
			}
			if p.RepoID != repo || p.Branch != remote.Name || (p.BranchID != "" && p.BranchID != legacy) {
				continue
			}
			p.BranchID = remote.BranchID
			p.Selection = nil
			if err := owner.writePosition(p); err != nil {
				return err
			}
		}
		return nil
	})
}
