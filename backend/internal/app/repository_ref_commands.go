package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryRefCommands struct {
	meta               RepositoryRefStore
	boundedTags        RepositoryRefBoundedTagStore
	engine             RepositoryRefEngine
	scope              RepositoryRefWriteScope
	history            RepositoryRefHistory
	append             RepositoryRefAppend
	revisions          RepositoryRefRevisions
	notifications      RepositoryRefNotifications
	transactional      bool
	profile            RepositoryRefProfile
	configurationError error
}

func (s *repositoryRefCommands) updateRefWithPending(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	out, err := s.updateRef(ctx, in)
	if err == nil {
		_, lifecycle, _ := domain.ParseBranchLifecycleRef(in.Ref)
		if in.Ref.Kind == domain.RefBranch || lifecycle {
			err = s.reconcileSharedPendingPointers(ctx, in.RepoID)
		}
	}
	return out, err
}

func (s *repositoryRefCommands) updateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	if s == nil {
		return inbound.UpdateRefOutput{}, domain.ErrValidation
	}
	if s.configurationError != nil {
		return inbound.UpdateRefOutput{}, s.configurationError
	}
	if err := domain.ValidateOptionalContentHash(in.ExpectedTarget); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	in.Ref.RepoID = in.RepoID
	if err := domain.ValidateRef(in.Ref); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if err := s.checkContextWrite(ctx, in.RepoID, in.Ref); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if in.ExpectedRevision != nil {
		if !in.Append || in.Force || in.Ref.Kind != domain.RefBranch || in.Ref.BranchID == "" {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: conditional append requires an identified branch and cannot force", domain.ErrValidation)
		}
		if err := domain.ValidateContentHash(in.ExpectedTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		if !s.transactional {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: conditional append requires repository transactions", domain.ErrConflict)
		}
		if s.revisions == nil {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: conditional append requires repository revisions", domain.ErrConflict)
		}
		revision, err := s.revisions.RepositoryRevision(ctx, in.RepoID)
		if err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		if revision.Graph != in.ExpectedRevision.Graph || revision.Evidence != in.ExpectedRevision.Evidence {
			return inbound.UpdateRefOutput{}, domain.ErrRefConflict
		}
	}
	if in.Ref.Kind == domain.RefTag && strings.HasPrefix(in.Ref.Name, "cxt/history/v1/") {
		current, err := s.meta.GetRef(ctx, in.RepoID, in.Ref.Kind, in.Ref.Name)
		if err == nil && current.Target == in.Ref.Target {
			return inbound.UpdateRefOutput{Ref: in.Ref, ServerTarget: current.Target, RequestedTarget: in.Ref.Target, Result: inbound.RefUpToDate}, nil
		}
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: history retention roots are managed by history operations", domain.ErrForbidden)
	}
	if in.Ref.Kind == domain.RefHead && in.Ref.Symbolic != "" {
		branch := strings.TrimPrefix(in.Ref.Symbolic, "refs/heads/")
		if _, err := s.meta.GetRef(ctx, in.RepoID, domain.RefBranch, branch); err != nil {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: symbolic HEAD branch %q does not exist", domain.ErrIntegrity, branch)
		}
	} else if err := s.checkRefTarget(ctx, in.RepoID, in.Ref); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if event, lifecycle, err := domain.ParseBranchLifecycleRef(in.Ref); err != nil {
		return inbound.UpdateRefOutput{}, err
	} else if lifecycle {
		if in.Force || in.Append {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: branch lifecycle events are immutable", domain.ErrValidation)
		}
		if event.State == domain.BranchArchived {
			if repo, repoErr := s.meta.GetRepo(ctx, in.RepoID); repoErr == nil &&
				repo.ProtectDefault && event.Branch == repo.DefaultBranch {
				return inbound.UpdateRefOutput{}, fmt.Errorf("%w: archiving protected branch %q is forbidden", domain.ErrForbidden, event.Branch)
			} else if repoErr != nil && !errors.Is(repoErr, domain.ErrNotFound) {
				return inbound.UpdateRefOutput{}, repoErr
			}
		}
		_, currentErr := s.meta.GetRef(ctx, in.RepoID, domain.RefTag, in.Ref.Name)
		if currentErr != nil && !errors.Is(currentErr, domain.ErrNotFound) {
			return inbound.UpdateRefOutput{}, currentErr
		}
		if err := s.meta.ApplyBranchLifecycleRef(ctx, in.RepoID, in.Ref); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		result := inbound.RefFastForward
		if currentErr == nil {
			result = inbound.RefUpToDate
		}
		return inbound.UpdateRefOutput{
			Ref: in.Ref, RequestedTarget: in.Ref.Target, ServerTarget: in.Ref.Target, Result: result,
		}, nil
	}
	cur, err := s.meta.GetRef(ctx, in.RepoID, in.Ref.Kind, in.Ref.Name)
	serverTarget := domain.ContentHash("")
	if err == nil {
		serverTarget = cur.Target
	} else if !errors.Is(err, domain.ErrNotFound) {
		return inbound.UpdateRefOutput{}, err
	}
	out := inbound.UpdateRefOutput{Ref: in.Ref, RequestedTarget: in.Ref.Target, ServerTarget: serverTarget}
	if in.ExpectedRevision != nil {
		branchID := cur.BranchID
		if branchID == "" {
			branchID = domain.LegacyContextBranchID(string(in.RepoID), in.Ref.Name)
		}
		if serverTarget != in.ExpectedTarget || branchID != in.Ref.BranchID {
			return inbound.UpdateRefOutput{}, domain.ErrRefConflict
		}
	}
	// Honor the caller's observation, including force requests. Retrying an
	// already-applied target is safe; a stale client cannot replace newer work.
	if in.ExpectedTarget != "" && serverTarget != in.ExpectedTarget && serverTarget != in.Ref.Target {
		return inbound.UpdateRefOutput{}, domain.ErrRefConflict
	}

	if in.Ref.Kind == domain.RefHead {
		if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		out.Result = inbound.RefFastForward
		out.ServerTarget = in.Ref.Target
		return out, nil
	}

	// session ref is a server internal pointer proving the git-branch scope left by a partial join. If created or moved in the general ref API, the client can spoof the scope to bypass cross-branch join restrictions. Only the same pointer echo after pull followed by push is allowed.
	if in.Ref.Kind == domain.RefSession {
		if err == nil && serverTarget == in.Ref.Target {
			out.Result = inbound.RefUpToDate
			return out, nil
		}
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: session refs are managed by join", domain.ErrForbidden)
	}

	// --force: move without ancestry classification, while still using CAS against the observed current value.
	if in.Force {
		// Protected branch: Basic branch rejects --force moves (record is P1 immutable —
		// this is a separate policy to block pointer force moves, akin to GitHub's protected branch handling).
		if in.Ref.Kind == domain.RefBranch {
			repo, rerr := s.meta.GetRepo(ctx, in.RepoID)
			if rerr != nil {
				return inbound.UpdateRefOutput{}, rerr
			}
			if repo.ProtectDefault && in.Ref.Name == repo.DefaultBranch {
				return inbound.UpdateRefOutput{}, fmt.Errorf("%w: --force move is forbidden on protected branch %q", domain.ErrForbidden, in.Ref.Name)
			}
		}
		if serverTarget == in.Ref.Target {
			out.Result = inbound.RefUpToDate
			return out, nil
		}
		if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		out.Result = inbound.RefForced
		out.ServerTarget = in.Ref.Target
		if err := s.notifyRefUpdate(ctx, in.RepoID, in.Ref, true, serverTarget == ""); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		return out, nil
	}

	// Tag immutability rule (same as git): Reject if it already exists and the target is different.
	if in.Ref.Kind == domain.RefTag {
		switch serverTarget {
		case "":
			if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, ""); err != nil {
				return inbound.UpdateRefOutput{}, err
			}
			out.Result = inbound.RefFastForward
			out.ServerTarget = in.Ref.Target
			return out, nil
		case in.Ref.Target:
			out.Result = inbound.RefUpToDate
			return out, nil
		default:
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: tag %q already points to another snapshot (--force required)", domain.ErrNonFastForward, in.Ref.Name)
		}
	}

	if s.engine == nil {
		return inbound.UpdateRefOutput{}, domain.ErrCatalogUnsupported
	}
	class, err := s.engine.ClassifyRefMove(ctx, in.RepoID, serverTarget, in.Ref.Target)
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	switch class {
	case outbound.MoveUpToDate:
		out.Result = inbound.RefUpToDate
		return out, nil
	case outbound.MoveFastForward:
		if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		out.Result = inbound.RefFastForward
		out.ServerTarget = in.Ref.Target
		if err := s.notifyRefUpdate(ctx, in.RepoID, in.Ref, false, serverTarget == ""); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		return out, nil
	case outbound.MoveDiverged:
		if in.Append {
			return s.appendDiverged(ctx, in, serverTarget, out)
		}
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: %s (must include previous commit — use --append to rebase head without loss, or retry after pull)", domain.ErrNonFastForward, in.Ref.Name)
	default: // MoveNonFastForward (behind) → like git, reject (prohibited overwrite)
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: %s (must include previous commit — retry after pull or use --force)", domain.ErrNonFastForward, in.Ref.Name)
	}
}

