// Package domain contains the core domain entities, value objects, and invariants of cxt.
//
// This package is a sink in the dependency graph — it does not import any other packages internally.
// It uses only the standard library (time, crypto/sha256, encoding/hex).
//
// Core Invariants (SPINE §4 Contract):
//   - Snapshot.ID == ContentHash(canonical_bytes(SessionDoc.CIR))
//   - Ref.Target must be a valid Snapshot.ID
//   - Same ContentHash ⇒ Same content (dedup key)
//
// Dependency Rules (SPINE §3.2 Immutability):
//   - This package must never import github.com/wnsdy95/cxthub/cli/internal/...
package domain
