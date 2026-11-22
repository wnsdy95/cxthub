package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) anchoredNoteStore() (outbound.AnchoredNoteStore, error) {
	store, ok := s.meta.(outbound.AnchoredNoteStore)
	if !ok {
		return nil, fmt.Errorf("%w: note storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

func (s *Service) reportedWorktreeStore() (outbound.ReportedWorktreeStore, error) {
	store, ok := s.meta.(outbound.ReportedWorktreeStore)
	if !ok {
		return nil, fmt.Errorf("%w: worktree storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// ListAnchoredNotes lists the caller's own notes, and of other members'
// notes those whose target is open to the team: a note is read by whoever
// may see what it was written about.
//
//   - A commit: when the remote holds it, by the server's own read.
//   - A lifetime of a branch name: always, because the ledger holds only
//     lifetimes of names on the remote. A branch named without a lifetime
//     is one that was never pushed.
//   - A worktree: never. Worktrees are their member's alone.
func (s *Service) ListAnchoredNotes(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredNote, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	notes, err := repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredNote, error) {
		store, err := s.anchoredNoteStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredReadActor(bound)
		if err != nil {
			return nil, err
		}
		all, err := store.ListRepositoryAnchoredNotes(bound, repo)
		if err != nil {
			return nil, err
		}
		var commits []string
		for _, note := range all {
			if !anchoredOwn(actor, note.Author) && note.Target.Kind == domain.AnchoredNoteCommit {
				commits = append(commits, note.Target.Commit)
			}
		}
		held := map[string]struct{}{}
		if len(commits) > 0 {
			remote, _, read, err := remoteGitCache(bound, s.meta, repo)
			if err != nil {
				return nil, err
			}
			if read {
				stored, err := remote.GetRemoteCommits(bound, repo, commits)
				if err != nil {
					return nil, err
				}
				for _, commit := range stored {
					held[commit.SHA] = struct{}{}
				}
			}
		}
		notes := []domain.AnchoredNote{}
		for _, note := range all {
			open := anchoredOwn(actor, note.Author)
			switch {
			case open:
			case note.Target.Kind == domain.AnchoredNoteCommit:
				_, open = held[note.Target.Commit]
			case note.Target.Kind == domain.AnchoredNoteBranch:
				open = note.Target.LedgerID != ""
			}
			if open {
				notes = append(notes, note)
			}
		}
		return notes, nil
	})
	if err != nil {
		return nil, err
	}
	accounts := map[string]struct{}{}
	for _, note := range notes {
		accounts[note.Author] = struct{}{}
	}
	names := s.anchoredAccountNames(ctx, accounts)
	for index := range notes {
		notes[index].AuthorName = names[notes[index].Author]
	}
	return notes, nil
}

// CreateAnchoredNote stores what the caller wrote about a commit, a branch or
// a worktree. The text is masked the way a captured session is before it is
// stored. A note names a lifetime of a branch name only when the ledger holds
// that lifetime under that name.
//
// A note does not move the repository revision: only its author reads it.
func (s *Service) CreateAnchoredNote(ctx context.Context, repo domain.ContentHash, target domain.AnchoredNoteTarget, body string) (domain.AnchoredNote, error) {
	if err := validateHashes(repo); err != nil {
		return domain.AnchoredNote{}, err
	}
	if err := target.Validate(); err != nil {
		return domain.AnchoredNote{}, err
	}
	body, err := domain.NormalizeAnchoredNoteBody(body)
	if err != nil {
		return domain.AnchoredNote{}, err
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_note.created"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredNote, error) {
		store, err := s.anchoredNoteStore()
		if err != nil {
			return domain.AnchoredNote{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredNote{}, err
		}
		if target.LedgerID != "" {
			ledger, err := s.branchLedgerStore()
			if err != nil {
				return domain.AnchoredNote{}, err
			}
			lifetime, err := ledger.BranchLifetimeHolding(bound, repo, target.LedgerID)
			if errors.Is(err, domain.ErrNotFound) || (err == nil && (lifetime.LedgerID != target.LedgerID || lifetime.Name != target.Branch)) {
				return domain.AnchoredNote{}, fmt.Errorf("%w: note target: the ledger holds no such lifetime of that branch name", domain.ErrValidation)
			}
			if err != nil {
				return domain.AnchoredNote{}, err
			}
		}
		now := time.Now().UTC()
		note := domain.AnchoredNote{RepoID: repo, ID: domain.NewAnchoredNoteID(), Target: target, Author: actor, Body: body, CreatedAt: now, UpdatedAt: now}
		if err := store.PutAnchoredNote(bound, note); err != nil {
			return domain.AnchoredNote{}, err
		}
		return note, nil
	})
}

// ownAnchoredNote reads a note for its author. To anyone else it does not
// exist.
func ownAnchoredNote(ctx context.Context, store outbound.AnchoredNoteStore, repo domain.ContentHash, id string) (domain.AnchoredNote, error) {
	actor, err := anchoredSessionActor(ctx)
	if err != nil {
		return domain.AnchoredNote{}, err
	}
	note, err := store.GetAnchoredNote(ctx, repo, id)
	if err != nil {
		return domain.AnchoredNote{}, err
	}
	if note.Author != actor {
		return domain.AnchoredNote{}, domain.ErrNotFound
	}
	return note, nil
}

// UpdateAnchoredNote replaces the text of the caller's own note. The author,
// the target and the time it was written stay.
func (s *Service) UpdateAnchoredNote(ctx context.Context, repo domain.ContentHash, id, body string) (domain.AnchoredNote, error) {
	if err := validateHashes(repo); err != nil {
		return domain.AnchoredNote{}, err
	}
	if !domain.ValidAnchoredNoteID(id) {
		return domain.AnchoredNote{}, fmt.Errorf("%w: note identifier", domain.ErrValidation)
	}
	body, err := domain.NormalizeAnchoredNoteBody(body)
	if err != nil {
		return domain.AnchoredNote{}, err
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_note.updated"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredNote, error) {
		store, err := s.anchoredNoteStore()
		if err != nil {
			return domain.AnchoredNote{}, err
		}
		note, err := ownAnchoredNote(bound, store, repo, id)
		if err != nil {
			return domain.AnchoredNote{}, err
		}
		if note.Body == body {
			return note, nil
		}
		note.Body, note.UpdatedAt = body, time.Now().UTC()
		if note.UpdatedAt.Before(note.CreatedAt) {
			note.UpdatedAt = note.CreatedAt
		}
		if err := store.PutAnchoredNote(bound, note); err != nil {
			return domain.AnchoredNote{}, err
		}
		return note, nil
	})
}

// DeleteAnchoredNote removes the caller's own note.
func (s *Service) DeleteAnchoredNote(ctx context.Context, repo domain.ContentHash, id string) error {
	if err := validateHashes(repo); err != nil {
		return err
	}
	if !domain.ValidAnchoredNoteID(id) {
		return fmt.Errorf("%w: note identifier", domain.ErrValidation)
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_note.deleted"), revisionScopeKey{}, "none")
	_, err := repositoryWrite(ctx, s, repo, func(bound context.Context) (struct{}, error) {
		store, err := s.anchoredNoteStore()
		if err != nil {
			return struct{}{}, err
		}
		if _, err := ownAnchoredNote(bound, store, repo, id); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, store.DeleteAnchoredNote(bound, repo, id)
	})
	return err
}

// ReportWorktrees takes the worktree list of one of the caller's machines and
// answers the caller's worktrees on every machine. Only names, branches and
// the machine's label are stored, and only the caller reads them.
func (s *Service) ReportWorktrees(ctx context.Context, repo domain.ContentHash, report domain.WorktreeReport) ([]domain.ReportedWorktree, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if err := report.Validate(); err != nil {
		return nil, err
	}
	ctx = context.WithValue(auditOperation(ctx, "worktrees.reported"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) ([]domain.ReportedWorktree, error) {
		store, err := s.reportedWorktreeStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return nil, err
		}
		stored, err := store.ListReportedWorktrees(bound, repo, actor)
		if err != nil {
			return nil, err
		}
		merged := domain.MergeWorktreeReport(stored, report, time.Now().UTC())
		if err := store.PutReportedWorktrees(bound, repo, actor, merged); err != nil {
			return nil, err
		}
		return merged, nil
	})
}

// ListReportedWorktrees lists the caller's own worktrees.
func (s *Service) ListReportedWorktrees(ctx context.Context, repo domain.ContentHash) ([]domain.ReportedWorktree, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.ReportedWorktree, error) {
		store, err := s.reportedWorktreeStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredReadActor(bound)
		if err != nil {
			return nil, err
		}
		if actor == "" {
			// A worktree is its member's alone.
			return []domain.ReportedWorktree{}, nil
		}
		return store.ListReportedWorktrees(bound, repo, actor)
	})
}

// AnchoredSessionCalls lists the recorded agent calls that name one of the
// caller's sessions as sender or receiver, oldest first. A call sits in the
// report of the session that made it, so the calls a session received are
// gathered from the caller's other reports of the same tool.
func (s *Service) AnchoredSessionCalls(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredAgentCall, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return nil, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredAgentCall, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredReadActor(bound)
		if err != nil {
			return nil, err
		}
		if actor == "" {
			// Agent calls are their uploader's alone.
			return nil, domain.ErrNotFound
		}
		sessions, err := store.ListAnchoredSessions(bound, repo, actor)
		if err != nil {
			return nil, err
		}
		calls, own := []domain.AnchoredAgentCall{}, false
		for _, session := range sessions {
			if session.Provider != provider {
				continue
			}
			own = own || session.SessionID == sessionID
			for _, call := range session.Body.AgentCalls {
				named := call.From == sessionID
				for _, receiver := range call.To {
					named = named || receiver == sessionID
				}
				if named {
					calls = append(calls, call)
				}
			}
		}
		if !own {
			return nil, domain.ErrNotFound
		}
		sort.SliceStable(calls, func(left, right int) bool {
			a, b := calls[left].At, calls[right].At
			if a == nil || b == nil {
				return a != nil && b == nil
			}
			return a.Before(*b)
		})
		return calls, nil
	})
}

var (
	_ inbound.AnchoredNotes     = (*Service)(nil)
	_ inbound.ReportedWorktrees = (*Service)(nil)
)
