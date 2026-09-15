package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type localBranchRecord struct {
	Event     domain.HistoryEvent `json:"event"`
	Detached  bool                `json:"detached,omitempty"`
	RenamedTo string              `json:"renamed_to,omitempty"`
}

func (s *FileStore) localBranchPath(name string) string {
	h := sha256.Sum256([]byte(name))
	return filepath.Join(s.storeDir(), "branch-bindings", fmt.Sprintf("%x.json", h[:]))
}

func (s *FileStore) readLocalBinding(repo, local string) (localBranchRecord, error) {
	raw, err := readCxtFile(s.localBranchPath(local))
	if os.IsNotExist(err) {
		return localBranchRecord{}, domain.ErrNotFound
	}
	if err != nil {
		return localBranchRecord{}, err
	}
	var record localBranchRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, err
	}
	e := record.Event
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return record, err
	}
	if (repo != "" && e.RepoID != repo) || e.LocalBranch != local {
		return record, domain.ErrHashMismatch
	}
	return record, nil
}

func (s *FileStore) ResolveLocalBranch(ctx context.Context, repo, local string) (domain.LocalBranchBinding, error) {
	b := domain.LocalBranchBinding{LocalBranch: local, Branch: local, BranchID: domain.LegacyContextBranchID(repo, local)}
	if local == "" {
		return b, nil
	}
	if err := domain.ValidateBranchName(local); err != nil {
		return b, err
	}
	record, bindingErr := s.readLocalBinding(repo, local)
	if bindingErr != nil && bindingErr != domain.ErrNotFound {
		return b, bindingErr
	}
	// Some local list/load callers omit repoID. The persisted binding supplies
	// its scope; an explicitly supplied conflicting repository still fails.
	if repo == "" && bindingErr == nil {
		repo = record.Event.RepoID
	}
	b.BranchID = domain.LegacyContextBranchID(repo, local)
	events, err := s.listHistoryEvents(repo)
	if err != nil {
		return b, err
	}
	state, err := domain.ProjectContextBranches(events)
	if err != nil {
		return b, err
	}
	if bindingErr == domain.ErrNotFound {
		b.BranchID = state.Identity(repo, local)
		return b, nil
	}
	e := record.Event
	if e.Kind != "attach" {
		b.BranchID = state.Identity(repo, local)
		return b, nil
	}
	b.Branch, b.BranchID, b.Tracking = e.Branch, e.BranchID, true
	b.Inactive, b.RenamedTo = record.Detached || record.RenamedTo != "", record.RenamedTo
	if known, ok := state.ByID[e.BranchID]; ok {
		if known.Archived {
			return b, domain.ErrBranchArchived
		}
		b.Branch = known.Name
	} else if e.BranchID != domain.LegacyContextBranchID(repo, e.Branch) {
		return b, fmt.Errorf("tracking branch identity %s is not available locally", e.BranchID)
	}
	return b, nil
}

func (s *FileStore) BindLocalBranch(ctx context.Context, e domain.HistoryEvent) error {
	local := e.LocalBranch
	if local == "" {
		local = e.Branch
	}
	if err := domain.ValidateBranchName(local); err != nil {
		return err
	}
	return s.withRefMutationLock(ctx, func() error {

		e.LocalBranch = local
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return err
		}
		raw, err := json.Marshal(localBranchRecord{Event: e})
		if err != nil {
			return err
		}
		return writeAtomic(s.localBranchPath(local), raw)
	})
}

func (s *FileStore) UnbindLocalBranch(ctx context.Context, repo, local string) error {
	return s.withRefMutationLock(ctx, func() error {
		record, err := s.readLocalBinding(repo, local)
		if err != nil {
			return err
		}
		record.Detached = true
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return writeAtomic(s.localBranchPath(local), raw)
	})
}

func (s *FileStore) RenameLocalBranch(ctx context.Context, repo, from, to string) error {
	if err := domain.ValidateBranchName(to); err != nil {
		return err
	}
	return s.withRefMutationLock(ctx, func() error {
		source, err := s.readLocalBinding(repo, from)
		if err != nil {
			return err
		}
		if source.Detached || (source.RenamedTo != "" && source.RenamedTo != to) {
			return domain.ErrBranchArchived
		}
		target, err := s.readLocalBinding(repo, to)
		if err == nil && target.Event.BranchID != source.Event.BranchID && !target.Detached {
			return domain.ErrBranchExists
		}
		if err != nil && err != domain.ErrNotFound {
			return err
		}
		target = localBranchRecord{Event: source.Event}
		target.Event.LocalBranch = to
		raw, err := json.Marshal(target)
		if err != nil {
			return err
		}
		if err := writeAtomic(s.localBranchPath(to), raw); err != nil {
			return err
		}
		if err := s.renameWorkingPositions(repo, from, to, source.Event.BranchID, false); err != nil {
			return err
		}
		source.RenamedTo = to
		raw, err = json.Marshal(source)
		if err != nil {
			return err
		}
		// A durable tombstone keeps retry from interpreting a local alias rename
		// as a later global rename, even when the alias equalled the server name.
		return writeAtomic(s.localBranchPath(from), raw)
	})
}
