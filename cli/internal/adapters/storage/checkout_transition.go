package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type checkoutJournal struct {
	Version    int                         `json:"version"`
	Transition outbound.CheckoutTransition `json:"transition"`
	Lifecycle  *domain.Ref                 `json:"lifecycle,omitempty"`
	Position   *domain.WorkingPosition     `json:"position,omitempty"`
}

func (s *FileStore) checkoutJournalPath() string {
	return filepath.Join(s.storeDir(), "checkout-transition.json")
}

// ReadCheckoutState does not recover interrupted mutations. The commit path
// recovers under the shared ref lock and then compares this complete selection.
func (s *FileStore) ReadCheckoutState(ctx context.Context, repo string) (outbound.CheckoutState, error) {
	if err := ctx.Err(); err != nil {
		return outbound.CheckoutState{}, err
	}
	var state outbound.CheckoutState
	head, err := s.GetRef(ctx, repo, domain.RefHEAD, "HEAD")
	if errors.Is(err, domain.ErrNotFound) {
		state.HeadMissing = true
	} else if err != nil {
		return state, err
	} else {
		state.Head = head
	}
	p, err := s.readPosition()
	if err == nil {
		state.Position = &p
	} else if !errors.Is(err, domain.ErrNotFound) {
		return state, err
	}
	if s.worktreeID != "" {
		indexRepo := repo
		if indexRepo == "" && state.Position != nil {
			indexRepo = state.Position.RepoID
		}
		if indexRepo != "" {
			index, err := s.readStagingIndex(indexRepo)
			if err != nil {
				return state, err
			}
			state.IndexRevision = index.Revision
		}
	}
	return state, nil
}

