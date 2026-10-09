package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"sort"
)

// PrepareFrozenMemory writes only immutable objects. The caller must persist
// the returned plan before SaveFrozen/FinishFrozenMemory can change a pointer.
// index -1 produces the final aggregate even when an earlier provider's dedup
// snapshot is the only target that contains all captured conversations.
func (s *SaveSessionService) PrepareFrozenMemory(ctx context.Context, cwd string, p domain.CaptureAttempt, index int) (*domain.FrozenCaptureMemory, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := s.verifyFrozenPredecessor(ctx, p); err != nil {
		return nil, err
	}
	if s.frozenDistiller == nil {
		return nil, nil
	}
	if !p.InputsReady || p.Version != 2 || index < -1 || index >= len(p.Outcomes) {
		return nil, domain.ErrHashMismatch
	}
	if index == -1 {
		saved := false
		for _, o := range p.Outcomes {
			saved = saved || o.State == "saved"
		}
		if !saved {
			// The worker already recorded B's immutable continuation of A.
			// With no new contribution, preserve that exact pin rather than
			// replacing a later attachment on A with its older frozen content.
			return nil, nil
		}
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if repo.ID != p.Proof.RepoID {
		return nil, domain.ErrSelectionChanged
	}
	retention, ok := s.store.(outbound.ObjectRetention)
	if !ok {
		return nil, fmt.Errorf("memory retention unavailable")
	}
	var plan *domain.FrozenCaptureMemory
	err = retention.WithObjectsRetained(ctx, func() error {
		history, ok := s.store.(outbound.HistoryStore)
		if !ok {
			return fmt.Errorf("capture history unavailable")
		}
		baseline := p.Proof
		baseline.Source, baseline.Target = p.Initial, p.Initial
		if _, err := NewContextHistoryService(s.store, history).ValidateHistorySource(ctx, baseline); err != nil {
			return err
		}
		digest := domain.MemoryDigest{}
		if p.Proof.MemoryHash != "" {
			var err error
			digest, err = s.store.GetMemory(ctx, p.Proof.MemoryHash)
			if err != nil {
				return err
			}
		}
		if predecessor := p.PredecessorObservation; predecessor != nil && predecessor.MemoryHash != "" {
			memory, err := s.store.GetMemory(ctx, predecessor.MemoryHash)
			if err != nil {
				return err
			}
			digest = domain.MergeDigests(digest, memory)
		}
		count := index
		if index == -1 {
			count = len(p.Outcomes)
		}
		for _, o := range p.Outcomes[:count] {
			if o.State == "absent" {
				continue
			}
			if o.State != "saved" {
				return fmt.Errorf("preceding capture is incomplete")
			}
			if o.MemoryHash != "" {
				m, err := s.store.GetMemory(ctx, o.MemoryHash)
				if err != nil {
					return err
				}
				digest = domain.MergeDigests(digest, m)
			}
		}
		target := p.Proof.Target
		observationID := domain.CaptureFinalObservationID(p.Proof.ID)
		if index >= 0 {
			o := p.Outcomes[index]
			if o.Input == nil {
				return domain.ErrHashMismatch
			}
			in := *o.Input
			frozen, ok := s.capture.(outbound.FrozenSessionCapture)
			if !ok {
				return fmt.Errorf("frozen capture unavailable")
			}
			src, ok := s.captures[in.Provider]
			if !ok {
				return domain.ErrUnsupportedProvider
			}
			codec, ok := s.codecs[in.Provider]
			if !ok {
				return domain.ErrUnsupportedProvider
			}
			env, ref, _, _, err := frozen.ProjectFrozen(ctx, repo.LocalPath, in, src, codec)
			if err != nil {
				return err
			}
			if env.SessionOriginID == "" || in.SessionID == "" || env.SessionOriginID != in.SessionID || (o.SessionID != "" && env.SessionOriginID != o.SessionID) {
				return domain.ErrHashMismatch
			}
			doc, err := readDocumentReference(ctx, s.store, ref)
			if err != nil {
				return err
			}
			native, err := frozen.ScrubFrozenMemory(ctx, repo.LocalPath, in)
			if err != nil {
				return err
			}
			fresh, err := s.frozenDistiller.Distill(ctx, doc.CIR.EffectiveContext(), native)
			if err != nil {
				return err
			}
			fresh.SnapshotID = ref.Hash
			digest = domain.MergeDigests(digest, fresh)
			target = ref.Hash
			observationID = domain.CaptureProviderObservationID(p.Proof.ID, index)
		}
		if target == "" {
			return nil
		}
		digest.SnapshotID = target
		// All carried fragments are intentional frozen imports. We did not project
		// today's mutable graph and cannot claim complete graph coverage.
		coverage := &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion}
		seen := map[domain.ContentHash]bool{}
		for _, f := range digest.Fragments {
			if f.SourceSnapshot != "" && f.SourceSnapshot != target && !seen[f.SourceSnapshot] {
				seen[f.SourceSnapshot] = true
				coverage.PinnedSources = append(coverage.PinnedSources, f.SourceSnapshot)
			}
		}
		sort.Slice(coverage.PinnedSources, func(i, j int) bool { return coverage.PinnedSources[i] < coverage.PinnedSources[j] })
		digest.GraftCoverage = coverage
		previous := domain.ContentHash("")
		snap, err := s.store.GetSnapshot(ctx, target)
		if err == nil {
			if snap.RepoID != repo.ID {
				return domain.ErrHashMismatch
			}
			previous = snap.MemoryHash
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		} else if index >= 0 && p.Proof.MemoryHash != "" {
			// Normal/live Save imports the selected memory when first creating a
			// snapshot. Freeze the identical initial version so that creation by
			// either producer is compatible; later, genuinely different versions
			// still lose the attachment CAS without being overwritten.
			memory, err := s.store.GetMemory(ctx, p.Proof.MemoryHash)
			if err != nil {
				return err
			}
			inherited := domain.MergeDigests(memory, domain.MemoryDigest{SnapshotID: target, Provider: p.Outcomes[index].Provider})
			inherited.PreviousMemoryHash = ""
			previous, err = s.store.PutMemory(ctx, inherited)
			if err != nil {
				return err
			}
		}
		digest.PreviousMemoryHash = previous
		if previous != "" {
			old, err := s.store.GetMemory(ctx, previous)
			if err != nil {
				return err
			}
			if old.SnapshotID != target {
				return domain.ErrHashMismatch
			}
			if sameMemoryDigestPayload(old, digest) {
				digest = old
			}
		}
		hash, err := s.store.PutMemory(ctx, digest)
		if err != nil {
			return err
		}
		plan = &domain.FrozenCaptureMemory{Version: 1, Snapshot: target, ExpectedMemory: previous, Memory: hash}
		// Empty/inherited -> first self-owned memory is an explicit transition,
		// not an order inferred from timestamps or mutable attachment pointers.
		{
			rootHash, err := s.frozenMemoryRoot(ctx, target, hash)
			if err != nil {
				return err
			}
			events, err := history.ListHistoryEvents(ctx, repo.ID)
			if err != nil {
				return err
			}
			candidates := map[string]bool{
				domain.CaptureBaselineObservationID(p.Proof.ID):     true,
				domain.CaptureContinuationObservationID(p.Proof.ID): true,
			}
			for i, o := range p.Outcomes {
				if o.State == "saved" {
					candidates[domain.CaptureProviderObservationID(p.Proof.ID, i)] = true
				}
			}
			after := p.Proof
			after.ID = observationID
			after.Source, after.Target = target, target
			after.MemoryHash, after.MemorySource, after.MemoryPinned = rootHash, target, true
			if rootHash != hash {
				after.ID = domain.CaptureBaselineObservationID(observationID)
			}
			for _, e := range events {
				if !candidates[e.ID] {
					continue
				}
				after.MemorySelectionParent = e.ID
				if domain.IsInitialMemorySelection(e, after) {
					if plan.SelectionParent != "" && plan.SelectionParent != e.ID {
						return domain.ErrSyncConflict
					}
					if rootHash == hash {
						plan.SelectionParent = e.ID
					} else {
						if plan.RootSelection != nil && plan.RootSelection.MemorySelectionParent != e.ID {
							return domain.ErrSyncConflict
						}
						copy := after
						plan.RootSelection = &copy
					}
				}
			}
		}
		return plan.Validate()
	})
	return plan, err
}

