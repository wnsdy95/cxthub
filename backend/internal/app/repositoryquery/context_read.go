package repositoryquery

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *ContextQueries) QueryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	if err := domain.ValidateContextSegmentSelection(selection); err != nil {
		return domain.ContextQueryView{}, err
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.ContextQueryView{}, err
	}
	if selection.CodeCommit != "" && domain.ValidateGitOID(selection.CodeCommit) != nil {
		return domain.ContextQueryView{}, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return domain.ContextQueryView{}, err
	}
	var result domain.ContextQueryView
	err := queries.scope.ReadContext(ctx, repo, func(bound context.Context) error {
		var err error
		result, err = queries.queryContext(bound, repo, selection)
		if err == nil {
			err = bound.Err()
		}
		return err
	})
	if err != nil && queries.atomicRead {
		return domain.ContextQueryView{}, err
	}
	return result, err
}

func (queries *ContextQueries) queryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	metadata, err := queries.metadata.GetRepo(ctx, repo)
	if err != nil {
		return domain.ContextQueryView{}, err
	}
	if metadata.ID != repo {
		return domain.ContextQueryView{}, domain.ErrIntegrity
	}
	var revision domain.RepositoryRevision
	if queries.revisions != nil {
		revision, err = queries.revisions.RepositoryRevision(ctx, repo)
		if err != nil {
			return domain.ContextQueryView{}, err
		}
	}
	key, err := queries.segmentCacheKey(ctx, repo, metadata, revision, selection)
	if err != nil {
		return domain.ContextQueryView{}, err
	}
	if key.cacheable() {
		if basis, hit := queries.cache.get(key); hit {
			return queries.contextSegmentPage(ctx, repo, selection, basis)
		}
	}
	view, err := LoadRepositoryView(ctx, repo, RepositoryViewReaders{
		Metadata: queries.metadata, History: queries.history, Revisions: queries.revisions, Archives: queries.archives,
	})
	if err != nil {
		return domain.ContextQueryView{}, err
	}
	branch := metadata.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	result, err := domain.SelectContext(view, selection, branch)
	if err != nil {
		return result, err
	}
	if selection.Scope != "current" {
		return queries.finishContextQuery(ctx, repo, selection, result, view, key)
	}
	name := selection.Branch
	if name == "" {
		name = selection.Position
		if strings.EqualFold(name, "HEAD") {
			name = branch
		}
	}
	for _, ref := range view.Refs {
		if ref.Kind != domain.RefBranch || ref.Name != name {
			continue
		}
		if ref.BranchID == "" {
			bindings, err := domain.ProjectContextBranches(view.History)
			if err != nil {
				return result, err
			}
			ref.BranchID = bindings.Identity(string(repo), name)
		}
		ref.Target = result.Position
		inputs, err := contextprojection.NewInputs(ctx, repo, view.Snapshots, view.History)
		if err != nil {
			return result, err
		}
		evidenceMetadata, err := queries.evidenceMetadata.GetRepo(ctx, repo)
		if err != nil {
			return result, err
		}
		if evidenceMetadata.ID != repo {
			return result, domain.ErrIntegrity
		}
		evidence, err := evidencequery.NewGitEvidence(evidencequery.GitEvidenceConfig{
			RepoID: repo, Origin: evidenceMetadata.GitRemoteURL, Trees: queries.trees, Deltas: queries.deltas,
			RequireComplete: queries.requireComplete,
		})
		if err != nil {
			return result, err
		}
		code := selection.CodeCommit
		if code == "" {
			code, err = contextprojection.ResolveBranchCodeFromIndex(ctx, ref, inputs.History(), evidence)
			if err != nil {
				return result, err
			}
		}
		plan, err := contextprojection.PrepareBranchContext(ctx, ref, code, inputs, evidence)
		if err != nil {
			return result, err
		}
		prs, err := queries.contributions.VerifiedPR(ctx, repo, view.History)
		if err != nil {
			return result, err
		}
		git, err := queries.contributions.VerifiedGit(ctx, repo, view.History)
		if err != nil {
			return result, err
		}
		inclusion, err := plan.Finish(ctx, legacyProjectionContributions{prs: prs, git: git}, evidence)
		if err != nil {
			return result, err
		}
		result.Snapshots = []domain.Snapshot{}
		for _, identifier := range inclusion.SnapshotIDs {
			result.Snapshots = append(result.Snapshots, inputs.Snapshot(identifier))
		}
		result.Inclusion = &inclusion
		result.Branch = name
		basis, err := json.Marshal(struct {
			Version   int
			Inclusion domain.BranchContext
			Snapshots []domain.Snapshot
		}{1, inclusion, result.Snapshots})
		if err != nil {
			return result, err
		}
		result.StateHash = domain.HashContent(basis)
		break
	}
	return queries.finishContextQuery(ctx, repo, selection, result, view, key)
}
