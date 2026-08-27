package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// SyncRepoService implements the SyncRepo inbound port as a use-case service.
//
// Dependencies: SessionStore (local .cxt/), RemoteSync (backendclient REST), GitContext (repoID resolution).
// push/pull authority operations are delegated to the central server REST (sync protocol) via RemoteSync.
type SyncRepoService struct {
	store  outbound.SessionStore
	remote outbound.RemoteSync
	gitCtx outbound.GitContext
}

// NewSyncRepoService creates and injects dependencies into SyncRepoService.
func NewSyncRepoService(store outbound.SessionStore, remote outbound.RemoteSync, gitCtx outbound.GitContext) *SyncRepoService {
	return &SyncRepoService{store: store, remote: remote, gitCtx: gitCtx}
}

// repoID returns the in.RepoID (if present) or the ID of the current repo interpreted from gitctx.
func (s *SyncRepoService) repoID(ctx context.Context, in inbound.SyncInput) (string, error) {
	if in.RepoID != "" {
		return in.RepoID, nil
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return "", err
	}
	return repo.ID, nil
}

func validateSnapshotObject(snap domain.Snapshot) error {
	if err := domain.ValidateContentHash(snap.ID); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(snap.DocHash); err != nil {
		return err
	}
	if snap.ID != snap.DocHash {
		return domain.ErrHashMismatch
	}
	if snap.RepoID != "" {
		if err := domain.ValidateContentHash(domain.ContentHash(snap.RepoID)); err != nil {
			return err
		}
	}
	for _, h := range []domain.ContentHash{snap.MemoryHash, snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
		if err := domain.ValidateOptionalContentHash(h); err != nil {
			return err
		}
	}
	for _, p := range snap.Parents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	for _, p := range snap.GraftParents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	return nil
}

func sameParents(left, right []domain.ContentHash) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// validatePullBatch validates the entire remote response before local write. The same snapshot ID's natural
// parent is immutable across replicas, and GraftParents can only be added as server overlays.
func validatePullBatch(ctx context.Context, store outbound.SessionStore, repoID string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	docByHash := make(map[domain.ContentHash]domain.SessionDoc, len(docs))
	for _, doc := range docs {
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			return err
		}
		if _, duplicate := docByHash[doc.Hash]; duplicate {
			return fmt.Errorf("%w: duplicate pulled doc %s", domain.ErrHashMismatch, doc.Hash)
		}
		docByHash[doc.Hash] = doc
	}

	snapByID := make(map[domain.ContentHash]domain.Snapshot, len(snaps))
	referencedDocs := make(map[domain.ContentHash]bool, len(snaps))
	for _, snap := range snaps {
		if err := validateSnapshotObject(snap); err != nil {
			return err
		}
		if snap.RepoID != repoID {
			return fmt.Errorf("%w: snapshot %s belongs to repo %s, not %s", domain.ErrHashMismatch, snap.ID, snap.RepoID, repoID)
		}
		if _, duplicate := snapByID[snap.ID]; duplicate {
			return fmt.Errorf("%w: duplicate pulled snapshot %s", domain.ErrHashMismatch, snap.ID)
		}
		snapByID[snap.ID] = snap
		referencedDocs[snap.DocHash] = true

		if _, err := store.GetSnapshot(ctx, snap.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}
	for hash := range docByHash {
		if !referencedDocs[hash] {
			return fmt.Errorf("%w: unreferenced pulled doc %s", domain.ErrHashMismatch, hash)
		}
	}
	for _, snap := range snaps {
		if _, ok := docByHash[snap.DocHash]; ok {
			continue
		}
		doc, err := store.GetDoc(ctx, snap.DocHash)
		if err != nil {
			return fmt.Errorf("%w: snapshot %s has no verified doc %s", domain.ErrHashMismatch, snap.ID, snap.DocHash)
		}
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			return err
		}
	}

	getSnapshot := func(id domain.ContentHash) (domain.Snapshot, error) {
		if snap, ok := snapByID[id]; ok {
			return snap, nil
		}
		snap, err := store.GetSnapshot(ctx, id)
		if err != nil {
			return domain.Snapshot{}, err
		}
		if err := validateSnapshotObject(snap); err != nil {
			return domain.Snapshot{}, err
		}
		return snap, nil
	}
	state := make(map[domain.ContentHash]uint8, len(snaps))
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: pulled snapshot graph cycle at %s", domain.ErrHashMismatch, id)
		case 2:
			return nil
		}
		snap, err := getSnapshot(id)
		if err != nil {
			return fmt.Errorf("%w: missing pulled parent %s", domain.ErrHashMismatch, id)
		}
		state[id] = 1
		seenParents := map[domain.ContentHash]bool{}
		for _, parent := range append(append([]domain.ContentHash{}, snap.Parents...), snap.GraftParents...) {
			if parent == id || seenParents[parent] {
				return fmt.Errorf("%w: invalid parent edge %s -> %s", domain.ErrHashMismatch, id, parent)
			}
			seenParents[parent] = true
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range snapByID {
		if err := visit(id); err != nil {
			return err
		}
	}

	branchRefs := map[string]bool{}
	seenRefs := map[string]bool{}
	for _, ref := range refs {
		if err := domain.ValidateRef(ref); err != nil {
			return err
		}
		if ref.RepoID != repoID {
			return fmt.Errorf("%w: ref belongs to another repo", domain.ErrHashMismatch)
		}
		key := string(ref.Kind) + "\x00" + ref.Name
		if seenRefs[key] {
			return fmt.Errorf("%w: duplicate pulled ref %s/%s", domain.ErrHashMismatch, ref.Kind, ref.Name)
		}
		seenRefs[key] = true
		if ref.Kind == domain.RefBranch {
			branchRefs[ref.Name] = true
		}
		if ref.Target != "" {
			if _, err := getSnapshot(ref.Target); err != nil {
				return fmt.Errorf("%w: ref %s/%s targets missing snapshot", domain.ErrHashMismatch, ref.Kind, ref.Name)
			}
		}
	}
	for _, ref := range refs {
		if ref.Kind != domain.RefHEAD || ref.Symbolic == "" {
			continue
		}
		branch := ref.Symbolic
		if len(branch) > len("refs/heads/") && branch[:len("refs/heads/")] == "refs/heads/" {
			branch = branch[len("refs/heads/"):]
		}
		if branchRefs[branch] {
			continue
		}
		if _, err := store.GetRef(ctx, repoID, domain.RefBranch, branch); err != nil {
			return fmt.Errorf("%w: symbolic HEAD targets missing branch %s", domain.ErrHashMismatch, branch)
		}
	}
	return nil
}

