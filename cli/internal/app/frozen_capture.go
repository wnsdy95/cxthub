package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ inbound.CommitCapture = (*SaveSessionService)(nil)

// The same deterministic distiller used by Memorize operates only on frozen
// inputs during replay. Native files are read at FreezeInput, never on retry.
func (s *SaveSessionService) WithFrozenMemory(sources map[domain.ProviderKind]outbound.MemorySource, distiller outbound.MemoryDistiller) *SaveSessionService {
	s.frozenMemorySources, s.frozenDistiller = sources, distiller
	return s
}

func (s *SaveSessionService) FreezeInput(ctx context.Context, cwd string, provider domain.ProviderKind, path string) (domain.FrozenCaptureInput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return domain.FrozenCaptureInput{}, err
	}
	capture, ok := s.capture.(outbound.FrozenSessionCapture)
	if !ok {
		return domain.FrozenCaptureInput{}, fmt.Errorf("durable capture input unavailable")
	}
	source, ok := s.captures[provider]
	if !ok {
		return domain.FrozenCaptureInput{}, domain.ErrUnsupportedProvider
	}
	if path == "" {
		path, err = source.LocateActiveSession(ctx, cwd)
		if err != nil {
			return domain.FrozenCaptureInput{}, err
		}
	}
	if !s.capture.Eligible(repo.LocalPath, path) {
		return domain.FrozenCaptureInput{}, domain.ErrNoActiveSession
	}
	identity, err := captureDocumentIdentity(ctx, s.gitCtx, repo, "")
	if err != nil {
		return domain.FrozenCaptureInput{}, err
	}
	input, err := capture.Freeze(ctx, repo.LocalPath, path, provider, identity)
	if err != nil {
		return input, err
	}
	input.SessionID, err = capture.FrozenSessionID(ctx, repo.LocalPath, input)
	if err != nil {
		return domain.FrozenCaptureInput{}, err
	}
	if input.SessionID == "" && s.frozenDistiller != nil {
		return domain.FrozenCaptureInput{}, fmt.Errorf("frozen session identity unavailable; native memory absence cannot be confirmed")
	}
	if nativeSource, ok := s.frozenMemorySources[provider]; ok && input.SessionID != "" {
		native, found, readErr := nativeSource.ReadNative(ctx, cwd, input.SessionID)
		if readErr != nil {
			return domain.FrozenCaptureInput{}, readErr
		}
		if found {
			input.NativeMemory = &native
		}
	}
	return input, input.Validate()
}

func (s *SaveSessionService) FreezeSettings(ctx context.Context, cwd string) (map[string]domain.ContentHash, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	result := map[string]domain.ContentHash{}
	for _, kind := range []string{"claude", "agents", "codex"} {
		if bundle, ok := s.capture.Settings(repo.LocalPath, kind); ok {
			hash, err := s.store.PutSettingsObject(ctx, bundle)
			if err != nil {
				return nil, err
			}
			result[kind] = hash
		}
	}
	return result, ctx.Err()
}

