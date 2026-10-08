package app

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) recordHistoryWithDocumentPreparation(ctx context.Context, event domain.HistoryEvent) error {
	repo := domain.ContentHash(event.RepoID)
	original := func(ctx context.Context) error {
		return repositoryWriteError(ctx, s, repo, func(tx context.Context) error { return s.recordHistory(tx, event, false, nil) })
	}
	capability, supported := s.meta.(outbound.HistoryDocumentProofStore)
	tx, transactional := s.meta.(outbound.RepositoryTransactions)
	state, hasState := s.meta.(outbound.RepositoryTransactionState)
	history, hasHistory := s.meta.(outbound.HistoryStore)
	_, hasVerifier := s.blobs.(outbound.StoredDocVerifier)
	if !supported || !transactional || !hasState || !hasHistory || !hasVerifier {
		return original(ctx)
	}
	return capability.WithinHistoryDocumentProof(ctx, repo, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
		if proof == nil {
			return original(ctx)
		}
		var replay bool
		var roots []domain.ContentHash
		// This authorized lookup is deliberately not repositoryWrite: an
		// unaccepted preflight must not advance revisions or create audit rows.
		err := tx.WithinRepository(ctx, repo, func(bound context.Context) error {
			if err := s.authorizeRepositoryWrite(bound, repo); err != nil {
				return err
			}
			if err := domain.ValidateHistoryEvent(event); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrValidation, err)
			}
			accepted, err := history.ListHistoryEvents(bound, repo)
			if err != nil {
				return err
			}
			roots = []domain.ContentHash{event.Source, event.Target, event.SharedTarget, event.MemorySource}
			for _, old := range accepted {
				if old.ID == event.ID {
					if !reflect.DeepEqual(old, event) {
						return domain.ErrRefConflict
					}
					replay = true
				}
				// Accepted predecessor evidence is immutable. Final admission
				// resolves it again under the write lock and checks the root set.
				if old.ID == event.MemorySelectionParent && old.MemoryHash != "" {
					roots = append(roots, old.Source, old.Target, old.SharedTarget, old.MemorySource)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if replay {
			return original(ctx) // Final authorization and replay wake behavior.
		}
		wanted := historyRootSet(roots)
		var snapshots []domain.Snapshot
		preparationErr := tx.WithinReadSnapshot(ctx, func(read context.Context) error {
			if !state.InReadOnlyTransaction(read) {
				return domain.ErrConflict
			}
			for _, id := range roots {
				if id == "" {
					continue
				}
				if _, seen := wanted[id]; !seen {
					continue
				}
				delete(wanted, id)
				snap, err := s.meta.GetSnapshot(read, repo, id)
				if err != nil {
					return err
				}
				if err := s.verifyStoredSnapshotDoc(read, repo, snap); err != nil {
					return err
				}
				snapshots = append(snapshots, snap)
			}
			return proof.Capture(read, snapshots)
		})
		wanted = historyRootSet(roots)
		return repositoryWriteError(ctx, s, repo, func(write context.Context) error {
			// recordHistoryPrepared checks accepted replay before invoking this
			// callback: concurrent exact acceptance wins a failed preparation.
			return s.recordHistoryPrepared(write, event, false, nil, func(write context.Context, current []domain.ContentHash) (historyVerification, error) {
				if preparationErr != nil {
					return nil, preparationErr
				}
				if !reflect.DeepEqual(historyRootSet(current), wanted) {
					return nil, domain.ErrConflict
				}
				if err := proof.Pin(write); err != nil {
					return nil, err
				}
				// Recreated after ALL pins succeed, including on each retry.
				verified := make(historyVerification, len(snapshots))
				for _, snap := range snapshots {
					verified[historyDocumentKey{repo: repo, snapshot: snap.ID, document: snap.DocumentRef()}] = struct{}{}
				}
				return verified, nil
			})
		})
	})
}

func historyRootSet(roots []domain.ContentHash) map[domain.ContentHash]struct{} {
	set := make(map[domain.ContentHash]struct{}, len(roots))
	for _, id := range roots {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	return set
}