// pushSettingsObjects uploads every settings object before a snapshot can
// publish its hash. It is shared by normal, pending, and unsync pushes so the
// server can enforce the same referential-integrity contract on every path.
func (s *SyncRepoService) pushSettingsObjects(ctx context.Context, repoID domain.ContentHash, snaps []domain.Snapshot) error {
	pushedSet := map[domain.ContentHash]bool{}
	for _, snap := range snaps {
		for _, h := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
			if h == "" || pushedSet[h] {
				continue
			}
			pushedSet[h] = true
			b, err := s.store.GetSettingsObject(ctx, h)
			if err != nil {
				return err
			}
			if err := s.remote.PushSettingsObject(ctx, repoID, h, b); err != nil {
				return err
			}
		}
	}
	return nil
}

// Push uploads local snapshots/docs/refs to the central server (sync protocol).
func (s *SyncRepoService) Push(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	// repo metadata (remote URL/workspace binding) is a prerequisite for object uploads. Server fail-closed on unbound
	// repos, so avoiding registration failures leads to 404/403 errors during subsequent negotiate, as if bypassing
	// permission/identity validation. Cwd push must interpret the current repo and register successfully before transitioning to object transmission.
	repoRoot := "" // promotion queue flush root (accurate for subdirectory pushes as well)
	if in.Cwd != "" {
		repo, rerr := s.gitCtx.CurrentRepo(ctx, in.Cwd)
		if rerr != nil {
			return inbound.SyncOutput{}, rerr
		}
		repoRoot = repo.LocalPath
		if _, rerr := s.remote.RegisterRepo(ctx, repo); rerr != nil {
			return inbound.SyncOutput{}, rerr
		}
	}
	man, err := s.store.Manifest(ctx, repoID)
	if err != nil {
		return inbound.SyncOutput{}, err
	}

	snaps, err := s.collectSnapshots(ctx, repoID, man)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	pushSnaps, pushDocs, err := s.selectPushObjects(ctx, repoID, snaps)
	if err != nil {
		return inbound.SyncOutput{}, err
	}

	var refs []domain.Ref
	for _, r := range man.Refs {
		r.RepoID = repoID
		refs = append(refs, r)
	}

	if err := s.pushSettingsObjects(ctx, repoID, snaps); err != nil {
		return inbound.SyncOutput{}, err
	}

	// Memory attaches through a causal CAS endpoint after snapshot creation.
	// Read the remote pointer catalog before reconstructing any large local
	// digest. The overwhelmingly common equal-pointer case needs no memory body
	// read at all; only changed attachments pay for causal-chain validation.
	var memorySnapshots []domain.Snapshot
	for _, snap := range snaps {
		if snap.MemoryHash != "" {
			memorySnapshots = append(memorySnapshots, snap)
		}
	}
	var remoteMemoryAttachments map[domain.ContentHash]domain.ContentHash
	remoteMemoryAhead := map[domain.ContentHash]bool{}
	if len(memorySnapshots) > 0 {
		remoteManifest, err := s.remote.RemoteManifest(ctx, repoID)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if remoteManifest.RepoID != "" && remoteManifest.RepoID != repoID {
			return inbound.SyncOutput{}, domain.ErrHashMismatch
		}
		for snapshotID, memoryHash := range remoteManifest.MemoryAttachments {
			if err := domain.ValidateContentHash(snapshotID); err != nil {
				return inbound.SyncOutput{}, err
			}
			if err := domain.ValidateContentHash(memoryHash); err != nil {
				return inbound.SyncOutput{}, err
			}
		}
		remoteMemoryAttachments = remoteManifest.MemoryAttachments
	}

	// Validate every changed local chain before publishing raw objects so a
	// missing or corrupt predecessor cannot leave a partially advanced ref.
	var memoryPlans []memoryPushPlan
	for _, snap := range memorySnapshots {
		if remoteMemoryAttachments != nil && remoteMemoryAttachments[snap.ID] == snap.MemoryHash {
			continue
		}
		plan, err := s.localMemoryPushPlan(ctx, snap.ID, snap.MemoryHash)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if remoteMemoryAttachments != nil {
			remoteHash := remoteMemoryAttachments[plan.snapshotID]
			ahead, err := s.preflightKnownRemoteMemory(ctx, repoID, plan, remoteHash)
			if err != nil {
				return inbound.SyncOutput{}, err
			}
			remoteMemoryAhead[plan.snapshotID] = ahead
		}
		memoryPlans = append(memoryPlans, plan)
	}

	// Publish order is object → graft overlay → ref. New snapshots arrive without server-owned graft metadata. Publishing refs first would make sibling-session histories that depend on the queued graft appear non-fast-forward. Call RemoteSync.Push twice to make the protocol boundary explicit: objects first, refs last.
	if len(pushSnaps) > 0 || len(pushDocs) > 0 {
		if err := s.remote.Push(ctx, repoID, pushSnaps, pushDocs, nil, false, false); err != nil {
			return inbound.SyncOutput{}, err
		}
	}
	for _, plan := range memoryPlans {
		// Another terminal or machine already advanced this snapshot's memory.
		// Do not rewind it, and do not let one stale historical attachment block
		// publishing otherwise independent snapshots and refs.
		if remoteMemoryAhead[plan.snapshotID] {
			continue
		}
		var err error
		if remoteMemoryAttachments != nil {
			err = s.pushMemoryPlanFromKnown(ctx, repoID, plan, remoteMemoryAttachments[plan.snapshotID])
		} else {
			err = s.pushMemoryPlan(ctx, repoID, plan)
		}
		if err != nil {
			return inbound.SyncOutput{}, err
		}
	}
	// Message promotion is a best-effort display metadata, but graft is a prerequisite for the reachability of the ref to be published. If stale/cycle adjustment occurs, the current ref publish is interrupted, and it retries after confirming the local/server state.
	if repoRoot != "" {
		s.flushPromotions(ctx, repoRoot, repoID)
		if err := s.flushGrafts(ctx, repoRoot, repoID); err != nil {
			return inbound.SyncOutput{}, err
		}
	}
	if err := s.remote.Push(ctx, repoID, nil, nil, refs, in.Force, in.Append); err != nil {
		return inbound.SyncOutput{}, err
	}
	// Pending reconciliation (safety net): deletes the remote pending of sessions that have been resolved locally (i.e., committed), allowing the server to follow the local state (best-effort·idempotent).
	if locals, lerr := s.store.ListPendings(ctx, repoID); lerr == nil {
		alive := map[string]bool{}
		for _, p := range locals {
			alive[p.SessionID] = true
		}
		cleaned := map[string]bool{}
		for _, snap := range snaps {
			if sid := snap.SessionID; sid != "" && !alive[sid] && !cleaned[sid] {
				cleaned[sid] = true
				_ = s.remote.DeletePendingRemote(ctx, repoID, sid)
			}
		}
	}
	// Resolve unsync: removes the "push pending" pointer for the branch whose ref has advanced (idempotent).
	for _, r := range refs {
		if r.Kind == domain.RefBranch {
			_ = s.remote.DeleteUnsyncRemote(ctx, repoID, r.Name)
		}
	}
	return inbound.SyncOutput{Pushed: len(pushSnaps), NewRefs: refs}, nil
}

