package app

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// This path never enters the global PR/pending/promotion/graft drains. The one
// plan and its object inventory survive transport retries without re-selection.
func (s *SyncRepoService) pushPublication(ctx context.Context, in inbound.SyncInput, repoID string) (inbound.SyncOutput, error) {
	fail := func(err error) (inbound.SyncOutput, error) { return inbound.SyncOutput{}, err }
	scope := domain.PublicationScope{}
	if in.Publication != nil {
		scope = *in.Publication
		scope.Branches = append([]domain.PublicationBranch(nil), scope.Branches...)
		scope.ExpectedTargets = maps.Clone(scope.ExpectedTargets)
	}
	if (in.Ref != "" && in.Publication != nil) || (in.Ref == "" && len(scope.Branches) == 0) {
		return fail(domain.ErrInvalidRef)
	}
	// Without the worktree root the existing outbox API cannot fence its queue.
	if in.Cwd == "" || s.gitCtx == nil || s.outbox == nil {
		return fail(fmt.Errorf("%w: selected publication requires a repository root and outbox", domain.ErrSyncConflict))
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return fail(err)
	}
	if repo.ID != repoID || repo.LocalPath == "" {
		return fail(domain.ErrHashMismatch)
	}
	_, err = s.prepareRepositoryConnection(ctx, repo)
	if err != nil {
		return fail(err)
	}
	protocol, err := s.remoteContextProtocol(ctx, repoID)
	if err != nil {
		return fail(err)
	}
	if protocol != 1 {
		return fail(domain.ErrContextProtocolRequired)
	}

	remote, ok := s.remote.(outbound.RemoteHistory)
	if !ok {
		return fail(fmt.Errorf("%w: remote history unavailable", domain.ErrSyncConflict))
	}
	accepted, err := remote.PullHistoryEvents(ctx, repoID)
	if err != nil {
		return fail(err)
	}
	var man domain.Manifest
	var history []domain.HistoryEvent
	var all []domain.Snapshot
	var queue []domain.GraftQueueEvent
	// Match Save's queue-before-snapshot lock order; no network under this lock.
	err = s.outbox.WithGrafts(ctx, repo.LocalPath, func(q outbound.GraftQueueAccess) error {
		var err error
		queue, err = q.Load()
		if err != nil {
			return err
		}
		man, history, err = s.readPushCatalog(ctx, repoID)
		if err != nil {
			return err
		}
		all, err = s.collectSnapshots(ctx, repoID, man)
		return err
	})
	if err != nil {
		return fail(err)
	}
	plan, err := domain.PlanPublication(domain.PublicationPlanInput{RepoID: repoID, ContextProtocol: protocol, Ref: in.Ref, Scope: scope, Refs: man.Refs, History: history, Accepted: accepted, Snapshots: all})
	if err != nil {
		return fail(err)
	}
	var pins []domain.ContentHash
	for _, e := range plan.HistoryToSend {
		if e.MemoryHash != "" {
			pins = append(pins, e.MemoryHash)
		}
	}
	snaps, _, err := s.snapshotDependencyClosureChecked(ctx, all, plan.SnapshotRoots, pins, nil, true)
	if err != nil {
		return fail(err)
	}
	if err = s.verifyPublicationPins(ctx, repoID, plan.HistoryToSend, snaps); err != nil {
		return fail(err)
	}
	initializations, err := s.prepareInitialAnchors(ctx, repo, plan, snaps)
	if err != nil {
		return fail(err)
	}
	prefix, err := publicationGraftPrefix(queue, snaps)
	if err != nil {
		return fail(err)
	}
	promotions, err := s.outbox.ListPromotions(ctx, repo.LocalPath)
	if err != nil {
		return fail(err)
	}
	selected := map[domain.ContentHash]bool{}
	for _, snap := range snaps {
		selected[snap.ID] = true
	}
	for id := range promotions {
		if !selected[id] {
			delete(promotions, id)
		}
	}
	// Validate and freeze attachment chains before the first object/effect write.
	attachments, err := s.remoteMemoryCatalog(ctx, repoID)
	if err != nil {
		return fail(err)
	}
	var memorySnaps []domain.Snapshot
	for _, snap := range snaps {
		if snap.MemoryHash != "" {
			memorySnaps = append(memorySnaps, snap)
		}
	}
	memoryPlans, ahead, err := s.prepareMemoryPushPlans(ctx, repoID, memorySnaps, attachments)
	if err != nil {
		return fail(err)
	}
	if err = s.checkPublicationState(ctx, repo.LocalPath, snaps, prefix); err != nil {
		return fail(err)
	}
	pushSnaps, pushDocs, err := s.selectPushObjects(ctx, repoID, snaps)
	if err != nil {
		return fail(err)
	}
	if err = s.pushSettingsObjects(ctx, repoID, snaps); err != nil {
		return fail(err)
	}
	if err = s.pushSelectedObjects(ctx, repoID, snaps, pushSnaps, pushDocs, in.Progress); err != nil {
		return fail(err)
	}
	for _, p := range memoryPlans {
		if ahead[p.snapshotID] {
			continue
		}
		// Existing bounded causal CAS retry, with the frozen chain. A missing archive
		// defers: the global restore helper re-reads mutable, potentially wider state.
		if err = s.pushMemoryPlanFromKnown(ctx, repoID, p, attachments[p.snapshotID]); err != nil {
			return fail(err)
		}
	}
	if err = s.executePublicationGrafts(ctx, repo.LocalPath, repoID, prefix); err != nil {
		return fail(err)
	}
	if err = s.checkPublicationState(ctx, repo.LocalPath, snaps, nil); err != nil {
		return fail(err)
	}
	ids := make([]domain.ContentHash, 0, len(promotions))
	for id := range promotions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		message := promotions[id]
		if err = s.remote.PromoteSnapshotMessage(ctx, repoID, id, message); err != nil {
			return fail(err)
		}
		if err = s.outbox.AcknowledgePromotion(ctx, repo.LocalPath, id, message); err != nil {
			return fail(err)
		}
	}
	for _, request := range initializations {
		if err = s.checkPublicationState(ctx, repo.LocalPath, snaps, nil); err != nil {
			return fail(err)
		}
		initializer := s.remote.(outbound.RepositoryInitialization)
		accepted, err := initializer.FinalizeRepositoryInitialization(ctx, repoID, request)
		if err != nil {
			return fail(err)
		}
		if accepted.Validate(repoID) != nil || accepted.CreationID != request.CreationID || accepted.Anchor == nil || !reflect.DeepEqual(*accepted.Anchor, request.Anchor) {
			return fail(domain.ErrHashMismatch)
		}
	}
	if err = s.pushPublicationHistory(ctx, repo.LocalPath, repoID, plan, snaps); err != nil {
		return fail(err)
	}
	if len(plan.RefsToPush) > 0 {
		if err = s.checkPublicationState(ctx, repo.LocalPath, snaps, nil); err != nil {
			return fail(err)
		}
		if err = s.remote.Push(ctx, repoID, nil, nil, plan.RefsToPush, in.Force, in.Append); err != nil {
			return fail(err)
		}
	}
	// Unsync deletion is keyed only by name, so it cannot prove that this frozen
	// publication covers a concurrently replaced target or reused identity.
	// Leave these markers for reachability-based reconciliation.
	return inbound.SyncOutput{Pushed: len(pushSnaps), NewRefs: plan.RefsToPush}, nil
}

