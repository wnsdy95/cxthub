package cli

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func mergeContextCoverage(ctx context.Context, c *Container, cwd string, evidence mergeEvidence, candidates []domain.ContentHash) (map[domain.ContentHash]bool, bool, error) {
	covered := map[domain.ContentHash]bool{}
	view, err := mergeBranchContext(ctx, c, inbound.SyncInput{Cwd: cwd, RepoID: evidence.RepoID, ExpectedBranchID: evidence.BranchID}, evidence.Branch)
	if err != nil {
		return nil, false, err
	}
	if view == nil {
		return covered, false, nil
	}
	if err := verifyMergeDeliveryScope(ctx, c, cwd, evidence); err != nil {
		return nil, false, err
	}
	for _, snapshot := range view.Inclusion.SnapshotIDs {
		covered[snapshot] = true
	}
	current := false
	for _, candidate := range candidates {
		if covered[candidate] {
			current = true
			continue
		}
		if mergeDeliveryAcknowledged(*view, evidence, candidate) {
			covered[candidate] = true
			continue
		}
		if _, err := mergeCandidateIncluded(*view, candidate); err != nil {
			return nil, false, err
		}
	}
	return covered, current, nil
}

func mergeDeliveryAcknowledged(view domain.ContextQueryView, evidence mergeEvidence, candidate domain.ContentHash) bool {
	if view.Inclusion == nil || view.Inclusion.BranchID != evidence.BranchID || domain.ValidateContentHash(candidate) != nil {
		return false
	}
	for _, merge := range view.Inclusion.Merges {
		if !merge.DeliveryAcknowledged || merge.Source != candidate || merge.DestinationBranchID != evidence.BranchID || !domain.ValidGitOID(merge.MergeSHA) {
			continue
		}
		for _, sha := range evidence.SHAs {
			if sha == merge.MergeSHA {
				return true
			}
		}
	}
	return false
}

func mergeBranchContext(ctx context.Context, c *Container, in inbound.SyncInput, branch string) (*domain.ContextQueryView, error) {
	reader, ok := c.Sync.(inbound.RemoteBranchContextReader)
	if !ok {
		return nil, nil
	}
	view, err := reader.ResolveRemoteBranchContext(ctx, in, branch)
	if err != nil {
		return nil, err
	}
	selection := domain.ContextSelection{Branch: branch, Position: branch, Scope: "current"}
	if err := domain.ValidateContextQuery(in.RepoID, selection, view); err != nil {
		return nil, err
	}
	if view.Branch != branch || view.Inclusion == nil || view.Inclusion.BranchID != in.ExpectedBranchID || view.Inclusion.SnapshotID != view.Position || domain.ValidateContentHash(view.Position) != nil {
		return nil, domain.ErrSelectionChanged
	}
	return &view, nil
}

func mergeCandidateIncluded(view domain.ContextQueryView, candidate domain.ContentHash) (bool, error) {
	for _, snapshot := range view.Inclusion.SnapshotIDs {
		if snapshot == candidate {
			return true, nil
		}
	}
	for _, merge := range view.Inclusion.Merges {
		if merge.Source == candidate {
			return false, fmt.Errorf("%w: server PR contribution is not included at the selected code position", domain.ErrSelectionChanged)
		}
	}
	return false, nil
}
