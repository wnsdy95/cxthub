package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// SaveSessionService implements the SaveSession inbound port as a use-case service.
//
// Dependency outbound ports: GitContext, CaptureSource (registry), ProviderCodec (registry), SessionStore.
//
// Save sequence (BACKEND-ARCHITECTURE §5.2):
//  1. GitContext.CurrentRepo(cwd) → repoID determined
//  2. CaptureSource.LocateActiveSession(cwd) → session file path detected
//  3. CaptureSource.ReadSession(path) → raw JSONL read
//  4. ProviderCodec.Decode(raw) → CIRDocument (extract envelope.git_branch)
//  5. If git_branch empty, use GitContext.CurrentBranch (especially for codex)
//  6. SessionDoc{CIR} → SessionStore.PutDoc → docHash (content hash, dedup)
//  7. If existing branch HEAD exists, connect to parent
//  8. Snapshot(ID=docHash) → PutSnapshot, branch ref/HEAD updated
type SaveSessionService struct {
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
) *SaveSessionService {
	return &SaveSessionService{gitCtx: gitCtx, captures: captures, codecs: codecs, store: store}
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
		// Explicit path (hook payload) isolation/growing materialization gate applies the same (CAPTURE §2.4).
		fi, serr := os.Stat(path)
		if serr != nil || providerfs.CaptureExcluded(in.Cwd, path, fi.Size()) {
			return inbound.SaveOutput{}, domain.ErrNoActiveSession
		}
	}
	raw, err := capt.ReadSession(ctx, path)
	if err != nil {
		return inbound.SaveOutput{}, err
	}
	// Secret masking (.cxtsecrets) — local deterministic replacement before saving (P1 previous step: immutable after saving here).
	raw, _ = capture.ScrubSecrets(raw, repo.LocalPath)
	cir, err := cdc.Decode(ctx, raw)
	if err != nil {
		return inbound.SaveOutput{}, err
	}
	// Pattern scrub (P2): after exact .cxtsecrets replacement above, mask known
	// credential formats and URL credentials in the CIR layer. Opaque locked
	// reasoning data (signatures/ciphertext) is intentionally excluded.
	cir = capture.ScrubDoc(cir, repo.LocalPath)

	branch := in.Branch // explicit branch takes precedence over checkpoint, etc.
	if branch == "" {
		branch = cir.Envelope.GitBranch
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

	docHash, err := s.store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		return inbound.SaveOutput{}, err
	}

	var parents []domain.ContentHash
	var refTarget domain.ContentHash
	if ref, gerr := s.store.GetRef(ctx, repo.ID, domain.RefBranch, branch); gerr == nil && ref.Target != "" && ref.Target != docHash {
		refTarget = ref.Target
		parents = []domain.ContentHash{ref.Target}
	}

	msg := in.Message
	if msg == "" {
		msg = "session snapshot"
	}
	// Attach the .claude/.agents/.codex folder state at commit time using content-addressed storage (similar to git history).
	settingsHashes := map[string]domain.ContentHash{}
	for _, kind := range []string{"claude", "agents", "codex"} {
		if b, ok := capture.ReadSettingsDir(repo.LocalPath, kind); ok {
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
		Fidelity:        cir.Envelope.Fidelity,
		Message:         msg,
		Author:          in.Author,
		CreatedAt:       time.Now().UTC(),
		SessionID:       cir.Envelope.SessionOriginID,
		Models:          cir.Envelope.OrderedModels(),
		CompactionCount: cir.Envelope.CompactionCount,
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
	if promote {
		queuePromotion(repo.LocalPath, docHash, msg)
	}
	// Identify the previous hook-capture leaf that sliding capture or commit incorporation will replace, so it can be garbage-collected safely.
	oldTarget := s.pendingTargetOf(ctx, repo.ID, cir.Envelope.SessionOriginID)

	if in.Pending {
		// Ongoing context: branch ref immutable — only update pending pointers (overwrite = "continuing conversation" slides to the latest). Commit snapshots delete to resolve.
		if err := s.store.PutPending(ctx, domain.Pending{
			RepoID:    repo.ID,
			SessionID: cir.Envelope.SessionOriginID,
			Branch:    branch,
			Provider:  provider,
			Target:    docHash,
			Author:    in.Author,
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return inbound.SaveOutput{}, err
		}
		s.gcHookLeaf(ctx, repo.ID, oldTarget, docHash)
		return inbound.SaveOutput{SnapshotID: docHash, Branch: branch, SessionID: cir.Envelope.SessionOriginID}, nil
	}
	// Never move a ref backward. Content-hash dedup may match an existing snapshot already reachable as an ancestor of the current head; in that case, leave the ref in place. This prevents repeated capture of an unchanged session (for example, an old rollout from another provider) from rolling the head back and orphaning intervening commits. Forward dedup still works for a replaceable hook leaf because that leaf is not an ancestor of the head.
	if refTarget == "" || !s.reachable(ctx, repo.ID, refTarget, docHash) {
		// Preserve sibling forward reachability (overlay graft): If the previous head is not an ancestor of the new head (multi-session commits — each session snapshot has the same parent, becoming siblings), the ref move orphans the entire previous head lineage (real case 578f170b4a). Server diverged push rule: connect the previous head to the new head's GraftParents (Parents immutable). Server replica propagates the graft queue on push (inventory-only push does not resend existing object metadata — same channel pattern as message promotion).
		//
		// fail-closed: if graft (local reachability) or queue persistence (server propagation guarantee) fails, the ref is not moved — "branch ref move does not reduce reach set (force exception)" structural enforcement. Best-effort approach can recreate the orphaning with a single disk error. If the ref is not moved, this save is reported as a failure (pending maintained), and the next save is retried. Applied grafts are additive-only, so any residual ones are harmless.
		if refTarget != "" && refTarget != docHash && !s.reachable(ctx, repo.ID, docHash, refTarget) {
			if gerr := s.graftLocalAndQueue(ctx, repo.LocalPath, docHash, refTarget); gerr != nil {
				return inbound.SaveOutput{}, fmt.Errorf("preservation of reachability (graft) failed — ref move aborted: %w", gerr)
			}
		}
		if err := s.store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: branch, RepoID: repo.ID, Target: docHash}); err != nil {
			return inbound.SaveOutput{}, err
		}
		// HEAD points to the current branch symbolically. Best-effort.
		_ = s.store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: branch})
	}
	// Commit storage absorbs the session's progress pointer (snapshot = full session up to that point).
	_ = s.store.DeletePending(ctx, repo.ID, cir.Envelope.SessionOriginID)
	s.gcHookLeaf(ctx, repo.ID, oldTarget, docHash)

	return inbound.SaveOutput{SnapshotID: docHash, Branch: branch, SessionID: cir.Envelope.SessionOriginID}, nil
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
func (s *SaveSessionService) pendingTargetOf(ctx context.Context, repoID, sessionID string) domain.ContentHash {
	if sessionID == "" {
		return ""
	}
	pendings, err := s.store.ListPendings(ctx, repoID)
	if err != nil {
		return ""
	}
	for _, p := range pendings {
		if p.SessionID == sessionID {
			return p.Target
		}
	}
	return ""
}

