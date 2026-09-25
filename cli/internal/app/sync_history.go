package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

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
	ref, err := s.ResolveRemoteBranch(ctx, in, branch)
	if err != nil {
		return ref, err
	}
	if history, ok := s.store.(outbound.HistoryStore); ok {
		events, err := history.ListHistoryEvents(ctx, ref.RepoID)
		if err != nil {
			return ref, err
		}
		bindings, err := domain.ProjectContextBranches(events)
		if err != nil {
			return ref, err
		}
		if bindings.Released[branch] != "" {
			return ref, fmt.Errorf("%w: PR source branch %q was renamed, archived, or reused; an exact historical source binding is required", domain.ErrSyncConflict, branch)
		}
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

func (s *SyncRepoService) pushSelectedHistory(ctx context.Context, repoID string, events []domain.HistoryEvent) error {
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
	for _, e := range events {
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
	}
	return nil
}

// Preflight the complete ready set before sending any completion barrier. A
// worker can bind permanently after the very first publication in this push.
// Accepted events are excluded: later captures cannot undo a terminal binding.
func (s *SyncRepoService) orderHistoryPublications(ctx context.Context, repoID string, events, accepted []domain.HistoryEvent) ([]domain.HistoryEvent, error) {
	byID := make(map[string]domain.HistoryEvent, len(accepted))
	for _, e := range accepted {
		byID[e.ID] = e
	}
	ordinary := make([]domain.HistoryEvent, 0, len(events))
	groups := map[[3]string][]domain.HistoryEvent{}
	var keys [][3]string
	identities := map[[3]string]string{}
	for _, e := range events {
		if old, ok := byID[e.ID]; ok {
			if !reflect.DeepEqual(old, e) {
				return nil, domain.ErrHashMismatch
			}
			continue
		}
		if e.Kind != "publish" {
			ordinary = append(ordinary, e)
			continue
		}
		if e.RepoID != repoID {
			return nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
		// A reused canonical name or tracking alias at this exact Git revision
		// must not let the first identity win before the other is uploaded.
		for _, name := range []string{e.Branch, e.LocalBranch} {
			if name == "" {
				continue
			}
			key := [3]string{e.RepoID, name, e.GitAfter}
			if old := identities[key]; old != "" && old != e.BranchID {
				return nil, fmt.Errorf("%w: publication name %q at Git %s belongs to multiple branch identities", domain.ErrSyncConflict, name, e.GitAfter)
			}
			identities[key] = e.BranchID
		}
		// Names and worktrees are observations of an identity, not separate
		// groups. Preserve full Git object IDs, including SHA-256 repositories.
		key := [3]string{e.RepoID, e.BranchID, e.GitAfter}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], e)
	}
	observations := append(append([]domain.HistoryEvent(nil), accepted...), events...)
	for _, key := range keys {
		group := groups[key]
		ranks := map[domain.ContentHash]int{}
		for _, e := range group {
			ranks[e.Target] = 0
		}
		var maximal domain.ContentHash
		for target := range ranks {
			if len(ranks) == 1 {
				maximal = target
				break
			}
			ancestors, err := s.publicationAncestors(ctx, repoID, target)
			if err != nil {
				return nil, fmt.Errorf("%w: cannot prove publication ancestry for %s: %w", domain.ErrSyncConflict, target, err)
			}
			for other := range ranks {
				if ancestors[other] {
					ranks[target]++
				}
			}
			if ranks[target] == len(ranks) {
				maximal = target
			}
		}
		if maximal == "" {
			return nil, fmt.Errorf("%w: branch identity %s at Git %s has incomparable publications", domain.ErrSyncConflict, key[1], key[2])
		}
		// An ancestor may expose an alias which the maximal publication cannot
		// satisfy. Refuse that ambiguity instead of allowing its worker to bind
		// the ancestor. Several publications of the maximal target may cover it.
		covered := map[string]bool{}
		names := map[string][]string{}
		for _, e := range group {
			names[e.ID] = publicationSourceNames(e, observations)
			if e.Target == maximal {
				for _, name := range names[e.ID] {
					covered[name] = true
				}
			}
		}
		for _, e := range group {
			for _, name := range names[e.ID] {
				if !covered[name] {
					return nil, fmt.Errorf("%w: maximal publication for branch identity %s at Git %s does not cover source name %q", domain.ErrSyncConflict, key[1], key[2], name)
				}
			}
		}
		// Every descendant has strictly more candidate ancestors in a DAG.
		// IDs only break ties between equivalent or already-covered sources.
		sort.Slice(group, func(i, j int) bool {
			if ranks[group[i].Target] != ranks[group[j].Target] {
				return ranks[group[i].Target] > ranks[group[j].Target]
			}
			return group[i].ID < group[j].ID
		})
		ordinary = append(ordinary, group...)
	}
	return ordinary, nil
}

// Alias eligibility follows the existing server proof/attachment contract.
// The publication clock is not evidence of when the source was observed.
func publicationSourceNames(p domain.HistoryEvent, events []domain.HistoryEvent) []string {
	names := []string{p.Branch}
	if p.LocalBranch == "" || p.LocalBranch == p.Branch || p.WorktreeID == "" {
		return names
	}
	for _, proof := range events {
		if proof.Kind == "publish" || proof.Kind == "pr-merge" || proof.RepoID != p.RepoID ||
			proof.BranchID != p.BranchID || proof.Branch != p.Branch || proof.LocalBranch != p.LocalBranch ||
			proof.WorktreeID != p.WorktreeID || proof.GitAfter != p.GitAfter || proof.Target != p.Target {
			continue
		}
		for _, attached := range events {
			if attached.Kind == "attach" && attached.RepoID == p.RepoID && attached.LocalBranch != "" &&
				attached.WorktreeID == p.WorktreeID && attached.BranchID == p.BranchID && !attached.CreatedAt.After(proof.CreatedAt) {
				return append(names, p.LocalBranch)
			}
		}
	}
	return names
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
