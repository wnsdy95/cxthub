package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// WorkingStateService has no capture, sync, mutation or repair capability.
// Queries pin stored observations; they do not promise live provider activity.
type WorkingStateService struct {
	pulls   outbound.AppliedPullReader
	remote  string
	git     outbound.GitContext
	code    outbound.CodePosition
	store   outbound.WorkingStateReader
	docs    outbound.ContextDocumentReader
	history inbound.HistoryQuery
}

func (s *WorkingStateService) WithAppliedPullReader(pulls outbound.AppliedPullReader, remote string) *WorkingStateService {
	s.pulls, s.remote = pulls, remote
	return s
}

func NewWorkingStateService(git outbound.GitContext, code outbound.CodePosition, store outbound.WorkingStateReader, docs outbound.ContextDocumentReader, history inbound.HistoryQuery) *WorkingStateService {
	return &WorkingStateService{git: git, code: code, store: store, docs: docs, history: history}
}

type workingObservation struct {
	// Only the enclosing observation fence consumes this full-catalog hash.
	// Keep the public working revision's existing serialization unchanged.
	CatalogRevision domain.ContentHash `json:"-"`
	State           domain.WorkingState
	Index           domain.StagingIndex
	Position        domain.WorkingPosition
	History         domain.HistoryQueryResult
	Commits         []domain.StagingCommit
	Pending         []domain.Pending
}

func (s *WorkingStateService) Status(ctx context.Context, cwd string) (domain.WorkingState, error) {
	value, err := s.stableWorkingObservation(ctx, cwd)
	return value.State, err
}

// Optimistic read fences include index sequence, the complete pinned position,
// mutable memory attachments, capture targets and finalization receipts. A
// continually changing state is a retryable selection error, never a mixed view.
func (s *WorkingStateService) stableWorkingObservation(ctx context.Context, cwd string) (workingObservation, error) {
	prior, err := s.readWorkingObservation(ctx, cwd)
	if err != nil {
		return prior, err
	}
	for n := 0; n < 3; n++ {
		next, err := s.readWorkingObservation(ctx, cwd)
		if err != nil {
			return next, err
		}
		if sameWorkingObservation(prior, next) {
			return next, nil
		}
		prior = next
	}
	return workingObservation{}, domain.ErrSelectionChanged
}

func sameWorkingObservation(a, b workingObservation) bool {
	return a.State.Revision == b.State.Revision && a.CatalogRevision == b.CatalogRevision
}

