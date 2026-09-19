package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// SaveSessionService implements the SaveSession inbound port as a use-case service.
//
// Dependency outbound ports: GitContext, CaptureSource (registry), ProviderCodec (registry), SessionStore.
//
// Save sequence (backend architecture):
//  1. GitContext.CurrentRepo(cwd) → repoID determined
//  2. CaptureSource.LocateActiveSession(cwd) → session file path detected
//  3. CaptureSource.ReadSession(path) → raw JSONL read
//  4. ProviderCodec.Decode(raw) → CIRDocument (extract envelope.git_branch)
//  5. If git_branch empty, use GitContext.CurrentBranch (especially for codex)
//  6. SessionDoc{CIR} → SessionStore.PutDoc → docHash (content hash, dedup)
//  7. If existing branch HEAD exists, connect to parent
//  8. Snapshot(ID=docHash) → PutSnapshot, branch ref/HEAD updated
type SaveSessionService struct {
	capture  outbound.SessionCapture
	outbox   outbound.SyncOutbox
	gitCtx   outbound.GitContext
	captures map[domain.ProviderKind]outbound.CaptureSource
	codecs   map[domain.ProviderKind]outbound.ProviderCodec
	store    outbound.SessionStore
}

// NewSaveSessionService creates a SaveSessionService and injects its dependencies.
func NewSaveSessionService(
	gitCtx outbound.GitContext,
	captures map[domain.ProviderKind]outbound.CaptureSource,
	codecs map[domain.ProviderKind]outbound.ProviderCodec,
	store outbound.SessionStore,
	capture outbound.SessionCapture,
	outbox outbound.SyncOutbox,
) *SaveSessionService {
	return &SaveSessionService{gitCtx: gitCtx, captures: captures, codecs: codecs, store: store, capture: capture, outbox: outbox}
}