// flushPromotions flushes the <repoRoot>/.cxt/promotions.json queue to the server, removing successful items.
func (s *SyncRepoService) flushPromotions(ctx context.Context, repoRoot, repoID string) {
	rel := ".cxt/promotions.json"
	b, err := providerfs.ReadRepoFile(repoRoot, rel)
	if err != nil {
		return
	}
	m := map[string]string{}
	if json.Unmarshal(b, &m) != nil || len(m) == 0 {
		return
	}
	changed := false
	for id, msg := range m {
		if perr := s.remote.PromoteSnapshotMessage(ctx, repoID, domain.ContentHash(id), msg); perr == nil {
			delete(m, id)
			changed = true
		}
	}
	if !changed {
		return
	}
	if len(m) == 0 {
		_ = providerfs.RemoveRepoFile(repoRoot, rel)
		return
	}
	if nb, merr := json.Marshal(m); merr == nil {
		_ = providerfs.WriteRepoFileAtomic(repoRoot, rel, nb, 0o644)
	}
}

type statusCodeError interface {
	StatusCode() int
}

func terminalGraftConflict(err error) bool {
	var statusErr statusCodeError
	return errors.As(err, &statusErr) && statusErr.StatusCode() == 409
}

func sameGraftQueueEvent(a, b graftQueueEvent) bool {
	if a.Snapshot != b.Snapshot || a.ExpectedSeq != b.ExpectedSeq || a.Legacy != b.Legacy || len(a.Parents) != len(b.Parents) {
		return false
	}
	for i := range a.Parents {
		if a.Parents[i] != b.Parents[i] {
			return false
		}
	}
	return true
}

// flushGrafts flushes ordered CAS events in the queue in sequence. It does not lock files during network calls and only checks and removes the same event from the current queue after completion, preventing the tail of concurrent Save operations from being overwritten.
//
// A 409 (stale/cycle) error is resolved by reloading the server snapshot and adjusting the local optimistic register. Events following the same snapshot are all rejected, so they are also removed. If an edge dependent on the removed ref is to be published in this push, ErrSyncConflict is returned. If any of the remote GET, local adjustment, or queue update fails, the event is preserved, and an error is returned.
func (s *SyncRepoService) flushGrafts(ctx context.Context, repoRoot, repoID string) error {
	rel := ".cxt/grafts.json"
	unlock, lockErr := lockGraftQueue(repoRoot)
	if lockErr != nil {
		return lockErr
	}
	state, err := readGraftQueue(repoRoot, rel)
	unlock()
	if err != nil {
		return err
	}
	if len(state.Events) == 0 {
		return nil
	}

	for _, event := range state.Events {
		ps := make([]domain.ContentHash, 0, len(event.Parents))
		for _, p := range event.Parents {
			ps = append(ps, domain.ContentHash(p))
		}
		gerr := s.remote.GraftSnapshotParents(ctx, repoID, domain.ContentHash(event.Snapshot), ps, event.ExpectedSeq)
		if gerr != nil && !terminalGraftConflict(gerr) {
			return fmt.Errorf("graft queue propagation failure (%s): %w", event.Snapshot, gerr)
		}

		conflicted := gerr != nil
		var authoritative *domain.Snapshot
		// 409 adopts the server LWW source of truth. Even if the local map queue has not advanced, a single GET is required to confirm the local register.
		if conflicted || event.Legacy {
			if s.store == nil {
				return fmt.Errorf("%w: graft rebase failed: no local repository", domain.ErrSyncConflict)
			}
			remoteSnap, rerr := s.remote.GetSnapshotRemote(ctx, repoID, domain.ContentHash(event.Snapshot))
			if rerr != nil {
				return fmt.Errorf("graft conflict source retrieval failed(%s): %w", event.Snapshot, rerr)
			}
			authoritative = &remoteSnap
		}

		unlock, lockErr = lockGraftQueue(repoRoot)
		if lockErr != nil {
			return lockErr
		}
		current, rerr := readGraftQueue(repoRoot, rel)
		if rerr != nil {
			unlock()
			return rerr
		}
		idx := -1
		for i, queued := range current.Events {
			if sameGraftQueueEvent(queued, event) {
				idx = i
				break
			}
		}
		if idx < 0 {
			// Another flusher has already completed. That flusher was responsible for the local rebase,
			// so we do not overwrite the stale GET with the latest local state here.
			unlock()
			continue
		}
		if idx != 0 {
			unlock()
			return fmt.Errorf("%w: graft queue order changed", domain.ErrSyncConflict)
		}

		if authoritative != nil {
			if rerr := s.store.ReconcileGraftState(ctx, *authoritative); rerr != nil {
				unlock()
				return fmt.Errorf("graft conflict local rebase failed(%s): %w", event.Snapshot, rerr)
			}
			if conflicted {
				// This event's add, piled up on the same snapshot, also originated from an optimistic state.
				// Remove all to block the re-emergence of the superseded edge.
				kept := current.Events[:0]
				for _, queued := range current.Events {
					if queued.Snapshot != event.Snapshot {
						kept = append(kept, queued)
					}
				}
				current.Events = kept
			} else {
				// Legacy success is confirmed by this event only. The order of other snapshots is preserved.
				current.Events = current.Events[1:]
			}
		} else {
			current.Events = current.Events[1:]
		}
		if len(current.Events) == 0 {
			rerr = providerfs.RemoveRepoFile(repoRoot, rel)
		} else {
			rerr = writeGraftQueue(repoRoot, rel, current)
		}
		unlock()
		if rerr != nil {
			return rerr
		}
		if conflicted {
			return fmt.Errorf("%w: server join superseded local graft; retry pull then push", domain.ErrSyncConflict)
		}
	}
	return nil
}

// ResolveRemoteBranch fetches the remote branch ref for web fork connections.
// If the target snapshot object is not local, it prepares a fetch-only pull to allow
// the caller to immediately create a local fork ref. If not found on the remote, returns domain.ErrNotFound.
func (s *SyncRepoService) ResolveRemoteBranch(ctx context.Context, in inbound.SyncInput, branch string) (domain.Ref, error) {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return domain.Ref{}, err
	}
	man, err := s.remote.RemoteManifest(ctx, repoID)
	if err != nil {
		return domain.Ref{}, err
	}
	for _, r := range man.Refs {
		if r.Kind != domain.RefBranch || r.Name != branch || r.Target == "" {
			continue
		}
		if _, gerr := s.store.GetSnapshot(ctx, r.Target); gerr != nil {
			if _, perr := s.Pull(ctx, inbound.SyncInput{RepoID: repoID, Cwd: in.Cwd, FetchOnly: true}); perr != nil {
				return domain.Ref{}, perr
			}
			if _, gerr := s.store.GetSnapshot(ctx, r.Target); gerr != nil {
				return domain.Ref{}, gerr
			}
		}
		return r, nil
	}
	return domain.Ref{}, domain.ErrNotFound
}

