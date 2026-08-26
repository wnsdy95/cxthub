package domain

import "encoding/json"

// SnapshotStateHash fingerprints the mutable snapshot projection replicated
// after object creation. Snapshot.ID detects new immutable objects; this token
// detects metadata-only changes to branch/message promotion, memory attachment,
// and the versioned graft register.
//
// Immutable fields are deliberately excluded. In particular, PostgreSQL may
// assign CreatedAt at insert time, so hashing the whole wire object would make
// equivalent replicas appear changed forever.
func SnapshotStateHash(s Snapshot) (ContentHash, error) {
	state := snapshotStateWire{
		ID:           string(s.ID),
		Branch:       s.Branch,
		MemoryHash:   string(s.MemoryHash),
		Message:      s.Message,
		Grafted:      s.Grafted,
		GraftParents: snapshotStateHashes(s.GraftParents),
		GraftSeq:     s.GraftSeq,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

type snapshotStateWire struct {
	ID           string   `json:"id"`
	Branch       string   `json:"branch"`
	MemoryHash   string   `json:"memory_hash"`
	Message      string   `json:"message"`
	Grafted      bool     `json:"grafted"`
	GraftParents []string `json:"graft_parents"`
	GraftSeq     uint64   `json:"graft_seq"`
}

func snapshotStateHashes(in []ContentHash) []string {
	out := make([]string, len(in))
	for i, hash := range in {
		out[i] = string(hash)
	}
	return out
}
