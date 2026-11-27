package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.CollectorStore = (*FSStore)(nil)

// The development store keeps a repository's collectors in one file and the
// pushes they claimed in another.
func (s *FSStore) reportedCollectorPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "reported-collectors.json")
}

func (s *FSStore) claimedPushPath(repo domain.ContentHash) string {
	return filepath.Join(s.repoDir(repo), "claimed-pushes.json")
}

// validateCollectorReport checks what is stored of one report: a valid
// collector, valid claims, and each push named once.
func validateCollectorReport(collector domain.ReportedCollector, claims []domain.ClaimedPush) error {
	if err := collector.Validate(); err != nil {
		return err
	}
	named := make(map[[2]string]struct{}, len(claims))
	for _, claim := range claims {
		if err := claim.Validate(); err != nil {
			return err
		}
		if _, twice := named[[2]string{claim.Branch, claim.SHA}]; twice {
			return fmt.Errorf("%w: a push is claimed twice at once", domain.ErrValidation)
		}
		named[[2]string{claim.Branch, claim.SHA}] = struct{}{}
	}
	return nil
}

// sortClaimedPushes orders claimed pushes as both stores list them: the one
// pushed last first, then by branch and commit.
func sortClaimedPushes(claims []domain.ClaimedPush) {
	sort.Slice(claims, func(left, right int) bool {
		a, b := claims[left], claims[right]
		if !a.PushedAt.Equal(b.PushedAt) {
			return a.PushedAt.After(b.PushedAt)
		}
		if a.Branch != b.Branch {
			return a.Branch < b.Branch
		}
		return a.SHA < b.SHA
	})
}

func (s *FSStore) PutCollectorReport(ctx context.Context, repo domain.ContentHash, collector domain.ReportedCollector, claims []domain.ClaimedPush) error {
	if err := validateHash(repo); err != nil {
		return err
	}
	if err := validateCollectorReport(collector, claims); err != nil {
		return err
	}
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	collectors, err := readRepoJSON[domain.ReportedCollector](ctx, s.reportedCollectorPath(repo), "collector list")
	if err != nil {
		return err
	}
	stored, err := readRepoJSON[domain.ClaimedPush](ctx, s.claimedPushPath(repo), "claimed push list")
	if err != nil {
		return err
	}
	replaced := false
	for index := range collectors {
		if collectors[index].Account == collector.Account && collectors[index].Machine == collector.Machine {
			collectors[index], replaced = collector, true
		}
	}
	if !replaced {
		collectors = append(collectors, collector)
	}
	position := make(map[[2]string]int, len(stored))
	for index, claim := range stored {
		position[[2]string{claim.Branch, claim.SHA}] = index
	}
	for _, claim := range claims {
		if index, held := position[[2]string{claim.Branch, claim.SHA}]; held {
			stored[index] = domain.MergeClaimedPush(stored[index], claim)
			continue
		}
		position[[2]string{claim.Branch, claim.SHA}] = len(stored)
		stored = append(stored, claim)
	}
	if err := writeRepoJSON(s.claimedPushPath(repo), stored); err != nil {
		return err
	}
	return writeRepoJSON(s.reportedCollectorPath(repo), collectors)
}

func (s *FSStore) ListReportedCollectors(ctx context.Context, repo domain.ContentHash) ([]domain.ReportedCollector, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	collectors, err := readRepoJSON[domain.ReportedCollector](ctx, s.reportedCollectorPath(repo), "collector list")
	if err != nil {
		return nil, err
	}
	for index := range collectors {
		if collectors[index].Validate() != nil {
			return nil, fmt.Errorf("%w: invalid stored collector", domain.ErrIntegrity)
		}
		collectors[index].HeardAt = collectors[index].HeardAt.UTC()
	}
	sort.Slice(collectors, func(left, right int) bool {
		if collectors[left].Account != collectors[right].Account {
			return collectors[left].Account < collectors[right].Account
		}
		return collectors[left].Machine < collectors[right].Machine
	})
	return collectors, nil
}

func (s *FSStore) ListClaimedPushes(ctx context.Context, repo domain.ContentHash, limit int) ([]domain.ClaimedPush, error) {
	if err := validateHash(repo); err != nil {
		return nil, err
	}
	lock := s.anchoredNoteLock(repo)
	lock.Lock()
	defer lock.Unlock()
	claims, err := readRepoJSON[domain.ClaimedPush](ctx, s.claimedPushPath(repo), "claimed push list")
	if err != nil {
		return nil, err
	}
	for index := range claims {
		if claims[index].Validate() != nil {
			return nil, fmt.Errorf("%w: invalid stored claimed push", domain.ErrIntegrity)
		}
		claims[index].PushedAt, claims[index].ClaimedAt = claims[index].PushedAt.UTC(), claims[index].ClaimedAt.UTC()
	}
	sortClaimedPushes(claims)
	if limit < 0 {
		limit = 0
	}
	if len(claims) > limit {
		claims = claims[:limit]
	}
	return claims, nil
}
