package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *SyncRepoService) remoteContextProtocol(ctx context.Context, repo string) (int, error) {
	if remote, ok := s.remote.(interface {
		ContextProtocol(context.Context, string) (int, error)
	}); ok {
		return remote.ContextProtocol(ctx, repo)
	}
	return 0, nil
}

// A name-only PR lookup cannot distinguish an old merged branch from a newer
// task using its name. Preserve both until an exact PR source binding exists.
func (s *SyncRepoService) ResolveRemotePRBranch(ctx context.Context, in inbound.SyncInput, branch string) (domain.Ref, error) {
	ref, events, err := s.fetchRemoteBranch(ctx, in, branch)
	if err != nil {
		return ref, err
	}
	if history, ok := s.store.(outbound.HistoryStore); ok {
		local, err := history.ListHistoryEvents(ctx, ref.RepoID)
		if err != nil {
			return ref, err
		}
		events = append(append([]domain.HistoryEvent{}, events...), local...)
	}
	bindings, err := domain.ProjectContextBranches(events)
	if err != nil {
		return ref, fmt.Errorf("%w: resolve PR source branch history: %v", domain.ErrSyncConflict, err)
	}
	if bindings.Released[branch] != "" {
		return ref, fmt.Errorf("%w: PR source branch %q was renamed, archived, or reused; an exact historical source binding is required", domain.ErrSyncConflict, branch)
	}
	if active, ok := bindings.Active[branch]; ok && ref.BranchID != "" && active.ID != ref.BranchID {
		return ref, fmt.Errorf("%w: PR source branch %q changed identity during fetch", domain.ErrSyncConflict, branch)
	}
	return ref, nil
}

func (s *SyncRepoService) localPushHistory(ctx context.Context, repoID string) ([]domain.HistoryEvent, error) {
	local, ok := s.store.(outbound.HistoryStore)
	if !ok {
		return nil, nil
	}
	return local.ListHistoryEvents(ctx, repoID)
}

func (s *SyncRepoService) readPushCatalog(ctx context.Context, repoID string) (domain.Manifest, []domain.HistoryEvent, error) {
	if reader, ok := s.store.(outbound.PushCatalogReader); ok {
		return reader.ReadPushCatalog(ctx, repoID)
	}
	// Narrow adapters without concurrent writers retain the existing contract.
	events, err := s.localPushHistory(ctx, repoID)
	if err != nil {
		return domain.Manifest{}, nil, err
	}
	man, err := s.store.Manifest(ctx, repoID)
	return man, events, err
}

func (s *SyncRepoService) pushHistory(ctx context.Context, repoID string) error {
	events, err := s.localPushHistory(ctx, repoID)
	if err != nil {
		return err
	}
	return s.pushSelectedHistory(ctx, repoID, events)
}

func (s *SyncRepoService) pushSelectedHistory(ctx context.Context, repoID string, events []domain.HistoryEvent, observers ...func(inbound.SyncProgress)) error {
	if len(events) == 0 {
		return nil
	}
	remote, ok := s.remote.(outbound.RemoteHistory)
	if !ok {
		return fmt.Errorf("server does not support durable context history; operations remain local")
	}
	accepted, err := remote.PullHistoryEvents(ctx, repoID)
	if err != nil {
		return err
	}
	events, err = s.orderHistoryPublications(ctx, repoID, events, accepted)
	if err != nil {
		return err
	}
	progress := inbound.SyncInput{}
	if len(observers) > 0 {
		progress.Progress = observers[0]
	}
	syncProgress(progress, "push", "publish-history", 0, len(events))
	for index, e := range events {
		if e.Kind == "advance" {
			// The previous local tip may never have been pushed. Publish that
			// prerequisite only as a normal fast-forward before the retained
			// continuation CAS. Concurrent server work is never force-replaced.
			man, err := s.remote.RemoteManifest(ctx, repoID)
			if err != nil {
				return err
			}
			var target domain.ContentHash
			for _, ref := range man.Refs {
				if ref.Kind == domain.RefBranch && ref.Name == e.Branch {
					target = ref.Target
					break
				}
			}
			if target != e.Source {
				if !s.isAncestor(ctx, target, e.Source) {
					return fmt.Errorf("history %s awaits reconciliation with concurrent server work: %w", e.ID, domain.ErrSyncConflict)
				}
				base := domain.Ref{RepoID: repoID, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Source}
				if err := s.remote.Push(ctx, repoID, nil, nil, []domain.Ref{base}, false, false); err != nil {
					return err
				}
			}
		}
		if err := remote.PushHistoryEvent(ctx, e); err != nil {
			return fmt.Errorf("history %s (%s) remains pending: %w", e.ID, e.Kind, err)
		}
		syncProgress(progress, "push", "publish-history", index+1, len(events))
	}
	return nil
}