// Save snapshots the active session in the current cwd.
func (s *SaveSessionService) Save(ctx context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	provider := in.Provider
	if provider == "" {
		provider = domain.ProviderClaude
	}
	capt, ok := s.captures[provider]
	if !ok {
		return inbound.SaveOutput{}, domain.ErrUnsupportedProvider
	}
	cdc, ok := s.codecs[provider]
	if !ok {
		return inbound.SaveOutput{}, domain.ErrUnsupportedProvider
	}

	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.SaveOutput{}, err
	}

	path := in.SessionPath
	if path == "" {
		path, err = capt.LocateActiveSession(ctx, in.Cwd)
		if err != nil {
			return inbound.SaveOutput{}, err // domain.ErrNoActiveSession included
		}
	} else {
		// Explicit path (hook payload) isolation/growing materialization gate applies the same (capture path).
		if !s.capture.Eligible(repo.LocalPath, path) {
			return inbound.SaveOutput{}, domain.ErrNoActiveSession
		}
	}
	envelope, docHash, capturedBytes, activityAt, err := s.capture.Project(ctx, repo.LocalPath, path, capt, cdc, in.Pending)
	if err != nil {
		return inbound.SaveOutput{}, err
	}

	branch := in.Branch // explicit branch takes precedence over checkpoint, etc.
	if branch == "" {
		branch, _ = s.gitCtx.CurrentBranch(ctx, in.Cwd)
		if branch == "" || branch == "HEAD" {
			branch = envelope.GitBranch
		}
	}
	// "HEAD" is not a detached marker branch name (session records can be recorded at the detached point)
	// — fallback to the empty value as the current branch in .git.
	if branch == "" || branch == "HEAD" {
		if b, berr := s.gitCtx.CurrentBranch(ctx, in.Cwd); berr == nil && b != "HEAD" {
			branch = b
		} else {
			branch = ""
		}
	}
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}

	var parents []domain.ContentHash
	var refTarget domain.ContentHash
	branchRefExists := false
	localBranch := branch
	if bindings, ok := s.store.(outbound.LocalBranchStore); ok {
		binding, err := bindings.ResolveLocalBranch(ctx, repo.ID, branch)
		if err != nil {
			return inbound.SaveOutput{}, err
		}
		if binding.Inactive {
			return inbound.SaveOutput{}, fmt.Errorf("local branch %q has been detached; replay its new branch creation before capture", localBranch)
		}
		branch = binding.Branch
	}
	if ref, gerr := s.store.GetRef(ctx, repo.ID, domain.RefBranch, branch); gerr == nil {
		branchRefExists = true
		refTarget = ref.Target
		if refTarget != "" && refTarget != docHash {
			parents = []domain.ContentHash{refTarget}
		}
	} else if !errors.Is(gerr, domain.ErrNotFound) {
		return inbound.SaveOutput{}, gerr
	}
	var position *domain.WorkingPosition
	if positions, ok := s.store.(outbound.WorkingPositionStore); ok {
		p, perr := positions.GetWorkingPosition(ctx)
		if perr != nil && !errors.Is(perr, domain.ErrNotFound) {
			return inbound.SaveOutput{}, perr
		}
		if perr == nil && p.RepoID == repo.ID && (p.Branch == branch || (p.Branch == "" && in.Branch == "")) {
			position = &p
			parents = nil
			if p.Snapshot != "" && p.Snapshot != docHash {
				parents = []domain.ContentHash{p.Snapshot}
			}
			// Pending captures retain private bytes against the selected ancestry;
			// they never publish a shared branch or replace the worktree position.
			if !in.Pending && p.Rewound && p.Branch != "" && p.SharedTarget != refTarget {
				return inbound.SaveOutput{}, fmt.Errorf("shared branch advanced after context selection: %w", domain.ErrSyncConflict)
			}
		}
	}

	msg := in.Message
	if msg == "" {
		msg = "session snapshot"
	}
	// Attach the .claude/.agents/.codex folder state at commit time using content-addressed storage (similar to git history).
	settingsHashes := map[string]domain.ContentHash{}
	for _, kind := range []string{"claude", "agents", "codex"} {
		if b, ok := s.capture.Settings(repo.LocalPath, kind); ok {
			if h, herr := s.store.PutSettingsObject(ctx, b); herr == nil {
				settingsHashes[kind] = h
			}
		}
	}
	snap := domain.Snapshot{
		ID:              docHash,
		RepoID:          repo.ID,
		Branch:          branch,
		Parents:         parents,
		DocHash:         docHash,
		ClaudeSettings:  settingsHashes["claude"],
		AgentsSettings:  settingsHashes["agents"],
		CodexSettings:   settingsHashes["codex"],
		Provider:        provider,
		Fidelity:        envelope.Fidelity,
		Message:         msg,
		Author:          in.Author,
		CreatedAt:       time.Now().UTC(),
		SessionID:       envelope.SessionOriginID,
		Models:          envelope.OrderedModels(),
		CompactionCount: envelope.CompactionCount,
	}
	// A new capture imports the selected immutable memory, including orphan
	// project memory, without inheriting later conversation or mutable grafts.
	if position != nil && position.MemoryHash != "" {
		if _, err := s.store.GetSnapshot(ctx, docHash); errors.Is(err, domain.ErrNotFound) {
			memory, err := s.store.GetMemory(ctx, position.MemoryHash)
			if err != nil {
				return inbound.SaveOutput{}, err
			}
			inherited := domain.MergeDigests(memory, domain.MemoryDigest{SnapshotID: docHash, Provider: provider})
			inherited.PreviousMemoryHash = ""
			snap.MemoryHash, err = s.store.PutMemory(ctx, inherited)
			if err != nil {
				return inbound.SaveOutput{}, err
			}
		} else if err != nil {
			return inbound.SaveOutput{}, err
		}
	}
	// Message promotion detection (dedup hook leaf → commit): PutSnapshot upgrades the local label,
	// and the server replica follows it into an upgrade queue on push (inventory-only push does not resend existing objects, so metadata updates propagate naturally).
	promote := false
	if !in.Pending && !strings.HasPrefix(msg, domain.HookMessagePrefix) {
		if existing, gerr := s.store.GetSnapshot(ctx, docHash); gerr == nil &&
			strings.HasPrefix(existing.Message, domain.HookMessagePrefix) {
			promote = true
		}
	}
	if err := s.store.PutSnapshot(ctx, snap); err != nil {
		return inbound.SaveOutput{}, err
	}
	s.capture.RecordAffinity(repo.LocalPath, provider, envelope.SessionOriginID)
	if promote {
		_ = s.outbox.EnqueuePromotion(ctx, repo.LocalPath, docHash, msg)
	}
	if in.Pending {
		// Uncommitted capture: branch ref remains immutable while the per-session
		// pointer advances to the latest durable snapshot.
		oldTarget, err := s.store.ReplacePending(ctx, domain.Pending{
			RepoID:     repo.ID,
			SessionID:  envelope.SessionOriginID,
			Branch:     branch,
			Provider:   provider,
			Target:     docHash,
			Author:     in.Author,
			UpdatedAt:  time.Now().UTC(),
			ActivityAt: activityAt,
		})
		if err != nil {
			return inbound.SaveOutput{}, err
		}
		s.gcHookLeaf(ctx, repo.ID, oldTarget, docHash)
		return inbound.SaveOutput{
			SnapshotID: docHash, Branch: branch, SessionID: envelope.SessionOriginID, CapturedBytes: capturedBytes,
		}, nil
	}
	if position != nil && position.Branch == "" {
		// Detached code has a worktree continuation, never an implicit main write.
		p := *position
		p.Snapshot = docHash
		stored, err := s.store.GetSnapshot(ctx, docHash)
		if err != nil {
			return inbound.SaveOutput{}, err
		}
		p.MemoryHash = stored.MemoryHash
		key := sha256.Sum256([]byte(p.WorktreeID + "\x00" + string(position.Snapshot) + "\x00" + string(docHash)))
		p.Selection = &domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: repo.ID, BranchID: p.BranchID, Kind: "position", Source: position.Snapshot, Target: docHash, MemoryHash: p.MemoryHash, GitAfter: p.GitCommit, CreatedAt: stored.CreatedAt}
		if err := s.store.(outbound.WorkingPositionStore).PutWorkingPosition(ctx, p); err != nil {
			return inbound.SaveOutput{}, err
		}
		return inbound.SaveOutput{SnapshotID: docHash, Branch: "HEAD", SessionID: envelope.SessionOriginID, CapturedBytes: capturedBytes}, nil
	}
	// Identify the exact capture observed by this commit. Resolution below is a
	// target CAS because a newer capture can arrive while the ref is moving.
	oldTarget, err := s.pendingTargetOf(ctx, repo.ID, envelope.SessionOriginID, provider)
	if err != nil {
		return inbound.SaveOutput{}, err
	}
	// Never move a ref backward. Content-hash dedup may match an existing snapshot already reachable as an ancestor of the current head; in that case, leave the ref in place. This prevents repeated capture of an unchanged session (for example, an old rollout from another provider) from rolling the head back and orphaning intervening commits. Forward dedup still works for a replaceable hook leaf because that leaf is not an ancestor of the head.
	if refTarget == "" || (position != nil && position.Rewound) || !s.reachable(ctx, repo.ID, refTarget, docHash) {
		// Preserve sibling forward reachability (overlay graft): If the previous head is not an ancestor of the new head (multi-session commits — each session snapshot has the same parent, becoming siblings), the ref move orphans the entire previous head lineage (real case 578f170b4a). Server diverged push rule: connect the previous head to the new head's GraftParents (Parents immutable). Server replica propagates the graft queue on push (inventory-only push does not resend existing object metadata — same channel pattern as message promotion).
		//
		// fail-closed: if graft (local reachability) or queue persistence (server propagation guarantee) fails, the ref is not moved — "branch ref move does not reduce reach set (force exception)" structural enforcement. Best-effort approach can recreate the orphaning with a single disk error. If the ref is not moved, this save is reported as a failure (pending maintained), and the next save is retried. Applied grafts are additive-only, so any residual ones are harmless.
		if (position == nil || !position.Rewound) && refTarget != "" && refTarget != docHash && !s.reachable(ctx, repo.ID, docHash, refTarget) {
			if gerr := s.graftLocalAndQueue(ctx, repo.LocalPath, docHash, refTarget); gerr != nil {
				return inbound.SaveOutput{}, fmt.Errorf("preservation of reachability (graft) failed — ref move aborted: %w", gerr)
			}
		}
		branchRef := domain.Ref{Kind: domain.RefBranch, Name: branch, RepoID: repo.ID, Target: docHash}
		if position != nil {
			branchRef.BranchID = position.BranchID
			commitStore, ok := s.store.(outbound.WorkingCommitStore)
			if !ok {
				return inbound.SaveOutput{}, fmt.Errorf("working commit store unavailable")
			}
			p := *position
			stored, err := s.store.GetSnapshot(ctx, docHash)
			if err != nil {
				return inbound.SaveOutput{}, err
			}
			p.Snapshot = docHash
			p.SharedTarget = docHash
			p.MemoryHash = stored.MemoryHash
			p.Rewound = false
			p.Orphan = false
			p.Selection = nil
			var event *domain.HistoryEvent
			if position.Rewound && refTarget != "" && refTarget != docHash {
				selectionID := ""
				observedAt := stored.CreatedAt
				if position.Selection != nil {
					selectionID = position.Selection.ID
					observedAt = position.Selection.CreatedAt
				}
				key := sha256.Sum256([]byte(repo.ID + "\x00" + branch + "\x00" + selectionID + "\x00" + string(refTarget) + "\x00" + string(docHash)))
				event = &domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: repo.ID, BranchID: p.BranchID, Branch: branch, Kind: "advance", Source: refTarget, Target: docHash, MemoryHash: stored.MemoryHash, GitBefore: position.GitCommit, GitAfter: p.GitCommit, WorktreeID: p.WorktreeID, CreatedAt: observedAt}
			}
			if conditional, ok := s.store.(outbound.WorkingCommitCASStore); ok {
				err = conditional.CommitWorkingSnapshotIfCurrent(ctx, branchRef, refTarget, *position, p, event)
			} else {
				err = commitStore.CommitWorkingSnapshot(ctx, branchRef, refTarget, p, event)
			}
			if err != nil {
				return inbound.SaveOutput{}, err
			}
		} else {
			if branchRefExists {
				if err := s.store.PutRef(ctx, branchRef); err != nil {
					return inbound.SaveOutput{}, err
				}
			} else {
				if _, err := s.store.CreateBranchRef(ctx, branchRef); err != nil {
					return inbound.SaveOutput{}, err
				}
			}
			// A departing-branch checkpoint runs after Git has switched. It
			// must not replace the new worktree's already-selected position.
			nativeBranch, _ := s.gitCtx.CurrentBranch(ctx, in.Cwd)
			if in.Branch == "" || nativeBranch == localBranch {
				if err := s.store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: branch}); err != nil {
					return inbound.SaveOutput{}, err
				}
			}
		}
	}
	// Commit storage absorbs exactly the progress pointer observed above. A hook
	// capture from the same still-running session may arrive while this save is
	// moving the branch ref; deleting by session identity would erase that newer
	// continuation. The target CAS preserves it for the next commit.
	if oldTarget != "" {
		_, _ = s.store.CompareAndDeletePending(ctx, repo.ID, envelope.SessionOriginID, oldTarget)
	}
	s.gcHookLeaf(ctx, repo.ID, oldTarget, docHash)

	return inbound.SaveOutput{
		SnapshotID:            docHash,
		Branch:                branch,
		SessionID:             envelope.SessionOriginID,
		CapturedBytes:         capturedBytes,
		ResolvedPendingTarget: oldTarget,
	}, nil
}

