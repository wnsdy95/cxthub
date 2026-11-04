package evidencequery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type RevisionReader interface {
	RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error)
}

type CodeConfig struct {
	Scope           Scope
	Metadata        MetadataReader
	Revisions       RevisionReader
	Trees           GitTreeReader
	Deltas          GitDeltaReader
	RequireComplete bool
	AtomicRead      bool
}

type CodeQueries struct {
	scope           Scope
	metadata        MetadataReader
	revisions       RevisionReader
	trees           GitTreeReader
	deltas          GitDeltaReader
	requireComplete bool
	atomicRead      bool
}

func NewCodeQueries(config CodeConfig) (*CodeQueries, error) {
	for _, required := range []any{config.Scope, config.Metadata} {
		if absent(required) {
			return nil, domain.ErrValidation
		}
	}
	for _, optional := range []any{config.Revisions, config.Trees, config.Deltas} {
		if optional != nil && absent(optional) {
			return nil, domain.ErrCatalogUnsupported
		}
	}
	return &CodeQueries{scope: config.Scope, metadata: config.Metadata, revisions: config.Revisions,
		trees: config.Trees, deltas: config.Deltas, requireComplete: config.RequireComplete, atomicRead: config.AtomicRead}, nil
}

func (queries *CodeQueries) QueryCodeApplicability(ctx context.Context, repo domain.ContentHash, in domain.CodeSelection) (domain.CodeApplicability, error) {
	if domain.ValidateContentHash(repo) != nil || in.Validate() != nil {
		return domain.CodeApplicability{}, domain.ErrValidation
	}
	var out domain.CodeApplicability
	err := queries.scope.ReadRepository(ctx, repo, func(bound context.Context) error {
		metadata, err := queries.metadata.GetRepo(bound, repo)
		if err != nil {
			return err
		}
		if metadata.ID != repo {
			return domain.ErrIntegrity
		}
		var revision domain.RepositoryRevision
		if queries.revisions != nil {
			revision, err = queries.revisions.RepositoryRevision(bound, repo)
			if err != nil {
				return err
			}
		}
		var evidence *GitEvidence
		if queries.trees != nil && queries.deltas != nil && metadata.GitRemoteURL != "" {
			err = bound.Err()
			if err == nil {
				evidence, err = NewGitEvidence(GitEvidenceConfig{
					RepoID: repo, Origin: metadata.GitRemoteURL, Trees: queries.trees, Deltas: queries.deltas,
					RequireComplete: queries.requireComplete,
				})
			}
			if err != nil {
				out = domain.CodeApplicability{Selection: in, Revision: revision, Relation: "unknown", Paths: []domain.CodePathState{}}
				return err
			}
		}
		out, err = evidence.Query(bound, in, revision)
		return err
	})
	if err != nil && queries.atomicRead {
		return domain.CodeApplicability{}, err
	}
	return out, err
}