// Exact membership admits foreign ordinary proof without granting branch effects.
func publicationAllowsEvent(plan domain.PublicationPlan, e domain.HistoryEvent) bool {
	member := false
	for _, planned := range plan.HistoryToSend {
		if reflect.DeepEqual(planned, e) {
			member = true
			break
		}
	}
	if !member || e.Kind == "pr-merge" {
		return false
	}
	if e.Kind == "position" || e.Kind == "attach" {
		return true
	}
	for _, b := range plan.Authority {
		if b.BranchID == e.BranchID {
			return true
		}
	}
	return false
}

func (s *SyncRepoService) pushPublicationHistory(ctx context.Context, root, repo string, plan domain.PublicationPlan, snaps []domain.Snapshot) error {
	remote, ok := s.remote.(outbound.RemoteHistory)
	if !ok {
		return domain.ErrSyncConflict
	}
	ancestor := publicationAncestor(snaps)
	for _, e := range plan.HistoryToSend {
		if err := s.checkPublicationState(ctx, root, snaps, nil); err != nil {
			return err
		}
		if !publicationAllowsEvent(plan, e) {
			return fmt.Errorf("%w: history exceeds frozen publication authority", domain.ErrSyncConflict)
		}
		if e.Kind == "advance" {
			man, err := s.remote.RemoteManifest(ctx, repo)
			if err != nil {
				return err
			}
			if man.RepoID != "" && man.RepoID != repo {
				return domain.ErrHashMismatch
			}
			var target domain.ContentHash
			found := false
			for _, ref := range man.Refs {
				if ref.Kind == domain.RefBranch && ref.Name == e.Branch {
					if found || domain.ValidateRef(ref) != nil {
						return domain.ErrHashMismatch
					}
					found = true
					id := ref.BranchID
					if id == "" {
						id = domain.LegacyContextBranchID(repo, ref.Name)
					}
					if id != e.BranchID {
						return domain.ErrSyncConflict
					}
					target = ref.Target
				}
			}
			if target != e.Source {
				if !ancestor(target, e.Source) {
					return fmt.Errorf("%w: continuation prerequisite is not a frozen fast-forward", domain.ErrSyncConflict)
				}
				ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Source}
				if err = s.remote.Push(ctx, repo, nil, nil, []domain.Ref{ref}, false, false); err != nil {
					return err
				}
			}
		}
		if err := remote.PushHistoryEvent(ctx, e); err != nil {
			return fmt.Errorf("history %s remains pending: %w", e.ID, err)
		}
	}
	return nil
}