// gcHookLeaf removes hook-capture leaf snapshots and documents replaced by sliding capture or commit incorporation.
// Hook leaves are always leaf nodes (no children), so they are safe to remove when all guards pass:
// hook prefix Message · ref not reachable (direct target + ancestor walk, same rule as server gcHookLeaf) · not another pending's target · differs from new object.
// (Commit history is never a target — hygiene to prevent dangling branches in the graph.)
func (s *SaveSessionService) gcHookLeaf(ctx context.Context, repoID string, old, current domain.ContentHash) {
	if old == "" || old == current {
		return
	}
	snap, err := s.store.GetSnapshot(ctx, old)
	if err != nil || !strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
		return
	}
	refs, err := s.store.ListRefs(ctx, repoID)
	if err != nil {
		return
	}
	// Reachability walk preparation — load full snapshot once, then in-memory walk (parents ∪ graft_parents).
	all, err := s.store.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return // Safely preserve when indeterminate
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(all))
	for _, sn := range all {
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
			return // reachable from ref — part of history, so preserved
		}
		seen[cur] = true
		if sn, ok := byID[cur]; ok {
			stack = append(stack, sn.ReachabilityParents()...)
		}
	}
	pendings, err := s.store.ListPendings(ctx, repoID)
	if err != nil {
		return
	}
	for _, p := range pendings {
		if p.Target == old {
			return
		}
	}
	_ = s.store.DeleteSnapshot(ctx, old)
	_ = s.store.DeleteDoc(ctx, snap.DocHash)
}

// graftsFile is a versioned graft event queue pending propagation to the server. Must preserve order and expected_seq to prevent edges from being restored after late-arriving adds following a join supersede.
const graftsFile = "grafts.json"

const graftQueueVersion = 1

const graftQueueLockStale = 2 * time.Minute

type graftQueueEvent struct {
	Snapshot    string   `json:"snapshot"`
	Parents     []string `json:"parents"`
	ExpectedSeq uint64   `json:"expected_seq"`
	// Legacy is an event promoted from the legacy map queue. The legacy queue did not upload a local GraftSeq, so a server projection must be re-fetched after success to match the seq.
	Legacy bool `json:"legacy,omitempty"`
}

type graftQueueState struct {
	Version int               `json:"version"`
	Events  []graftQueueEvent `json:"events"`
}

