package repositoryquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type BootstrapReader interface {
	GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	GetSessionArchive(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionArchive, error)
}

var _ inbound.RepositoryBootstrapQuery = (*Queries)(nil)

func (queries *Queries) GetRepositoryBootstrap(ctx context.Context, repo domain.ContentHash, branch string) (domain.RepositoryBootstrap, error) {
	if domain.ValidateContentHash(repo) != nil || branch != "" && domain.ValidateBranchName(branch) != nil {
		return domain.RepositoryBootstrap{}, domain.ErrValidation
	}
	if queries.bootstrap == nil || queries.revisions == nil || !queries.atomicRead {
		return domain.RepositoryBootstrap{}, domain.ErrCatalogUnsupported
	}
	ctx, release, err := queries.resources.AcquireInteractive(ctx)
	if err != nil {
		return domain.RepositoryBootstrap{}, err
	}
	defer release()
	ctx, err = queries.scope.AuthorizeRead(ctx, repo, true)
	if err != nil {
		return domain.RepositoryBootstrap{}, err
	}
	return readRepository(ctx, queries, repo, func(bound context.Context) (domain.RepositoryBootstrap, error) {
		view, err := queries.readBootstrap(bound, repo, branch)
		if err != nil {
			return domain.RepositoryBootstrap{}, err
		}
		return view, bound.Err()
	})
}

func (queries *Queries) readBootstrap(ctx context.Context, repo domain.ContentHash, branch string) (domain.RepositoryBootstrap, error) {
	metadata, err := queries.metadata.GetRepo(ctx, repo)
	if err != nil {
		return domain.RepositoryBootstrap{}, err
	}
	if metadata.ID != repo {
		return domain.RepositoryBootstrap{}, domain.ErrIntegrity
	}
	if metadata.ContextProtocol != 1 {
		return domain.RepositoryBootstrap{}, domain.ErrCatalogUnsupported
	}
	view := domain.RepositoryBootstrap{DefaultBranch: metadata.DefaultBranch}
	if view.DefaultBranch == "" {
		view.DefaultBranch = "main"
	}
	if branch == "" {
		branch = view.DefaultBranch
	}
	if domain.ValidateBranchName(branch) != nil {
		return domain.RepositoryBootstrap{}, domain.ErrIntegrity
	}
	view.Branch = branch
	view.Revision, err = queries.revisions.RepositoryRevision(ctx, repo)
	if err != nil {
		return domain.RepositoryBootstrap{}, err
	}
	ref, err := queries.bootstrap.GetRef(ctx, repo, domain.RefBranch, branch)
	if errors.Is(err, domain.ErrNotFound) {
		return view, nil
	}
	if err != nil {
		return domain.RepositoryBootstrap{}, err
	}
	if ref.RepoID != repo || ref.Kind != domain.RefBranch || ref.Name != branch || domain.ValidateContentHash(ref.Target) != nil {
		return domain.RepositoryBootstrap{}, domain.ErrIntegrity
	}
	view.Ref = &ref
	snapshot, err := queries.bootstrapSnapshot(ctx, repo, ref.Target)
	if err != nil {
		return domain.RepositoryBootstrap{}, err
	}
	archiveKey := domain.SessionArchiveKey(snapshot)
	keys := []domain.ContentHash{archiveKey}
	if legacyKey := domain.SessionArchiveKey(domain.Snapshot{ID: snapshot.ID}); legacyKey != archiveKey {
		keys = append(keys, legacyKey)
	}
	for _, key := range keys {
		archive, err := queries.bootstrap.GetSessionArchive(ctx, repo, key)
		if err == nil {
			if archive.RepoID != repo || archive.Key != key {
				return domain.RepositoryBootstrap{}, domain.ErrIntegrity
			}
			view.Archived = true
			return view, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.RepositoryBootstrap{}, err
		}
	}
	view.Snapshot = &snapshot
	if len(snapshot.Parents) != 0 {
		parent, err := queries.bootstrapSnapshot(ctx, repo, snapshot.Parents[0])
		if err != nil {
			return domain.RepositoryBootstrap{}, err
		}
		view.Parent = &parent
	}
	return view, nil
}

func (queries *Queries) bootstrapSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	if domain.ValidateContentHash(id) != nil {
		return domain.Snapshot{}, domain.ErrIntegrity
	}
	snapshot, err := queries.bootstrap.GetSnapshot(ctx, repo, id)
	if err != nil {
		return domain.Snapshot{}, err
	}
	if snapshot.RepoID != repo || snapshot.ID != id || domain.ValidateContentHash(snapshot.DocHash) != nil {
		return domain.Snapshot{}, domain.ErrIntegrity
	}
	return snapshot, nil
}