func (s *FileStore) CommitCheckout(ctx context.Context, change outbound.CheckoutTransition) error {
	if change.ExpectedGitCommit != "" {
		if !domain.ValidGitOID(change.ExpectedGitCommit) {
			return domain.ErrHashMismatch
		}
		if s.worktreeID != "" && (s.gitCommit != change.ExpectedGitCommit || (change.ExpectedGitBranch != "" && s.gitBranch != change.ExpectedGitBranch)) {
			return domain.ErrCodePositionMismatch
		}
	} else if change.ExpectedGitBranch != "" {
		return domain.ErrHashMismatch
	}
	if err := change.MemoryPin.Validate(); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(change.ExpectedMemoryHash); err != nil {
		return err
	}
	if err := domain.ValidateRef(change.Head); err != nil {
		return err
	}
	if change.Head.Kind != domain.RefHEAD || change.Head.Name != "HEAD" || change.Head.RepoID != change.RepoID {
		return domain.ErrInvalidRef
	}
	if change.Branch != nil {
		if err := domain.ValidateRef(*change.Branch); err != nil {
			return err
		}
		if change.Branch.Kind != domain.RefBranch || change.Branch.RepoID != change.RepoID || change.Head.Symbolic != change.Branch.Name || change.Head.Target != "" {
			return domain.ErrInvalidRef
		}
	} else if change.CreateBranch || change.Head.Symbolic != "" || change.Head.Target == "" {
		return domain.ErrInvalidRef
	}
	if change.RestoreEvent != nil {
		if err := domain.ValidateHistoryEvent(*change.RestoreEvent); err != nil {
			return err
		}
		if !change.CreateBranch || change.Branch == nil || change.RestoreEvent.Kind != "birth" || change.RestoreEvent.Branch != change.Branch.Name || change.RestoreEvent.Target != change.Branch.Target || change.RestoreEvent.BranchID != change.Branch.BranchID {
			return domain.ErrHashMismatch
		}
	}
	return s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			current, err := s.ReadCheckoutState(ctx, change.RepoID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, change.Expected) {
				return domain.ErrSyncConflict
			}
			op := checkoutJournal{Version: 2, Transition: change}
			if change.RestoreEvent != nil {
				events, err := s.listHistoryEvents(change.RestoreEvent.RepoID)
				if err != nil {
					return err
				}
				state, err := domain.ProjectContextBranches(events)
				if err != nil {
					return err
				}
				if state.Released[change.RestoreEvent.Branch] != change.RestoreEvent.BindingParent {
					return domain.ErrSyncConflict
				}
				if _, err := domain.ProjectContextBranches(append(events, *change.RestoreEvent)); err != nil {
					return err
				}
			}
			target := change.Head.Target
			if change.Branch != nil {
				ref := *change.Branch
				target = ref.Target
				current, err := s.GetRef(ctx, change.RepoID, domain.RefBranch, ref.Name)
				if change.CreateBranch {
					if err == nil {
						return domain.ErrBranchExists
					}
					if !errors.Is(err, domain.ErrNotFound) {
						return err
					}
					refs, err := s.listRefsRaw(ctx, change.RepoID)
					if err != nil {
						return err
					}
					generation, err := domain.NextBranchLifecycleGeneration(refs, ref.Name)
					if err != nil {
						return err
					}
					lifecycle, err := domain.NewBranchLifecycleRef(change.RepoID, ref.Name, ref.Target, generation, domain.BranchActive)
					if err != nil {
						return err
					}
					op.Lifecycle = &lifecycle
					if ref.BranchID == "" {
						events, err := s.listHistoryEvents(change.RepoID)
						if err != nil {
							return err
						}
						branches, err := domain.ProjectContextBranches(events)
						if err != nil {
							return err
						}
						ref.BranchID = branches.Identity(change.RepoID, ref.Name)
					}
				} else {
					if err != nil {
						return err
					}
					if current != ref {
						return domain.ErrSyncConflict
					}
				}
				op.Transition.Branch = &ref
			}
			// Attachment writers use this same lock. Compare and durably accept the
			// prepared hash together; recovery must keep that hash even if it moves later.
			return s.withSnapshotMutationLock(ctx, target, func() error {
				snap, err := s.GetSnapshot(ctx, target)
				if err != nil {
					return err
				}
				if change.RepoID != "" && snap.RepoID != change.RepoID {
					return domain.ErrHashMismatch
				}
				if snap.MemoryHash != change.ExpectedMemoryHash {
					return domain.ErrSelectionChanged
				}
				if _, err := s.GetDoc(ctx, snap.DocHash); err != nil {
					return err
				}
				memoryHash, memorySource := checkoutPreparedMemory(change, target)
				if err := s.validateCheckoutMemory(ctx, snap.RepoID, target, memoryHash, memorySource); err != nil {
					return err
				}
				if s.worktreeID != "" {
					// Naming a historical selection must not turn its explicit memory
					// pin back into a live attachment on the next load or checkout.
					p := domain.WorkingPosition{RepoID: snap.RepoID, WorktreeID: s.worktreeID, GitCommit: s.gitCommit, Snapshot: target, MemoryHash: memoryHash, MemorySource: memorySource, MemoryPinned: true, Rewound: change.MemoryPin != nil}
					if op.Transition.Branch != nil {
						p.Branch, p.BranchID = op.Transition.Branch.Name, op.Transition.Branch.BranchID
						p.SharedTarget = target
					} else {
						p.BranchID = "position"
						p.Rewound = true
					}
					if s.gitBranch != p.Branch {
						p.LocalBranch = s.gitBranch
					}
					op.Position = &p
				}
				raw, err := json.Marshal(op)
				if err != nil {
					return err
				}
				if err := writeAtomic(s.checkoutJournalPath(), raw); err != nil {
					return err
				}
				return s.recoverCheckoutTransition()
			})
		})
	})
}

func checkoutPreparedMemory(change outbound.CheckoutTransition, target domain.ContentHash) (domain.ContentHash, domain.ContentHash) {
	if change.MemoryPin != nil {
		return change.MemoryPin.MemoryHash, change.MemoryPin.SnapshotID
	}
	if change.ExpectedMemoryHash != "" {
		return change.ExpectedMemoryHash, target
	}
	return "", ""
}

