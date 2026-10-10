package app

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) SetSessionArchived(ctx context.Context, repo, snapshotID domain.ContentHash, archived bool) error {
	if err := validateHashes(repo, snapshotID); err != nil {
		return err
	}
	action := "session.restored"
	if archived {
		action = "session.archived"
	}
	return repositoryWriteError(auditOperation(writeAction(ctx, "manage"), action), s, repo, func(bound context.Context) error {
		store, ok := s.meta.(outbound.SessionArchiveStore)
		if !ok {
			return fmt.Errorf("%w: session archive storage unavailable", domain.ErrConflict)
		}
		snapshot, err := s.meta.GetSnapshot(bound, repo, snapshotID)
		if err != nil {
			return err
		}
		records, err := store.ListSessionArchives(bound, repo)
		if err != nil {
			return err
		}
		key := domain.SessionArchiveKey(snapshot)
		for _, record := range records {
			if record.Key != key && record.SnapshotID != snapshotID {
				continue
			}
			if archived {
				return nil
			}
			if err := store.DeleteSessionArchive(bound, repo, record.Key); err != nil {
				return err
			}
		}
		if !archived {
			return nil
		}
		actor, system := inbound.RepositoryActor(bound)
		if system {
			actor = "system"
		}
		return store.PutSessionArchive(bound, domain.SessionArchive{
			RepoID: repo, Key: key, SnapshotID: snapshotID, Provider: snapshot.Provider,
			SessionID: snapshot.SessionID, ArchivedAt: time.Now().UTC(), ArchivedBy: actor,
		})
	})
}

func (s *Service) sessionArchiveRecords(ctx context.Context, repo domain.ContentHash) ([]domain.SessionArchive, error) {
	if store, ok := s.meta.(outbound.SessionArchiveStore); ok {
		return store.ListSessionArchives(ctx, repo)
	}
	return []domain.SessionArchive{}, nil
}

var _ inbound.SessionArchiver = (*Service)(nil)
