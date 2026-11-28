// Package domain contains the core domain entities, value objects, and invariants of cxt.
//
// This package is a sink in the dependency graph — it does not import any other packages internally.
// It uses only the standard library (time, crypto/sha256, encoding/hex).
//
// Core Invariants (domain invariants):
//   - SessionDoc.Hash addresses its CIR under SessionDoc.Identity
//   - Same ContentHash ⇒ Same content (dedup key)
//
// Dependency Rules (dependency boundaries):
//   - This package must never import github.com/wnsdy95/cxthub-cli/internal/...
package domain
