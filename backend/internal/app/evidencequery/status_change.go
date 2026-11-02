package evidencequery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ChangeReader interface {
	GetGitChange(context.Context, domain.ContentHash, string) (domain.GitChangeJob, error)
	ListGitChanges(context.Context, domain.ContentHash, string, int) ([]domain.GitChangeSummary, error)
}

type ChangeConfig struct {
	Scope      Scope
	Changes    ChangeReader
	AtomicRead bool
}

type ChangeQueries struct {
	scope      Scope
	changes    ChangeReader
	atomicRead bool
}

func NewChangeQueries(config ChangeConfig) (*ChangeQueries, error) {
	if absent(config.Scope) || absent(config.Changes) {
		return nil, domain.ErrValidation
	}
	return &ChangeQueries{scope: config.Scope, changes: config.Changes, atomicRead: config.AtomicRead}, nil
}

func (queries *ChangeQueries) Get(ctx context.Context, repo domain.ContentHash, id string) (domain.GitChangeJob, error) {
	var out domain.GitChangeJob
	err := queries.scope.ReadRepository(ctx, repo, func(bound context.Context) error {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		if err := domain.ValidateGitChangeID(id); err != nil {
			return err
		}
		var err error
		out, err = queries.changes.GetGitChange(bound, repo, id)
		return err
	})
	if err != nil && queries.atomicRead {
		return domain.GitChangeJob{}, err
	}
	return out, err
}

func (queries *ChangeQueries) List(ctx context.Context, repo domain.ContentHash, cursor string, limit int) (domain.GitChangePage, error) {
	var out domain.GitChangePage
	err := queries.scope.ReadRepository(ctx, repo, func(bound context.Context) error {
		out = domain.GitChangePage{Items: []domain.GitChangeSummary{}}
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		if cursor != "" {
			if err := domain.ValidateGitChangeID(cursor); err != nil {
				return err
			}
		}
		if limit < 1 || limit > 100 {
			return domain.ErrValidation
		}
		jobs, err := queries.changes.ListGitChanges(bound, repo, cursor, limit+1)
		if err != nil {
			return err
		}
		if len(jobs) > limit {
			out.NextCursor = jobs[limit-1].ID
			jobs = jobs[:limit]
		}
		out.Items = jobs
		return nil
	})
	if err != nil && queries.atomicRead {
		return domain.GitChangePage{}, err
	}
	return out, err
}
