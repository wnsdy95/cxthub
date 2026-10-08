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
// Implementation: sort events in ascending Seq order stably, then normalize the
// envelope and each event separately through schema JSON, a UseNumber generic
// representation, and sorted-key JSON. The wire identity remains unchanged.
// encoding/json sorts map keys in ascending UTF-8 byte order and outputs compactly,
// ensuring all nested object keys are deterministically sorted.
func CanonicalBytes(doc CIRDocument) ([]byte, error) {
	return canonicalBytes(doc, nil)
}

// observe receives the canonical bytes already computed for each sorted event.
// Callers must not admit this provisional evidence before full hash validation.
func canonicalBytes(doc CIRDocument, observe func([]byte, int)) ([]byte, error) {
	if err := ValidateCIRVersion(doc); err != nil {
		return nil, fmt.Errorf("canonical bytes: %w", err)
	}
	// Normalize one event at a time. Whole-document generic JSON otherwise
	// duplicates every cumulative transcript string during each hash check.
	env, err := canonicalJSON(doc.Envelope)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(`{"envelope":`)
	out.Write(env)
	out.WriteString(`,"events":[`)
	for i, event := range canonicalEvents(doc.Events) {
		raw, err := canonicalJSON(event)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(raw)
		if observe != nil {
			observe(raw, event.Seq)
		}
	}
	out.WriteString(`]}`)
	return out.Bytes(), nil
}

func canonicalJSON(value any) ([]byte, error) {
	first, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonical bytes: marshal: %w", err)
	}
	var generic any
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

// ValidateSessionDocHash is the legacy-only production boundary. Dual readers
// must explicitly use VerifySessionDocIdentity once their consumers are wired.
func ValidateSessionDocHash(doc SessionDoc) error {
	return validateSessionDocHash(doc, nil)
}

func validateSessionDocHash(doc SessionDoc, observe func([]byte, int)) error {
	if err := ValidateContentHash(doc.Hash); err != nil {
		return err
	}
	if doc.Identity != DocumentIdentityLegacy {
		return fmt.Errorf("%w: legacy document verifier requires absent identity", ErrUnsupportedDocumentIdentity)
	}
	canonical, err := canonicalBytes(doc.CIR, observe)
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
