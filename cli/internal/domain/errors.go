package domain

import "errors"

// Domain public sentinel errors (domain model, backend architecture).
// Meaningful errors shared by use-cases/adapters, converted to HTTP status/MCP error at the delivery boundary.

// ErrNotFound indicates that a snapshot/document/ref/repo does not exist.
// Occurs in SessionStore.Get*, ref resolve.
var ErrNotFound = errors.New("not found")

// ErrHashMismatch indicates a content integrity violation.
// Occurs when snap.ID != doc.Hash or rehash mismatch.
// Happens during PutSnapshot, GetDoc validation.
var ErrHashMismatch = errors.New("hash mismatch")

// ErrInvalidCIR indicates a violation of the CIR schema/invariants.
// Occurs in ProviderCodec.Decode on decode failure.
var ErrInvalidCIR = errors.New("invalid CIR")

// ErrInvalidRef indicates a violation of the ref kind/name/target format.
var ErrInvalidRef = errors.New("invalid ref")

// ErrUnsupportedCIRVersion indicates that a peer or local object uses a CIR
// version this binary cannot safely interpret.
var ErrUnsupportedCIRVersion = errors.New("unsupported CIR version")

// ErrSyncConflict indicates a pull merge conflict (fast-forward not possible).
// Occurs in SyncRepo.Pull.
var ErrSyncConflict = errors.New("sync conflict")

// ErrNoActiveSession indicates that no session file of the provider matches.
// Codex rollout lookups return it, and callers treat it as nothing to read.
var ErrNoActiveSession = errors.New("no active session")

// ErrNotGitRepo indicates the cwd is not part of a git worktree.
// cxt is a shadow of git — repository information is always read from the local .git,
// and it fails like git outside a git repository (no path fallback).
var ErrNotGitRepo = errors.New("not a git repository (or any of the parent directories): .git")

// ErrSelectionChanged stops prompt installation after a concurrent worktree move.
var ErrSelectionChanged = errors.New("context selection changed")