func (s *FileStore) validateCheckoutMemory(ctx context.Context, repo string, target, hash, source domain.ContentHash) error {
	if hash == "" {
		if source != "" {
			return domain.ErrHashMismatch
		}
		return nil
	}
	if source == "" {
		source = target // Legacy journals used an implicit target owner.
	}
	owner, err := s.GetSnapshot(ctx, source)
	if err != nil {
		return err
	}
	if owner.RepoID != repo {
		return domain.ErrHashMismatch
	}
	if _, err := s.GetDoc(ctx, owner.DocHash); err != nil {
		return err
	}
	memory, err := s.GetMemory(ctx, hash)
	if err != nil {
		return err
	}
	if memory.SnapshotID != source {
		return domain.ErrHashMismatch
	}
	return nil
}

// Validate without opening referenced objects or replaying writes. Collection
// must reject corrupt journals even when deciding about an unrelated object.
func checkoutJournalTarget(op checkoutJournal) (domain.ContentHash, error) {
	if op.Version != 1 && op.Version != 2 {
		return "", domain.ErrHashMismatch
	}
	if err := op.Transition.MemoryPin.Validate(); err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(op.Transition.ExpectedMemoryHash); err != nil {
		return "", err
	}
	if err := domain.ValidateRef(op.Transition.Head); err != nil {
		return "", err
	}
	if op.Transition.Head.Kind != domain.RefHEAD || op.Transition.Head.Name != "HEAD" || op.Transition.Head.RepoID != op.Transition.RepoID {
		return "", domain.ErrHashMismatch
	}
	if op.Transition.Branch == nil && (op.Transition.CreateBranch || op.Transition.Head.Symbolic != "" || op.Transition.Head.Target == "") {
		return "", domain.ErrHashMismatch
	}
	target := op.Transition.Head.Target
	if op.Transition.Branch != nil {
		if err := domain.ValidateRef(*op.Transition.Branch); err != nil {
			return "", err
		}
		if op.Transition.Branch.Kind != domain.RefBranch || op.Transition.Branch.RepoID != op.Transition.RepoID || op.Transition.Head.Symbolic != op.Transition.Branch.Name || op.Transition.Head.Target != "" {
			return "", domain.ErrHashMismatch
		}
		target = op.Transition.Branch.Target
	}
	if op.Position != nil {
		_, hexErr := hex.DecodeString(op.Position.WorktreeID)
		if hexErr != nil || len(op.Position.WorktreeID) != 32 || op.Position.WorktreeID != strings.ToLower(op.Position.WorktreeID) || op.Position.Snapshot != target || (op.Transition.RepoID != "" && op.Position.RepoID != op.Transition.RepoID) {
			return "", domain.ErrHashMismatch
		}
		for _, hash := range []domain.ContentHash{op.Position.MemoryHash, op.Position.MemorySource, op.Position.SharedTarget} {
			if err := domain.ValidateOptionalContentHash(hash); err != nil {
				return "", err
			}
		}
		// Version 1 journals predate the explicit preparation fence. Their
		// already accepted position remains authoritative during recovery.
		if op.Version >= 2 {
			hash, source := checkoutPreparedMemory(op.Transition, target)
			if op.Position.MemoryHash != hash || op.Position.MemorySource != source || !op.Position.MemoryPinned {
				return "", domain.ErrHashMismatch
			}
		}
		if op.Transition.Branch != nil {
			if op.Position.Branch != op.Transition.Branch.Name || op.Position.BranchID != op.Transition.Branch.BranchID || op.Position.SharedTarget != target {
				return "", domain.ErrHashMismatch
			}
		} else if op.Position.Branch != "" || op.Position.SharedTarget != "" {
			return "", domain.ErrHashMismatch
		}
	}
	if op.Transition.RestoreEvent != nil {
		e := op.Transition.RestoreEvent
		if err := domain.ValidateHistoryEvent(*e); err != nil {
			return "", err
		}
		if op.Transition.Branch == nil || !op.Transition.CreateBranch || e.Kind != "birth" || e.Target != target || (op.Transition.RepoID != "" && e.RepoID != op.Transition.RepoID) || e.BranchID != op.Transition.Branch.BranchID || e.Branch != op.Transition.Branch.Name {
			return "", domain.ErrHashMismatch
		}
	}
	if op.Lifecycle != nil {
		event, ok, err := domain.ParseBranchLifecycleRef(*op.Lifecycle)
		if err != nil {
			return "", err
		}
		if !ok || op.Transition.Branch == nil || op.Lifecycle.RepoID != op.Transition.RepoID || event.Branch != op.Transition.Branch.Name || event.Target != target || event.State != domain.BranchActive {
			return "", domain.ErrHashMismatch
		}
	}
	return target, nil
}

