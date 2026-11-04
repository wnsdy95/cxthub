package memoryquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *LiveQueries) QueryBranchMemory(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string) (domain.MemoryProjection, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || domain.ValidateBranchName(branch) != nil || code != "" && domain.ValidateGitOID(code) != nil {
		return domain.MemoryProjection{}, domain.ErrValidation
	}
	return readLiveMemory(ctx, queries, repo, func(current context.Context) (domain.MemoryProjection, error) {
		return queries.queryBranchMemory(current, repo, branch, snapshot, code, nil)
	})
}

func (queries *LiveQueries) queryBranchMemory(ctx context.Context, repo domain.ContentHash, branch string, snapshot domain.ContentHash, code string, selected *liveEvidence) (domain.MemoryProjection, error) {
	ref, err := queries.metadata.GetRef(ctx, repo, domain.RefBranch, branch)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	var cache *liveContributionCache
	if selected != nil {
		cache = selected.contributions
	}
	if cache == nil {
		history, err := queries.history.ListHistoryEvents(ctx, repo)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		cache = &liveContributionCache{repo: repo, history: history}
		if selected != nil {
			selected.contributions = cache
		}
	}
	history := cache.history
	if ref.BranchID == "" {
		bindings, err := domain.ProjectContextBranches(history)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
		ref.BranchID = bindings.Identity(string(repo), branch)
	}
	ref.Target = snapshot
	snapshots, err := queries.metadata.ListSnapshots(ctx, repo, "")
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	inputs, err := contextprojection.NewInputs(ctx, repo, snapshots, history)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	evidence, err := queries.newLiveEvidence(ctx, repo, cache)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	if code == "" {
		code, err = contextprojection.ResolveBranchCodeFromIndex(ctx, ref, inputs.History(), evidence)
		if err != nil {
			return domain.MemoryProjection{}, err
		}
	}
	branchPlan, err := contextprojection.PrepareBranchContext(ctx, ref, code, inputs, evidence)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	contributions, err := queries.contextContributions(ctx, repo, history, evidence)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	inclusion, err := branchPlan.Finish(ctx, contributions.facts, evidence)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	pins, err := queries.contributionPins(ctx, repo, contributions.records)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	plan, err := PrepareVerifiedContributionMemory(ctx, queries.projection, repo, inclusion, snapshots, contributions.records, pins)
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	return ProjectBranchWithPlan(ctx, queries.projection, repo, inclusion, plan)
}

func (queries *LiveQueries) contributionPins(ctx context.Context, repo domain.ContentHash, records []VerifiedContribution) ([]ContributionPin, error) {
	pins := []ContributionPin{}
	if len(records) == 0 {
		return pins, nil
	}
	if queries.prPins != nil {
		stored, err := queries.prPins.ListPRContributionMemories(ctx, repo)
		if err != nil {
			return nil, err
		}
		for _, pin := range stored {
			if pin.Validate() != nil || pin.RepoID != repo {
				return nil, domain.ErrIntegrity
			}
			pins = append(pins, ContributionPin(pin))
		}
	}
	if queries.gitPins != nil {
		stored, err := queries.gitPins.ListGitContributionMemories(ctx, repo)
		if err != nil {
			return nil, err
		}
		for _, pin := range stored {
			if pin.Validate() != nil || pin.RepoID != repo {
				return nil, domain.ErrIntegrity
			}
			pins = append(pins, ContributionPin(pin))
		}
	}
	return pins, nil
}

func (queries *LiveQueries) QueryMemoryPositions(ctx context.Context, repo, snapshot domain.ContentHash, event string) (domain.MemoryPositions, error) {
	if domain.ValidateContentHash(repo) != nil || domain.ValidateContentHash(snapshot) != nil || len(event) > 128 {
		return domain.MemoryPositions{}, domain.ErrValidation
	}
	return readLiveMemory(ctx, queries, repo, func(current context.Context) (domain.MemoryPositions, error) {
		if _, err := queries.metadata.GetSnapshot(current, repo, snapshot); err != nil {
			return domain.MemoryPositions{}, err
		}
		history, err := queries.history.ListHistoryEvents(current, repo)
		if err != nil {
			return domain.MemoryPositions{}, err
		}
		evidence := &liveEvidence{contributions: &liveContributionCache{repo: repo, history: history}}
		contributions, err := queries.contextContributions(current, repo, history, evidence)
		if err != nil {
			return domain.MemoryPositions{}, err
		}
		options := domain.ResolveMemoryPositions(snapshot, "", history).Options
		for index, contribution := range contributions.records {
			record := contribution.Record
			if !contribution.Valid || record.Capture != snapshot {
				continue
			}
			kind := "merge"
			fact := contributions.facts[index]
			if fact.PRNumber > 0 {
				kind = "pr-merge"
			}
			options = append(options, domain.MemoryCodePosition{EventID: record.ID, CodeCommit: record.IntegrationSHA, Branch: fact.DestinationBranch, Kind: kind, PRNumber: fact.PRNumber, CreatedAt: fact.MergedAt})
		}
		return domain.ResolveMemoryPositionOptions(snapshot, event, options), nil
	})
}
