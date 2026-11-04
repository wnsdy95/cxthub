package domain

import (
	"fmt"
	"slices"
)

func ValidateSnapshotHashes(snapshot Snapshot) error {
	for _, hash := range []ContentHash{snapshot.RepoID, snapshot.ID, snapshot.DocHash} {
		if err := ValidateContentHash(hash); err != nil {
			return err
		}
	}
	if snapshot.ID != snapshot.DocHash {
		return fmt.Errorf("%w: snapshot id must equal doc_hash", ErrIntegrity)
	}
	for _, hash := range []ContentHash{snapshot.MemoryHash, snapshot.ClaudeSettings, snapshot.AgentsSettings, snapshot.CodexSettings} {
		if err := ValidateOptionalContentHash(hash); err != nil {
			return err
		}
	}
	for _, parent := range snapshot.Parents {
		if err := ValidateContentHash(parent); err != nil {
			return err
		}
	}
	for _, parent := range snapshot.GraftParents {
		if err := ValidateContentHash(parent); err != nil {
			return err
		}
	}
	return nil
}

func ValidateSnapshotGraphUpdate(existing, incoming []Snapshot) error {
	graph := make(map[ContentHash]Snapshot, len(existing)+len(incoming))
	for _, snap := range existing {
		graph[snap.ID] = snap
	}
	incomingIDs := make(map[ContentHash]bool, len(incoming))
	for _, snap := range incoming {
		if incomingIDs[snap.ID] {
			return fmt.Errorf("%w: duplicate snapshot %s in commit batch", ErrIntegrity, snap.ID)
		}
		incomingIDs[snap.ID] = true
		if current, ok := graph[snap.ID]; ok {
			if !slices.Equal(current.Parents, snap.Parents) {
				return fmt.Errorf("%w: snapshot %s parent metadata disagrees with stored object", ErrIntegrity, snap.ID)
			}
			allowedGrafts := make(map[ContentHash]bool, len(current.GraftParents))
			for _, parent := range current.GraftParents {
				allowedGrafts[parent] = true
			}
			for _, parent := range snap.GraftParents {
				if !allowedGrafts[parent] {
					return fmt.Errorf("%w: client cannot add graft parent to snapshot %s", ErrIntegrity, snap.ID)
				}
			}
			if snap.Grafted && !current.Grafted {
				return fmt.Errorf("%w: client cannot mark snapshot %s as grafted", ErrIntegrity, snap.ID)
			}
			if snap.GraftSeq > current.GraftSeq {
				return fmt.Errorf("%w: client cannot advance graft sequence for snapshot %s", ErrIntegrity, snap.ID)
			}
			continue
		}
		if snap.Grafted || len(snap.GraftParents) > 0 || snap.GraftSeq != 0 {
			return fmt.Errorf("%w: new snapshot %s contains server-owned graft metadata", ErrIntegrity, snap.ID)
		}
		graph[snap.ID] = snap
	}

	state := make(map[ContentHash]uint8, len(graph))
	var visit func(ContentHash) error
	visit = func(id ContentHash) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: snapshot graph contains a cycle at %s", ErrIntegrity, id)
		case 2:
			return nil
		}
		snap, ok := graph[id]
		if !ok {
			return fmt.Errorf("%w: missing snapshot parent %s", ErrIntegrity, id)
		}
		state[id] = 1
		seenParents := map[ContentHash]bool{}
		for _, parent := range snap.ReachabilityParents() {
			if parent == id {
				return fmt.Errorf("%w: snapshot %s cannot parent itself", ErrIntegrity, id)
			}
			if seenParents[parent] {
				return fmt.Errorf("%w: snapshot %s contains duplicate parent %s", ErrIntegrity, id, parent)
			}
			seenParents[parent] = true
			if _, ok := graph[parent]; !ok {
				return fmt.Errorf("%w: snapshot %s references missing parent %s", ErrIntegrity, id, parent)
			}
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range incomingIDs {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
