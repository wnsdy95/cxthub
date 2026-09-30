package domain

import (
	"errors"
	"fmt"
)

var ErrFederationUnavailable = errors.New("enterprise identity connection unavailable")
var ErrVerifiedDomainRequired = errors.New("verify or renew this Enterprise domain before using its identity connection")
var ErrRecentIdentityLogin = errors.New("sign in to CXTHub again before linking an external identity for the first time")

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

// ErrForbidden indicates authentication is valid but permission is denied (403). Example: repository non-member.
var ErrForbidden = errors.New("forbidden")

// ErrConflict indicates a duplicate creation/state conflict (409). Example: already used invite, occupied username.
var ErrConflict = errors.New("conflict")

// ErrEffectiveMemoryCursorStale means an effective-memory continuation cursor no longer
// matches the current query state. It does not establish unchanged content.
var ErrEffectiveMemoryCursorStale = fmt.Errorf("%w: effective memory changed; restart without cursor", ErrConflict)

// ErrValidation indicates an input format violation (422). Example: invalid slug username, incorrect visibility.
var ErrValidation = errors.New("validation")

// ErrMemoryProjectionLimit rejects derived memory that exceeds its bounded
// retained payload, entry count, or rendered wire size (422).
var ErrMemoryProjectionLimit = errors.New("memory projection limit exceeded")

// ErrUnsupportedCIRVersion requires a peer upgrade before a document can be
// transferred without changing its content hash.
var ErrUnsupportedCIRVersion = errors.New("unsupported CIR version")

// ErrGitOriginMismatch indicates an attempt to connect to cxthub repo from a different folder with a different git origin (409 git_origin_mismatch). Onboarding safety measure.
var ErrGitOriginMismatch = errors.New("git origin mismatch")

// ErrStorageLimit rejects net growth while preserving existing read access.
var ErrStorageLimit = errors.New("storage limit reached; existing context remains readable")
var ErrUsageUnavailable = errors.New("storage reporting is unavailable")
var ErrDomainVerificationUnavailable = errors.New("domain verification requires transactional storage and a working DNS resolver")
