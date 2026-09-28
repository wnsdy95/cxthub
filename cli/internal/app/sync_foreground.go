package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// foregroundSnapshots selects a complete local dependency closure. It is not a
// retention policy: everything outside the closure remains a backfill obligation.
// Both conversation parents and memory-only provenance participate. Unknown
// external memory sources are not invented locally; existing server validation
// still decides their eligibility, as it does for an unsplit full push.
func (s *SyncRepoService) foregroundSnapshots(ctx context.Context, repo, root string, snapshots []domain.Snapshot, refs []domain.Ref, history []domain.HistoryEvent, remoteMemory map[domain.ContentHash]domain.ContentHash) ([]domain.Snapshot, []domain.Snapshot, error) {
	var pending []domain.ContentHash
	add := func(id domain.ContentHash) {
		if id != "" {
			pending = append(pending, id)
		}
	}
	for _, ref := range refs {
		add(ref.Target)
	}
	var memory []domain.ContentHash
	for _, event := range history {
		for _, id := range []domain.ContentHash{event.Source, event.Target, event.SharedTarget, event.MemorySource} {
			add(id)
		}
		if event.MemoryHash != "" {
			memory = append(memory, event.MemoryHash)
		}
	}
	pendings, err := s.store.ListPendings(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range pendings {
		add(p.Target)
	}
	if root != "" {
		err := s.outbox.WithGrafts(ctx, root, func(q outbound.GraftQueueAccess) error {
			events, err := q.Load()
			if err != nil {
				return err
			}
			for _, event := range events {
				add(domain.ContentHash(event.Snapshot))
				for _, parent := range event.Parents {
					add(domain.ContentHash(parent))
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		promotions, err := s.outbox.ListPromotions(ctx, root)
		if err != nil {
			return nil, nil, err
		}
		for id := range promotions {
			add(id)
		}
	}
	return s.snapshotDependencyClosure(ctx, snapshots, pending, memory, remoteMemory)
}

func (s *SyncRepoService) snapshotDependencyClosure(ctx context.Context, snapshots []domain.Snapshot, roots, memory []domain.ContentHash, remoteMemory map[domain.ContentHash]domain.ContentHash) ([]domain.Snapshot, []domain.Snapshot, error) {
	byID := make(map[domain.ContentHash]domain.Snapshot, len(snapshots))
	for _, snap := range snapshots {
		byID[snap.ID] = snap
	}
	var pending []domain.ContentHash
	add := func(id domain.ContentHash) {
		if _, ok := byID[id]; ok {
			pending = append(pending, id)
		}
	}
	for _, id := range roots {
		add(id)
	}
	seen, seenMemory := map[domain.ContentHash]bool{}, map[domain.ContentHash]bool{}
	// An authoritative server attachment proves that this immutable memory and
	// its publication prerequisites already exist remotely. Local metadata for
	// otherwise-unselected source snapshots remains a backfill obligation.
	for _, hash := range remoteMemory {
		if hash != "" {
			seenMemory[hash] = true
		}
	}
	for len(pending) > 0 || len(memory) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(pending) > 0 {
			id := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			snap := byID[id]
			for _, parent := range snap.ReachabilityParents() {
				add(parent)
			}
			if snap.MemoryHash != "" {
				memory = append(memory, snap.MemoryHash)
			}
			continue
		}
		hash := memory[len(memory)-1]
		memory = memory[:len(memory)-1]
		if seenMemory[hash] {
			continue
		}
		seenMemory[hash] = true
		digest, err := s.store.GetMemory(ctx, hash)
		if err != nil {
			return nil, nil, fmt.Errorf("read foreground memory dependency %s: %w", hash, err)
		}
		if err := validateMemoryAttachmentObject(digest, hash, digest.SnapshotID); err != nil {
			return nil, nil, err
		}
		add(digest.SnapshotID)
		for _, fragment := range digest.Fragments {
			add(fragment.SourceSnapshot)
		}
		if coverage := digest.GraftCoverage; coverage != nil {
			for _, id := range coverage.PinnedSources {
				add(id)
			}
			for _, id := range coverage.GraftParents {
				add(id)
			}
		}
		if digest.PreviousMemoryHash != "" {
			memory = append(memory, digest.PreviousMemoryHash)
		}
	}
	var foreground, retained []domain.Snapshot
	for _, snap := range snapshots {
		if seen[snap.ID] {
			foreground = append(foreground, snap)
		} else {
			retained = append(retained, snap)
		}
	}
	return foreground, retained, nil
}
