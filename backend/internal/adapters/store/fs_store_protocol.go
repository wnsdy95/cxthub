package store

import (
	"context"
	"encoding/json"
	"fmt"
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

func (s *FSStore) validateContextWrite(ctx context.Context, repo domain.ContentHash, next domain.Ref) error {
	protocol, err := s.contextProtocol(repo)
	if err != nil || protocol == 0 {
		return err
	}
	events, err := s.listHistoryEventsRaw(repo)
	if err != nil {
		return err
	}
	var current *domain.Ref
	ref, err := s.getRefRaw(ctx, repo, next.Kind, next.Name)
	if err == nil {
		current = &ref
	} else if err != domain.ErrNotFound {
		return err
	}
	return domain.ValidateContextRefWrite(protocol, events, current, next)
}

func (s *FSStore) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	lock := s.refLock(repo, domain.RefBranch, "")
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryEvent(ctx, repo); err != nil {
		return err
	}
	if _, err := s.GetRepo(ctx, repo); err != nil {
		return err
	}
	version, err := s.contextProtocol(repo)
	if err != nil || version == 1 {
		return err
	}
	if _, err := os.Stat(s.joinJournalPath(repo)); err == nil {
		return fmt.Errorf("%w: graph transaction recovery required", domain.ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	events, err := s.listHistoryEventsRaw(repo)
	if err != nil {
		return err
	}
	refs, err := s.listRefsRaw(ctx, repo)
	if err != nil {
		return err
	}
	next, err := domain.ContextProtocolRefs(repo, refs, events)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, ref := range next {
		live[ref.Name] = true
	}
	for _, ref := range refs {
		if ref.Kind == domain.RefBranch && !live[ref.Name] {
			if err := removeFileDurable(s.refFile(repo, ref.Kind, ref.Name)); err != nil {
				return err
			}
		}
	}
	// All pointer IDs are durable before the irreversible protocol marker. A
	// stopped upgrade is safely repeatable while legacy mode remains active.
	for _, ref := range next {
		if err := s.requireSnapshots(ctx, repo, ref.Target); err != nil {
			return err
		}
		if err := writeAtomic(s.refFile(repo, ref.Kind, ref.Name), encodeRef(ref)); err != nil {
			return err
		}
	}
	return writeAtomic(filepath.Join(s.repoDir(repo), "context-protocol"), []byte("1\n"))
}

// The history journal has already retained source roots. On protocol 1, names
// are projected atomically from those verified identity operations as well.
func (s *FSStore) validateProtocolHistory(ctx context.Context, e domain.HistoryEvent) error {
	repo := domain.ContentHash(e.RepoID)
	protocol, err := s.contextProtocol(repo)
	if err != nil || protocol == 0 {
		return err
	}
	if e.Kind != "birth" && e.Kind != "orphan" && e.Kind != "rename" && e.Kind != "archive" && e.Kind != "advance" {
		return nil
	}
	name := e.Branch
	if e.Kind == "rename" {
		name = e.PreviousBranch
	}
	current, readErr := s.getRefRaw(ctx, repo, domain.RefBranch, name)
	if readErr != nil && readErr != domain.ErrNotFound {
		return readErr
	}
	if e.Kind == "birth" || e.Kind == "orphan" {
		if readErr == nil {
			return domain.ErrRefConflict
		}
	}
	if e.Kind == "rename" || e.Kind == "archive" {
		// An empty orphan has no physical ref, but still has an identity receipt.
		events, err := s.listHistoryEventsRaw(repo)
		if err != nil {
			return err
		}
		p, err := domain.ProjectContextBranches(events)
		if err != nil {
			return err
		}
		if readErr == nil && current.BranchID != e.BranchID {
			return domain.ErrRefConflict
		}
		if readErr == domain.ErrNotFound && p.Active[name].ID != e.BranchID {
			return domain.ErrRefConflict
		}
		if e.Kind == "rename" {
			if _, err := s.getRefRaw(ctx, repo, domain.RefBranch, e.Branch); err == nil {
				return domain.ErrRefConflict
			} else if err != domain.ErrNotFound {
				return err
			}
		}
	}
	if e.Kind == "advance" {
		return s.validateContextWrite(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Target})
	}
	return nil
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

// A failed graph journal must be recovered before unrelated graph mutations;
// otherwise a later archive or reused name could invalidate its rollback.
func (s *FSStore) requireNoPendingJoin(repo domain.ContentHash) error {
	if _, err := os.Stat(s.joinJournalPath(repo)); err == nil {
		return fmt.Errorf("%w: pending graph transaction; restart to recover", domain.ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
