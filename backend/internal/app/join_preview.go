package app

import (
	"context"
	"errors"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) PreviewJoin(ctx context.Context, in inbound.JoinPreviewInput) (inbound.JoinPreviewOutput, error) {
	if err := domain.ValidateContentHash(in.Snapshot); err != nil {
		return inbound.JoinPreviewOutput{}, err
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (inbound.JoinPreviewOutput, error) {
		if err := s.authorizeJoin(ctx, in.RepoID, in.ActorID, false); err != nil {
			return inbound.JoinPreviewOutput{}, err
		}
		g, err := s.loadJoinGraph(ctx, in.RepoID)
		if err != nil {
			return inbound.JoinPreviewOutput{}, err
		}
		found := false
		for _, snap := range g.Snapshots {
			found = found || snap.ID == in.Snapshot
		}
		if !found {
			return inbound.JoinPreviewOutput{}, domain.ErrNotFound
		}
		out := inbound.JoinPreviewOutput{Snapshot: in.Snapshot, Branches: []inbound.JoinBranchOption{}, DropTargets: []domain.ContentHash{}}
		for _, ref := range g.Refs {
			if ref.Kind == domain.RefBranch && ref.Name != domain.HeadRefName && ref.Target != "" && g.Memberships[in.Snapshot][ref.Name] {
				out.Branches = append(out.Branches, inbound.JoinBranchOption{Branch: ref.Name, BranchID: ref.BranchID})
			}
		}
		sort.Slice(out.Branches, func(i, j int) bool { return out.Branches[i].Branch < out.Branches[j].Branch })
		branch := in.Branch
		if branch == "" && len(out.Branches) == 1 {
			branch = out.Branches[0].Branch
		}
		if branch == "" {
			out.Reason = "branch_required"
			return out, nil
		}
		p, err := domain.PlanJoin(g, domain.JoinRequest{RepoID: in.RepoID, Source: in.Snapshot, Branch: branch})
		if err != nil {
			var denied *domain.JoinPolicyError
			if errors.As(err, &denied) {
				out.Reason = denied.Code
				out.Branch = branch
				return out, nil
			}
			return out, err
		}
		out.Branch, out.BranchID, out.ExpectedHead = p.Branch, p.BranchID, p.ExpectedHead
		out.Tip, out.Descendants = p.Segment[len(p.Segment)-1], len(p.Segment)-1
		out.OnlyRevision = p.Revision()
		whole := p
		whole.NewHead, whole.RemainingTip = out.Tip, ""
		out.AllRevision = whole.Revision()
		byID := make(map[domain.ContentHash]domain.Snapshot, len(g.Snapshots))
		for _, snap := range g.Snapshots {
			byID[snap.ID] = snap
		}
		seen := map[domain.ContentHash]bool{}
		for id := p.ExpectedHead; id != "" && !seen[id]; {
			seen[id] = true
			out.DropTargets = append(out.DropTargets, id)
			snap := byID[id]
			if len(snap.Parents) == 0 {
				break
			}
			id = snap.Parents[0]
		}
		return out, nil
	})
}

// ConfirmJoin is the user-facing command. A preview is never an authorization
// capability: membership and the exact plan are checked again inside the write.
func (s *Service) ConfirmJoin(ctx context.Context, in inbound.ConfirmJoinInput) (inbound.JoinOutput, error) {
	if in.PlanRevision == "" || in.ExpectedHead == "" {
		return inbound.JoinOutput{}, domain.ErrJoinPreviewChanged
	}
	return repositoryWrite(ctx, s, in.RepoID, func(ctx context.Context) (inbound.JoinOutput, error) {
		if err := s.authorizeJoin(ctx, in.RepoID, in.ActorID, true); err != nil {
			return inbound.JoinOutput{}, err
		}
		return s.join(ctx, in.JoinInput)
	})
}

func (s *Service) authorizeJoin(ctx context.Context, repoID domain.ContentHash, actor string, lock bool) error {
	if actor == "" {
		return domain.ErrUnauthorized
	}
	if s.repositories == nil {
		return domain.ErrForbidden
	}
	repo, err := s.meta.GetRepo(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.RepositoryID == "" {
		return domain.ErrForbidden
	}
	if lock {
		if _, tx := s.meta.(outbound.RepositoryTransactions); tx {
			locker, ok := s.repositories.(outbound.RepositoryAccessLocker)
			if !ok {
				return domain.ErrForbidden
			}
			if err := locker.LockRepositoryAccess(ctx, repo.RepositoryID, actor); err != nil {
				return err
			}
		}
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return err
	}
	if repositoryRecord.Archived {
		return domain.ErrForbidden
	}
	role, ok, err := repositoryRoleFor(ctx, s.repositories, repositoryRecord, actor)
	if err != nil {
		return err
	}
	if !ok || !role.AtLeast(domain.RoleMember) {
		return domain.ErrForbidden
	}
	return nil
}
