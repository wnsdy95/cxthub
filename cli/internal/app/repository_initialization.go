package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// The server proves creation. A missing GET is only a reason to ask for its
// atomic create-only operation, never permission to upgrade an existing repo.
func (s *SyncRepoService) prepareRepositoryConnection(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	if err := s.preflightLocalRoots(ctx, repo.ID); err != nil {
		return domain.Repo{}, err
	}
	initializer, ok := s.remote.(outbound.RepositoryInitialization)
	if !ok {
		return s.remote.RegisterRepo(ctx, repo)
	}
	state, err := initializer.RepositoryInitializationState(ctx, repo.ID, "")
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, err
	}
	if err == nil {
		if state.Repo.ID != repo.ID {
			return domain.Repo{}, domain.ErrHashMismatch
		}
		// Ordinary member connections do not require creation/manager authority.
		return s.remote.RegisterRepo(ctx, repo)
	}
	receipt, err := initializer.BeginRepositoryInitialization(ctx, repo)
	if err != nil {
		return domain.Repo{}, err
	}
	if err := receipt.Validate(repo.ID); err != nil || receipt.Anchor != nil {
		return domain.Repo{}, domain.ErrHashMismatch
	}
	// A recovered creation receipt is never the current repository state.
	state, err = initializer.RepositoryInitializationState(ctx, repo.ID, "")
	if err != nil {
		return domain.Repo{}, err
	}
	if state.Repo.ID != repo.ID || state.Repo.ContextProtocol != 1 {
		return domain.Repo{}, domain.ErrHashMismatch
	}
	return state.Repo, nil
}

// Only selected pre-existing branches need observation anchors. Actual births
// retain ordinary member authorization; eligibility never grants write access.
func (s *SyncRepoService) prepareInitialAnchors(ctx context.Context, repo domain.Repo, plan domain.PublicationPlan, snapshots []domain.Snapshot) ([]domain.RepositoryInitializationFinalize, error) {
	initializer, ok := s.remote.(outbound.RepositoryInitialization)
	if !ok {
		return nil, nil
	}
	var requests []domain.RepositoryInitializationFinalize
	for _, branch := range plan.Authority {
		if branch.BranchID != domain.LegacyContextBranchID(repo.ID, branch.Branch) {
			continue
		}
		state, err := initializer.RepositoryInitializationState(ctx, repo.ID, branch.Branch)
		if err != nil {
			return nil, err
		}
		if state.Repo.ID != repo.ID || state.Repo.ContextProtocol != 1 {
			return nil, domain.ErrHashMismatch
		}
		if !state.InitialAnchorAvailable {
			continue
		}
		var ref domain.Ref
		for _, selected := range plan.RefsToPush {
			if selected.Name == branch.Branch && selected.BranchID == branch.BranchID {
				ref = selected
				break
			}
		}
		if ref.Target == "" {
			return nil, fmt.Errorf("%w: initial observation requires a selected branch tip", domain.ErrContextProtocolRequired)
		}
		anchor, err := initializationAnchorForBranch(repo.ID, ref, plan.HistoryToSend, snapshots)
		if err != nil {
			return nil, err
		}
		requests = append(requests, domain.RepositoryInitializationFinalize{Anchor: anchor})
	}
	if len(requests) == 0 {
		return nil, nil
	}
	// Recover immutable creation metadata independently of later origin/default
	// changes. The server rechecks current manager permission on read and write.
	receipt, err := initializer.ReadRepositoryInitialization(ctx, repo.ID)
	if err != nil {
		return nil, err
	}
	if err := receipt.Validate(repo.ID); err != nil || receipt.Anchor != nil {
		return nil, domain.ErrHashMismatch
	}
	for i := range requests {
		requests[i].CreationID = receipt.CreationID
	}
	return requests, nil
}

