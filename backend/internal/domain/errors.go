package domain

import "errors"

// Server domain sentinel errors. Mapped from sync protocol error codes/HTTP status at delivery/http boundary.

// ErrNotFound indicates the absence of repo/snapshot/doc/ref (404).
var ErrNotFound = errors.New("not found")

// ErrIntegrity indicates an integrity violation (422 integrity_violation).
var ErrIntegrity = errors.New("integrity violation")

// ErrNonFastForward indicates that ref CAS advancement is not a fast-forward (409 non_fast_forward).
var ErrNonFastForward = errors.New("non fast-forward")

// ErrRefConflict indicates a mismatch in ref CAS expected (concurrent update) (409).
var ErrRefConflict = errors.New("ref conflict")

// ErrBranchArchived prevents a stale replica from recreating a branch pointer
// after a newer immutable lifecycle event archived it.
var ErrBranchArchived = errors.New("branch is archived")

// ErrUnauthorized indicates an invalid or missing token (401).
var ErrUnauthorized = errors.New("unauthorized")

// ErrForbidden indicates authentication is valid but permission is denied (403). Example: workspace non-member.
var ErrForbidden = errors.New("forbidden")

// ErrConflict indicates a duplicate creation/state conflict (409). Example: already used invite, occupied username.
var ErrConflict = errors.New("conflict")

// ErrValidation indicates an input format violation (422). Example: invalid slug username, incorrect visibility.
var ErrValidation = errors.New("validation")

// ErrUnsupportedCIRVersion requires a peer upgrade before a document can be
// transferred without changing its content hash.
var ErrUnsupportedCIRVersion = errors.New("unsupported CIR version")

// ErrGitOriginMismatch indicates an attempt to connect to cxthub repo from a different folder with a different git origin (409 git_origin_mismatch). Onboarding safety measure.
var ErrGitOriginMismatch = errors.New("git origin mismatch")