func (s *repositoryRefCommands) appendDiverged(ctx context.Context, in inbound.UpdateRefInput, serverTarget domain.ContentHash, out inbound.UpdateRefOutput) (inbound.UpdateRefOutput, error) {
	if s.configurationError != nil {
		return inbound.UpdateRefOutput{}, s.configurationError
	}
	if s.append == nil {
		return inbound.UpdateRefOutput{}, fmt.Errorf("atomic append storage unavailable")
	}
	if err := s.append.AppendRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	out.Result = inbound.RefAppended
	out.ServerTarget = in.Ref.Target
	if err := s.notifyRefUpdate(ctx, in.RepoID, in.Ref, false, false); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	return out, nil
}

func (s *repositoryRefCommands) reconcileSharedPendingPointers(ctx context.Context, repoID domain.ContentHash) error {
	if s.configurationError != nil {
		return s.configurationError
	}
	pendings, err := s.meta.ListPendings(ctx, repoID)
	if err != nil {
		return err
	}
	if len(pendings) == 0 {
		return nil
	}
	refs, err := s.meta.ListRefs(ctx, repoID)
	if err != nil {
		return err
	}
	roots := make([]domain.ContentHash, 0, len(refs))
	for _, ref := range refs {
		if sharedTimelineRef(ref) {
			roots = append(roots, ref.Target)
		}
	}
	if len(roots) == 0 {
		return nil
	}
	snaps, err := s.meta.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return err
	}
	parentsOf := make(map[domain.ContentHash][]domain.ContentHash, len(snaps))
	for _, snap := range snaps {
		parentsOf[snap.ID] = snap.ReachabilityParents()
	}
	shared := make(map[domain.ContentHash]bool, len(snaps))
	stack := append([]domain.ContentHash(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == "" || shared[id] {
			continue
		}
		shared[id] = true
		stack = append(stack, parentsOf[id]...)
	}
	for _, p := range pendings {
		if !shared[p.Target] {
			continue
		}
		// The target is already ref-reachable, so no hook-leaf GC is needed (or
		// allowed): immutable history remains and only the mutable pointer leaves.
		if _, err := s.meta.CompareAndDeletePending(ctx, repoID, p.SessionID, p.Target); err != nil {
			return err
		}
	}
	return nil
}

func (s *repositoryRefCommands) checkContextWrite(ctx context.Context, repoID domain.ContentHash, next domain.Ref) error {
	if s.configurationError != nil {
		return s.configurationError
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if needsState, err := domain.ContextRefWriteNeedsState(repo.ContextProtocol, next); err != nil || !needsState {
		return err
	}
	events, err := s.ListHistory(ctx, repoID)
	if err != nil {
		return err
	}
	var current *domain.Ref
	ref, err := s.meta.GetRef(ctx, repoID, next.Kind, next.Name)
	if err == nil {
		current = &ref
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return domain.ValidateContextRefWrite(repo.ContextProtocol, events, current, next)
}