// Historical pins must either be in a frozen attachment's causal chain (which
// existing memory CAS uploads), or already exist as verified remote objects.
// There is no immutable-only memory upload port; never invent a pointer advance.
func (s *SyncRepoService) verifyPublicationPins(ctx context.Context, repo string, history []domain.HistoryEvent, snaps []domain.Snapshot) error {
	local := map[domain.ContentHash]domain.MemoryDigest{}
	for _, snap := range snaps {
		if snap.MemoryHash != "" {
			plan, err := s.localMemoryPushPlan(ctx, snap.ID, snap.MemoryHash)
			if err != nil {
				return err
			}
			for _, object := range plan.chain {
				local[object.hash] = object.digest
			}
		}
	}
	for _, e := range history {
		if e.MemoryHash == "" {
			continue
		}
		digest, ok := local[e.MemoryHash]
		if !ok {
			var err error
			digest, err = s.remote.PullMemoryObject(ctx, repo, e.MemoryHash)
			if err != nil {
				return fmt.Errorf("pinned memory prerequisite remains pending: %w", err)
			}
			if err = validateMemoryAttachmentObject(digest, e.MemoryHash, digest.SnapshotID); err != nil {
				return err
			}
		}
		if e.MemorySource != "" && e.MemorySource != digest.SnapshotID {
			return domain.ErrHashMismatch
		}
		if e.MemorySource == "" && e.Source != digest.SnapshotID && e.Target != digest.SnapshotID {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

// Both initial observation and replay use the same frozen ancestry rule.
func publicationAncestor(snaps []domain.Snapshot) func(domain.ContentHash, domain.ContentHash) bool {
	graph := map[domain.ContentHash]domain.Snapshot{}
	for _, snap := range snaps {
		graph[snap.ID] = snap
	}
	return func(from, to domain.ContentHash) bool {
		if from == "" {
			return true
		}
		seen := map[domain.ContentHash]bool{}
		stack := []domain.ContentHash{to}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if id == from {
				return true
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			stack = append(stack, graph[id].ReachabilityParents()...)
		}
		return false
	}
}
