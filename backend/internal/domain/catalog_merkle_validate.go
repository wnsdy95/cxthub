package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Backend mirror of the CLI catalog validation boundary. These definitions
// stay private to Merkle so the independent modules do not share Go code.
func catalogMerkleInvalid(message string) error {
	return fmt.Errorf("%w: catalog merkle %s", ErrValidation, message)
}

func catalogMerkleValidateEntry(repo ContentHash, entry CatalogEntry) error {
	return catalogMerkleValidateStoredEntry(string(repo), entry)
}

// catalogMerkleValidateStoredEntry validates one after-image, never its graph closure. A
// transaction can publish referenced objects on a later page of the same run.
func catalogMerkleValidateStoredEntry(repo string, entry CatalogEntry) error {
	_, err := decodeCatalogMerkleEntry(repo, entry)
	return err
}

type catalogMerkleEntity struct {
	snapshot Snapshot
	ref      Ref
	protocol int
}

type catalogMerkleProtocol struct {
	ContextProtocol int `json:"context_protocol"`
}

func catalogMerkleRefKey(key string) ([2]string, error) {
	var names []string
	if err := decodeCatalogMerkleJSON([]byte(key), &names); err != nil || len(names) != 2 {
		return [2]string{}, catalogMerkleInvalid("ref key")
	}
	if ValidateRefName(RefKind(names[0]), names[1]) != nil {
		return [2]string{}, catalogMerkleInvalid("ref key identity")
	}
	return [2]string{names[0], names[1]}, nil
}

func catalogMerkleHistoryIDValid(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func decodeCatalogMerkleEntry(repo string, e CatalogEntry) (catalogMerkleEntity, error) {
	var out catalogMerkleEntity
	if ValidateContentHash(ContentHash(repo)) != nil || e.Sequence < 0 {
		return out, catalogMerkleInvalid("entry scope or sequence")
	}
	var refKey [2]string
	switch e.Kind {
	case "snapshot":
		if ValidateContentHash(ContentHash(e.Key)) != nil {
			return out, catalogMerkleInvalid("snapshot key")
		}
	case "ref":
		var err error
		refKey, err = catalogMerkleRefKey(e.Key)
		if err != nil {
			return out, err
		}
	case "history":
		if !catalogMerkleHistoryIDValid(e.Key) {
			return out, catalogMerkleInvalid("history key")
		}
	case "protocol":
		if e.Key != repo {
			return out, catalogMerkleInvalid("protocol key")
		}
	default:
		return out, catalogMerkleInvalid("unknown entry kind")
	}
	if e.Deleted {
		if len(e.Value) != 0 {
			return out, catalogMerkleInvalid("tombstone has a value")
		}
		return out, nil
	}
	switch e.Kind {
	case "snapshot":
		if err := decodeCatalogMerkleJSON(e.Value, &out.snapshot); err != nil {
			return out, err
		}
		s := out.snapshot
		if string(s.RepoID) != repo || string(s.ID) != e.Key || validateCatalogMerkleSnapshot(s) != nil {
			return out, catalogMerkleInvalid("snapshot metadata")
		}
	case "ref":
		if err := decodeCatalogMerkleJSON(e.Value, &out.ref); err != nil {
			return out, err
		}
		r := out.ref
		if string(r.RepoID) != repo || string(r.Kind) != refKey[0] || r.Name != refKey[1] || ValidateRef(r) != nil || (r.Kind != RefHead && r.Symbolic != "") {
			return out, catalogMerkleInvalid("ref metadata")
		}
		if _, _, err := ParseBranchLifecycleRef(r); err != nil {
			return out, catalogMerkleInvalid("lifecycle ref metadata")
		}
	case "history":
		var event HistoryEvent
		if err := decodeCatalogMerkleJSON(e.Value, &event); err != nil {
			return out, err
		}
		if event.RepoID != repo || event.ID != e.Key || ValidateHistoryEvent(event) != nil {
			return out, catalogMerkleInvalid("history metadata")
		}
	case "protocol":
		var protocol catalogMerkleProtocol
		if err := decodeCatalogMerkleJSON(e.Value, &protocol); err != nil {
			return out, err
		}
		if protocol.ContextProtocol < 0 || protocol.ContextProtocol > 1 {
			return out, catalogMerkleInvalid("unsupported context protocol")
		}
		out.protocol = protocol.ContextProtocol
	}
	return out, nil
}

// Match the object adapter's identity/hash validation without importing an
// adapter into domain. Do not invent newer required presentation metadata for
// historical objects accepted by the existing pull path.
func validateCatalogMerkleSnapshot(s Snapshot) error {
	if ValidateContentHash(s.ID) != nil || ValidateContentHash(s.DocHash) != nil || s.ID != s.DocHash || s.GraftSeq > MaxGraftSeq || s.CompactionCount < 0 {
		return catalogMerkleInvalid("snapshot identity or scalar")
	}
	for _, hash := range []ContentHash{s.MemoryHash, s.ClaudeSettings, s.AgentsSettings, s.CodexSettings} {
		if ValidateOptionalContentHash(hash) != nil {
			return catalogMerkleInvalid("snapshot attachment")
		}
	}
	for _, parents := range [][]ContentHash{s.Parents, s.GraftParents} {
		for _, parent := range parents {
			if ValidateContentHash(parent) != nil {
				return catalogMerkleInvalid("snapshot parent")
			}
		}
	}
	_, err := SnapshotStateHash(s)
	return err
}
