package app

import (
	"context"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// GitScans turns durable observations into immutable evidence. It owns no
// context ref writes, so late or local observations cannot move shared state.
type GitScans struct {
	*gitScanQuery
	sourceAccess GitSourceAuthorizer
	reader       outbound.GitCommitReader
}

func NewGitScans(core *Service, reader outbound.GitCommitReader) (*GitScans, error) {
	st, ok := core.meta.(outbound.GitScanStore)
	_, headStore := core.meta.(outbound.GitHeadScanStore)
	_, treeStore := core.meta.(outbound.GitTreeStore)
	_, treeReader := reader.(outbound.GitTreeReader)
	if !ok || !headStore || !treeStore || !treeReader || reader == nil {
		return nil, domain.ErrValidation
	}
	return &GitScans{gitScanQuery: &gitScanQuery{core: core, store: st}, reader: reader}, nil
}
func (s *Service) queueHistoryGitScan(ctx context.Context, e domain.HistoryEvent) error {
	return queueHistoryGitScanFromStore(ctx, s.meta, e)
}

func (g *GitScans) ObservePush(ctx context.Context, origin, ref, before, after string, forced bool, delivery string) (int, error) {
	repos, err := g.core.meta.ListRepos(ctx, "default")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range repos {
		if origin == "" || normalizeGitURL(origin) != normalizeGitURL(r.GitRemoteURL) {
			continue
		}
		o := domain.GitRefObservation{RepoID: r.ID, GitOrigin: r.GitRemoteURL, Source: "github-push", Ref: ref, Before: before, After: after, Forced: forced, Delivery: delivery}.WithID()
		err = repositoryWriteError(evidenceWriteContext(ctx), g.core, r.ID, func(tx context.Context) error {
			current, err := g.core.meta.GetRepo(tx, r.ID)
			if err != nil {
				return err
			}
			if current.GitRemoteURL != r.GitRemoteURL {
				return domain.ErrConflict
			}
			return g.store.RecordGitRefObservation(tx, o, time.Now().UTC())
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
func (g *GitScans) run(ctx context.Context, j domain.GitScanJob) error {
	return g.integration().run(ctx, j)
}
func planGitIndex(p *domain.GitScanFinish, deltas []domain.GitCommitDelta) error {
	if len(deltas) == 0 || len(deltas) > 16 {
		return domain.ErrIntegrity
	}
	parents := deltas[0].Parents
	want := len(parents)
	if want == 0 {
		want = 1
	}
	if len(deltas) != want {
		return domain.ErrIntegrity
	}
	seen := map[string]bool{}
	for _, d := range deltas {
		if d.Commit != p.Job.Commit || !d.Complete || !reflect.DeepEqual(d.Parents, parents) || seen[d.Parent] {
			return domain.ErrIntegrity
		}
		if err := d.Validate(); err != nil {
			return err
		}
		seen[d.Parent] = true
		p.Deltas = append(p.Deltas, domain.NewGitDelta(p.Job.RepoID, p.Job.GitOrigin, d))
	}
	for _, sha := range parents {
		p.Parents = append(p.Parents, domain.NewGitScan(p.Job.RepoID, p.Job.GitOrigin, sha, p.Job.UpdatedAt))
	}
	p.Job.Indexed = true
	return p.Validate()
}
func (g *GitScans) Process(ctx context.Context, limit int) error {
	return g.integration().Process(ctx, limit)
}
func (g *GitScans) Run(ctx context.Context) {
	g.integration().Run(ctx)
}

// Cached evidence is repository-scoped and immutable. It avoids re-reading
// large provider trees for every candidate. Unknown objects use the provider.
type cachedGitEvidence struct {
	store  outbound.GitScanStore
	repo   domain.ContentHash
	reader outbound.GitEvidenceReader
}

func (cached cachedGitEvidence) readerForRepository() (*evidencequery.CachedReversalReader, error) {
	return evidencequery.NewCachedReversalReader(evidencequery.CachedReversalConfig{
		RepoID: cached.repo, Cache: cached.store, Provider: gitchangeworker.RepositoryEvidence(cached.repo, cached.reader),
	})
}

func (cached cachedGitEvidence) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	reader, err := cached.readerForRepository()
	if err != nil {
		return domain.GitCommitDelta{}, err
	}
	return reader.ReadCommitDelta(ctx, origin, commit, parent)
}

func (cached cachedGitEvidence) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	reader, err := cached.readerForRepository()
	if err != nil {
		return false, err
	}
	return reader.IsGitAncestor(ctx, origin, ancestor, descendant)
}

func (g *GitScans) RetryScan(ctx context.Context, repo domain.ContentHash, id string) error {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateGitChangeID(id) != nil {
		return domain.ErrValidation
	}
	return repositoryWriteError(evidenceWriteContext(ctx), g.core, repo, func(tx context.Context) error { return g.store.RetryGitScan(tx, repo, id, time.Now().UTC()) })
}

func (g *GitScans) Reconcile(ctx context.Context) error {
	return g.integration().Reconcile(ctx)
}

func (g *GitScans) reconcile(ctx context.Context) (bool, error) {
	return g.integration().reconcile(ctx)
}
func (g *GitScans) RunReconciler(ctx context.Context) {
	g.integration().RunReconciler(ctx)
}

func (s *Service) queuePRGitScans(ctx context.Context, repo domain.ContentHash, origin string, pr domain.PullRequestMerge) error {
	return queuePRGitScansFromStore(ctx, s.meta, repo, origin, pr)
}

func (g *GitScans) validateIndexTree(ctx context.Context, j domain.GitScanJob, deltas []domain.GitCommitDelta) error {
	return g.integration().validateIndexTree(ctx, j, deltas)
}

// SetSourceAuthorizer configures source policy before workers start.
func (g *GitScans) SetSourceAuthorizer(a GitSourceAuthorizer) { g.sourceAccess = a }