// SaveFrozen retains a historical observation. It deliberately does not use
// Save's mutable branch/position/settings/provider-memory discovery path.
func (s *SaveSessionService) SaveFrozen(ctx context.Context, cwd string, p domain.CaptureAttempt, index int) (out inbound.SaveOutput, err error) {
	if err = p.Validate(); err != nil {
		return out, err
	}
	if p.Version != 2 || !p.InputsReady || index < 0 || index >= len(p.Outcomes) || p.Outcomes[index].Input == nil {
		return out, domain.ErrHashMismatch
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return out, err
	}
	if repo.ID != p.Proof.RepoID {
		return out, domain.ErrSelectionChanged
	}
	frozen, ok := s.capture.(outbound.FrozenSessionCapture)
	if !ok {
		return out, fmt.Errorf("durable capture input unavailable")
	}
	retention, ok := s.store.(outbound.ObjectRetention)
	if !ok {
		return out, fmt.Errorf("durable capture requires coordinated object retention")
	}
	history, ok := s.store.(outbound.HistoryStore)
	if !ok {
		return out, fmt.Errorf("durable capture history unavailable")
	}
	input := *p.Outcomes[index].Input
	source, sourceOK := s.captures[input.Provider]
	codec, codecOK := s.codecs[input.Provider]
	if !sourceOK || !codecOK {
		return out, domain.ErrUnsupportedProvider
	}
	err = retention.WithObjectsRetained(ctx, func() error {
		for kind, hash := range p.Settings {
			bundle, err := s.store.GetSettingsObject(ctx, hash)
			if err != nil {
				return err
			}
			if err := domain.ValidateSettingsBundle(kind, hash, bundle); err != nil {
				return err
			}
		}
		// Verify immutable memory provenance and baseline bytes before projection.
		baseline := p.Proof
		baseline.Source, baseline.Target = p.Initial, p.Initial
		if _, err := NewContextHistoryService(s.store, history).ValidateHistorySource(ctx, baseline); err != nil {
			return err
		}
		env, ref, n, _, err := frozen.ProjectFrozen(ctx, repo.LocalPath, input, source, codec)
		if err != nil {
			return err
		}
		if env.SourceProvider != input.Provider || env.SessionOriginID == "" || (input.SessionID != "" && env.SessionOriginID != input.SessionID) || (p.Outcomes[index].SessionID != "" && env.SessionOriginID != p.Outcomes[index].SessionID) || ref.Validate() != nil {
			return domain.ErrHashMismatch
		}
		parents := []domain.ContentHash{}
		seen := map[domain.ContentHash]bool{}
		addParent := func(h domain.ContentHash) {
			if h != "" && h != ref.Hash && !seen[h] {
				seen[h] = true
				parents = append(parents, h)
			}
		}
		addParent(p.Initial)
		for _, previous := range p.Outcomes[:index] {
			if previous.State == "saved" {
				addParent(previous.Target)
			} else if previous.State != "absent" {
				return fmt.Errorf("preceding provider is not complete")
			}
		}
		memoryHash, memorySource := p.Proof.MemoryHash, p.Proof.MemorySource
		if memoryHash != "" {
			memory, err := s.store.GetMemory(ctx, memoryHash)
			if err != nil {
				return err
			}
			memorySource = memory.SnapshotID
		}
		if s.frozenDistiller != nil {
			plan := p.Outcomes[index].MemoryPlan
			if plan == nil || plan.Validate() != nil || plan.Snapshot != ref.Hash {
				return domain.ErrHashMismatch
			}
			memoryHash, memorySource = plan.Memory, ref.Hash
		}
		author := domain.TeamIdentity{}
		if p.Author != nil {
			author = *p.Author
		}
		snap := domain.Snapshot{ID: ref.Hash, RepoID: repo.ID, Branch: p.Proof.Branch, Parents: parents, DocHash: ref.Hash, DocIdentity: ref.Identity, Provider: input.Provider, Fidelity: env.Fidelity, Message: p.Message, Author: author, CreatedAt: p.Proof.CreatedAt, SessionID: env.SessionOriginID, Models: env.OrderedModels(), CompactionCount: env.CompactionCount, ClaudeSettings: p.Settings["claude"], AgentsSettings: p.Settings["agents"], CodexSettings: p.Settings["codex"]}
		existing, readErr := s.store.GetSnapshot(ctx, ref.Hash)
		if readErr != nil && !errors.Is(readErr, domain.ErrNotFound) {
			return readErr
		}
		if readErr == nil {
			if existing.RepoID != repo.ID || existing.DocumentRef() != ref {
				return domain.ErrHashMismatch
			}
			// PutSnapshot promotes a hook message while retaining natural parents,
			// graft registers and concurrent memory attachments.
			if err := s.store.PutSnapshot(ctx, snap); err != nil {
				return err
			}
		} else {
			if memoryHash != "" && s.frozenDistiller == nil {
				memory, err := s.store.GetMemory(ctx, memoryHash)
				if err != nil {
					return err
				}
				inherited := memory
				if memory.SnapshotID != ref.Hash {
					inherited = domain.MergeDigests(memory, domain.MemoryDigest{SnapshotID: ref.Hash, Provider: input.Provider})
				}
				inherited.PreviousMemoryHash = ""
				snap.MemoryHash, err = s.store.PutMemory(ctx, inherited)
				if err != nil {
					return err
				}
			}
			if err := s.store.PutSnapshot(ctx, snap); err != nil {
				// A concurrent attachment may win this first creation. Keep
				// its pointer; our exact version is pinned in the observation.
				if !errors.Is(err, domain.ErrSyncConflict) {
					return err
				}
				snap.MemoryHash = ""
				if err := s.store.PutSnapshot(ctx, snap); err != nil {
					return err
				}
			}
		}
		// Retrying repairs enqueue after local promotion succeeded. Use the
		// winning stored label, never overwrite it with a stale capture's label.
		if s.outbox != nil && !strings.HasPrefix(p.Message, domain.HookMessagePrefix) {
			winner, err := s.store.GetSnapshot(ctx, ref.Hash)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(winner.Message, domain.HookMessagePrefix) {
				if err := s.outbox.EnqueuePromotion(ctx, repo.LocalPath, ref.Hash, winner.Message); err != nil {
					return err
				}
			}
		}

		for _, parent := range parents {
			contains, err := s.frozenReachable(ctx, repo.ID, ref.Hash, parent)
			if err != nil {
				return err
			}
			reverse, err := s.frozenReachable(ctx, repo.ID, parent, ref.Hash)
			if err != nil {
				return err
			}
			if !contains && !reverse {
				if err := s.graftLocalAndQueueChecked(ctx, repo.LocalPath, ref.Hash, parent, func() (bool, error) {
					// Recheck inside the shared graft queue lock; another capture
					// may have installed the reverse path after our first read.
					reverse, err := s.frozenReachable(ctx, repo.ID, parent, ref.Hash)
					return !reverse, err
				}); err != nil {
					return err
				}
			}
		}

		if plan := p.Outcomes[index].MemoryPlan; plan != nil {
			if err := s.attachFrozenMemory(ctx, repo.ID, *plan); err != nil {
				return err
			}
			if plan.RootSelection != nil {
				if err := history.PutHistoryEvent(ctx, *plan.RootSelection); err != nil {
					return err
				}
			}
		}

		observation := p.Proof
		observation.ID = domain.CaptureProviderObservationID(p.Proof.ID, index)
		observation.Source, observation.Target = ref.Hash, ref.Hash
		observation.MemoryHash, observation.MemorySource, observation.MemoryPinned = memoryHash, memorySource, true
		if plan := p.Outcomes[index].MemoryPlan; plan != nil {
			observation.MemorySelectionParent = plan.SelectionParent
		}
		if _, err := NewContextHistoryService(s.store, history).ValidateHistorySource(ctx, observation); err != nil {
			return err
		}
		if err := history.PutHistoryEvent(ctx, observation); err != nil {
			return err
		}
		out = inbound.SaveOutput{SnapshotID: ref.Hash, Branch: p.Proof.Branch, SessionID: env.SessionOriginID, CapturedBytes: n, MemoryHash: memoryHash, MemorySource: memorySource}
		return ctx.Err()
	})
	return out, err
}

