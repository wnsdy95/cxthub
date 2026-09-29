package storage

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"reflect"
)

func (s *FileStore) CompareAndDropStash(ctx context.Context, _ string, expected []domain.StashEntry) error {
	if len(expected) == 0 {
		return domain.ErrNotFound
	}
	return s.withMutationLock(ctx, "stash", "stack", func() error {
		current, err := s.readStash()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return domain.ErrSyncConflict
		}
		return s.writeStash(current[1:])
	})
}

var _ outbound.StashRestoreStore = (*FileStore)(nil)
