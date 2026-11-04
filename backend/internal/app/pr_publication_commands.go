package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (commands *prPublicationCommands) submitPRPromotion(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	if err := pr.Validate(); err != nil {
		return domain.PRPromotionJob{}, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.PRPromotionJob{}, err
	}
	metadata, err := commands.work.meta.GetRepo(ctx, repo)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	if metadata.GitRemoteURL == "" {
		return domain.PRPromotionJob{}, fmt.Errorf("%w: repository has no Git origin", domain.ErrValidation)
	}
	st, err := commands.prJobs()
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	existing, err := st.GetPRJob(ctx, repo, domain.PRPromotionID(repo, pr.Number))
	if err == nil {
		if existing.PR != pr || existing.GitOrigin != normalizeGitURL(metadata.GitRemoteURL) {
			return existing, domain.ErrConflict
		}
		if e := commands.queuePRGitScans(ctx, repo, metadata.GitRemoteURL, pr); e != nil {
			return existing, e
		}
		return existing, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.PRPromotionJob{}, err
	}
	events, err := commands.ListHistory(ctx, repo)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	bindings, err := domain.ProjectContextBranches(events)
	if err != nil {
		return domain.PRPromotionJob{}, err
	}
	baseID := domain.LegacyContextBranchID(string(repo), pr.BaseBranch)
	if b, ok := bindings.Active[pr.BaseBranch]; ok {
		baseID = b.ID
	} else if bindings.Released[pr.BaseBranch] != "" {
		return domain.PRPromotionJob{}, fmt.Errorf("%w: PR base was released", domain.ErrConflict)
	}
	if err := commands.queuePRGitScans(ctx, repo, metadata.GitRemoteURL, pr); err != nil {
		return domain.PRPromotionJob{}, err
	}
	now := time.Now().UTC()
	return st.EnqueuePRJob(ctx, domain.PRPromotionJob{ID: domain.PRPromotionID(repo, pr.Number), RepoID: repo, PR: pr, BaseBranchID: baseID, GitOrigin: normalizeGitURL(metadata.GitRemoteURL), State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now})
}

func (commands *prPublicationCommands) preparePRContributions(ctx context.Context, checkpoint domain.PRHistoryCheckpoint, proofs []domain.PRParticipation, history []domain.HistoryEvent) ([]domain.PRContribution, []domain.PRContributionMemory, error) {
	if len(proofs) == 0 {
		return nil, nil, nil
	}
	if _, ok := commands.work.meta.(outbound.PRContributionStore); !ok {
		return nil, nil, nil
	}
	jobs, err := commands.prJobs()
	if err != nil {
		return nil, nil, err
	}
	job, err := jobs.GetPRJob(ctx, checkpoint.RepoID, domain.PRPromotionID(checkpoint.RepoID, checkpoint.PR.Number))
	if err != nil {
		return nil, nil, err
	}
	if job.PR != checkpoint.PR || job.GitOrigin != normalizeGitURL(checkpoint.GitOrigin) {
		return nil, nil, domain.ErrIntegrity
	}
	ref, err := commands.contributionDestination(ctx, checkpoint.RepoID, checkpoint.PR.BaseBranch, job.BaseBranchID, history)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	evidence, err := commands.newCodeEvidence(ctx, checkpoint.RepoID)
	if err != nil {
		return nil, nil, err
	}
	code, err := resolvedBranchCode(ctx, ref, history, evidence)
	if err != nil {
		return nil, nil, err
	}
	ordered, err := contributionInCode(ctx, evidence, checkpoint.PR.MergeSHA, code)
	if err != nil || !ordered {
		return nil, nil, err
	}
	publications := map[string]domain.HistoryEvent{}
	for _, event := range history {
		publications[event.ID] = event
	}
	records := map[string]domain.PRContribution{}
	memories := map[string]domain.PRContributionMemory{}
	verifiedCaptures := map[domain.ContentHash]bool{}
	for _, proof := range proofs {
		record := contributionFromParticipation(proof, checkpoint, ref.BranchID)
		if err := record.Validate(); err != nil {
			return nil, nil, err
		}
		if !verifiedCaptures[record.Capture] {
			if err := commands.verifyContributionClosure(ctx, record); err != nil {
				return nil, nil, err
			}
			verifiedCaptures[record.Capture] = true
		}
		records[record.ID] = record
		publication := publications[proof.PublicationID]
		if !publication.MemoryPinned {
			continue
		}
		memorySource, err := commands.contributionPublicationMemorySource(ctx, record.RepoID, record.Capture, publication)
		if err != nil {
			return nil, nil, err
		}
		memory := (domain.PRContributionMemory{RepoID: record.RepoID, ContributionID: record.ID, PublicationID: publication.ID, MemorySource: memorySource, MemoryHash: publication.MemoryHash, MemoryPinned: true}).WithID()
		memories[memory.ID] = memory
	}
	out := make([]domain.PRContribution, 0, len(records))
	for _, record := range records {
		out = append(out, record)
	}
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	versions := make([]domain.PRContributionMemory, 0, len(memories))
	for _, memory := range memories {
		versions = append(versions, memory)
	}
	sort.Slice(versions, func(left, right int) bool { return versions[left].ID < versions[right].ID })
	return out, versions, nil
}
