package app

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (scans *GitScans) readGitAdmissionSources(ctx context.Context, frozen *frozenGitAdmissionBatch) ([]domain.GitRefObservation, error) {
	reader, supported := scans.reader.(outbound.GitHeadReader)
	_, persisted := scans.core.meta.(outbound.GitHeadSourceScanStore)
	if !supported || !persisted || frozen == nil || frozen.snapshot == nil {
		return nil, nil
	}
	if len(frozen.publications) > domain.MaxGitHeadSourceReads {
		return nil, domain.ErrIntegrity
	}
	names := map[string]bool{}
	for _, event := range frozen.publications {
		publication, err := frozen.snapshot.index.Publication(event.ID)
		if gitAdmissionDeferred(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(publication, event) {
			return nil, domain.ErrIntegrity
		}
		name := publication.LocalBranch
		if name == "" {
			name = publication.Branch
		}
		ref, err := frozen.snapshot.destination(name, publication.BranchID)
		if gitAdmissionDeferred(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ref.Name == name && ref.Target == publication.Target {
			names[name] = true
		}
	}
	budget := 10 * time.Second
	if deadline, exists := ctx.Deadline(); exists {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		budget = min(budget, remaining/2)
	}
	work, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	type sourceResult struct {
		observation domain.GitRefObservation
		err         error
	}
	results := make(chan sourceResult, len(names))
	for name := range names {
		go func() {
			head, err := reader.ReadGitHead(work, frozen.claim.GitOrigin, name)
			if err != nil {
				results <- sourceResult{err: err}
				return
			}
			observation := domain.GitRefObservation{RepoID: frozen.claim.RepoID, GitOrigin: frozen.claim.GitOrigin,
				Source: "reconciliation", Ref: head.Ref, After: head.Commit, Provider: &frozen.authorization.provider}.WithID()
			if head.Ref != "refs/heads/"+name || observation.Validate() != nil {
				results <- sourceResult{err: domain.ErrIntegrity}
				return
			}
			results <- sourceResult{observation: observation}
		}()
	}
	observations := []domain.GitRefObservation{}
	for range len(names) {
		select {
		case <-work.Done():
			return observations, ctx.Err()
		case result := <-results:
			if errors.Is(result.err, domain.ErrForbidden) || errors.Is(result.err, domain.ErrConflict) || errors.Is(result.err, domain.ErrDocumentIdentityUpgradeRequired) {
				return nil, result.err
			}
			if result.err == nil {
				observations = append(observations, result.observation)
			}
		}
	}
	sort.Slice(observations, func(left, right int) bool { return observations[left].Ref < observations[right].Ref })
	return observations, ctx.Err()
}