// The application supplies verified ancestry; grouping and alias policy are shared
// with the pure scoped planner rather than implemented twice.
func (s *SyncRepoService) orderHistoryPublications(ctx context.Context, repoID string, events, accepted []domain.HistoryEvent) ([]domain.HistoryEvent, error) {
	roots, err := domain.PublicationAncestryRoots(repoID, events, accepted)
	if err != nil {
		return nil, err
	}
	ancestry := map[domain.ContentHash]map[domain.ContentHash]bool{}
	for _, target := range roots {
		found, err := s.publicationAncestors(ctx, repoID, target)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot prove publication ancestry for %s: %w", domain.ErrSyncConflict, target, err)
		}
		ancestry[target] = found
	}
	return domain.OrderHistoryPublications(repoID, events, accepted, ancestry)
}

func (s *SyncRepoService) publicationAncestors(ctx context.Context, repoID string, target domain.ContentHash) (map[domain.ContentHash]bool, error) {
	seen, visiting := map[domain.ContentHash]bool{}, map[domain.ContentHash]bool{}
	var walk func(domain.ContentHash) error
	walk = func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if visiting[id] {
			return fmt.Errorf("cyclic snapshot ancestry at %s", id)
		}
		if seen[id] {
			return nil
		}
		snapshot, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if snapshot.ID != id || snapshot.RepoID != repoID {
			return domain.ErrHashMismatch
		}
		visiting[id] = true
		for _, parent := range snapshot.ReachabilityParents() {
			if err := walk(parent); err != nil {
				return err
			}
		}
		delete(visiting, id)
		seen[id] = true
		return nil
	}
	err := walk(target)
	return seen, err
}

func (s *SyncRepoService) readRemoteHistory(ctx context.Context, repoID string) ([]domain.HistoryEvent, error) {
	remote, ok := s.remote.(outbound.RemoteHistory)
	if !ok {
		return nil, nil
	}
	events, err := remote.PullHistoryEvents(ctx, repoID)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.RepoID != repoID {
			return nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (s *SyncRepoService) storeRemoteHistory(ctx context.Context, events []domain.HistoryEvent) error {
	ordered, err := domain.OrderHistoryEvents(events)
	if err != nil {
		return err
	}
	events = ordered
	local, ok := s.store.(outbound.HistoryStore)
	if !ok && len(events) > 0 {
		return fmt.Errorf("local context history storage unavailable")
	}
	for _, e := range events {
		for _, id := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
			if id == "" {
				continue
			}
			snap, err := s.store.GetSnapshot(ctx, id)
			if err != nil {
				return err
			}
			if snap.RepoID != e.RepoID {
				return domain.ErrHashMismatch
			}
		}
	}
	// Validate the complete incoming set before publishing any retained roots.
	for _, e := range events {
		if err := local.PutHistoryEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// PromotePullRequest delegates binding and append to the cloud service, then
// adopts its exact result without a second name-only server mutation.
func (s *SyncRepoService) PromotePullRequest(ctx context.Context, in inbound.SyncInput, pr outbound.MergedPullRequest) error {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return err
	}
	local, ok := s.store.(outbound.PRDeliveryStore)
	if !ok {
		return fmt.Errorf("durable PR delivery storage unavailable")
	}
	request := domain.PullRequestMerge{Number: pr.Number, BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeCommitSHA}
	if err := local.QueuePRDelivery(ctx, repoID, request); err != nil {
		return fmt.Errorf("persist PR delivery before sending: %w", err)
	}
	remote, ok := s.remote.(interface {
		PromotePullRequest(context.Context, string, domain.PullRequestMerge) (domain.Ref, error)
	})
	if !ok {
		return fmt.Errorf("server does not support exact PR promotion")
	}
	ref, err := remote.PromotePullRequest(ctx, repoID, domain.PullRequestMerge{Number: pr.Number, BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeCommitSHA})
	if err != nil {
		return err
	}
	if ref.RepoID != repoID || ref.Kind != domain.RefBranch || ref.Name != request.BaseBranch || ref.BranchID == "" || ref.Target == "" || domain.ValidateRef(ref) != nil {
		return domain.ErrHashMismatch
	}
	if err := local.AcceptPRDelivery(ctx, repoID, request); err != nil {
		return err
	}
	if _, err := s.Pull(ctx, inbound.SyncInput{RepoID: repoID, Cwd: in.Cwd, FetchOnly: true}); err != nil {
		return errors.Join(domain.ErrPRLocalReconciliation, err)
	}
	if err := s.convergeAppendedBranch(ctx, repoID, ref); err != nil {
		return errors.Join(domain.ErrPRLocalReconciliation, err)
	}
	return nil
}
