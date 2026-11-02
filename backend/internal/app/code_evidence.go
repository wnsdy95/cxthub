package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const maxCodeEvidenceReads = contextprojection.MaxCodeEvidenceReads

type codeEvidence struct {
	repo                   domain.ContentHash
	origin                 string
	trees                  outbound.GitTreeStore
	deltas                 outbound.GitDeltaReader
	git                    *evidencequery.GitEvidence
	requireComplete        bool
	contributions          []verifiedPRContribution
	contributionsLoaded    bool
	gitContributions       []verifiedGitContribution
	gitContributionsLoaded bool
	contributionHistory    []domain.HistoryEvent
}

func (evidence *codeEvidence) bindContributionHistory(repo domain.ContentHash, history []domain.HistoryEvent) error {
	if evidence == nil {
		return nil
	}
	if evidence.repo != repo {
		return domain.ErrIntegrity
	}
	same := len(evidence.contributionHistory) == len(history)
	if same && len(history) != 0 {
		same = &evidence.contributionHistory[0] == &history[0]
	}
	if !same {
		evidence.contributions, evidence.gitContributions = nil, nil
		evidence.contributionsLoaded, evidence.gitContributionsLoaded = false, false
	}
	evidence.contributionHistory = history
	return nil
}

func (service *Service) newCodeEvidence(ctx context.Context, repo domain.ContentHash) (*codeEvidence, error) {
	return newCodeEvidenceFromReader(ctx, service.graphEvidenceReader(), repo, service.requireCompleteMemory)
}

func newCodeEvidenceFromReader(ctx context.Context, reader outbound.RepositoryReader, repo domain.ContentHash, requireComplete bool) (*codeEvidence, error) {
	metadata, err := reader.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	if metadata.ID != repo {
		return nil, domain.ErrIntegrity
	}
	trees, _ := reader.(outbound.GitTreeStore)
	deltas, _ := reader.(outbound.GitDeltaReader)
	return &codeEvidence{repo: repo, origin: metadata.GitRemoteURL, trees: trees, deltas: deltas,
		requireComplete: requireComplete}, nil
}

func (evidence *codeEvidence) gitEvidence() (*evidencequery.GitEvidence, error) {
	if evidence.git == nil {
		owner, err := evidencequery.NewGitEvidence(evidencequery.GitEvidenceConfig{
			RepoID: evidence.repo, Origin: evidence.origin, Trees: evidence.trees, Deltas: evidence.deltas,
			RequireComplete: evidence.requireComplete,
		})
		if err != nil {
			return nil, err
		}
		evidence.git = owner
	}
	return evidence.git, nil
}

func (evidence *codeEvidence) allow(ctx context.Context, repo domain.ContentHash, origin string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return err
	}
	return owner.ConsumeRead(ctx, repo, origin)
}

func (evidence *codeEvidence) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitCommitTree, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitTree{}, err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return domain.GitCommitTree{}, err
	}
	return owner.GetGitCommitTree(ctx, repo, origin, identifier)
}

func (evidence *codeEvidence) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, identifier string) (domain.GitTreeNode, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitTreeNode{}, err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return domain.GitTreeNode{}, err
	}
	return owner.GetGitTreeNode(ctx, repo, origin, identifier)
}

func (evidence *codeEvidence) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, identifier, parent string) (domain.GitCommitDelta, error) {
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return domain.GitCommitDelta{}, err
	}
	return owner.GetGitDelta(ctx, repo, origin, identifier, parent)
}

func (evidence *codeEvidence) relation(ctx context.Context, source, selected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	owner, err := evidence.gitEvidence()
	if err != nil {
		return "", err
	}
	return owner.Relation(ctx, source, selected)
}

func (evidence *codeEvidence) exhaustTraversal() error {
	owner, err := evidence.gitEvidence()
	if err != nil {
		return err
	}
	return owner.ExhaustTraversal()
}

func (evidence *codeEvidence) ReadCount() int {
	if evidence.git == nil {
		return 0
	}
	return evidence.git.ReadCount()
}

func (evidence *codeEvidence) Limited() bool {
	return evidence.git != nil && evidence.git.Limited()
}

func (evidence *codeEvidence) TraversalLimited() bool {
	return evidence.git != nil && evidence.git.TraversalLimited()
}

func (evidence *codeEvidence) CachedCommit(identifier string) (domain.GitCommitTree, error, bool) {
	if evidence.git == nil {
		return domain.GitCommitTree{}, nil, false
	}
	return evidence.git.CachedCommit(identifier)
}