// Unlike the legacy best-effort ancestry helper, replay never treats a read
// failure as permission to add an edge.
func (s *SaveSessionService) frozenReachable(ctx context.Context, repo string, tip, ancestor domain.ContentHash) (bool, error) {
	seen := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{tip}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == ancestor {
			return true, nil
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return false, err
		}
		if snap.RepoID != repo {
			return false, domain.ErrHashMismatch
		}
		stack = append(stack, snap.ReachabilityParents()...)
	}
	return false, ctx.Err()
}

// ApplyFrozen may follow a completed capture only while the full original
// selection still matches. Historical completion never rewinds active work.
func (s *SaveSessionService) ApplyFrozen(ctx context.Context, cwd string, p domain.CaptureAttempt) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Version != 2 || !p.Complete || p.Proof.Target == "" {
		return nil
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	if repo.ID != p.Proof.RepoID {
		return domain.ErrSelectionChanged
	}
	positions, ok := s.store.(outbound.WorkingPositionStore)
	if !ok {
		return nil
	}
	commits, ok := s.store.(outbound.FrozenCommitStore)
	if !ok {
		return nil
	}
	code, ok := s.gitCtx.(outbound.CodePosition)
	if !ok {
		return nil
	}
	sha, err := code.CurrentCommit(ctx, cwd)
	if err != nil {
		return err
	}
	branch, err := s.gitCtx.CurrentBranch(ctx, cwd)
	if err != nil {
		return err
	}
	if sha != p.Proof.GitAfter || branch != p.FrozenPosition.GitBranch() {
		return nil
	}
	current, err := positions.GetWorkingPosition(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, *p.FrozenPosition) {
		return nil
	}
	ref, err := s.store.GetRef(ctx, repo.ID, domain.RefBranch, p.Proof.Branch)
	if errors.Is(err, domain.ErrNotFound) && current.SharedTarget == "" {
		ref = domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: p.Proof.Branch, BranchID: p.Proof.BranchID}
		err = nil
	}
	if err != nil {
		return err
	}
	if ref.Target != current.SharedTarget || ref.BranchID != "" && ref.BranchID != current.BranchID {
		return nil
	}
	next := current
	next.Snapshot, next.SharedTarget, next.GitCommit = p.Proof.Target, p.Proof.Target, p.Proof.GitAfter
	next.Rewound, next.Orphan = false, false
	next.Selection = p.Observation
	if p.Observation != nil {
		next.MemoryHash, next.MemorySource, next.MemoryPinned = p.Observation.MemoryHash, p.Observation.MemorySource, true
	}
	ref.Target, ref.BranchID = p.Proof.Target, p.Proof.BranchID
	err = commits.CommitFrozenSnapshotIfCurrent(ctx, ref, current.SharedTarget, current, next, nil)
	if errors.Is(err, domain.ErrSyncConflict) || errors.Is(err, domain.ErrSelectionChanged) || errors.Is(err, domain.ErrBranchArchived) {
		return nil
	}
	return err
}