// Follow only immutable predecessor hashes from the supplied pin. Snapshot
// attachments and current selections are not evidence for this root.
func (s *SaveSessionService) frozenMemoryRoot(ctx context.Context, owner, hash domain.ContentHash) (domain.ContentHash, error) {
	seen := map[domain.ContentHash]bool{}
	for hash != "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if len(seen) >= maxMemoryAttachmentDepth || seen[hash] {
			return "", domain.ErrHashMismatch
		}
		seen[hash] = true
		digest, err := s.store.GetMemory(ctx, hash)
		if err != nil {
			return "", err
		}
		if err := validateMemoryAttachmentObject(digest, hash, owner); err != nil {
			return "", err
		}
		if digest.PreviousMemoryHash == "" {
			return hash, nil
		}
		hash = digest.PreviousMemoryHash
	}
	return "", domain.ErrHashMismatch
}

func (s *SaveSessionService) attachFrozenMemory(ctx context.Context, repo string, p domain.FrozenCaptureMemory) error {
	if err := p.Validate(); err != nil {
		return err
	}
	// A later causal child proves this exact version was already attached. It
	// must not be rolled back after a lost worker acknowledgement.
	snap, err := s.store.GetSnapshot(ctx, p.Snapshot)
	if err != nil {
		return err
	}
	if snap.RepoID != repo {
		return domain.ErrHashMismatch
	}
	hash := snap.MemoryHash
	seen := map[domain.ContentHash]bool{}
	for hash != "" {
		if len(seen) >= maxMemoryAttachmentDepth || seen[hash] {
			return domain.ErrHashMismatch
		}
		if hash == p.Memory {
			return nil
		}
		seen[hash] = true
		m, err := s.store.GetMemory(ctx, hash)
		if err != nil {
			return err
		}
		if err := validateMemoryAttachmentObject(m, hash, p.Snapshot); err != nil {
			return err
		}
		hash = m.PreviousMemoryHash
	}
	committer, ok := s.store.(outbound.WorkingMemoryStore)
	if !ok {
		return fmt.Errorf("conditional memory attachment unavailable")
	}
	return committer.CommitWorkingMemory(ctx, outbound.WorkingMemoryCommit{RepoID: repo, Snapshot: p.Snapshot, ExpectedMemory: p.ExpectedMemory, Memory: p.Memory})
}

