package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// HashContent calculates the SHA-256 ContentHash of a byte slice.
// Format: "sha256:<lowercase-hex-64chars>".
// Same bytes ⇒ same hash (basis of dedup invariant).
func HashContent(data []byte) ContentHash {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// CanonicalBytes returns the deterministic normalized bytes of a CIRDocument.
// A single source of truth for Snapshot.ID generation (domain model, data model).
//
// Normalization rules (contract, data model):
//  1. JSON key sorting — all object keys sorted in ascending UTF-8 byte order.
//  2. Whitespace removal — compact JSON (no unnecessary whitespace/tabs).
//  3. events sorted in ascending Seq order.
//
// Implementation: first, sort events in ascending Seq order stably, then serialize
// in schema form (Event.MarshalJSON), re-parse as interface{}, and marshal again.
// encoding/json sorts map keys in ascending UTF-8 byte order and outputs compactly,
// ensuring all nested object keys are deterministically sorted.
func CanonicalBytes(doc CIRDocument) ([]byte, error) {
	if err := ValidateCIRVersion(doc); err != nil {
		return nil, fmt.Errorf("canonical bytes: %w", err)
	}
	doc.Events = canonicalEvents(doc.Events)

	first, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("canonical bytes: marshal: %w", err)
	}
	var generic interface{}
	decoder := json.NewDecoder(bytes.NewReader(first))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		return nil, fmt.Errorf("canonical bytes: reparse: %w", err)
	}
	out, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("canonical bytes: remarshal: %w", err)
	}
	return out, nil
}

func canonicalEvents(events []Event) []Event {
	sorted := make([]Event, len(events))
	copy(sorted, events)
	for i := range sorted {
		if sorted[i].Replacement != nil {
			sorted[i].Replacement = canonicalEvents(sorted[i].Replacement)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	return sorted
}

// ValidateSessionDocHash recalculates the claimed hash from wire as a canonical CIR and validates it.
func ValidateSessionDocHash(doc SessionDoc) error {
	if err := ValidateContentHash(doc.Hash); err != nil {
		return err
	}
	canonical, err := CanonicalBytes(doc.CIR)
	if err != nil {
		return err
	}
	if got := HashContent(canonical); got != doc.Hash {
		return fmt.Errorf("%w: doc hash mismatch: got %s want %s", ErrHashMismatch, got, doc.Hash)
	}
	return nil
}

// MemoryDigestHash calculates the content hash of a memory object JSON shared by server/local.
func MemoryDigestHash(digest MemoryDigest) (ContentHash, error) {
	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}
	return HashContent(data), nil
}
