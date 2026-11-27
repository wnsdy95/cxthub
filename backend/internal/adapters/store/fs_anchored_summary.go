package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.AnchoredSummaryStore = (*FSStore)(nil)

// The development store keeps a repository's summaries in one file.
func (s *FSStore) anchoredSummaryPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "anchored-summaries.json")
}

func sortAnchoredSummaries(summaries []domain.AnchoredSummary) {
	sort.Slice(summaries, func(left, right int) bool {
		a, b := summaries[left], summaries[right]
		if a.Commit != b.Commit {
			return a.Commit < b.Commit
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.SessionID < b.SessionID
	})
}

func (s *FSStore) readAnchoredSummaries(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSummary, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	summaries, err := readRepoJSON[domain.AnchoredSummary](ctx, s.anchoredSummaryPath(repo), "summary list")
	if err != nil {
		return nil, err
	}
	for index := range summaries {
		summaries[index].CreatedAt, summaries[index].UpdatedAt = summaries[index].CreatedAt.UTC(), summaries[index].UpdatedAt.UTC()
		if err := summaries[index].Validate(); err != nil || summaries[index].RepoID != repo {
			return nil, fmt.Errorf("%w: invalid stored summary", domain.ErrIntegrity)
		}
	}
	sortAnchoredSummaries(summaries)
	return summaries, nil
}

func (s *FSStore) PutAnchoredSummary(ctx context.Context, summary domain.AnchoredSummary) error {
	if err := summary.Validate(); err != nil {
		return err
	}
	lock := s.anchoredSessionLock(summary.RepoID)
	lock.Lock()
	defer lock.Unlock()
	summaries, err := s.readAnchoredSummaries(ctx, summary.RepoID)
	if err != nil {
		return err
	}
	summary.Body = summary.Body.Canonical()
	summary.CreatedAt, summary.UpdatedAt = summary.CreatedAt.UTC(), summary.UpdatedAt.UTC()
	next := make([]domain.AnchoredSummary, 0, len(summaries)+1)
	for _, existing := range summaries {
		if existing.Provider != summary.Provider || existing.SessionID != summary.SessionID || existing.Commit != summary.Commit {
			next = append(next, existing)
		}
	}
	next = append(next, summary)
	sortAnchoredSummaries(next)
	return writeRepoJSON(s.anchoredSummaryPath(summary.RepoID), next)
}

func (s *FSStore) GetAnchoredSummary(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID, commit string) (domain.AnchoredSummary, error) {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	summaries, err := s.readAnchoredSummaries(ctx, repo)
	if err != nil {
		return domain.AnchoredSummary{}, err
	}
	for _, summary := range summaries {
		if summary.Provider == provider && summary.SessionID == sessionID && summary.Commit == commit {
			return summary, nil
		}
	}
	return domain.AnchoredSummary{}, domain.ErrNotFound
}

func (s *FSStore) ListCommitAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error) {
	lock := s.anchoredSessionLock(repo)
	lock.Lock()
	defer lock.Unlock()
	summaries, err := s.readAnchoredSummaries(ctx, repo)
	if err != nil {
		return nil, err
	}
	asked := make(map[string]struct{}, len(commits))
	for _, commit := range commits {
		asked[commit] = struct{}{}
	}
	listed := []domain.AnchoredSummary{}
	for _, summary := range summaries {
		if _, ok := asked[summary.Commit]; ok {
			listed = append(listed, summary)
		}
	}
	return listed, nil
}
