package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var (
	_ outbound.AnchoredNoteStore     = (*FSStore)(nil)
	_ outbound.ReportedWorktreeStore = (*FSStore)(nil)
)

var fsAnchoredNoteLocks sync.Map

func (s *FSStore) anchoredNoteLock(repo domain.ContentHash) *sync.Mutex {
	lock, _ := fsAnchoredNoteLocks.LoadOrStore(s.dataDir+"\x00"+string(repo), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// readRepoJSON reads one of a repository's whole-file lists. A file that is
// not there is an empty list.
func readRepoJSON[T any](ctx context.Context, path, what string) ([]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []T{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", domain.ErrIntegrity, what)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	items := []T{}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: invalid %s JSON: %v", domain.ErrIntegrity, what, err)
	}
	return items, nil
}

func writeRepoJSON[T any](path string, items []T) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}

// The development store keeps a repository's notes in one file.
func (s *FSStore) anchoredNotePath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "anchored-notes.json")
}

func sortAnchoredNotes(notes []domain.AnchoredNote) {
	sort.Slice(notes, func(left, right int) bool {
		if !notes[left].CreatedAt.Equal(notes[right].CreatedAt) {
			return notes[left].CreatedAt.Before(notes[right].CreatedAt)
		}
		return notes[left].ID < notes[right].ID
	})
}

func (s *FSStore) readAnchoredNotes(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredNote, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	notes, err := readRepoJSON[domain.AnchoredNote](ctx, s.anchoredNotePath(repo), "note list")
	if err != nil {
		return nil, err
	}
	for _, note := range notes {
		if err := note.Validate(); err != nil || note.RepoID != repo {
			return nil, fmt.Errorf("%w: invalid stored note", domain.ErrIntegrity)
		}
	}
	sortAnchoredNotes(notes)
	return notes, nil
}

func (s *FSStore) PutAnchoredNote(ctx context.Context, note domain.AnchoredNote) error {
	if err := note.Validate(); err != nil {
		return err
	}
	lock := s.anchoredNoteLock(note.RepoID)
	lock.Lock()
	defer lock.Unlock()
	notes, err := s.readAnchoredNotes(ctx, note.RepoID)
	if err != nil {
		return err
	}
	next := make([]domain.AnchoredNote, 0, len(notes)+1)
	for _, existing := range notes {
		if existing.ID != note.ID {
			next = append(next, existing)
		}
	}
	next = append(next, note)
	sortAnchoredNotes(next)
	return writeRepoJSON(s.anchoredNotePath(note.RepoID), next)
}

func (s *FSStore) GetAnchoredNote(ctx context.Context, repo domain.ContentHash, id string) (domain.AnchoredNote, error) {
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	notes, err := s.readAnchoredNotes(ctx, repo)
	if err != nil {
		return domain.AnchoredNote{}, err
	}
	for _, note := range notes {
		if note.ID == id {
			return note, nil
		}
	}
	return domain.AnchoredNote{}, domain.ErrNotFound
}

func (s *FSStore) ListAnchoredNotes(ctx context.Context, repo domain.ContentHash, author string) ([]domain.AnchoredNote, error) {
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	notes, err := s.readAnchoredNotes(ctx, repo)
	if err != nil {
		return nil, err
	}
	own := []domain.AnchoredNote{}
	for _, note := range notes {
		if note.Author == author {
			own = append(own, note)
		}
	}
	return own, nil
}

func (s *FSStore) ListRepositoryAnchoredNotes(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredNote, error) {
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	notes, err := s.readAnchoredNotes(ctx, repo)
	if err != nil {
		return nil, err
	}
	return append([]domain.AnchoredNote{}, notes...), nil
}

func (s *FSStore) DeleteAnchoredNote(ctx context.Context, repo domain.ContentHash, id string) error {
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	notes, err := s.readAnchoredNotes(ctx, repo)
	if err != nil {
		return err
	}
	next := make([]domain.AnchoredNote, 0, len(notes))
	for _, note := range notes {
		if note.ID != id {
			next = append(next, note)
		}
	}
	if len(next) == len(notes) {
		return domain.ErrNotFound
	}
	return writeRepoJSON(s.anchoredNotePath(repo), next)
}

// A member's worktrees are one file per account; the file name is a hash of
// the account, so no account identifier becomes a path.
func (s *FSStore) reportedWorktreePath(repo domain.ContentHash, account string) string {
	return filepath.Join(s.repoDir(repo), "reported-worktrees", hexOf(domain.HashContent([]byte(account)))+".json")
}

func validateReportedWorktrees(account string, worktrees []domain.ReportedWorktree) error {
	if strings.TrimSpace(account) == "" {
		return fmt.Errorf("%w: worktree list needs its account", domain.ErrValidation)
	}
	if len(worktrees) > 64*domain.ReportedWorktreeLimit {
		return fmt.Errorf("%w: too many stored worktrees", domain.ErrValidation)
	}
	seen := map[[2]string]struct{}{}
	for _, worktree := range worktrees {
		if err := worktree.Validate(); err != nil {
			return err
		}
		key := [2]string{worktree.Machine, worktree.Name}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: a worktree is stored twice", domain.ErrValidation)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (s *FSStore) ListReportedWorktrees(ctx context.Context, repo domain.ContentHash, account string) ([]domain.ReportedWorktree, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	worktrees, err := readRepoJSON[domain.ReportedWorktree](ctx, s.reportedWorktreePath(repo, account), "worktree list")
	if err != nil {
		return nil, err
	}
	if err := validateReportedWorktrees(account, worktrees); err != nil {
		return nil, fmt.Errorf("%w: invalid stored worktree list", domain.ErrIntegrity)
	}
	domain.SortReportedWorktrees(worktrees)
	return worktrees, nil
}

func (s *FSStore) PutReportedWorktrees(ctx context.Context, repo domain.ContentHash, account string, worktrees []domain.ReportedWorktree) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := validateReportedWorktrees(account, worktrees); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	next := append([]domain.ReportedWorktree{}, worktrees...)
	domain.SortReportedWorktrees(next)
	path := s.reportedWorktreePath(repo, account)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeRepoJSON(path, next)
}
