package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredRemovalStore = (*FSStore)(nil)

// DeleteAnchoredSession removes a session's report file and, from the
// repository's lists, its summaries and its open ranges. The commit index is
// derived from the stored reports and needs no change.
func (s *FSStore) DeleteAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if !domain.ValidAnchoredSessionKey(provider, sessionID) {
		return fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.anchoredSessionPath(repo, provider, sessionID)
	if _, err := s.readAnchoredSessionFile(repo, path); err != nil {
		return err
	}
	summaries, err := s.readAnchoredSummaries(ctx, repo)
	if err != nil {
		return err
	}
	keptSummaries := make([]domain.AnchoredSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.Provider != provider || summary.SessionID != sessionID {
			keptSummaries = append(keptSummaries, summary)
		}
	}
	publications, err := s.readAnchoredPublications(ctx, repo)
	if err != nil {
		return err
	}
	keptPublications := make([]domain.AnchoredPublication, 0, len(publications))
	for _, publication := range publications {
		if publication.Provider != provider || publication.SessionID != sessionID {
			keptPublications = append(keptPublications, publication)
		}
	}
	// What depends on the report goes first, so that a failure in between
	// leaves a report without them and never the other way round.
	if len(keptSummaries) != len(summaries) {
		if err := writeRepoJSON(s.anchoredSummaryPath(repo), keptSummaries); err != nil {
			return err
		}
	}
	if len(keptPublications) != len(publications) {
		if err := writeRepoJSON(s.anchoredPublicationPath(repo), keptPublications); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AnchoredContentInUse reports whether a stored report names the document as
// its content.
func (s *FSStore) AnchoredContentInUse(ctx context.Context, repo, doc domain.ContentHash) (bool, error) {
	if err := validateHashes(repo, doc); err != nil {
		return false, err
	}
	sessions, err := s.ListRepositoryAnchoredSessions(ctx, repo)
	if err != nil {
		return false, err
	}
	for _, session := range sessions {
		if content := session.Body.Content; content != nil && content.Doc == doc {
			return true, nil
		}
	}
	return false, nil
}

// The development store keeps a repository's removals in one file.
func (s *FSStore) anchoredRemovalPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "anchored-removals.json")
}

func (s *FSStore) readAnchoredRemovals(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionRemoval, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	removals, err := readRepoJSON[domain.AnchoredSessionRemoval](ctx, s.anchoredRemovalPath(repo), "removal list")
	if err != nil {
		return nil, err
	}
	for index := range removals {
		removals[index].RemovedAt = removals[index].RemovedAt.UTC()
		if err := removals[index].Validate(); err != nil || removals[index].RepoID != repo {
			return nil, fmt.Errorf("%w: invalid stored removal", domain.ErrIntegrity)
		}
	}
	sort.Slice(removals, func(left, right int) bool {
		if removals[left].Provider != removals[right].Provider {
			return removals[left].Provider < removals[right].Provider
		}
		return removals[left].SessionID < removals[right].SessionID
	})
	return removals, nil
}

func (s *FSStore) PutAnchoredSessionRemoval(ctx context.Context, removal domain.AnchoredSessionRemoval) error {
	if err := validateHash(removal.RepoID); err != nil {
		return err
	}
	if err := removal.Validate(); err != nil {
		return err
	}
	lock := s.anchoredSessionLock(removal.RepoID)
	lock.Lock()
	defer lock.Unlock()
	removals, err := s.readAnchoredRemovals(ctx, removal.RepoID)
	if err != nil {
		return err
	}
	for _, existing := range removals {
		if existing.Provider == removal.Provider && existing.SessionID == removal.SessionID {
			return nil
		}
	}
	removal.RemovedAt = removal.RemovedAt.UTC()
	return writeRepoJSON(s.anchoredRemovalPath(removal.RepoID), append(removals, removal))
}

func (s *FSStore) GetAnchoredSessionRemoval(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSessionRemoval, error) {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	removals, err := s.readAnchoredRemovals(ctx, repo)
	if err != nil {
		return domain.AnchoredSessionRemoval{}, err
	}
	for _, removal := range removals {
		if removal.Provider == provider && removal.SessionID == sessionID {
			return removal, nil
		}
	}
	return domain.AnchoredSessionRemoval{}, domain.ErrNotFound
}

func (s *FSStore) DeleteAnchoredSessionRemoval(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	removals, err := s.readAnchoredRemovals(ctx, repo)
	if err != nil {
		return err
	}
	kept := make([]domain.AnchoredSessionRemoval, 0, len(removals))
	for _, removal := range removals {
		if removal.Provider != provider || removal.SessionID != sessionID {
			kept = append(kept, removal)
		}
	}
	if len(kept) == len(removals) {
		return domain.ErrNotFound
	}
	return writeRepoJSON(s.anchoredRemovalPath(repo), kept)
}

func (s *FSStore) ListAnchoredSessionRemovals(ctx context.Context, repo domain.ContentHash, uploader string) ([]domain.AnchoredSessionRemoval, error) {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	removals, err := s.readAnchoredRemovals(ctx, repo)
	if err != nil {
		return nil, err
	}
	own := []domain.AnchoredSessionRemoval{}
	for _, removal := range removals {
		if removal.Uploader == uploader {
			own = append(own, removal)
		}
	}
	return own, nil
}
