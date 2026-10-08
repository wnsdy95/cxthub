package storage

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Called only under retention and ref-mutation exclusion. An application or
// acknowledgement record is not evidence that its current closure survived.
// The set below avoids rereading a document within this one verification; it
// is never persisted and never skips the first current-byte check.
func (s *FileStore) verifyRootStagingReplay(ctx context.Context, op domain.StagingCommit) error {
	if err := s.verifyStagedDocs(ctx, op.Index); err != nil {
		return err
	}
	verified := make(map[domain.ContentHash]bool, len(op.Index.Entries))
	for _, entry := range op.Index.Entries {
		snapshot, err := s.GetSnapshot(ctx, entry.DocHash)
		if err != nil {
			return err
		}
		if snapshot.RepoID != op.Index.RepoID || !entry.DocumentRef().MatchesSnapshot(snapshot) || snapshot.Provider != entry.Provider || snapshot.SessionID != entry.SessionID {
			return domain.ErrHashMismatch
		}
		verified[entry.DocHash] = true
	}
	verifySnapshot := func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id == "" || verified[id] {
			return nil
		}
		snapshot, err := s.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.RepoID != op.Index.RepoID || snapshot.ID != id || !snapshot.DocumentRef().MatchesSnapshot(snapshot) {
			return domain.ErrHashMismatch
		}
		// The caller already owns retention. Use the nonlocking current-byte
		// checker instead of entering a second retention boundary.
		if err := s.inspectDocReference(ctx, snapshot.DocumentRef(), nil); err != nil {
			return err
		}
		verified[id] = true
		return nil
	}
	for _, id := range []domain.ContentHash{op.ExpectedRef.Target, op.Position.Snapshot, op.Position.MemorySource} {
		if err := verifySnapshot(id); err != nil {
			return err
		}
	}
	events := append(append(domain.StagingObservations(op), op.Publications...), *op.Position.Selection)
	if op.Advance != nil {
		events = append(events, *op.Advance)
	}
	memories := map[domain.ContentHash]domain.ContentHash{}
	for _, event := range events {
		for _, id := range []domain.ContentHash{event.Source, event.Target, event.SharedTarget, event.MemorySource} {
			if err := verifySnapshot(id); err != nil {
				return err
			}
		}
		if event.MemoryHash == "" {
			continue
		}
		owner, ok := memories[event.MemoryHash]
		if !ok {
			if err := ctx.Err(); err != nil {
				return err
			}
			memory, err := s.GetMemory(ctx, event.MemoryHash)
			if err != nil {
				return err
			}
			owner = memory.SnapshotID
			memories[event.MemoryHash] = owner
		}
		if owner != event.Target && owner != event.MemorySource {
			return domain.ErrHashMismatch
		}
		if err := verifySnapshot(owner); err != nil {
			return err
		}
	}
	return ctx.Err()
}