// reachable determines if anc is an ancestor (or the same) of from using a local snapshot walk.
func (s *SaveSessionService) reachable(ctx context.Context, repoID string, from, anc domain.ContentHash) bool {
	if from == anc {
		return true
	}
	all, err := s.store.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return false // Indeterminate — ref move allowed (maintain existing behavior)
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(all))
	for _, sn := range all {
		byID[sn.ID] = sn
	}
	seen := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		if sn, ok := byID[cur]; ok {
			stack = append(stack, sn.ReachabilityParents()...)
		}
	}
	return false
}

// pendingTargetOf returns the current pending target of the session (empty if none).
func (s *SaveSessionService) pendingTargetOf(ctx context.Context, repoID, sessionID string, provider domain.ProviderKind) (domain.ContentHash, error) {
	if sessionID == "" {
		return "", nil
	}
	pendings, err := s.store.ListPendings(ctx, repoID)
	if err != nil {
		return "", err
	}
	for _, p := range pendings {
		if p.SessionID == sessionID {
			if p.Provider != provider {
				return "", fmt.Errorf("%w: pending session %q belongs to provider %q, not %q", domain.ErrSyncConflict, sessionID, p.Provider, provider)
			}
			return p.Target, nil
		}
	}
	return "", nil
}

// gcHookLeaf removes hook-capture leaf snapshots and documents replaced by sliding capture or commit incorporation.
// A replacement must contain the old session's complete event prefix. Identity
// alone is insufficient: a stale or divergent capture can reuse the native ID.
// Branch and cwd are not identity; the same native session can move worktrees.
func (s *SaveSessionService) gcHookLeaf(ctx context.Context, repoID string, old, current domain.ContentHash) {
	if old == "" || old == current {
		return
	}
	retention, ok := s.store.(outbound.ObjectRetention)
	if !ok {
		return
	} // A store without reader coordination must retain data.
	job := outbound.CaptureCollection{RepoID: repoID, Previous: old, Replacement: current}
	if err := retention.QueueCaptureCollection(ctx, job); err != nil {
		return
	}
	_, _ = retention.TryCollectObjects(ctx, func() error {
		jobs, err := retention.CaptureCollections(ctx, repoID, 32)
		if err != nil {
			return err
		}
		for _, queued := range jobs {
			// The original successor may itself have been collected. The current
			// capture is also eligible, but must pass the same provider/session/
			// complete-prefix and reachability checks before any removal.
			done := s.collectHookLeaf(ctx, repoID, queued.Previous, queued.Replacement)
			if !done {
				done = s.collectHookLeaf(ctx, repoID, queued.Previous, current)
			}
			if done {
				if err := retention.CompleteCaptureCollection(ctx, queued); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// true means a final retain/remove decision; false leaves a failed read/write
// queued. A retained historical object must not starve later collection jobs.
func (s *SaveSessionService) collectHookLeaf(ctx context.Context, repoID string, old, current domain.ContentHash) bool {
	if old == "" || current == "" || old == current {
		return true
	}
	snap, err := s.store.GetSnapshot(ctx, old)
	if err != nil {
		return errors.Is(err, domain.ErrNotFound)
	}
	if !strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
		return true
	}
	// Session-prefix coverage cannot prove coverage of a separate memory object.
	if snap.MemoryHash != "" {
		return true
	}
	replacement, err := s.store.GetSnapshot(ctx, current)
	if err != nil {
		return false
	}
	if snap.Provider == "" || snap.SessionID == "" ||
		replacement.Provider != snap.Provider || replacement.SessionID != snap.SessionID {
		return true
	}
	oldDoc, err := s.store.GetDoc(ctx, snap.DocHash)
	if err != nil {
		return false
	}
	newDoc, err := s.store.GetDoc(ctx, replacement.DocHash)
	if err != nil {
		return false
	}
	if oldDoc.CIR.Envelope.SourceProvider != snap.Provider ||
		newDoc.CIR.Envelope.SourceProvider != snap.Provider ||
		oldDoc.CIR.Envelope.SessionOriginID != snap.SessionID ||
		newDoc.CIR.Envelope.SessionOriginID != snap.SessionID ||
		len(newDoc.CIR.Events) < len(oldDoc.CIR.Events) {
		return true
	}
	for i, event := range oldDoc.CIR.Events {
		if !reflect.DeepEqual(event, newDoc.CIR.Events[i]) {
			return true
		}
	}
	refs, err := s.store.ListRefs(ctx, repoID)
	if err != nil {
		return false
	}
	// Reachability walk preparation — load full snapshot once, then in-memory walk (parents ∪ graft_parents).
	all, err := s.store.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return false // Safely preserve when indeterminate
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(all))
	for _, sn := range all {
		for _, parent := range sn.ReachabilityParents() {
			if parent == old {
				return true // even an unreferenced child still needs this object
			}
		}
		if sn.ID != old && sn.DocHash == snap.DocHash {
			return true // another snapshot still owns the document
		}
		byID[sn.ID] = sn
	}
	seen := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{}
	for _, r := range refs {
		if r.Target != "" {
			stack = append(stack, r.Target)
		}
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || seen[cur] {
			continue
		}
		if cur == old {
			return true // reachable from ref — part of history, so preserved
		}
		seen[cur] = true
		if sn, ok := byID[cur]; ok {
			stack = append(stack, sn.ReachabilityParents()...)
		}
	}
	pendings, err := s.store.ListPendings(ctx, repoID)
	if err != nil {
		return false
	}
	for _, p := range pendings {
		if p.Target == old {
			return true
		}
	}
	if err := s.store.DeleteSnapshot(ctx, old); err != nil {
		return false
	}
	return s.store.DeleteDoc(ctx, snap.DocHash) == nil
}

func appendGraftQueueEvent(state *[]domain.GraftQueueEvent, event domain.GraftQueueEvent) bool {
	for _, queued := range *state {
		if queued.Snapshot == event.Snapshot && queued.ExpectedSeq == event.ExpectedSeq &&
			len(queued.Parents) == 1 && len(event.Parents) == 1 && queued.Parents[0] == event.Parents[0] {
			return false
		}
	}
	*state = append(*state, event)
	return true
}

func hasLegacyGraftEvent(state []domain.GraftQueueEvent, snapshot string) bool {
	for _, event := range state {
		if event.Snapshot == snapshot && event.Legacy {
			return true
		}
	}
	return false
}

// graftLocalAndQueue serializes local LWW register advancement and remote propagation events under the same process lock. It durable writes the queue first and then increments local seq. It avoids creating a state where "there is an edge locally but no remote event". The opposite (queue only) can be idempotently recovered on retry, and the ref does not move, making it safe.
func (s *SaveSessionService) graftLocalAndQueue(ctx context.Context, repoRoot string, head, parent domain.ContentHash) error {
	return s.outbox.WithGrafts(ctx, repoRoot, func(q outbound.GraftQueueAccess) error {
		state, err := q.Load()
		if err != nil {
			return err
		}
		snap, err := s.store.GetSnapshot(ctx, head)
		if err != nil {
			return err
		}
		for _, p := range snap.Parents {
			if p == parent {
				return nil // if natural parent, no graft/queue is needed.
			}
		}
		for _, p := range snap.GraftParents {
			if p == parent {
				return nil // already reflected by previous success or remote pull.
			}
		}
		// The old map queue has all expected_seq as 0 and local seq was not advanced. If new events are appended to the same snapshot, it forms a [0,0] chain, causing the second to be discarded as stale 409 after the first propagation. First, confirm and adjust the old event with cxt push.
		if hasLegacyGraftEvent(state, string(head)) {
			return fmt.Errorf("legacy graft queue remains; retry save after cxt push")
		}
		if snap.GraftSeq == domain.MaxGraftSeq {
			return fmt.Errorf("graft sequence exhausted")
		}
		event := domain.GraftQueueEvent{
			Snapshot: string(head), Parents: []string{string(parent)}, ExpectedSeq: snap.GraftSeq,
		}
		if appendGraftQueueEvent(&state, event) {
			if err := q.Store(state); err != nil {
				return err
			}
		}
		snap.GraftParents = append(snap.GraftParents, parent)
		snap.Grafted = true
		snap.GraftSeq++
		return s.store.PutSnapshot(ctx, snap)
	})
}

// Ensure SaveSessionService implements inbound.SaveSession.
var _ inbound.SaveSession = (*SaveSessionService)(nil)
