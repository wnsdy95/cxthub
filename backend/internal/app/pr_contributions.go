package app

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type verifiedPRContribution struct {
	record       domain.PRContribution
	valid        bool
	publications map[string]domain.HistoryEvent
	mergedAt     time.Time
}

func contributionFromParticipation(proof domain.PRParticipation, checkpoint domain.PRHistoryCheckpoint, destination string) domain.PRContribution {
	return (domain.PRContribution{RepoID: proof.RepoID, GitOrigin: proof.GitOrigin, ProviderRepositoryID: proof.ProviderRepositoryID,
		PullRequestID: proof.PullRequestID, PR: checkpoint.PR, DestinationBranchID: destination, SourceBranchID: proof.BranchID,
		Capture: proof.Capture, OriginalHead: proof.OriginalHead}).WithID()
}

func (s *Service) preparePRContributions(ctx context.Context, checkpoint domain.PRHistoryCheckpoint, proofs []domain.PRParticipation, history []domain.HistoryEvent) ([]domain.PRContribution, []domain.PRContributionMemory, error) {
	if len(proofs) == 0 {
		return nil, nil, nil
	}
	if _, ok := s.meta.(outbound.PRContributionStore); !ok {
		return nil, nil, nil
	}
	jobs, err := s.prJobs()
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
	ref, err := s.contributionDestination(ctx, checkpoint.RepoID, checkpoint.PR.BaseBranch, job.BaseBranchID, history)
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	evidence, err := s.newCodeEvidence(ctx, checkpoint.RepoID)
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
			if err := s.verifyContributionClosure(ctx, record); err != nil {
				return nil, nil, err
			}
			verifiedCaptures[record.Capture] = true
		}
		records[record.ID] = record
		publication := publications[proof.PublicationID]
		if !publication.MemoryPinned {
			continue
		}
		memorySource, err := s.contributionPublicationMemorySource(ctx, record.RepoID, record.Capture, publication)
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

func contributionInCode(ctx context.Context, evidence *codeEvidence, merge, code string) (bool, error) {
	if evidence == nil || code == "" {
		return false, nil
	}
	seen := map[string]bool{}
	for current := code; current != "" && len(seen) < maxCodeEvidenceReads; {
		if seen[current] {
			return false, domain.ErrIntegrity
		}
		seen[current] = true
		commit, err := evidence.GetGitCommitTree(ctx, evidence.repo, evidence.origin, current)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if current == merge {
			return true, nil
		}
		if len(commit.Parents) == 0 {
			return false, nil
		}
		current = commit.Parents[0]
	}
	return false, nil
}

func (s *Service) verifyContributionClosure(ctx context.Context, record domain.PRContribution) error {
	return s.verifyContributionCapture(ctx, record.RepoID, record.Capture)
}

func (s *Service) verifyContributionCapture(ctx context.Context, repo domain.ContentHash, capture domain.ContentHash) error {
	active, done := map[domain.ContentHash]bool{}, map[domain.ContentHash]bool{}
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if active[id] {
			return domain.ErrIntegrity
		}
		if done[id] {
			return nil
		}
		if len(done)+len(active) >= maxProjectionSnapshots {
			return domain.ErrIntegrity
		}
		snapshot, err := s.meta.GetSnapshot(ctx, repo, id)
		if err != nil {
			return err
		}
		if snapshot.RepoID != repo || snapshot.ID != id {
			return domain.ErrIntegrity
		}
		active[id] = true
		for _, parent := range snapshot.Parents {
			if err := visit(parent); err != nil {
				return err
			}
		}
		delete(active, id)
		done[id] = true
		return nil
	}
	return visit(capture)
}

func (s *Service) contributionDestination(ctx context.Context, repo domain.ContentHash, name, identity string, history []domain.HistoryEvent) (domain.Ref, error) {
	bindings, err := domain.ProjectContextBranches(history)
	if err != nil {
		return domain.Ref{}, err
	}
	name, err = contributionBranchName(repo, name, identity, bindings)
	if err != nil {
		return domain.Ref{}, err
	}
	ref, err := s.meta.GetRef(ctx, repo, domain.RefBranch, name)
	if err != nil {
		return ref, err
	}
	return bindContributionRef(ref, identity)
}

func contributionBranchName(repo domain.ContentHash, name, identity string, bindings domain.ContextBranchProjection) (string, error) {
	if binding, ok := bindings.ByID[identity]; ok {
		if binding.Archived {
			return "", domain.ErrConflict
		}
		name = binding.Name
	} else if bindings.Identity(string(repo), name) != identity || bindings.Released[name] != "" {
		return "", domain.ErrConflict
	}
	return name, nil
}

func bindContributionRef(ref domain.Ref, identity string) (domain.Ref, error) {
	if ref.BranchID != "" && ref.BranchID != identity {
		return ref, domain.ErrConflict
	}
	ref.BranchID = identity
	return ref, nil
}

