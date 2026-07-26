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

	snaps, docs, err := s.collectObjects(ctx, repoID, man)
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

	// Memory attaches a dedicated API pointer after snapshot creation. First, all local objects are validated to reduce the chance of discovering missing objects after raw/ref push.
	var memories []domain.MemoryDigest
	for _, snap := range snaps {
		if snap.MemoryHash == "" {
			continue
		}
		digest, err := s.store.GetMemory(ctx, snap.MemoryHash)
		if err != nil {
			return inbound.SyncOutput{}, err
		}
		if digest.SnapshotID != snap.ID {
			return inbound.SyncOutput{}, domain.ErrHashMismatch
		}
		memories = append(memories, digest)
	}

	// Publish order is object → graft overlay → ref. New snapshots arrive without server-owned graft metadata. Publishing refs first would make sibling-session histories that depend on the queued graft appear non-fast-forward. Call RemoteSync.Push twice to make the protocol boundary explicit: objects first, refs last.
	if err := s.remote.Push(ctx, repoID, snaps, docs, nil, false, false); err != nil {
		return inbound.SyncOutput{}, err
	}
	for _, memory := range memories {
		if err := s.remote.PushMemory(ctx, repoID, memory); err != nil {
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
	return inbound.SyncOutput{Pushed: len(snaps), NewRefs: refs}, nil
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
	// Local mirror preserves the history when fast-forwarding — it keeps my chain when the local branch is ahead (unpushed segments).
	if cur, gerr := s.store.GetRef(ctx, repoID, domain.RefBranch, branch); gerr != nil || cur.Target == "" || s.isAncestor(ctx, cur.Target, target) {
		_ = s.store.PutRef(ctx, ref)
	}
	return nil
}

// collectObjects gathers push target objects (snapshots+docs) for Push/Shadow Sync.
// Stash is local-only (like git), so it is excluded, but the determination is based on ref reachability, not labels.
// Content-hash deduplication ensures that if the same session content is stored in both stash and commits, one "(stash)" label object can be part of the commit history.
// Removing the label alone can lead to parent loss (fsck corruption) and permanent ref advancement blockage (stash-dedup trap).
func (s *SyncRepoService) collectObjects(ctx context.Context, repoID string, man domain.Manifest) ([]domain.Snapshot, []domain.SessionDoc, error) {
	byID := make(map[domain.ContentHash]domain.Snapshot, len(man.SnapshotIndex))
	for _, id := range man.SnapshotIndex {
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return nil, nil, err
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
	var docs []domain.SessionDoc
	seenDoc := map[domain.ContentHash]bool{}
	for _, id := range man.SnapshotIndex {
		snap := byID[id]
		if snap.Branch == domain.StashBranchLabel && !reachable[id] {
			continue // Exclude pure stashes (unreachable from any ref)
		}
		snap.RepoID = repoID // Normalize to current origin-derived repoID (remote redirection)
		snaps = append(snaps, snap)
		if snap.DocHash != "" && !seenDoc[snap.DocHash] {
			doc, err := s.store.GetDoc(ctx, snap.DocHash)
			if err != nil {
				return nil, nil, err
			}
			docs = append(docs, doc)
			seenDoc[snap.DocHash] = true
		}
	}
	return snaps, docs, nil
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
			if snaps, docs, cerr := s.collectObjects(ctx, repoID, man); cerr == nil {
				if serr := s.pushSettingsObjects(ctx, repoID, snaps); serr == nil {
					if perr := s.remote.Push(ctx, repoID, snaps, docs, nil, false, false); perr == nil {
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

// Pull merges the snapshot/doc/ref from the central server into the local repository (fast-forward first).
func (s *SyncRepoService) Pull(ctx context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	repoID, err := s.repoID(ctx, in)
	if err != nil {
		return inbound.SyncOutput{}, err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return inbound.SyncOutput{}, err
	}
	// Delta pull: Does not re-receive documents that already exist locally. Existence is confirmed by checking DocHash in the local snapshot index (body not loaded — cheap negotiation).
	var docHaves []domain.ContentHash
	if man, merr := s.store.Manifest(ctx, repoID); merr == nil {
		seen := map[domain.ContentHash]bool{}
		for _, id := range man.SnapshotIndex {
			snap, gerr := s.store.GetSnapshot(ctx, id)
			if gerr != nil || snap.DocHash == "" || seen[snap.DocHash] {
				continue
			}
			seen[snap.DocHash] = true
			if ok, herr := s.store.HasDoc(ctx, snap.DocHash); herr == nil && ok {
				docHaves = append(docHaves, snap.DocHash)
			}
		}
	}
	snaps, docs, refs, err := s.remote.Pull(ctx, repoID, docHaves)
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
	// Memory also validates snapshot_id and content hash before staging. If either is missing or corrupted, the entire batch is rejected before writing doc/settings/snapshot.
	stagedMemories := map[domain.ContentHash]domain.MemoryDigest{}
	for _, snap := range snaps {
		if snap.MemoryHash == "" {
			continue
		}
		local, gerr := s.store.GetMemory(ctx, snap.MemoryHash)
		switch {
		case gerr == nil:
			got, herr := domain.MemoryDigestHash(local)
			if herr != nil || got != snap.MemoryHash || local.SnapshotID != snap.ID {
				return inbound.SyncOutput{}, domain.ErrHashMismatch
			}
		case errors.Is(gerr, domain.ErrNotFound):
			digest, perr := s.remote.PullMemory(ctx, repoID, snap.ID)
			if perr != nil {
				return inbound.SyncOutput{}, perr
			}
			got, herr := domain.MemoryDigestHash(digest)
			if herr != nil {
				return inbound.SyncOutput{}, herr
			}
			if digest.SnapshotID != snap.ID || got != snap.MemoryHash {
				return inbound.SyncOutput{}, domain.ErrHashMismatch
			}
			if prior, ok := stagedMemories[snap.MemoryHash]; ok && prior.SnapshotID != digest.SnapshotID {
				return inbound.SyncOutput{}, domain.ErrHashMismatch
			}
			stagedMemories[snap.MemoryHash] = digest
		default:
			return inbound.SyncOutput{}, gerr
		}
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
		if existing, gerr := s.store.GetSnapshot(ctx, snap.ID); gerr == nil && !sameParents(existing.Parents, snap.Parents) {
			id := string(snap.ID)
			if len(id) > 17 {
				id = id[:17]
			}
			conflicts = append(conflicts, "snapshot/"+id+" (parent metadata — local kept)")
			continue
		}
		if err := s.store.PutSnapshot(ctx, snap); err != nil {
			return inbound.SyncOutput{}, err
		}
	}
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
