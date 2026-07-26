package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// HashContent calculates the SHA-256 ContentHash of a byte slice ("sha256:<hex>").
func HashContent(data []byte) ContentHash {
	h := sha256.Sum256(data)
	return ContentHash("sha256:" + hex.EncodeToString(h[:]))
}

// CanonicalBytes returns the deterministic canonical bytes of a CIRDocument (events seq sorted + key sorted + compact).
//
// Server push re-hashes this byte to compare with the content hash provided by the client.
func CanonicalBytes(doc CIRDocument) ([]byte, error) {
	sorted := make([]CIREvent, len(doc.Events))
	copy(sorted, doc.Events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	doc.Events = sorted

	first, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("canonical bytes: marshal: %w", err)
	}
	var generic any
	if err := json.Unmarshal(first, &generic); err != nil {
		return nil, fmt.Errorf("canonical bytes: reparse: %w", err)
	}
	out, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("canonical bytes: remarshal: %w", err)
	}
	return out, nil
}

// ValidateSessionDocHash recalculates the claimed hash at the wire/storage boundary into a canonical CIR.
func ValidateSessionDocHash(doc SessionDoc) error {
	if err := ValidateContentHash(doc.Hash); err != nil {
		return err
	}
	canonical, err := CanonicalBytes(doc.CIR)
	if err != nil {
		return fmt.Errorf("%w: doc canonicalization failed: %v", ErrIntegrity, err)
	}
	if got := HashContent(canonical); got != doc.Hash {
		return fmt.Errorf("%w: doc hash mismatch: got %s want %s", ErrIntegrity, got, doc.Hash)
	}
	return nil
}

// MemoryDigestHash calculates the wire JSON content hash of a memory object.
func MemoryDigestHash(digest MemoryDigest) (ContentHash, error) {
	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}
	return HashContent(data), nil
}
