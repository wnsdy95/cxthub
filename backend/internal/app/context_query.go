package app

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
)

func (s *Service) QueryContext(ctx context.Context, repo domain.ContentHash, in domain.ContextSelection) (domain.ContextQueryView, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.ContextQueryView{}, err
	}
	if in.CodeCommit != "" && domain.ValidateGitOID(in.CodeCommit) != nil {
		return domain.ContextQueryView{}, domain.ErrValidation
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.ContextQueryView, error) {
		r, err := s.meta.GetRepo(ctx, repo)
		if err != nil {
			return domain.ContextQueryView{}, err
		}
		view, err := s.loadRepositoryView(ctx, repo)
		if err != nil {
			return domain.ContextQueryView{}, err
		}
		branch := r.DefaultBranch
		if branch == "" {
			branch = "main"
		}
		out, err := domain.SelectContext(view, in, branch)
		if err != nil || in.Scope != "current" {
			return out, err
		}
		name := in.Branch
		if name == "" {
			name = in.Position
			if strings.EqualFold(name, "HEAD") {
				name = branch
			}
		}
		for _, ref := range view.Refs {
			if ref.Kind != domain.RefBranch || ref.Name != name {
				continue
			}
			if ref.BranchID == "" {
				bindings, e := domain.ProjectContextBranches(view.History)
				if e != nil {
					return out, e
				}
				ref.BranchID = bindings.Identity(string(repo), name)
			}
			ref.Target = out.Position
			code := in.CodeCommit
			if code == "" {
				code = branchCode(ref, view.History)
			}
			evidence, e := s.newCodeEvidence(ctx, repo)
			if e != nil {
				return out, e
			}
			included, e := s.branchContext(ctx, ref, code, view.Snapshots, view.History, evidence)
			if e != nil {
				return out, e
			}
			byID := map[domain.ContentHash]domain.Snapshot{}
			for _, snapshot := range view.Snapshots {
				byID[snapshot.ID] = snapshot
			}
			out.Snapshots = []domain.Snapshot{}
			for _, id := range included.SnapshotIDs {
				out.Snapshots = append(out.Snapshots, byID[id])
			}
			out.Inclusion = &included
			out.Branch = name
			basis, e := json.Marshal(struct {
				Version   int
				Inclusion domain.BranchContext
				Snapshots []domain.Snapshot
			}{1, included, out.Snapshots})
			if e != nil {
				return out, e
			}
			out.StateHash = domain.HashContent(basis)
			break
		}
		return out, nil
	})
}