const maxAppendReconcileSnapshots = 256

// reconcileAppendedPath adopts the authoritative remote graft registers on one
// path from target to ancestor. appendDiverged can attach its overlay to an
// ancestor of target rather than target itself, so re-reading only target is
// insufficient.
//
// Remote metadata is collected and checked against immutable local metadata
// before any graft register is replaced. The traversal is bounded because this
// path runs from a Git hook; a later full pull remains the recovery path for an
// unusually large graph.
func (s *SyncRepoService) reconcileAppendedPath(
	ctx context.Context,
	repoID string,
	target, ancestor domain.ContentHash,
) (bool, error) {
	if ancestor == "" || ancestor == target {
		return true, nil
	}

	queue := []domain.ContentHash{target}
	discovered := map[domain.ContentHash]bool{target: true}
	// towardTarget[parent] is the child from which the parent was discovered.
	towardTarget := make(map[domain.ContentHash]domain.ContentHash)
	remoteByID := make(map[domain.ContentHash]domain.Snapshot)
	found := false

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == ancestor {
			found = true
			break
		}
		if len(remoteByID) >= maxAppendReconcileSnapshots {
			return false, fmt.Errorf(
				"%w: authoritative append path exceeded %d snapshots; run cxt pull",
				domain.ErrSyncConflict,
				maxAppendReconcileSnapshots,
			)
		}

		snap, err := s.remote.GetSnapshotRemote(ctx, repoID, id)
		if err != nil {
			return false, fmt.Errorf("read authoritative append snapshot %s: %w", id, err)
		}
		if err := validateSnapshotObject(snap); err != nil {
			return false, err
		}
		if snap.ID != id || snap.RepoID != repoID {
			return false, domain.ErrHashMismatch
		}
		remoteByID[id] = snap

		for _, parent := range snap.ReachabilityParents() {
			if discovered[parent] {
				continue
			}
			discovered[parent] = true
			towardTarget[parent] = id
			queue = append(queue, parent)
		}
	}
	if !found {
		return false, nil
	}

	// Reconstruct only the proven path. Side branches visited by the bounded
	// search are not required for this ref movement and are left for normal pull.
	type pathStep struct {
		snapshot domain.Snapshot
		parent   domain.ContentHash
	}
	path := make([]pathStep, 0)
	for id := ancestor; id != target; {
		child, ok := towardTarget[id]
		if !ok {
			return false, domain.ErrHashMismatch
		}
		snap, ok := remoteByID[child]
		if !ok {
			return false, domain.ErrHashMismatch
		}
		path = append(path, pathStep{snapshot: snap, parent: id})
		id = child
	}

	// Preflight all immutable metadata before replacing any local graft register.
	localByID := make(map[domain.ContentHash]domain.Snapshot, len(path))
	for _, step := range path {
		remoteSnap := step.snapshot
		localSnap, err := s.store.GetSnapshot(ctx, remoteSnap.ID)
		if err != nil {
			return false, err
		}
		if err := validateSnapshotObject(localSnap); err != nil {
			return false, err
		}
		if localSnap.RepoID != repoID || !sameParents(localSnap.Parents, remoteSnap.Parents) {
			return false, domain.ErrHashMismatch
		}
		localByID[localSnap.ID] = localSnap
	}
	for _, step := range path {
		localSnap := localByID[step.snapshot.ID]
		edgePresent := false
		for _, parent := range localSnap.ReachabilityParents() {
			if parent == step.parent {
				edgePresent = true
				break
			}
		}
		if edgePresent {
			continue
		}
		if err := s.store.ReconcileGraftState(ctx, step.snapshot); err != nil {
			return false, fmt.Errorf("reconcile authoritative append snapshot %s: %w", step.snapshot.ID, err)
		}
	}
	return true, nil
}

// AppendBranch appends the server branch ref to the target (lossless graft) without modifying the commit history.
// It is idempotent, conflict-free, and does not interfere with branch protection policies (e.g., --force).
func (s *SyncRepoService) AppendBranch(ctx context.Context, in inbound.SyncInput, branch string, target domain.ContentHash) error {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return err
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: branch, RepoID: repoID, Target: target}
	if err := s.remote.UpdateRefRemote(ctx, repoID, ref, true); err != nil {
		return err
	}

	cur, err := s.store.GetRef(ctx, repoID, domain.RefBranch, branch)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return s.store.PutRef(ctx, ref)
	case err != nil:
		return err
	case cur.Target == "":
		return s.store.PutRef(ctx, ref)
	case cur.Target == target:
		return nil
	case s.isAncestor(ctx, cur.Target, target):
		return s.store.PutRef(ctx, ref)
	}

	// A diverged server append may have added its graft to an ancestor of target.
	// Re-read that narrow authoritative path before deciding whether the local
	// branch can safely fast-forward. If the local branch is genuinely ahead,
	// the path will not reach it and its ref is preserved.
	reached, err := s.reconcileAppendedPath(ctx, repoID, target, cur.Target)
	if err != nil {
		return err
	}
	if !reached {
		return nil
	}
	if !s.isAncestor(ctx, cur.Target, target) {
		return fmt.Errorf("%w: authoritative append path did not converge", domain.ErrSyncConflict)
	}
	return s.store.PutRef(ctx, ref)
}

// collectSnapshots gathers push target metadata for Push/Shadow Sync without
// opening cumulative document bodies.
// Stash is local-only (like git), so it is excluded, but the determination is based on ref reachability, not labels.
// Content-hash deduplication ensures that if the same session content is stored in both stash and commits, one "(stash)" label object can be part of the commit history.
// Removing the label alone can lead to parent loss (fsck corruption) and permanent ref advancement blockage (stash-dedup trap).
func (s *SyncRepoService) collectSnapshots(ctx context.Context, repoID string, man domain.Manifest) ([]domain.Snapshot, error) {
	byID := make(map[domain.ContentHash]domain.Snapshot, len(man.SnapshotIndex))
	for _, id := range man.SnapshotIndex {
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		byID[id] = snap
	}
	// Reachable set from ref (branch/session/tag) — session is an internal pointer to partial merge remnants.
	// parents ∪ graft_parents (reachability single rule).
	reachable := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{}
	for _, r := range man.Refs {
		if (r.Kind == domain.RefBranch || r.Kind == domain.RefSession || r.Kind == domain.RefTag) && r.Target != "" {
			stack = append(stack, r.Target)
		}
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || reachable[cur] {
			continue
		}
		snap, ok := byID[cur]
		if !ok {
			continue
		}
		reachable[cur] = true
		stack = append(stack, snap.ReachabilityParents()...)
	}
	var snaps []domain.Snapshot
	for _, id := range man.SnapshotIndex {
		snap := byID[id]
		if snap.Branch == domain.StashBranchLabel && !reachable[id] {
			continue // Exclude pure stashes (unreachable from any ref)
		}
		snap.RepoID = repoID // Normalize to current origin-derived repoID (remote redirection)
		snaps = append(snaps, snap)
	}
	return snaps, nil
}

