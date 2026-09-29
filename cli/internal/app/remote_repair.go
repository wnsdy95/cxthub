package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var ErrPullRepairRequired = errors.New("pull --force requires an explicit remote repair preview and apply; no remote pointers were adopted")

// RemoteRepairInput targets one existing ref OR one snapshot memory pointer.
// A caller must display the returned plan and submit that exact ID to Apply.
type RemoteRepairInput struct {
	RepoID, Cwd, Reason string
	RefKind             domain.RefKind
	RefName             string
	Snapshot            domain.ContentHash
}

type RemoteRepairService struct {
	sync  *SyncRepoService
	store outbound.RemoteRepairStore
	code  outbound.CodePosition
}

func NewRemoteRepairService(sync *SyncRepoService, store outbound.RemoteRepairStore, code outbound.CodePosition) *RemoteRepairService {
	return &RemoteRepairService{sync: sync, store: store, code: code}
}

func (s *RemoteRepairService) Preview(ctx context.Context, in RemoteRepairInput) (outbound.RemoteRepairPlan, error) {
	var plan outbound.RemoteRepairPlan
	if s.sync == nil || s.store == nil || s.code == nil || s.sync.observationRemote() == "configured" {
		return plan, fmt.Errorf("remote repair requires explicit endpoint identity and transaction support")
	}
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 {
		return plan, fmt.Errorf("repair requires a reason of at most 512 bytes")
	}
	if in.Snapshot == "" {
		if in.RefName == "" || (in.RefKind != domain.RefBranch && in.RefKind != domain.RefSession && in.RefKind != domain.RefTag) || strings.HasPrefix(in.RefName, "cxt-") {
			return plan, domain.ErrInvalidRef
		}
	} else if in.RefName != "" || in.RefKind != "" || domain.ValidateContentHash(in.Snapshot) != nil {
		return plan, domain.ErrInvalidRef
	}
	repo, err := s.sync.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return plan, err
	}
	if in.RepoID == "" {
		in.RepoID = string(repo.ID)
	}
	if in.RepoID != string(repo.ID) {
		return plan, domain.ErrHashMismatch
	}
	state, err := s.store.ReadCheckoutState(ctx, in.RepoID)
	if err != nil {
		return plan, err
	}
	if err := s.checkPosition(ctx, in.Cwd, in.RepoID, state); err != nil {
		return plan, err
	}
	if _, err := s.sync.Pull(ctx, inbound.SyncInput{RepoID: in.RepoID, Cwd: in.Cwd, FetchOnly: true}); err != nil {
		return plan, err
	}
	observed, err := s.store.ReadRemoteObservation(ctx, in.RepoID, s.sync.observationRemote())
	if err != nil {
		return plan, err
	}
	plan = outbound.RemoteRepairPlan{Version: 1, RepoID: in.RepoID, Remote: s.sync.observationRemote(), Reason: strings.TrimSpace(in.Reason), Observation: observed.Revision, Expected: state}
	if in.Snapshot != "" {
		local, err := s.sync.store.GetSnapshot(ctx, in.Snapshot)
		if err != nil {
			return plan, err
		}
		if local.RepoID != in.RepoID {
			return plan, domain.ErrHashMismatch
		}
		var remote *domain.Snapshot
		for i := range observed.Snapshots {
			if observed.Snapshots[i].ID == in.Snapshot {
				remote = &observed.Snapshots[i]
				break
			}
		}
		// Empty remote memory is not authority to erase an attachment.
		if remote == nil || remote.MemoryHash == "" {
			return plan, domain.ErrNotFound
		}
		if local.MemoryHash == remote.MemoryHash {
			return plan, fmt.Errorf("memory already matches the remote; no repair needed")
		}
		plan.Snapshot, plan.BeforeMemory, plan.AfterMemory = local.ID, local.MemoryHash, remote.MemoryHash
		if local.MemoryHash != "" {
			old, err := s.sync.store.GetMemory(ctx, local.MemoryHash)
			if err != nil {
				return plan, err
			}
			if err := validateMemoryAttachmentObject(old, local.MemoryHash, local.ID); err != nil {
				return plan, err
			}
			plan.PreservedMemory = &old
		}
	} else {
		before, err := s.sync.store.GetRef(ctx, in.RepoID, in.RefKind, in.RefName)
		if err != nil {
			return plan, err
		}
		var after *domain.Ref
		for i := range observed.Refs {
			ref := observed.Refs[i]
			if ref.Kind == in.RefKind && ref.Name == in.RefName {
				after = &ref
				break
			}
		}
		if after == nil {
			return plan, domain.ErrNotFound
		}
		if before == *after {
			return plan, fmt.Errorf("ref already matches the remote; no repair needed")
		}
		if before.BranchID != after.BranchID || before.Symbolic != "" || after.Symbolic != "" {
			return plan, fmt.Errorf("%w: branch identity changes require branch recovery, not pointer repair", domain.ErrSyncConflict)
		}
		plan.BeforeRef, plan.AfterRef = &before, after
	}
	if err := s.checkPosition(ctx, in.Cwd, in.RepoID, state); err != nil {
		return plan, err
	}
	plan.ID = outbound.RemoteRepairPlanID(plan)
	return plan, nil
}

