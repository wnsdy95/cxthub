package store

import (
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// appendPlan is computed while the repository graph lock is held. Only incoming
// segment boundaries receive an overlay; natural parents stay immutable.
func appendPlan(snaps []domain.Snapshot, old, next domain.ContentHash) ([]domain.GraftPatch, error) {
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
	}
	walk := func(tip domain.ContentHash) (map[domain.ContentHash]bool, error) {
		seen := map[domain.ContentHash]bool{}
		stack := []domain.ContentHash{tip}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				continue
			}
			s, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("%w: missing append ancestor %s", domain.ErrIntegrity, id)
			}
			seen[id] = true
			stack = append(stack, s.ReachabilityParents()...)
		}
		return seen, nil
	}
	shared, err := walk(old)
	if err != nil {
		return nil, err
	}
	incoming, err := walk(next)
	if err != nil {
		return nil, err
	}
	if shared[next] || incoming[old] {
		return nil, domain.ErrRefConflict
	}
	var ids []domain.ContentHash
	for id := range incoming {
		if !shared[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	patches := []domain.GraftPatch{}
	for _, id := range ids {
		snap := byID[id]
		boundary := len(snap.Parents) == 0
		for _, p := range snap.Parents {
			if shared[p] {
				boundary = true
				break
			}
		}
		if !boundary {
			continue
		}
		if snap.GraftSeq == domain.MaxGraftSeq {
			return nil, domain.ErrConflict
		}
		parents := dedupHashParents(id, snap.Parents, append(append([]domain.ContentHash{}, snap.GraftParents...), old))
		patches = append(patches, domain.GraftPatch{SnapshotID: id, ExpectedSeq: snap.GraftSeq, Parents: parents})
	}
	if len(patches) == 0 {
		return nil, domain.ErrIntegrity
	}
	return patches, nil
}