func lockGraftQueue(repoRoot string) (func(), error) {
	path, err := providerfs.PrepareRepoFile(repoRoot, ".cxt/grafts.json.lock", 0o755)
	if err != nil {
		return func() {}, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return func() {}, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > graftQueueLockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}, fmt.Errorf("graft queue lock timeout")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readGraftQueue(repoRoot, rel string) (graftQueueState, error) {
	state := graftQueueState{Version: graftQueueVersion}
	b, err := providerfs.ReadRepoFile(repoRoot, rel)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	var current graftQueueState
	if json.Unmarshal(b, &current) == nil && current.Version == graftQueueVersion {
		// current format
		state = current
	} else {
		// Read the map format of 558155c~254ab52 once and promote it to an ordered CAS event (seq=0). Silently discard corrupted JSON and fail-closed.
		legacy := map[string][]string{}
		if err := json.Unmarshal(b, &legacy); err != nil {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue: %w", err)
		}
		state = graftQueueState{Version: graftQueueVersion}
		ids := make([]string, 0, len(legacy))
		for id := range legacy {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			state.Events = append(state.Events, graftQueueEvent{Snapshot: id, Parents: legacy[id], Legacy: true})
		}
	}
	for _, event := range state.Events {
		if err := domain.ValidateContentHash(domain.ContentHash(event.Snapshot)); err != nil {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue snapshot: %w", err)
		}
		if len(event.Parents) == 0 || len(event.Parents) > 16 {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue parents")
		}
		if event.ExpectedSeq > domain.MaxGraftSeq {
			return graftQueueState{}, fmt.Errorf("corrupted graft queue expected_seq")
		}
		for _, parent := range event.Parents {
			if err := domain.ValidateContentHash(domain.ContentHash(parent)); err != nil {
				return graftQueueState{}, fmt.Errorf("corrupted graft queue parent: %w", err)
			}
		}
	}
	return state, nil
}

func writeGraftQueue(repoRoot, rel string, state graftQueueState) error {
	state.Version = graftQueueVersion
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(repoRoot, rel, b, 0o644)
}

func appendGraftQueueEvent(state *graftQueueState, event graftQueueEvent) bool {
	for _, queued := range state.Events {
		if queued.Snapshot == event.Snapshot && queued.ExpectedSeq == event.ExpectedSeq &&
			len(queued.Parents) == 1 && len(event.Parents) == 1 && queued.Parents[0] == event.Parents[0] {
			return false
		}
	}
	state.Events = append(state.Events, event)
	return true
}

func hasLegacyGraftEvent(state graftQueueState, snapshot string) bool {
	for _, event := range state.Events {
		if event.Snapshot == snapshot && event.Legacy {
			return true
		}
	}
	return false
}

// graftLocalAndQueue serializes local LWW register advancement and remote propagation events under the same process lock. It durable writes the queue first and then increments local seq. It avoids creating a state where "there is an edge locally but no remote event". The opposite (queue only) can be idempotently recovered on retry, and the ref does not move, making it safe.
func (s *SaveSessionService) graftLocalAndQueue(ctx context.Context, repoRoot string, head, parent domain.ContentHash) error {
	rel := ".cxt/" + graftsFile
	unlock, err := lockGraftQueue(repoRoot)
	if err != nil {
		return err
	}
	defer unlock()

	state, err := readGraftQueue(repoRoot, rel)
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
	event := graftQueueEvent{
		Snapshot: string(head), Parents: []string{string(parent)}, ExpectedSeq: snap.GraftSeq,
	}
	if appendGraftQueueEvent(&state, event) {
		if err := writeGraftQueue(repoRoot, rel, state); err != nil {
			return err
		}
	}
	snap.GraftParents = append(snap.GraftParents, parent)
	snap.Grafted = true
	snap.GraftSeq++
	return s.store.PutSnapshot(ctx, snap)
}

// queueGraft records the graft edge in the queue. Failure returns an error — the caller (Save) stops ref movement on fail-closed (queue loss = server replica permanent unpropagation).
func queueGraft(repoRoot string, head, parent domain.ContentHash, expectedSeq uint64) error {
	rel := ".cxt/" + graftsFile
	unlock, err := lockGraftQueue(repoRoot)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := readGraftQueue(repoRoot, rel)
	if err != nil {
		return err
	}
	event := graftQueueEvent{Snapshot: string(head), Parents: []string{string(parent)}, ExpectedSeq: expectedSeq}
	if hasLegacyGraftEvent(state, event.Snapshot) {
		return fmt.Errorf("legacy graft queue remains; flush first")
	}
	if !appendGraftQueueEvent(&state, event) {
		return nil // already in queue — idempotent
	}
	return writeGraftQueue(repoRoot, rel, state)
}

// promotionsFile is a server-propagation pending message promotion queue (.cxt/promotions.json —
// snapshotID→commit message). push flushes (removes successful items, keeps failures — idempotent retry).
const promotionsFile = "promotions.json"

// queuePromotion records message promotions to the queue (best-effort — failure does not invalidate commit).
func queuePromotion(repoRoot string, id domain.ContentHash, msg string) {
	rel := ".cxt/" + promotionsFile
	m := map[string]string{}
	if b, err := providerfs.ReadRepoFile(repoRoot, rel); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[string(id)] = msg
	if b, err := json.Marshal(m); err == nil {
		_ = providerfs.WriteRepoFileAtomic(repoRoot, rel, b, 0o644)
	}
}

// Ensure SaveSessionService implements inbound.SaveSession.
var _ inbound.SaveSession = (*SaveSessionService)(nil)
