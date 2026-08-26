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

// ErrUnsupportedProvider indicates an unregistered ProviderKind in the registry.
// Occurs during codec/capture routing.
var ErrUnsupportedProvider = errors.New("unsupported provider")

// ErrUnsupportedCIRVersion indicates that a peer or local object uses a CIR
// version this binary cannot safely interpret.
var ErrUnsupportedCIRVersion = errors.New("unsupported CIR version")

// ErrUnsupportedFidelity indicates that the requested fidelity mode cannot be satisfied.
// Example: Occurs during full request in cross-provider.
// Occurs in LoadSession.
var ErrUnsupportedFidelity = errors.New("unsupported fidelity")

// ErrSyncConflict indicates a pull merge conflict (fast-forward not possible).
// Occurs in SyncRepo.Pull.
var ErrSyncConflict = errors.New("sync conflict")

// ErrNoActiveSession indicates the absence of an active session file in the cwd.
// This error occurs in CaptureSource.LocateActiveSession.
// Auto hooks gracefully exit as no-op when encountering this error.
var ErrNoActiveSession = errors.New("no active session")

// ErrBranchExists indicates an attempt to fork (checkout -b) an existing branch.
// This error has the same meaning as in git (Invariant F2): it fails silently without moving the existing branch.
var ErrBranchExists = errors.New("branch already exists")

// ErrNotGitRepo indicates the cwd is not part of a git worktree.
// cxt is a shadow of git — repository information is always read from the local .git,
// and it fails like git outside a git repository (no path fallback).
var ErrNotGitRepo = errors.New("not a git repository (or any of the parent directories): .git")