func (s *SaveSessionService) FinishFrozenMemory(ctx context.Context, cwd string, p domain.CaptureAttempt) (domain.HistoryEvent, error) {
	var empty domain.HistoryEvent
	if err := p.Validate(); err != nil {
		return empty, err
	}
	if err := s.verifyFrozenPredecessor(ctx, p); err != nil {
		return empty, err
	}
	if p.FinalMemory == nil {
		return empty, nil
	}
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return empty, err
	}
	if repo.ID != p.Proof.RepoID {
		return empty, domain.ErrSelectionChanged
	}
	retention, ok := s.store.(outbound.ObjectRetention)
	if !ok {
		return empty, fmt.Errorf("memory retention unavailable")
	}
	event := p.Proof
	event.ID = domain.CaptureFinalObservationID(p.Proof.ID)
	event.Source, event.Target = p.FinalMemory.Snapshot, p.FinalMemory.Snapshot
	event.MemoryHash, event.MemorySource, event.MemoryPinned = p.FinalMemory.Memory, p.FinalMemory.Snapshot, true
	event.MemorySelectionParent = p.FinalMemory.SelectionParent
	err = retention.WithObjectsRetained(ctx, func() error {
		if err := s.attachFrozenMemory(ctx, repo.ID, *p.FinalMemory); err != nil {
			return err
		}
		history, ok := s.store.(outbound.HistoryStore)
		if !ok {
			return fmt.Errorf("capture history unavailable")
		}
		if p.FinalMemory.RootSelection != nil {
			if err := history.PutHistoryEvent(ctx, *p.FinalMemory.RootSelection); err != nil {
				return err
			}
		}
		if _, err := NewContextHistoryService(s.store, history).ValidateHistorySource(ctx, event); err != nil {
			return err
		}
		return history.PutHistoryEvent(ctx, event)
	})
	return event, err
}
