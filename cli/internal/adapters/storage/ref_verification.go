package storage

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// MatchingTagRefs batches no-op recognition, not mutations. Thousands of
// retained-history tags otherwise each acquire a durable legacy-compatible
// ref lock just to reinstall the same value. Missing/changed tags still follow
// the caller's normal conflict and durable write paths.
func (s *FileStore) MatchingTagRefs(ctx context.Context, refs []domain.Ref) (map[string]bool, error) {
	matched := map[string]bool{}
	if len(refs) == 0 {
		return matched, ctx.Err()
	}
	for _, ref := range refs {
		if err := domain.ValidateRef(ref); err != nil {
			return nil, err
		}
		if ref.Kind != domain.RefTag || ref.RepoID != refs[0].RepoID {
			return nil, domain.ErrInvalidRef
		}
	}
	err := s.withRefMutationLock(ctx, func() error {
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, err := s.getRefRaw(ctx, ref.RepoID, ref.Kind, ref.Name)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if current == ref {
				matched[ref.Name] = true
			}
		}
		return nil
	})
	return matched, err
}
