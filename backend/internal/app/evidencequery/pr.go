package evidencequery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) VerifiedPR(ctx context.Context, repo domain.ContentHash, history []domain.HistoryEvent) ([]VerifiedPRContribution, error) {
	if queries.prLedger == nil {
		return nil, nil
	}
	records, err := queries.prLedger.ListPRContributions(ctx, repo)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	if queries.prHistory == nil {
		return nil, domain.ErrIntegrity
	}
	metadata, err := queries.metadata.GetRepo(ctx, repo)
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
	out := make([]VerifiedPRContribution, 0, len(records))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.Validate() != nil || record.RepoID != repo || metadata.ID != repo {
			return nil, domain.ErrIntegrity
		}
		verified := VerifiedPRContribution{Record: record, Publications: map[string]domain.HistoryEvent{}}
		if domain.NormalizeGitRemoteURL(record.GitOrigin) != domain.NormalizeGitRemoteURL(metadata.GitRemoteURL) {
			out = append(out, verified)
			continue
		}
		key := proofScope{record.GitOrigin, record.ProviderRepositoryID, record.PR.Number}
		set, loaded := scopes[key]
		if !loaded {
			set.checkpoint, err = queries.prHistory.GetPRHistoryCheckpoint(ctx, repo, key.origin, key.repository, key.number)
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
			proofs, err := queries.prHistory.ListPRParticipations(ctx, repo, key.origin, key.number)
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
					observation, err = queries.prHistory.GetPRRefObservation(ctx, repo, record.GitOrigin, proof.ObservationID)
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
		verified.MergedAt = set.checkpoint.MergedAt
		verified.Publications = set.publications[[3]string{string(record.Capture), record.SourceBranchID, record.OriginalHead}]
		verified.Valid = len(verified.Publications) != 0
		out = append(out, verified)
	}
	return out, nil
}
