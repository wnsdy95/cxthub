package memoryquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type liveEvidence struct {
	repo            domain.ContentHash
	origin          string
	trees           evidencequery.GitTreeReader
	deltas          evidencequery.GitDeltaReader
	requireComplete bool
	git             *evidencequery.GitEvidence
	contributions   *liveContributionCache
}

type liveContributionCache struct {
	repo    domain.ContentHash
	history []domain.HistoryEvent
	loaded  bool
	records []VerifiedContribution
	facts   liveContributionFacts
}

type liveContributionFacts []contextprojection.Contribution

func (facts liveContributionFacts) Len() int {
	return len(facts)
}

func (facts liveContributionFacts) At(index int) contextprojection.Contribution {
	return facts[index]
}

var _ contextprojection.CodeEvidence = (*liveEvidence)(nil)
var _ contextprojection.Contributions = liveContributionFacts(nil)

func (queries *LiveQueries) newLiveEvidence(ctx context.Context, repo domain.ContentHash, cache *liveContributionCache) (*liveEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata, err := queries.repository.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	if metadata.ID != repo {
		return nil, domain.ErrIntegrity
	}
	return &liveEvidence{repo: repo, origin: metadata.GitRemoteURL, trees: queries.trees, deltas: queries.deltas,
		requireComplete: queries.requireComplete, contributions: cache}, nil
}

func (evidence *liveEvidence) gitEvidence() (*evidencequery.GitEvidence, error) {
	if evidence.git == nil {
		owner, err := evidencequery.NewGitEvidence(evidencequery.GitEvidenceConfig{
			RepoID: evidence.repo, Origin: evidence.origin, Trees: evidence.trees, Deltas: evidence.deltas, RequireComplete: evidence.requireComplete,
		})
		if err != nil {
			return nil, err
		}
		evidence.git = owner
	}
	return evidence.git, nil
}

func (evidence *liveEvidence) Origin() string {
	return evidence.origin
}

func (evidence *liveEvidence) Limited() bool {
	return evidence.git != nil && evidence.git.Limited()
}

func (evidence *liveEvidence) ReadCount() int {
	if evidence.git == nil {
		return 0
	}
	return evidence.git.ReadCount()
}

func (evidence *liveEvidence) TraversalLimited() bool {
	return evidence.git != nil && evidence.git.TraversalLimited()
}

func (evidence *liveEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitTree{}, err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return domain.GitCommitTree{}, err
	}
	return owner.GetGitCommitTree(ctx, repo, origin, identifier)
}

func (evidence *liveEvidence) Relation(ctx context.Context, source, selected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return "", err
	}
	return owner.Relation(ctx, source, selected)
}

func (evidence *liveEvidence) ExhaustTraversal() error {
	owner, err := evidence.gitEvidence()
	if err != nil {
		return err
	}
	return owner.ExhaustTraversal()
}

func (evidence *liveEvidence) query(ctx context.Context, selection domain.CodeSelection, revision domain.RepositoryRevision) (domain.CodeApplicability, error) {
	if err := ctx.Err(); err != nil {
		return domain.CodeApplicability{}, err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return domain.CodeApplicability{}, err
	}
	return owner.Query(ctx, selection, revision)
}

func (cache *liveContributionCache) bind(repo domain.ContentHash, history []domain.HistoryEvent) error {
	if cache.repo != repo {
		return domain.ErrIntegrity
	}
	same := len(cache.history) == len(history)
	if same && len(history) != 0 {
		same = &cache.history[0] == &history[0]
	}
	if !same {
		cache.records, cache.facts, cache.loaded = nil, nil, false
	}
	cache.history = history
	return nil
}

func (queries *LiveQueries) contextContributions(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent, evidence *liveEvidence) (*liveContributionCache, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cache := evidence.contributions
	if cache == nil {
		cache = &liveContributionCache{repo: repo}
		evidence.contributions = cache
	}
	if err := cache.bind(repo, history); err != nil {
		return nil, err
	}
	if cache.loaded {
		return cache, nil
	}
	prs, err := queries.evidence.VerifiedPR(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	git, err := queries.evidence.VerifiedGit(ctx, repo, history)
	if err != nil {
		return nil, err
	}
	records := make([]VerifiedContribution, 0, len(prs)+len(git))
	facts := make(liveContributionFacts, 0, len(prs)+len(git))
	for _, verified := range prs {
		record := verified.Record
		records = append(records, VerifiedContribution{
			Record: ContributionRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID,
				SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.PR.MergeSHA},
			Valid: verified.Valid, Publications: verified.Publications,
		})
		facts = append(facts, contextprojection.Contribution{ID: record.ID, DestinationBranchID: record.DestinationBranchID,
			Capture: record.Capture, IntegrationSHA: record.PR.MergeSHA, Valid: verified.Valid,
			SourceBranch: record.PR.HeadBranch, DestinationBranch: record.PR.BaseBranch, PRNumber: record.PR.Number, MergedAt: verified.MergedAt})
	}
	for _, verified := range git {
		record := verified.Record
		records = append(records, VerifiedContribution{
			Record: ContributionRecord{ID: record.ID, RepoID: record.RepoID, DestinationBranchID: record.DestinationBranchID,
				SourceBranchID: record.SourceBranchID, Capture: record.Capture, OriginalHead: record.OriginalHead, IntegrationSHA: record.IntegrationSHA},
			Valid: verified.Valid, Publications: verified.Publications,
		})
		facts = append(facts, contextprojection.Contribution{ID: record.ID, DestinationBranchID: record.DestinationBranchID,
			Capture: record.Capture, IntegrationSHA: record.IntegrationSHA, Valid: verified.Valid,
			SourceBranch: verified.SourceBranch, DestinationBranch: verified.DestinationBranch})
	}
	cache.records, cache.facts, cache.loaded = records, facts, true
	return cache, nil
}
