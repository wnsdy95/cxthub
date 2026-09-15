package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"sync"
	"time"
)

// AppendRef shares join's validated prepared/committed redo format, but its
// plan is derived from reachability rather than session-reordering semantics.
func (s *FSStore) AppendRef(ctx context.Context, repoID domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	ref.RepoID = repoID
	if err := domain.ValidateRef(ref); err != nil {
		return err
	}
	if ref.Kind != domain.RefBranch {
		return domain.ErrValidation
	}
	if err := domain.ValidateContentHash(expected); err != nil {
		return err
	}
	lock := s.refLock(repoID, ref.Kind, ref.Name)
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryEvent(ctx, repoID); err != nil {
		return err
	}
	path := s.joinJournalPath(repoID)
	if _, err := os.Stat(path); err == nil {
		return domain.ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	current, err := s.GetRef(ctx, repoID, ref.Kind, ref.Name)
	if err != nil {
		return err
	}
	if current.Target != expected {
		return domain.ErrRefConflict
	}
	if err := s.validateContextWrite(ctx, repoID, ref); err != nil {
		return err
	}
	snaps, err := s.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return err
	}
	patches, err := appendPlan(snaps, expected, ref.Target)
	if err != nil {
		return err
	}
	var locks []*sync.Mutex
	for _, patch := range patches {
		lock := s.snapshotLock(repoID, patch.SnapshotID)
		lock.Lock()
		locks = append(locks, lock)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	journal := fsJoinJournal{Version: fsJoinJournalVersion, Phase: fsJoinPrepared, RepoID: repoID, Branch: ref.Name, BranchID: current.BranchID, ExpectedHead: expected, NewHead: ref.Target, CreatedAt: time.Now().UTC()}
	for _, patch := range patches {
		snap, err := s.GetSnapshot(ctx, repoID, patch.SnapshotID)
		if err != nil {
			return err
		}
		if snap.GraftSeq != patch.ExpectedSeq {
			return domain.ErrConflict
		}
		before, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		snap.GraftParents = patch.Parents
		snap.Grafted = len(patch.Parents) > 0
		snap.GraftSeq++
		after, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		journal.Snapshots = append(journal.Snapshots, fsJoinSnapshot{ID: snap.ID, Before: before, After: after})
	}
	if err := validateJoinTransitionOrder(snaps, journal.Snapshots, repoID); err != nil {
		return err
	}
	if err := s.writeJoinJournal(path, journal); err != nil {
		return err
	}
	if err := s.applyJoinProjection(journal, true); err != nil {
		return s.failPreparedJoin(path, journal, err)
	}
	journal.Phase = fsJoinCommitted
	if err := s.writeJoinJournal(path, journal); err != nil {
		return s.failPreparedJoin(path, journal, err)
	}
	return s.finishCommittedJoin(path, journal)
}