// selectPushObjects asks the server which independent snapshot/doc objects it
// lacks before opening any cumulative SessionDoc body. A server can retain a
// snapshot while losing its doc (or vice versa), so manifest snapshot indexes
// are not a sufficient repair proof. Optional capability fallback preserves
// compatibility with non-HTTP remotes and older adapters.
func (s *SyncRepoService) selectPushObjects(ctx context.Context, repoID string, snaps []domain.Snapshot) ([]domain.Snapshot, []domain.SessionDoc, error) {
	negotiator, ok := s.remote.(outbound.PushObjectNegotiator)
	if !ok {
		docs, err := s.loadPushDocs(ctx, snaps, nil)
		return snaps, docs, err
	}

	snapshotHaves := make([]domain.ContentHash, 0, len(snaps))
	docHaves := make([]domain.ContentHash, 0, len(snaps))
	seenDocs := map[domain.ContentHash]bool{}
	for _, snap := range snaps {
		snapshotHaves = append(snapshotHaves, snap.ID)
		if snap.DocHash != "" && !seenDocs[snap.DocHash] {
			seenDocs[snap.DocHash] = true
			docHaves = append(docHaves, snap.DocHash)
		}
	}
	wants, err := negotiator.NegotiatePushObjects(ctx, repoID, snapshotHaves, docHaves)
	if err != nil {
		return nil, nil, err
	}
	wantSnaps, err := negotiatedWantSet(snapshotHaves, wants.Snapshots)
	if err != nil {
		return nil, nil, err
	}
	wantDocs, err := negotiatedWantSet(docHaves, wants.Docs)
	if err != nil {
		return nil, nil, err
	}

	pushSnaps := make([]domain.Snapshot, 0, len(wantSnaps))
	for _, snap := range snaps {
		if wantSnaps[snap.ID] {
			pushSnaps = append(pushSnaps, snap)
		}
	}
	pushDocs, err := s.loadPushDocs(ctx, snaps, wantDocs)
	if err != nil {
		return nil, nil, err
	}
	return pushSnaps, pushDocs, nil
}

func negotiatedWantSet(haves, wants []domain.ContentHash) (map[domain.ContentHash]bool, error) {
	offered := make(map[domain.ContentHash]bool, len(haves))
	for _, hash := range haves {
		offered[hash] = true
	}
	out := make(map[domain.ContentHash]bool, len(wants))
	for _, hash := range wants {
		if err := domain.ValidateContentHash(hash); err != nil {
			return nil, err
		}
		if !offered[hash] || out[hash] {
			return nil, domain.ErrHashMismatch
		}
		out[hash] = true
	}
	return out, nil
}

func (s *SyncRepoService) loadPushDocs(ctx context.Context, snaps []domain.Snapshot, wanted map[domain.ContentHash]bool) ([]domain.SessionDoc, error) {
	var docs []domain.SessionDoc
	seen := map[domain.ContentHash]bool{}
	for _, snap := range snaps {
		hash := snap.DocHash
		if hash == "" || seen[hash] || (wanted != nil && !wanted[hash]) {
			continue
		}
		doc, err := s.store.GetDoc(ctx, hash)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
		seen[hash] = true
	}
	return docs, nil
}

