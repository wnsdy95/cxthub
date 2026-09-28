package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// A metadata-only request still verifies its complete stored document while
// holding the server repository transaction. Publish one snapshot per request
// so a large retained backlog does not monopolize that lock. Ref publication
// remains a later phase, after every object and memory/history dependency.
func (s *SyncRepoService) publishSnapshotObjects(ctx context.Context, repo string, ordered []domain.Snapshot, progress inbound.SyncInput) error {
	for i, snap := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.remote.Push(ctx, repo, []domain.Snapshot{snap}, nil, nil, false, false); err != nil {
			return err
		}
		syncProgress(progress, "push", "publish-snapshots", i+1, len(ordered))
	}
	return nil
}

// Natural parents must exist before their children can be accepted separately.
// Overlay parents are server-owned and are not sent by snapshotForCreate; graft
// commands run only after all prerequisite snapshots exist. Do not turn a stale
// local overlay into an additional upload dependency or rewrite either lineage.
func orderSnapshotPublication(snaps []domain.Snapshot) ([]domain.Snapshot, error) {
	byID := make(map[domain.ContentHash]int, len(snaps))
	for i, snap := range snaps {
		if _, ok := byID[snap.ID]; ok {
			return nil, fmt.Errorf("%w: duplicate snapshot in upload", domain.ErrHashMismatch)
		}
		byID[snap.ID] = i
	}
	degree := make([]int, len(snaps))
	children := make([][]int, len(snaps))
	for i, snap := range snaps {
		for _, parent := range snap.Parents {
			if p, ok := byID[parent]; ok {
				degree[i]++
				children[p] = append(children[p], i)
			}
		}
	}
	ready := make([]int, 0, len(snaps))
	for i, d := range degree {
		if d == 0 {
			ready = append(ready, i)
		}
	}
	out := make([]domain.Snapshot, 0, len(snaps))
	for head := 0; head < len(ready); head++ {
		i := ready[head]
		out = append(out, snaps[i])
		for _, child := range children[i] {
			degree[child]--
			if degree[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(out) != len(snaps) {
		return nil, fmt.Errorf("%w: cyclic natural parents in upload", domain.ErrHashMismatch)
	}
	return out, nil
}
