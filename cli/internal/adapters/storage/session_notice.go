package storage

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"os"
	"path/filepath"
	"syscall"
)

type sessionNoticeCursor struct {
	Scope     domain.SessionNoticeScope `json:"scope"`
	Selection domain.ContentHash        `json:"selection"`
}

func (s *FileStore) WithSessionNoticeCursor(ctx context.Context, scope domain.SessionNoticeScope, deliver func(domain.ContentHash) (domain.ContentHash, error)) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if s.worktreeID == "" || s.worktreeID != scope.WorktreeID {
		return domain.ErrHashMismatch
	}
	_, err := s.withOSLock(ctx, "session-notices", scope.Key(), syscall.LOCK_EX, true, func() error {
		path := filepath.Join(s.storeDir(), "session-notices", scope.Key()+".json")
		var current domain.ContentHash
		raw, err := readCxtFile(path)
		if err == nil {
			var cursor sessionNoticeCursor
			if json.Unmarshal(raw, &cursor) != nil || cursor.Scope != scope || domain.ValidateContentHash(cursor.Selection) != nil {
				return domain.ErrHashMismatch
			}
			current = cursor.Selection
		} else if !os.IsNotExist(err) {
			return err
		}
		next, err := deliver(current)
		if err != nil {
			return err
		}
		if next == current {
			return nil
		}
		if err = domain.ValidateContentHash(next); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		raw, err = json.Marshal(sessionNoticeCursor{Scope: scope, Selection: next})
		if err != nil {
			return err
		}
		return writeAtomic(path, raw)
	})
	return err
}