func (s *WorkingStateService) readWorkingObservation(ctx context.Context, cwd string) (workingObservation, error) {
	var out workingObservation
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if s.git == nil || s.code == nil || s.store == nil || s.history == nil {
		return out, fmt.Errorf("working_state_unavailable")
	}
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return out, err
	}
	branch, err := s.git.CurrentBranch(ctx, cwd)
	if err != nil {
		return out, err
	}
	if branch == "HEAD" {
		branch = ""
	}
	commit, err := s.code.CurrentCommit(ctx, cwd)
	if err != nil {
		return out, err
	}
	out.Index, out.Position, err = s.store.ReadStaging(ctx, repo.ID)
	if err != nil {
		return out, err
	}
	if err = domain.ValidateStagingIndex(out.Index); err != nil {
		return out, err
	}
	p := out.Position
	if out.Index.RepoID != repo.ID || p.RepoID != repo.ID || out.Index.WorktreeID != p.WorktreeID {
		return out, domain.ErrHashMismatch
	}
	// HEAD is resolved by the shared query contract, never a creation-label filter.
	if observer, ok := s.history.(inbound.LocalHistoryObserver); ok {
		out.History, out.CatalogRevision, err = observer.ObserveLocalHistory(ctx, cwd)
		if err == nil {
			err = domain.ValidateContentHash(out.CatalogRevision)
		}
	} else {
		out.History, err = s.history.QueryHistory(ctx, inbound.HistoryQueryInput{Cwd: cwd})
	}
	if err != nil {
		return out, err
	}
	if out.History.Version != domain.QueryContractVersion || out.History.ServerChecked {
		return out, fmt.Errorf("working_state_requires_local_history")
	}
	if out.History.Position != p.Snapshot {
		return out, domain.ErrSelectionChanged
	}
	out.Pending, err = s.store.ListPendings(ctx, repo.ID)
	if err != nil {
		return out, err
	}
	out.Commits, err = s.store.ListStagingCommits(ctx, repo.ID)
	if err != nil {
		return out, err
	}
	out.Pending = append([]domain.Pending{}, out.Pending...)
	out.Commits = append([]domain.StagingCommit{}, out.Commits...)
	sort.Slice(out.Pending, func(i, j int) bool {
		a, b := out.Pending[i], out.Pending[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.SessionID < b.SessionID
	})
	sort.Slice(out.Commits, func(i, j int) bool { return out.Commits[i].ID < out.Commits[j].ID })
	state := domain.WorkingState{Version: domain.WorkingStateVersion, HistoryRevision: out.History.StateHash, IndexRevision: out.Index.Revision, IndexSequence: out.Index.Sequence,
		Selection: domain.WorkingSelection{RepoID: repo.ID, WorktreeID: p.WorktreeID, GitBranch: branch, GitCommit: commit, ContextMode: "named", ContextBranch: p.Branch, ContextBranchID: p.BranchID, ContextSnapshot: p.Snapshot, SelectedCodeCommit: p.GitCommit, CodeMatchesSelection: commit == p.GitCommit && branch == p.GitBranch()},
		Memory:    domain.WorkingMemory{Pinned: p.MemoryPinned, Source: p.MemorySource, AppliedHash: p.MemoryHash},
		Freshness: domain.ObservationFreshness{Source: "local_stored_capture", WatcherState: "unknown", ServerChecked: false},
		Staged:    []domain.StagedSummary{}, Pending: []domain.PendingSummary{}, LocalCommits: []domain.LocalCommitSummary{}, OutboxState: "server_acknowledgement_unknown", Coverage: "stored_observations_only", Gaps: []string{"provider_activity_unknown", "server_not_checked"},
	}
	if p.Branch == "" {
		state.Selection.ContextMode = "detached"
	}
	state.LocalCommitScope = "worktree_staging_receipts"
	if !state.Selection.CodeMatchesSelection {
		state.Gaps = append(state.Gaps, "code_selection_mismatch")
	}
	if !out.History.Complete {
		state.Gaps = append(state.Gaps, "history_incomplete")
	}
	reachable := map[domain.ContentHash]bool{}
	for _, snap := range out.History.Snapshots {
		if snap.RepoID != repo.ID {
			return out, domain.ErrHashMismatch
		}
		reachable[snap.ID] = true
	}
	memorySource := p.MemorySource
	if memorySource == "" {
		memorySource = p.Snapshot
	}
	if memorySource != "" {
		snap, e := s.store.GetSnapshot(ctx, memorySource)
		if errors.Is(e, domain.ErrNotFound) {
			state.Gaps = append(state.Gaps, "memory_source_missing")
		} else if e != nil {
			return out, e
		} else {
			if snap.ID != memorySource || snap.RepoID != repo.ID {
				return out, domain.ErrHashMismatch
			}
			state.Memory.ObservedSource = memorySource
			state.Memory.ObservedAttachment = snap.MemoryHash
			state.Memory.AttachmentChanged = p.MemoryPinned && p.MemorySource != "" && p.MemoryHash != snap.MemoryHash
		}
	}
	if p.MemoryPinned && p.MemoryHash != "" && p.MemorySource == "" {
		state.Gaps = append(state.Gaps, "pinned_memory_source_unknown")
	}
	if !p.MemoryPinned {
		state.Gaps = append(state.Gaps, "applied_memory_not_pinned")
		state.Memory.AppliedHash = ""
	}
	for _, e := range out.Index.Entries {
		state.Staged = append(state.Staged, domain.StagedSummary{Key: e.Key, Provider: e.Provider, SessionID: e.SessionID, SourceID: e.SourceID, Generation: e.Generation, DocHash: e.DocHash, Events: e.Events, StartEvent: e.StartEvent, CapturedAt: e.CapturedAt})
	}
	seenPending := map[string]bool{}
	for _, p := range out.Pending {
		if (p.RepoID != "" && p.RepoID != repo.ID) || domain.ValidateContentHash(p.Target) != nil || p.SessionID == "" {
			return out, domain.ErrHashMismatch
		}
		key := string(p.Provider) + "\x00" + p.SessionID
		if seenPending[key] {
			return out, domain.ErrHashMismatch
		}
		seenPending[key] = true
		state.Pending = append(state.Pending, domain.PendingSummary{Provider: p.Provider, SessionID: p.SessionID, Target: p.Target, Branch: p.Branch, UpdatedAt: p.UpdatedAt, ActivityAt: p.ActivityAt, Dismissed: p.Dismissed, Scope: "repository_observation"})
		if !p.UpdatedAt.IsZero() && (state.Freshness.LastCaptureAt == nil || p.UpdatedAt.After(*state.Freshness.LastCaptureAt)) {
			t := p.UpdatedAt
			state.Freshness.LastCaptureAt = &t
		}
		if p.ActivityAt != nil && (state.Freshness.LastActivityAt == nil || p.ActivityAt.After(*state.Freshness.LastActivityAt)) {
			t := *p.ActivityAt
			state.Freshness.LastActivityAt = &t
		}
	}
	seenOps := map[string]bool{}
	for _, op := range out.Commits {
		if (op.Version != 1 && op.Version != domain.StagingCommitVersion && op.Version != domain.RootStagingCommitVersion) || (op.Index.HasRootDocuments() != (op.Version == domain.RootStagingCommitVersion)) || op.Index.RepoID != repo.ID || op.Index.WorktreeID != p.WorktreeID || seenOps[op.ID] {
			return out, domain.ErrHashMismatch
		}
		seenOps[op.ID] = true
		if err := domain.ValidateStagingIndex(op.Index); err != nil {
			return out, err
		}
		ids := []string{}
		for _, e := range op.Publications {
			ids = append(ids, e.ID)
		}
		if op.Position.Selection != nil {
			ids = append(ids, op.Position.Selection.ID)
		}
		sort.Strings(ids)
		state.LocalCommits = append(state.LocalCommits, domain.LocalCommitSummary{OperationID: op.ID, Target: op.Position.Snapshot, IndexRevision: op.Index.Revision, CreatedAt: op.CreatedAt, LocalFinalized: op.LocalFinalized, SelectedHistory: reachable[op.Position.Snapshot], PublicationIDs: ids, ServerReceiptState: "unknown"})
	}
	if s.pulls != nil && s.remote != "" {
		receipt, err := s.pulls.ReadAppliedPull(ctx, repo.ID, s.remote)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return out, err
		}
		if err == nil {
			plan := receipt.Plan
			if plan.Expected.Position == nil || plan.RepoID != repo.ID || plan.Remote != s.remote || plan.Expected.Position.WorktreeID != p.WorktreeID || plan.ID != outbound.SelectedPullPlanID(plan) {
				return out, domain.ErrHashMismatch
			}
			matches := state.Selection.CodeMatchesSelection && plan.Selection.CodeCommit == commit &&
				plan.Selection.Position == string(p.Snapshot) && plan.Selection.Branch == p.Branch &&
				plan.Expected.Position.BranchID == p.BranchID && plan.Expected.Position.GitBranch() == branch
			state.AppliedProjection = &domain.AppliedProjectionSummary{ReceiptID: plan.ID, ContextStateHash: plan.Context.StateHash, GitCommit: plan.Selection.CodeCommit, AppliedAt: receipt.AppliedAt, MatchesSelection: matches}
			if len(plan.Memory) > 0 {
				state.AppliedProjection.MemoryStateHash = plan.Memory[0].StateHash
			}
			if !state.AppliedProjection.MatchesSelection {
				state.Gaps = append(state.Gaps, "applied_projection_for_previous_selection")
			}
		}
	}
	// Include full manifests/position in the fence even though diagnostics omit
	// potentially sensitive snapshot messages, provider paths and command data.
	out.State = state
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.State.Revision = domain.HashContent(raw)
	return out, nil
}

var _ inbound.WorkingStateQuery = (*WorkingStateService)(nil)
var _ inbound.ContextDiffQuery = (*WorkingStateService)(nil)