func (s *RemoteRepairService) Apply(ctx context.Context, cwd string, approvedID domain.ContentHash, plan outbound.RemoteRepairPlan) (outbound.RemoteRepairReceipt, error) {
	var zero outbound.RemoteRepairReceipt
	if plan.Version != 1 || approvedID != plan.ID || plan.ID != outbound.RemoteRepairPlanID(plan) || plan.Remote != s.sync.observationRemote() {
		return zero, domain.ErrHashMismatch
	}
	// A retry reports the original local receipt. It does not reactivate the
	// repaired pointer or claim the remote still has the same current state.
	receipt, err := s.store.ReadRemoteRepairReceipt(ctx, plan.ID)
	if err == nil {
		return receipt, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return zero, err
	}
	if err := s.checkPosition(ctx, cwd, plan.RepoID, plan.Expected); err != nil {
		return zero, err
	}
	// Refresh using the read-authorized transport. Revocation, changed remote
	// evidence or a moved pointer invalidates a previously displayed plan.
	if _, err := s.sync.Pull(ctx, inbound.SyncInput{RepoID: plan.RepoID, Cwd: cwd, FetchOnly: true}); err != nil {
		return zero, err
	}
	observed, err := s.store.ReadRemoteObservation(ctx, plan.RepoID, plan.Remote)
	if err != nil {
		return zero, err
	}
	if observed.Revision != plan.Observation {
		return zero, domain.ErrSelectionChanged
	}
	if err := s.checkPosition(ctx, cwd, plan.RepoID, plan.Expected); err != nil {
		return zero, err
	}
	return s.store.ApplyRemoteRepair(ctx, plan)
}

func (s *RemoteRepairService) checkPosition(ctx context.Context, cwd, repoID string, expected outbound.CheckoutState) error {
	repo, err := s.sync.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	if string(repo.ID) != repoID {
		return domain.ErrHashMismatch
	}
	actual, err := s.code.CurrentCommit(ctx, cwd)
	if err != nil {
		return err
	}
	if expected.Position == nil || expected.Position.RepoID != repoID || expected.Position.WorktreeID == "" || !domain.ValidGitOID(actual) || expected.Position.GitCommit != actual {
		return domain.ErrSelectionChanged
	}
	branch, err := s.sync.gitCtx.CurrentBranch(ctx, cwd)
	if err != nil {
		return err
	}
	if branch != expected.Position.GitBranch() {
		return domain.ErrSelectionChanged
	}
	current, err := s.store.ReadCheckoutState(ctx, repoID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return domain.ErrSelectionChanged
	}
	return nil
}