func (s *Service) verifiedPRContributions(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]verifiedPRContribution, error) {
	ledger, ok := s.meta.(outbound.PRContributionStore)
	if !ok {
		return nil, nil
	}
	records, err := ledger.ListPRContributions(ctx, repo)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	storage, ok := s.meta.(outbound.PRHistoryStore)
	if !ok {
		return nil, domain.ErrIntegrity
	}
	metadata, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return nil, err
	}
	publications := map[string]domain.HistoryEvent{}
	for _, event := range history {
		publications[event.ID] = event
	}
	type proofScope struct {
		origin     string
		repository int64
		number     int
	}
	type proofSet struct {
		checkpoint   domain.PRHistoryCheckpoint
		publications map[[3]string]map[string]domain.HistoryEvent
	}
	scopes := map[proofScope]proofSet{}
	var historyIndex *domain.PRParticipationHistoryIndex
	observations := map[string]domain.GitRefObservation{}
	out := make([]verifiedPRContribution, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		verified := verifiedPRContribution{record: record, publications: map[string]domain.HistoryEvent{}}
		if normalizeGitURL(record.GitOrigin) != normalizeGitURL(metadata.GitRemoteURL) {
			out = append(out, verified)
			continue
		}
		key := proofScope{record.GitOrigin, record.ProviderRepositoryID, record.PR.Number}
		set, loaded := scopes[key]
		if !loaded {
			set.checkpoint, err = storage.GetPRHistoryCheckpoint(ctx, repo, key.origin, key.repository, key.number)
			if errors.Is(err, domain.ErrNotFound) {
				out = append(out, verified)
				continue
			}
			if err != nil {
				return nil, err
			}
			if historyIndex == nil {
				historyIndex, err = domain.NewPRParticipationHistoryIndex(ctx, repo, history)
				if err != nil {
					return nil, err
				}
			}
			verifier, err := historyIndex.Verifier(ctx, set.checkpoint)
			if err != nil {
				return nil, err
			}
			proofs, err := storage.ListPRParticipations(ctx, repo, key.origin, key.number)
			if err != nil {
				return nil, err
			}
			set.publications = map[[3]string]map[string]domain.HistoryEvent{}
			for _, proof := range proofs {
				publication, exists := publications[proof.PublicationID]
				if !exists {
					continue
				}
				observation, exists := observations[proof.ObservationID]
				if !exists {
					observation, err = storage.GetPRRefObservation(ctx, repo, record.GitOrigin, proof.ObservationID)
					if err != nil {
						return nil, err
					}
					observations[observation.ID] = observation
				}
				current, err := verifier.Verify(observation, publication)
				if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPRSourcePending) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if current != proof {
					return nil, domain.ErrIntegrity
				}
				identity := [3]string{string(proof.Capture), proof.BranchID, proof.OriginalHead}
				if set.publications[identity] == nil {
					set.publications[identity] = map[string]domain.HistoryEvent{}
				}
				set.publications[identity][publication.ID] = publication
			}
			scopes[key] = set
		}
		if set.checkpoint.PR != record.PR || set.checkpoint.PullRequestID != record.PullRequestID {
			return nil, domain.ErrIntegrity
		}
		verified.mergedAt = set.checkpoint.MergedAt
		verified.publications = set.publications[[3]string{string(record.Capture), record.SourceBranchID, record.OriginalHead}]
		verified.valid = len(verified.publications) != 0
		out = append(out, verified)
	}
	return out, nil
}

func (s *Service) promoteHistoricalPRContribution(ctx context.Context, repo domain.ContentHash, pr domain.PullRequestMerge, baseID string) (inbound.UpdateRefOutput, bool, error) {
	var empty inbound.UpdateRefOutput
	history, err := s.ListHistory(ctx, repo)
	if err != nil {
		return empty, false, err
	}
	records, err := s.verifiedPRContributions(ctx, repo, history)
	if err != nil {
		return empty, false, err
	}
	var candidates []domain.PRContribution
	applied := false
	for _, verified := range records {
		if verified.record.PR != pr || (baseID != "" && verified.record.DestinationBranchID != baseID) {
			continue
		}
		applied = true
		if verified.valid {
			candidates = append(candidates, verified.record)
		}
	}
	if len(candidates) == 0 {
		if applied {
			return empty, true, domain.ErrConflict
		}
		return empty, false, nil
	}
	selected := candidates[0]
	ref, err := s.contributionDestination(ctx, repo, pr.BaseBranch, selected.DestinationBranchID, history)
	if err != nil {
		return empty, true, err
	}
	out := inbound.UpdateRefOutput{Ref: ref, ServerTarget: ref.Target, RequestedTarget: selected.Capture, Result: inbound.RefUpToDate}
	return out, true, nil
}