// Bare push explicitly authorizes all branches. Observe each eligible legacy
// branch through the same selected pipeline before the existing broad replay.
func (s *SyncRepoService) initializeBeforeBroadPublication(ctx context.Context, in inbound.SyncInput, repoID string) error {
	initializer, ok := s.remote.(outbound.RepositoryInitialization)
	if !ok || in.Cwd == "" {
		return nil
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return err
	}
	if repo.ID != repoID {
		return domain.ErrHashMismatch
	}
	registered, err := s.prepareRepositoryConnection(ctx, repo)
	if err != nil || registered.ContextProtocol != 1 {
		return err
	}
	manifest, _, err := s.readPushCatalog(ctx, repoID)
	if err != nil {
		return err
	}
	for _, ref := range manifest.Refs {
		if ref.Kind != domain.RefBranch || ref.Target == "" || (ref.BranchID != "" && ref.BranchID != domain.LegacyContextBranchID(repoID, ref.Name)) {
			continue
		}
		state, err := initializer.RepositoryInitializationState(ctx, repoID, ref.Name)
		if err != nil {
			return err
		}
		if !state.InitialAnchorAvailable {
			continue
		}
		selected := in
		selected.Ref = ref.Name
		selected.Force, selected.Append, selected.ForegroundOnly = false, false, true
		if _, err := s.pushPublication(ctx, selected, repoID); err != nil {
			return err
		}
	}
	return nil
}

func initializationAnchorForBranch(repo string, ref domain.Ref, history []domain.HistoryEvent, snapshots []domain.Snapshot) (domain.RepositoryInitializationAnchor, error) {
	anchor := domain.RepositoryInitializationAnchor{Ref: ref, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
	ancestor := publicationAncestor(snapshots)
	var tip domain.ContentHash
	for _, e := range history {
		if e.Kind != "advance" || e.BranchID != ref.BranchID {
			continue
		}
		if e.Branch != ref.Name || (tip != "" && !ancestor(tip, e.Source)) {
			return domain.RepositoryInitializationAnchor{}, fmt.Errorf("%w: incompatible initial continuation sequence", domain.ErrSyncConflict)
		}
		if tip == "" {
			anchor.Ref.Target = e.Source
		}
		tip = e.Target
	}
	if tip != "" && !ancestor(tip, ref.Target) {
		return domain.RepositoryInitializationAnchor{}, fmt.Errorf("%w: initial continuation does not reach the selected tip", domain.ErrSyncConflict)
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(snapshots))
	for _, snap := range snapshots {
		byID[snap.ID] = snap
	}
	queue := []domain.ContentHash{anchor.Ref.Target}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, visited := anchor.SnapshotStates[id]; visited {
			continue
		}
		snap, ok := byID[id]
		if !ok || snap.RepoID != repo {
			return domain.RepositoryInitializationAnchor{}, domain.ErrHashMismatch
		}
		state, err := domain.SnapshotStateHash(snap)
		if err != nil {
			return domain.RepositoryInitializationAnchor{}, err
		}
		anchor.SnapshotStates[id] = state
		queue = append(queue, snap.ReachabilityParents()...)
	}
	if err := anchor.Validate(repo); err != nil {
		return domain.RepositoryInitializationAnchor{}, fmt.Errorf("%w: initial legacy anchor is unavailable: %v", domain.ErrContextProtocolRequired, err)
	}
	return anchor, nil
}

// InitialCaptureEligibility is a read-only, branch-specific runtime query.
// Absence of the capability is not authorization for an empty first capture.
func (s *SyncRepoService) InitialCaptureEligibility(ctx context.Context, in inbound.SyncInput, branch string) (domain.Repo, bool, error) {
	if domain.ValidateBranchName(branch) != nil {
		return domain.Repo{}, false, domain.ErrInvalidRef
	}
	initializer, ok := s.remote.(outbound.RepositoryInitialization)
	if !ok {
		return domain.Repo{}, false, nil
	}
	repo, err := s.repoID(ctx, in)
	if err != nil {
		return domain.Repo{}, false, err
	}
	state, err := initializer.RepositoryInitializationState(ctx, repo, branch)
	if err != nil {
		return domain.Repo{}, false, err
	}
	if state.Repo.ID != repo || (state.InitialAnchorAvailable && state.Repo.ContextProtocol != 1) {
		return domain.Repo{}, false, domain.ErrHashMismatch
	}
	return state.Repo, state.InitialAnchorAvailable, nil
}