func (s *FileStore) HasCheckoutPin(ctx context.Context, hash domain.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := domain.ValidateContentHash(hash); err != nil {
		return false, err
	}
	raw, err := readCxtFile(s.checkoutJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var op checkoutJournal
	if json.Unmarshal(raw, &op) != nil {
		return false, domain.ErrHashMismatch
	}
	target, err := checkoutJournalTarget(op)
	if err != nil {
		return false, err
	}
	roots := []domain.ContentHash{target}
	if pin := op.Transition.MemoryPin; pin != nil {
		roots = append(roots, pin.SnapshotID)
	}
	if op.Position != nil {
		roots = append(roots, op.Position.MemorySource)
	}
	if e := op.Transition.RestoreEvent; e != nil {
		roots = append(roots, e.Source, e.SharedTarget, e.MemorySource)
	}
	for _, root := range roots {
		if root == hash {
			return true, nil
		}
	}
	return false, nil
}

// An accepted checkout finishes before any later ref writer enters. A crash
// can leave a pending journal, never an untracked half-created active branch.
func (s *FileStore) recoverCheckoutTransition() error {
	raw, err := readCxtFile(s.checkoutJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var op checkoutJournal
	if json.Unmarshal(raw, &op) != nil {
		return domain.ErrHashMismatch
	}
	target, err := checkoutJournalTarget(op)
	if err != nil {
		return err
	}
	snap, err := s.GetSnapshot(context.Background(), target)
	if err != nil {
		return err
	}
	if (op.Transition.RepoID != "" && snap.RepoID != op.Transition.RepoID) || (op.Position != nil && op.Position.RepoID != snap.RepoID) {
		return domain.ErrHashMismatch
	}
	if _, err := s.GetDoc(context.Background(), snap.DocHash); err != nil {
		return err
	}
	if op.Position != nil {
		if err := s.validateCheckoutMemory(context.Background(), snap.RepoID, target, op.Position.MemoryHash, op.Position.MemorySource); err != nil {
			return err
		}
	}
	if op.Transition.Branch != nil {
		current, err := s.getRefRaw(context.Background(), op.Transition.RepoID, domain.RefBranch, op.Transition.Branch.Name)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err == nil && (current.Target != op.Transition.Branch.Target || (op.Transition.RestoreEvent == nil && current.BranchID != "" && current.BranchID != op.Transition.Branch.BranchID)) {
			return domain.ErrSyncConflict
		}
		if errors.Is(err, domain.ErrNotFound) && !op.Transition.CreateBranch {
			return domain.ErrSyncConflict
		}
	}
	if op.Transition.RestoreEvent != nil {
		e := op.Transition.RestoreEvent
		if e.RepoID != snap.RepoID {
			return domain.ErrHashMismatch
		}
	}
	if op.Transition.RestoreEvent != nil {
		if err := s.putHistoryEvent(*op.Transition.RestoreEvent); err != nil {
			return err
		}
	}
	if op.Lifecycle != nil {
		if err := s.putRefRaw(*op.Lifecycle); err != nil {
			return err
		}
	}
	if op.Transition.Branch != nil && op.Transition.CreateBranch {
		if err := s.putRefRaw(*op.Transition.Branch); err != nil {
			return err
		}
	}
	if op.Position != nil {
		owner := *s
		owner.worktreeID = op.Position.WorktreeID
		if err := owner.writePosition(*op.Position); err != nil {
			return err
		}
	} else {
		owner := *s
		owner.worktreeID = ""
		if err := owner.putRefRaw(op.Transition.Head); err != nil {
			return err
		}
	}
	if err := os.Remove(s.checkoutJournalPath()); err != nil {
		return err
	}
	return syncCxtParents(s.checkoutJournalPath())
}

var _ outbound.CheckoutTransactionStore = (*FileStore)(nil)
var _ outbound.CheckoutPins = (*FileStore)(nil)
