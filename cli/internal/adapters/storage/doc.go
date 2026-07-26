// Package storage contains the implementation of a content-addressed local file store for the SessionStore outbound port.
//
// It is a client-specific local store. Mimics a USER DECISION 2 (self-store + git semantics);
// does not use a real git object DB. Uses content-addressing to mimic git's mental model (SQL not used).
// Remote synchronization (RemoteSync) is handled by adapters/backendclient (role separation).
//
// repo local .cxt/ store layout (repoRoot = repo working tree root, corresponding to git's .git):
//   - .cxt/objects/<sha256-hex>  immutable blob: SessionDoc(CIR) / SnapshotMeta / MemoryDigest (flat)
//   - .cxt/refs/heads/<branch>   branch ref → target Snapshot.ID(ContentHash)
//   - .cxt/refs/tags/<name>      tag ref → target Snapshot.ID
//   - .cxt/HEAD                  symbolic ref (current branch)
//   - .cxt/config                local config (remote URL, TeamIdentity)
//
// Implementations:
//   - FileStore: A content-addressed local file store implementing SessionStore.
//
// Dependency rules (domain model):
//   - Only imports domain + ports.outbound.
//   - Does not import other adapters/* packages.
package storage