// SyncPendings reflects in-progress context pointers to the server (detached helper path).
// After deleting remote pending from resolveSessions (commit resolution propagation), it pushes each local pending's snapshot/doc objects as objects-only and upserts the pointers.
// Branch refs are not modified (hook path does not move server refs — same spirit as capture path).
func (s *SyncRepoService) SyncPendings(ctx context.Context, in inbound.SyncInput, resolveSessions []string) (int, error) {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return 0, err
	}
	for _, sid := range resolveSessions {
		if sid != "" {
			_ = s.remote.DeletePendingRemote(ctx, repoID, sid) // best-effort·idempotent
		}
	}
	pendings, err := s.store.ListPendings(ctx, repoID)
	if err != nil {
		return 0, err
	}
	man, merr := s.store.Manifest(ctx, repoID)
	remoteMan, rerr := s.remote.RemoteManifest(ctx, repoID)
	// Known remote set: reachability walk from remote branch/session/tag ref to local objects.
	// (If remote target is not in local, only what is available — renegotiate dedup is harmless.)
	remoteKnown := map[domain.ContentHash]bool{}
	if rerr == nil {
		stack := []domain.ContentHash{}
		for _, r := range remoteMan.Refs {
			if (r.Kind == domain.RefBranch || r.Kind == domain.RefSession || r.Kind == domain.RefTag) && r.Target != "" {
				stack = append(stack, r.Target)
			}
		}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if cur == "" || remoteKnown[cur] {
				continue
			}
			remoteKnown[cur] = true
			if sn, gerr := s.store.GetSnapshot(ctx, cur); gerr == nil {
				stack = append(stack, sn.ReachabilityParents()...)
			}
		}
	}
	// Chain mode gate: if remote manifest cannot be read or remote has no refs (new repo first push), remoteKnown is empty and walking the local entire history happens — hook path can load up to several GB of docs into memory (weekly 0.9GB empirically) so force a single target push.
	chainMode := rerr == nil && len(remoteKnown) > 0
	synced := 0
	// pending pushes the target snapshot "and its push ancestor chain" — pushing the target alone would result in server rejection (after hardening) or orphaned in the graph (actual case). renegotiate dedupes known ancestors so they are not retransmitted. If chain assembly is incorrect (ancestor snapshot/doc local failure, walk limit exceeded, push rejected), fallback to single target push — no failure state is worse than the previous (single push).
	const maxPendingChain = 200 // walk limit — large backlog defense (same spirit as appendMergedContexts)
	for _, p := range pendings {
		targetSnap, gerr := s.store.GetSnapshot(ctx, p.Target)
		if gerr != nil {
			continue
		}
		targetDoc, gerr := s.store.GetDoc(ctx, targetSnap.DocHash)
		if gerr != nil {
			continue // target itself is missing — nothing to send (existing behavior)
		}
		targetSnap.RepoID = repoID
		p.RepoID = repoID

		var chainSnaps []domain.Snapshot
		var chainDocs []domain.SessionDoc
		if chainMode {
			seenSnap := map[domain.ContentHash]bool{}
			seenDoc := map[domain.ContentHash]bool{}
			stack := []domain.ContentHash{p.Target}
			for len(stack) > 0 && len(chainSnaps) < maxPendingChain {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if cur == "" || seenSnap[cur] || remoteKnown[cur] {
					continue
				}
				snap, serr := s.store.GetSnapshot(ctx, cur)
				if serr != nil {
					continue // local missing ancestor (GC etc.) — only what is available (fallback on failure)
				}
				var doc domain.SessionDoc
				if snap.DocHash != "" && !seenDoc[snap.DocHash] {
					var derr error
					if doc, derr = s.store.GetDoc(ctx, snap.DocHash); derr != nil {
						continue // snapshot without doc is push impossible — proceed without ancestor (fallback safety net)
					}
				}
				seenSnap[cur] = true
				snap.RepoID = repoID
				chainSnaps = append(chainSnaps, snap)
				if snap.DocHash != "" && !seenDoc[snap.DocHash] {
					seenDoc[snap.DocHash] = true
					chainDocs = append(chainDocs, doc)
				}
				stack = append(stack, snap.ReachabilityParents()...)
			}
		}

		pushed := false
		if len(chainSnaps) > 0 {
			if perr := s.pushSettingsObjects(ctx, repoID, chainSnaps); perr == nil {
				if perr := s.remote.Push(ctx, repoID, chainSnaps, chainDocs, nil, false, false); perr == nil {
					pushed = true
				}
			}
		}
		if !pushed {
			// Fallback: Target single push (original behavior) — Keeps pending in fallback state even if chain assembly is impossible or rejected by the server.
			if perr := s.pushSettingsObjects(ctx, repoID, []domain.Snapshot{targetSnap}); perr != nil {
				continue
			}
			if perr := s.remote.Push(ctx, repoID, []domain.Snapshot{targetSnap}, []domain.SessionDoc{targetDoc}, nil, false, false); perr != nil {
				continue
			}
		}
		if perr := s.remote.PushPending(ctx, repoID, p); perr == nil {
			synced++
		}
	}
	// Push unsync reconciliation: If local branch ref differs from server and server is my ancestor (meaning I am ahead), create a shadow push (ref unchanged — negotiate dedup) and update the pointer. If the same or behind, release my pointer.
	if merr == nil && rerr == nil {
		remoteRef := map[string]domain.ContentHash{}
		for _, r := range remoteMan.Refs {
			if r.Kind == domain.RefBranch {
				remoteRef[r.Name] = r.Target
			}
		}
		var ahead []domain.Ref
		for _, r := range man.Refs {
			if r.Kind != domain.RefBranch || r.Target == "" {
				continue
			}
			if r.Target == remoteRef[r.Name] || s.isAncestor(ctx, r.Target, remoteRef[r.Name]) {
				_ = s.remote.DeleteUnsyncRemote(ctx, repoID, r.Name) // Synchronize/behind — Resolve (idempotent)
				continue
			}
			ahead = append(ahead, r)
		}
		if len(ahead) > 0 {
			if snaps, cerr := s.collectSnapshots(ctx, repoID, man); cerr == nil {
				if serr := s.pushSettingsObjects(ctx, repoID, snaps); serr == nil {
					pushSnaps, pushDocs, perr := s.selectPushObjects(ctx, repoID, snaps)
					objectsReady := perr == nil
					if objectsReady && (len(pushSnaps) > 0 || len(pushDocs) > 0) {
						objectsReady = s.remote.Push(ctx, repoID, pushSnaps, pushDocs, nil, false, false) == nil
					}
					if objectsReady {
						for _, r := range ahead {
							if uerr := s.remote.PushUnsync(ctx, repoID, domain.Unsync{
								RepoID: repoID, Branch: r.Name, Target: r.Target, UpdatedAt: time.Now().UTC(),
							}); uerr == nil {
								synced++
							}
						}
					}
				}
			}
		}
	}
	return synced, nil
}

// isAncestor determines if anc is an ancestor (or the same/empty) of desc using the local snapshot DAG. Used for fast-forward determination in pull (remote snapshots are stored before ref merge).
// Reachability is the same as server (engine.parentsOf) — Parents ∪ GraftParents — Graft (diverged append) movement only reaches old head via overlay edges, so missing this can cause server to classify ref movement as a conflict, permanently blocking pull.
func (s *SyncRepoService) isAncestor(ctx context.Context, anc, desc domain.ContentHash) bool {
	if anc == "" || anc == desc {
		return true
	}
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{desc}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == anc {
			return true
		}
		if seen[cur] || cur == "" {
			continue
		}
		seen[cur] = true
		snap, err := s.store.GetSnapshot(ctx, cur)
		if err != nil {
			continue
		}
		queue = append(queue, snap.ReachabilityParents()...)
	}
	return false
}

// preparePullSnapshotStates overlays validated remote projection tokens on the
// local manifest for snapshots whose local metadata intentionally remains
// ahead. The cursor is only a negotiation hint: the current local state is the
// guard, and any local mutation makes the cached remote token ineligible.
func preparePullSnapshotStates(
	ctx context.Context,
	store outbound.SessionStore,
	repoID string,
	local map[domain.ContentHash]domain.ContentHash,
) (
	map[domain.ContentHash]domain.ContentHash,
	outbound.RemoteSnapshotStateCursorStore,
	map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry,
) {
	var advertised map[domain.ContentHash]domain.ContentHash
	if local != nil {
		advertised = make(map[domain.ContentHash]domain.ContentHash, len(local))
		for id, state := range local {
			advertised[id] = state
		}
	}

	cursorStore, ok := store.(outbound.RemoteSnapshotStateCursorStore)
	if !ok {
		return advertised, nil, nil
	}
	active := map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{}
	if local == nil {
		return advertised, cursorStore, active
	}
	loaded, err := cursorStore.LoadRemoteSnapshotStateCursor(ctx, repoID)
	if err != nil {
		// This sidecar is a disposable performance hint. Corruption, an old
		// format, or an unavailable cache must degrade to the normal pull.
		return advertised, cursorStore, active
	}
	for id, entry := range loaded {
		current, exists := local[id]
		if !exists || current != entry.LocalState {
			continue
		}
		active[id] = entry
		advertised[id] = entry.RemoteState
	}
	return advertised, cursorStore, active
}

