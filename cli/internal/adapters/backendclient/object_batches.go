package backendclient

import (
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Bound reassembly work, not merely the manifest's small wire size. Each request
// finalizes at most one document and one snapshot. Natural parents precede their
// children; immutable objects may remain after failure, but refs move only after
// every request is acknowledged. Retries negotiate the remaining objects.
func objectCommitBatches(snaps []domain.Snapshot, docs []domain.SessionDoc, manifests []chunkedDocWire, chunks []chunkObjWire) ([]objectsReq, error) {
	byID := make(map[domain.ContentHash]int, len(snaps))
	for i, s := range snaps {
		if _, ok := byID[s.ID]; ok {
			return nil, fmt.Errorf("%w: duplicate push snapshot %s", domain.ErrHashMismatch, s.ID)
		}
		byID[s.ID] = i
	}
	remaining := make([]int, len(snaps))
	children := make([][]int, len(snaps))
	ready := make([]int, 0, len(snaps))
	for i, s := range snaps {
		seen := map[domain.ContentHash]bool{}
		for _, p := range s.Parents {
			if seen[p] {
				return nil, fmt.Errorf("%w: duplicate parent of push snapshot %s", domain.ErrHashMismatch, s.ID)
			}
			seen[p] = true
			if j, ok := byID[p]; ok {
				remaining[i]++
				children[j] = append(children[j], i)
			}
		}
		if remaining[i] == 0 {
			ready = append(ready, i)
		}
	}
	for i := 0; i < len(ready); i++ {
		for _, child := range children[ready[i]] {
			remaining[child]--
			if remaining[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(ready) != len(snaps) {
		return nil, fmt.Errorf("%w: cycle in push snapshots", domain.ErrHashMismatch)
	}
	bodies := map[domain.ContentHash]domain.SessionDoc{}
	metadata := map[domain.ContentHash]chunkedDocWire{}
	for _, d := range docs {
		if _, ok := bodies[d.Hash]; ok {
			return nil, domain.ErrHashMismatch
		}
		bodies[d.Hash] = d
	}
	for _, m := range manifests {
		if _, ok := metadata[m.Hash]; ok {
			return nil, domain.ErrHashMismatch
		}
		if _, ok := bodies[m.Hash]; ok {
			return nil, domain.ErrHashMismatch
		}
		metadata[m.Hash] = m
	}
	chunkByID := map[domain.ContentHash]chunkObjWire{}
	for _, ch := range chunks {
		chunkByID[ch.Hash] = ch
	}
	take := func(id domain.ContentHash) objectsReq {
		var batch objectsReq
		if d, ok := bodies[id]; ok {
			batch.Docs = []domain.SessionDoc{d}
			delete(bodies, id)
		}
		if m, ok := metadata[id]; ok {
			batch.ChunkedDocs = []chunkedDocWire{m}
			delete(metadata, id)
			for _, id := range m.Chunks {
				if ch, ok := chunkByID[id]; ok {
					batch.ChunkObjects = append(batch.ChunkObjects, ch)
					delete(chunkByID, id)
				}
			}
		}
		return batch
	}
	out := make([]objectsReq, 0, len(snaps)+len(docs)+len(manifests))
	for _, i := range ready {
		batch := take(snaps[i].DocHash)
		batch.Snapshots = []domain.Snapshot{snaps[i]}
		out = append(out, batch)
	}
	for _, d := range docs {
		if _, ok := bodies[d.Hash]; ok {
			out = append(out, take(d.Hash))
		}
	}
	for _, m := range manifests {
		if _, ok := metadata[m.Hash]; ok {
			out = append(out, take(m.Hash))
		}
	}
	return out, nil
}
