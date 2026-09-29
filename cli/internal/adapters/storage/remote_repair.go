package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *FileStore) ReadRemoteRepairReceipt(ctx context.Context, id domain.ContentHash) (outbound.RemoteRepairReceipt, error) {
	var out outbound.RemoteRepairReceipt
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if domain.ValidateContentHash(id) != nil {
		return out, domain.ErrHashMismatch
	}
	raw, err := readCxtFile(filepath.Join(s.storeDir(), "repair-receipts", hexOf(id)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return out, domain.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if json.Unmarshal(raw, &out) != nil || out.Plan.ID != id || outbound.RemoteRepairPlanID(out.Plan) != id || out.AppliedAt.IsZero() {
		return out, domain.ErrHashMismatch
	}
	return out, nil
}

func (s *FileStore) ApplyRemoteRepair(ctx context.Context, plan outbound.RemoteRepairPlan) (outbound.RemoteRepairReceipt, error) {
	var out outbound.RemoteRepairReceipt
	if plan.Version != 1 || domain.ValidateContentHash(plan.ID) != nil || plan.ID != outbound.RemoteRepairPlanID(plan) || plan.Expected.Position == nil || plan.Expected.Position.WorktreeID != s.worktreeID || plan.Expected.Position.RepoID != plan.RepoID || plan.Reason == "" {
		return out, domain.ErrHashMismatch
	}
	if (plan.Snapshot == "") == (plan.BeforeRef == nil || plan.AfterRef == nil) {
		return out, domain.ErrInvalidRef
	}
	path, err := s.remoteObservationPath(plan.RepoID, plan.Remote)
	if err != nil {
		return out, err
	}
	err = s.WithObjectsRetained(ctx, func() error {
		return s.withMutationLock(ctx, "remote-observations", filepath.Base(path), func() error {
			return s.withRefMutationLock(ctx, func() error {
				if existing, err := s.ReadRemoteRepairReceipt(ctx, plan.ID); err == nil {
					out = existing
					return nil
				} else if !errors.Is(err, domain.ErrNotFound) {
					return err
				}
				observation, err := s.ReadRemoteObservation(ctx, plan.RepoID, plan.Remote)
				if err != nil {
					return err
				}
				if observation.Revision != plan.Observation {
					return domain.ErrSelectionChanged
				}
				// A plan ID authenticates its bytes, not the proposed remote target.
				// Prove the exact pointer against the verified observation as well.
				observedTarget := false
				if plan.Snapshot != "" {
					for _, snap := range observation.Snapshots {
						if snap.ID == plan.Snapshot && snap.MemoryHash != "" && snap.MemoryHash == plan.AfterMemory {
							observedTarget = true
						}
					}
				} else {
					for _, ref := range observation.Refs {
						if ref == *plan.AfterRef {
							observedTarget = true
						}
					}
				}
				if !observedTarget {
					return domain.ErrHashMismatch
				}
				state, err := s.ReadCheckoutState(ctx, plan.RepoID)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(state, plan.Expected) {
					return domain.ErrSelectionChanged
				}
				complete := func() error {
					out = outbound.RemoteRepairReceipt{Plan: plan, AppliedAt: time.Now().UTC()}
					raw, err := json.Marshal(out)
					if err != nil {
						return err
					}
					return writeAtomic(filepath.Join(s.storeDir(), "repair-receipts", hexOf(plan.ID)+".json"), raw)
				}
				// The approved immutable intent survives a lost response or receipt write.
				// Retrying compares the exact old/new pointer; it never rewinds a later writer.
				raw, err := json.Marshal(plan)
				if err != nil {
					return err
				}
				intentPath := filepath.Join(s.storeDir(), "repair-intents", hexOf(plan.ID)+".json")
				if plan.Snapshot != "" {
					memory, err := s.GetMemory(ctx, plan.AfterMemory)
					if err != nil {
						return err
					}
					if memory.SnapshotID != plan.Snapshot {
						return domain.ErrHashMismatch
					}
					if plan.BeforeMemory != "" {
						if plan.PreservedMemory == nil || plan.PreservedMemory.SnapshotID != plan.Snapshot {
							return domain.ErrHashMismatch
						}
						hash, err := s.PutMemory(ctx, *plan.PreservedMemory)
						if err != nil {
							return err
						}
						if hash != plan.BeforeMemory {
							return domain.ErrHashMismatch
						}
					}
					return s.withSnapshotMutationLock(ctx, plan.Snapshot, func() error {
						snap, err := s.GetSnapshot(ctx, plan.Snapshot)
						if err != nil {
							return err
						}
						if snap.RepoID != plan.RepoID || (snap.MemoryHash != plan.BeforeMemory && snap.MemoryHash != plan.AfterMemory) {
							return domain.ErrSyncConflict
						}
						if err := writeAtomic(intentPath, raw); err != nil {
							return err
						}
						snap.MemoryHash = plan.AfterMemory
						data, err := json.Marshal(snap)
						if err != nil {
							return err
						}
						if err := writeAtomic(s.objectPath("snapshots", snap.ID), data); err != nil {
							return err
						}
						return complete()
					})
				}
				before, after := *plan.BeforeRef, *plan.AfterRef
				if before.RepoID != plan.RepoID || after.RepoID != plan.RepoID || before.Name != after.Name || before.Kind != after.Kind || before.Kind == domain.RefHEAD || before.BranchID != after.BranchID || before.Symbolic != "" || after.Symbolic != "" {
					return domain.ErrInvalidRef
				}
				if err := domain.ValidateRef(after); err != nil {
					return err
				}
				current, err := s.GetRef(ctx, plan.RepoID, before.Kind, before.Name)
				if err != nil {
					return err
				}
				if current != before && current != after {
					return domain.ErrSyncConflict
				}
				snap, err := s.GetSnapshot(ctx, after.Target)
				if err != nil {
					return err
				}
				if snap.RepoID != plan.RepoID {
					return domain.ErrHashMismatch
				}
				if _, err := s.GetDoc(ctx, snap.DocHash); err != nil {
					return err
				}
				if err := writeAtomic(intentPath, raw); err != nil {
					return err
				}
				if err := s.putRefRaw(domain.Ref{Kind: domain.RefTag, Name: "cxt-retained/repair/" + hexOf(plan.ID), RepoID: plan.RepoID, Target: before.Target}); err != nil {
					return err
				}
				if err := s.putRefRaw(after); err != nil {
					return err
				}
				return complete()
			})
		})
	})
	return out, err
}

var _ outbound.RemoteRepairStore = (*FileStore)(nil)