// updateRemoteSnapshotStateCursor records only remote projections that were
// returned, validated, and successfully reconciled locally. It runs after all
// snapshot and memory writes. Cache failures never change pull correctness.
func (s *SyncRepoService) updateRemoteSnapshotStateCursor(
	ctx context.Context,
	repoID string,
	cursorStore outbound.RemoteSnapshotStateCursorStore,
	entries map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry,
	remoteSnapshots []domain.Snapshot,
) {
	if cursorStore == nil {
		return
	}
	if entries == nil {
		entries = map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{}
	}
	for _, remote := range remoteSnapshots {
		delete(entries, remote.ID)
		remoteState, err := domain.SnapshotStateHash(remote)
		if err != nil {
			continue
		}
		local, err := s.store.GetSnapshot(ctx, remote.ID)
		if err != nil {
			continue
		}
		localState, err := domain.SnapshotStateHash(local)
		if err != nil {
			continue
		}
		if localState != remoteState {
			entries[remote.ID] = domain.RemoteSnapshotStateCursorEntry{
				LocalState:  localState,
				RemoteState: remoteState,
			}
		}
	}
	_ = cursorStore.SaveRemoteSnapshotStateCursor(ctx, repoID, entries)
}

// Pull merges the snapshot/doc/ref from the central server into the local repository (fast-forward first).
func (s *SyncRepoService) Pull(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return inbound.SyncOutput{}, err
	}
	// Delta pull advertises the verified local metadata and document inventory.
	// Snapshot IDs equal DocHash, so document existence needs only a cheap stat;
	// opening every cumulative session body or re-reading snapshots a second time
	// would make the post-merge hook O(total history).
	var snapshotStates map[domain.ContentHash]domain.ContentHash
	var docHaves []domain.ContentHash
	if man, merr := s.store.Manifest(ctx, repoID); merr == nil {
		snapshotStates = man.SnapshotStates
		for _, id := range man.SnapshotIndex {
			if ok, herr := s.store.HasDoc(ctx, id); herr == nil && ok {
				docHaves = append(docHaves, id)
			}
		}
	}
	advertisedSnapshotStates, cursorStore, cursorEntries := preparePullSnapshotStates(ctx, s.store, repoID, snapshotStates)
	snaps, docs, refs, err := s.remote.Pull(ctx, repoID, advertisedSnapshotStates, docHaves)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	if err := validatePullBatch(ctx, s.store, repoID, snaps, docs, refs); err != nil {
		return inbound.SyncOutput{}, err
	}

	// The settings object pointed to by the snapshot is validated before kind/hash storage. An infected remote cannot change the local application path for bundle.Kind or inject another object under the requested hash.
	type settingsWant struct {
		kind string
		hash domain.ContentHash
	}
	wantsByHash := map[domain.ContentHash]string{}
	for _, snap := range snaps {
		for _, want := range []settingsWant{{"claude", snap.ClaudeSettings}, {"agents", snap.AgentsSettings}, {"codex", snap.CodexSettings}} {
			if want.hash == "" {
				continue
			}
			if previous, ok := wantsByHash[want.hash]; ok && previous != want.kind {
				return inbound.SyncOutput{}, domain.ErrHashMismatch
			}
			wantsByHash[want.hash] = want.kind
		}
	}
	stagedSettings := make(map[domain.ContentHash]domain.SettingsBundle, len(wantsByHash))
	for hash, kind := range wantsByHash {
		local, gerr := s.store.GetSettingsObject(ctx, hash)
		switch {
		case gerr == nil:
			if err := domain.ValidateSettingsBundle(kind, hash, local); err != nil {
				return inbound.SyncOutput{}, err
			}
		case errors.Is(gerr, domain.ErrNotFound):
			bundle, perr := s.remote.PullSettingsObject(ctx, repoID, hash)
			if perr != nil {
				return inbound.SyncOutput{}, perr
			}
			if err := domain.ValidateSettingsBundle(kind, hash, bundle); err != nil {
				return inbound.SyncOutput{}, err
			}
			stagedSettings[hash] = bundle
		default:
			return inbound.SyncOutput{}, gerr
		}
	}
	// Memory pointers are mutable refs over immutable causal digest chains.
	// Stage every object needed to prove a fast-forward before any local write;
	// unrelated roots are a conflict, never an arrival-order winner.
	stagedMemories := map[domain.ContentHash]domain.MemoryDigest{}
	knownMemories := map[domain.ContentHash]domain.MemoryDigest{}
	existingByID := map[domain.ContentHash]domain.Snapshot{}
	memoryAdoptions := map[domain.ContentHash]domain.ContentHash{}
	for _, snap := range snaps {
		existing, existingErr := s.store.GetSnapshot(ctx, snap.ID)
		switch {
		case existingErr == nil:
			existingByID[snap.ID] = existing
		case errors.Is(existingErr, domain.ErrNotFound):
		default:
			return inbound.SyncOutput{}, existingErr
		}
		if snap.MemoryHash != "" {
			local, gerr := s.store.GetMemory(ctx, snap.MemoryHash)
			switch {
			case gerr == nil:
				if err := validateMemoryAttachmentObject(local, snap.MemoryHash, snap.ID); err != nil {
					return inbound.SyncOutput{}, err
				}
				knownMemories[snap.MemoryHash] = local
			case errors.Is(gerr, domain.ErrNotFound):
				digest, perr := s.remote.PullMemory(ctx, repoID, snap.ID)
				if perr != nil {
					return inbound.SyncOutput{}, perr
				}
				if err := validateMemoryAttachmentObject(digest, snap.MemoryHash, snap.ID); err != nil {
					return inbound.SyncOutput{}, err
				}
				stagedMemories[snap.MemoryHash] = digest
			default:
				return inbound.SyncOutput{}, gerr
			}
		}
		// A pointer is usable offline only when its immutable ancestry is present,
		// not merely its tip. This also repairs checkouts produced by older clients
		// that adopted a remote tip while omitting one of its causal parents.
		if snap.MemoryHash != "" && (existingErr != nil || existing.MemoryHash == "" || existing.MemoryHash == snap.MemoryHash) {
			loader := &memoryPullLoader{
				service: s, ctx: ctx, repoID: repoID, snapshotID: snap.ID, staged: stagedMemories, loaded: knownMemories,
			}
			complete, err := memoryAttachmentAncestor(loader, "", snap.MemoryHash)
			if err != nil {
				return inbound.SyncOutput{}, err
			}
			if !complete {
				return inbound.SyncOutput{}, fmt.Errorf("%w: incomplete memory attachment chain for snapshot %s", domain.ErrHashMismatch, snap.ID)
			}
		}
		if existingErr != nil || existing.MemoryHash == snap.MemoryHash {
			continue
		}
		if existing.MemoryHash == "" {
			memoryAdoptions[snap.ID] = ""
			continue
		}
		// A local pointer must always resolve before it can participate in a
		// merge. Fetching it from the server could hide local corruption.
		localCurrent, localErr := s.store.GetMemory(ctx, existing.MemoryHash)
		if localErr != nil {
			return inbound.SyncOutput{}, localErr
		}
		if err := validateMemoryAttachmentObject(localCurrent, existing.MemoryHash, snap.ID); err != nil {
			return inbound.SyncOutput{}, err
		}
		if snap.MemoryHash == "" {
			continue // remote empty is an ancestor of every local chain
		}
		loader := &memoryPullLoader{
			service: s, ctx: ctx, repoID: repoID, snapshotID: snap.ID, staged: stagedMemories, loaded: knownMemories,
		}
		remoteBehind, err := memoryAttachmentAncestor(loader, snap.MemoryHash, existing.MemoryHash)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if remoteBehind {
			continue // local causal descendant remains attached
		}
		localBehind, err := memoryAttachmentAncestor(loader, existing.MemoryHash, snap.MemoryHash)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if !localBehind {
			if in.Force {
				// Explicit recovery: adopt the remote memory ref while retaining the
				// losing immutable local digest object. Raw session data is untouched,
				// so a subsequent memorize can project it again on top of the winner.
				memoryAdoptions[snap.ID] = existing.MemoryHash
				continue
			}
			return inbound.SyncOutput{}, fmt.Errorf(
				"%w: divergent memory attachments for snapshot %s", domain.ErrSyncConflict, snap.ID,
			)
		}
		memoryAdoptions[snap.ID] = existing.MemoryHash
	}

	// Start local write only after all remote objects preflight complete.
	for _, d := range docs {
		stored, err := s.store.PutDoc(ctx, d)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if stored != d.Hash {
			return inbound.SyncOutput{}, domain.ErrHashMismatch
		}
	}
	for expected, bundle := range stagedSettings {
		stored, err := s.store.PutSettingsObject(ctx, bundle)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if stored != expected {
			return inbound.SyncOutput{}, domain.ErrHashMismatch
		}
	}
	for expected, digest := range stagedMemories {
		stored, err := s.store.PutMemory(ctx, digest)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if stored != expected {
			return inbound.SyncOutput{}, domain.ErrHashMismatch
		}
	}
	// Parent meta mismatch (replica divergence): parents can differ among replicas (legacy empirical verification: 7/7 stash-dedup bug snapshots). Local objects are immutable (PutSnapshot does not overwrite parents), so reject remote version adoption and proceed with batch — total rejection makes known divergence permanent pull failure (availability). Pollution server defense intent maintained: mismatched meta never reflected locally.
	var conflicts []string
	for _, snap := range snaps {
		if existing, ok := existingByID[snap.ID]; ok && !sameParents(existing.Parents, snap.Parents) {
			id := string(snap.ID)
			if len(id) > 17 {
				id = id[:17]
			}
			conflicts = append(conflicts, "snapshot/"+id+" (parent metadata — local kept)")
			continue
		}
		storedSnapshot := snap
		if _, exists := existingByID[snap.ID]; exists {
			// Existing memory was preflighted above and is merged only through the
			// dedicated causal CAS. Generic metadata adoption must not reinterpret
			// an intentional local-ahead keep as a conflicting first attachment.
			storedSnapshot.MemoryHash = ""
		}
		if err := s.store.PutSnapshot(ctx, storedSnapshot); err != nil {
			return inbound.SyncOutput{}, err
		}
		if expected, adopt := memoryAdoptions[snap.ID]; adopt {
			if err := s.store.CompareAndSwapSnapshotMemory(ctx, snap.ID, expected, snap.MemoryHash); err != nil {
				return inbound.SyncOutput{}, fmt.Errorf("%w: memory attachment changed during pull for snapshot %s", domain.ErrSyncConflict, snap.ID)
			}
		}
	}
	s.updateRemoteSnapshotStateCursor(ctx, repoID, cursorStore, cursorEntries, snaps)
	// FetchOnly (hook auto-pull): fetch objects only, local refs do not move — context does not force convergence unlike code. Instead, report remote branch ahead hint (pull is user choice).
	if in.FetchOnly {
		var ahead []string
		for _, r := range refs {
			if r.Kind != domain.RefBranch || r.Target == "" {
				continue
			}
			local, lerr := s.store.GetRef(ctx, repoID, r.Kind, r.Name)
			if lerr != nil || local.Target == "" || local.Target == r.Target {
				continue
			}
			if !s.isAncestor(ctx, r.Target, local.Target) { // Remote has new snapshots after local
				ahead = append(ahead, r.Name)
			}
		}
		return inbound.SyncOutput{Pulled: len(snaps), RemoteAhead: ahead}, nil
	}

	// Ref merge — git policy (fast-forward only): move local head only if it is an ancestor of remote head. If diverged, cancel that ref (local kept) and report Conflicts. --force adopts remote.
	// HEAD is local.
	var newRefs []domain.Ref
	for _, r := range refs {
		if r.Kind == domain.RefHEAD || r.Target == "" {
			continue
		}
		local, lerr := s.store.GetRef(ctx, repoID, r.Kind, r.Name)
		switch {
		case lerr != nil || local.Target == "" || local.Target == r.Target:
			// Local absent or same → reflect as is.
		case in.Force:
			// Force: adopt remote state (local snapshot objects preserved — ref pointers only move).
		case s.isAncestor(ctx, local.Target, r.Target):
			// Fast-forward possible.
		default:
			conflicts = append(conflicts, string(r.Kind)+"/"+r.Name)
			continue // cancel — local maintenance (equivalent to git pull --ff-only rejection)
		}
		if err := s.store.PutRef(ctx, domain.Ref{Kind: r.Kind, Name: r.Name, RepoID: repoID, Target: r.Target}); err != nil {
			return inbound.SyncOutput{}, err
		}
		newRefs = append(newRefs, r)
	}
	return inbound.SyncOutput{Pulled: len(snaps), NewRefs: newRefs, Conflicts: conflicts}, nil
}

// Ensure SyncRepoService implements inbound.SyncRepo.
var _ inbound.SyncRepo = (*SyncRepoService)(nil)

// Connect immediately registers the origin repo with the server (without waiting for the first push) and returns a definitive record.
// After running cxt remote add origin, it is used in the "connected" feedback. The server interprets the remote URL path to bind the workspace_id,
// so the return value indicates whether the workspace is connected.
func (s *SyncRepoService) Connect(ctx context.Context, in inbound.SyncInput) (inbound.ConnectOutput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.ConnectOutput{}, err
	}
	registered, err := s.remote.RegisterRepo(ctx, repo)
	if err != nil {
		return inbound.ConnectOutput{}, err
	}
	return inbound.ConnectOutput{Repo: registered}, nil
}
